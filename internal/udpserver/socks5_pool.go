// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
//
// Step 17 — Upstream SOCKS5 connection pool.
//
// When USE_EXTERNAL_SOCKS5=true the server chains every client stream
// through an external SOCKS5 proxy. Each fresh stream pays:
//
//   1. TCP handshake to the proxy   (1 RTT)
//   2. SOCKS5 greeting + method     (1 RTT)
//   3. (optional) user/pass auth    (1 RTT)
//   4. CONNECT request + reply      (1 RTT, unavoidable — target-specific)
//
// On lossy mobile links the extra 2-3 RTTs of (1)+(2)+(3) can dominate
// stream-open latency. The pool keeps a small set of TCP connections
// idle that have already completed (1)+(2)+(3) — but have NOT yet sent
// a CONNECT request. When a new stream arrives we pop one of these
// "primed" connections and only pay (4).
//
// Lifecycle / invariants:
//   * The pool never holds a connection that has already received a
//     CONNECT reply — primed entries are pre-CONNECT only. This is
//     critical: after CONNECT the TCP connection becomes a tunnel for
//     one specific target and is no longer reusable.
//   * Each entry has a wall-clock TTL. When TTL expires the entry is
//     closed and removed. The reaper goroutine wakes once per ~TTL/4
//     period.
//   * On server shutdown every idle connection is closed.
//   * The pool is safe for concurrent Get / Put / Close.
//   * Get prefers the most-recently-added entry (LIFO) so connections
//     that have been idle longest age out via TTL rather than spinning
//     in/out of use.
//
// Wire compatibility: pooling lives entirely on the proxy side of the
// VPN tunnel — it changes nothing about the on-wire DNS/UDP protocol
// the client speaks.
// ==============================================================================

package udpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	Enums "masterdnsvpn-go/internal/enums"
	"masterdnsvpn-go/internal/logger"
)

// socks5PrimedConn is a TCP connection to the upstream SOCKS5 proxy that
// has already completed greeting and (optional) authentication. The
// remaining work to make it usable is a single CONNECT request + reply
// for the desired target.
type socks5PrimedConn struct {
	conn      net.Conn
	addedAt   time.Time
	idleUntil time.Time
}

// socks5UpstreamPool maintains a small set of pre-authenticated TCP
// connections to a single upstream SOCKS5 proxy address. The zero value
// is a valid disabled pool — every Get returns (nil, false). Operators
// enable the pool by configuring SOCKS5PoolMaxIdle > 0; the dialing
// hot-path then calls Get to skip greeting+auth on new streams.
type socks5UpstreamPool struct {
	mu      sync.Mutex
	entries []socks5PrimedConn

	maxIdle  int
	idleTTL  time.Duration
	prewarm  int
	log      *logger.Logger
	addr     string
	hasAuth  bool
	greeting func() []byte                            // returns the greeting bytes (no-auth or user/pass)
	prime    func(conn net.Conn) error                // performs greeting+auth on a fresh conn
	dial     func(ctx context.Context) (net.Conn, error)

	// Lifetime / reaper bookkeeping. closed is set during Close so Put
	// rejects further inserts (instead of leaking on shutdown). The
	// reaper goroutine watches reaperStop. Step 19 added reaperWG so
	// callers can deterministically join the reaper exit (previously
	// only Close could signal stop, but there was no way to wait).
	closed     bool
	reaperOnce sync.Once
	reaperStop chan struct{}
	reaperWG   sync.WaitGroup

	// Lightweight counters surfaced via the metrics package. Stored as
	// regular int64 under the same mu lock — pool ops are not on a
	// per-packet hot path so atomic.* would be over-engineered.
	stats socks5PoolStats
}

// socks5PoolStats is the diagnostic surface visible through
// (s *Server) socks5PoolSnapshot. It is intentionally simple — a deeper
// observability story can layer on top via metrics.* later without
// breaking the API.
type socks5PoolStats struct {
	Gets           int64 // every Get call
	Hits           int64 // Get that returned a primed conn
	Puts           int64 // every Put call (regardless of acceptance)
	Inserted       int64 // Put that actually accepted the conn
	Evicted        int64 // entries closed due to TTL expiry
	OverflowClosed int64 // entries closed because pool was full at Put time
	PrewarmFailed  int64 // background prewarm dial attempts that failed
	PrewarmSucceeded int64
}

// newSOCKS5UpstreamPool builds a pool that calls the supplied dial+prime
// functions whenever it needs to manufacture a fresh primed connection.
// Passing maxIdle == 0 returns an explicitly disabled pool (Get always
// misses, Put always rejects). dialFn / primeFn are required when
// maxIdle > 0 — the constructor returns nil otherwise so callers can
// detect misconfiguration cheaply.
func newSOCKS5UpstreamPool(
	maxIdle int,
	idleTTL time.Duration,
	prewarm int,
	addr string,
	hasAuth bool,
	greeting func() []byte,
	dialFn func(ctx context.Context) (net.Conn, error),
	primeFn func(conn net.Conn) error,
	log *logger.Logger,
) *socks5UpstreamPool {
	if maxIdle <= 0 {
		return &socks5UpstreamPool{} // disabled pool
	}
	if idleTTL <= 0 {
		idleTTL = 30 * time.Second
	}
	if prewarm < 0 {
		prewarm = 0
	}
	if prewarm > maxIdle {
		prewarm = maxIdle
	}
	return &socks5UpstreamPool{
		maxIdle:    maxIdle,
		idleTTL:    idleTTL,
		prewarm:    prewarm,
		log:        log,
		addr:       addr,
		hasAuth:    hasAuth,
		greeting:   greeting,
		prime:      primeFn,
		dial:       dialFn,
		entries:    make([]socks5PrimedConn, 0, maxIdle),
		reaperStop: make(chan struct{}),
	}
}

// Enabled reports whether the pool was constructed with maxIdle > 0. It
// is the cheap (no-lock) check callers use on the hot path before
// reaching for Get/Put.
func (p *socks5UpstreamPool) Enabled() bool {
	return p != nil && p.maxIdle > 0
}

// Get pops the most-recently-inserted primed connection or returns
// (nil, false) if the pool is empty / disabled / closed. Connections
// past their TTL are closed and skipped — the pool always returns a
// fresh-enough connection or nothing at all.
func (p *socks5UpstreamPool) Get() (net.Conn, bool) {
	if !p.Enabled() {
		return nil, false
	}
	now := time.Now()
	p.mu.Lock()
	p.stats.Gets++
	for len(p.entries) > 0 {
		idx := len(p.entries) - 1
		entry := p.entries[idx]
		p.entries[idx] = socks5PrimedConn{} // help GC
		p.entries = p.entries[:idx]
		if entry.conn == nil {
			continue
		}
		if !entry.idleUntil.IsZero() && now.After(entry.idleUntil) {
			p.stats.Evicted++
			p.mu.Unlock()
			_ = entry.conn.Close()
			p.mu.Lock()
			continue
		}
		p.stats.Hits++
		p.mu.Unlock()
		return entry.conn, true
	}
	p.mu.Unlock()
	return nil, false
}

// Put attempts to re-insert a primed connection that wasn't actually
// used (e.g. the caller decided to take a different code path before
// sending CONNECT). It rejects the insert and closes the connection
// when the pool is full, closed, or disabled. Returns true iff the
// connection was kept.
//
// NOTE: callers MUST NOT Put a connection on which they have already
// sent a SOCKS5 CONNECT request. Doing so would corrupt the pool —
// the next Get user would see CONNECT-reply bytes instead of a clean
// primed state.
func (p *socks5UpstreamPool) Put(conn net.Conn) bool {
	if conn == nil {
		return false
	}
	if !p.Enabled() {
		_ = conn.Close()
		return false
	}
	now := time.Now()
	p.mu.Lock()
	p.stats.Puts++
	if p.closed || len(p.entries) >= p.maxIdle {
		p.stats.OverflowClosed++
		p.mu.Unlock()
		_ = conn.Close()
		return false
	}
	p.entries = append(p.entries, socks5PrimedConn{
		conn:      conn,
		addedAt:   now,
		idleUntil: now.Add(p.idleTTL),
	})
	p.stats.Inserted++
	p.mu.Unlock()
	return true
}

// Snapshot returns a copy of the current pool statistics. Cheap; takes
// the pool mutex once. Safe to call concurrently with Get/Put.
func (p *socks5UpstreamPool) Snapshot() socks5PoolStats {
	if p == nil {
		return socks5PoolStats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// Len returns the current number of idle connections. Useful in tests
// and for the prewarm worker. O(1).
func (p *socks5UpstreamPool) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// reapExpired closes every entry whose TTL has elapsed. Returns the
// number of entries actually closed; useful for tests.
func (p *socks5UpstreamPool) reapExpired(now time.Time) int {
	if !p.Enabled() {
		return 0
	}
	p.mu.Lock()
	expired := make([]net.Conn, 0)
	live := p.entries[:0]
	for _, entry := range p.entries {
		if entry.conn == nil {
			continue
		}
		if !entry.idleUntil.IsZero() && now.After(entry.idleUntil) {
			expired = append(expired, entry.conn)
			p.stats.Evicted++
			continue
		}
		live = append(live, entry)
	}
	// Zero out the trailing slots that are no longer part of live so the
	// underlying array doesn't pin closed conns alive in GC.
	for i := len(live); i < len(p.entries); i++ {
		p.entries[i] = socks5PrimedConn{}
	}
	p.entries = live
	p.mu.Unlock()
	for _, conn := range expired {
		_ = conn.Close()
	}
	return len(expired)
}

// refillOnce dials and primes connections until the idle count reaches
// prewarm or the first error. It returns the number of connections that
// were successfully added. Safe to call from the prewarm goroutine —
// every dial uses ctx for cancellation.
func (p *socks5UpstreamPool) refillOnce(ctx context.Context) int {
	if !p.Enabled() || p.prewarm <= 0 {
		return 0
	}
	added := 0
	for {
		if ctx.Err() != nil {
			return added
		}
		p.mu.Lock()
		if p.closed || len(p.entries) >= p.prewarm {
			p.mu.Unlock()
			return added
		}
		p.mu.Unlock()

		conn, err := p.dial(ctx)
		if err != nil || conn == nil {
			p.mu.Lock()
			p.stats.PrewarmFailed++
			p.mu.Unlock()
			return added
		}
		if err := p.prime(conn); err != nil {
			_ = conn.Close()
			p.mu.Lock()
			p.stats.PrewarmFailed++
			p.mu.Unlock()
			return added
		}
		// Reuse Put's accounting so OverflowClosed remains a real
		// indicator. We expect Put to succeed because the loop checked
		// len(entries) < prewarm above; race with concurrent Puts is
		// fine because Put just closes the conn on overflow.
		if !p.Put(conn) {
			return added
		}
		p.mu.Lock()
		p.stats.PrewarmSucceeded++
		p.mu.Unlock()
		added++
	}
}

// startReaper launches a goroutine that periodically reaps expired
// entries and tops up the pool to the prewarm target. The reaper exits
// when ctx is cancelled OR Close is called. Idempotent.
func (p *socks5UpstreamPool) startReaper(ctx context.Context) {
	if !p.Enabled() {
		return
	}
	p.reaperOnce.Do(func() {
		p.reaperWG.Add(1)
		go func() {
			defer p.reaperWG.Done()
			p.reaperLoop(ctx)
		}()
	})
}

// WaitForShutdown blocks until the reaper goroutine has returned, or
// the supplied timeout elapses. Returns true on clean exit, false on
// timeout. Caller must have already cancelled ctx or called Close,
// otherwise this will always hit the timeout. Added in Step 19 to
// give the server a deterministic teardown.
func (p *socks5UpstreamPool) WaitForShutdown(timeout time.Duration) bool {
	if p == nil || !p.Enabled() {
		return true
	}
	done := make(chan struct{})
	go func() {
		p.reaperWG.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return true
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

func (p *socks5UpstreamPool) reaperLoop(ctx context.Context) {
	interval := p.idleTTL / 4
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	// First refill happens immediately so steady-state load doesn't
	// have to wait one full tick to see the prewarm benefit.
	_ = p.refillOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.reaperStop:
			return
		case <-tick.C:
			now := time.Now()
			_ = p.reapExpired(now)
			_ = p.refillOnce(ctx)
		}
	}
}

// Close marks the pool as terminated and closes every idle connection.
// Subsequent Put calls reject; Get returns no-hit (pool is drained).
// Safe to call multiple times.
func (p *socks5UpstreamPool) Close() {
	if p == nil || !p.Enabled() {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns := make([]net.Conn, 0, len(p.entries))
	for _, entry := range p.entries {
		if entry.conn != nil {
			conns = append(conns, entry.conn)
		}
	}
	for i := range p.entries {
		p.entries[i] = socks5PrimedConn{}
	}
	p.entries = p.entries[:0]
	p.mu.Unlock()

	// Signal the reaper. Use non-blocking close to keep Close idempotent
	// even when called before startReaper.
	select {
	case <-p.reaperStop:
		// already closed
	default:
		close(p.reaperStop)
	}

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// performExternalSOCKS5Greeting runs steps (1)-(3) of the upstream
// handshake on a freshly-dialed TCP connection: send greeting, parse
// reply, run user/pass auth if requested. It returns nil on a primed
// connection ready for CONNECT, or an error (and CLOSES conn) otherwise.
//
// This is extracted from dialExternalSOCKS5TargetContext so the pool
// constructor can call it on prewarmed connections.
func (s *Server) performExternalSOCKS5Greeting(conn net.Conn) error {
	if s == nil || conn == nil {
		return errors.New("server or conn nil")
	}

	timeout := s.socksConnectTimeout
	if timeout <= 0 {
		timeout = s.cfg.SOCKSConnectTimeout()
	}
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	if err := writeAll(conn, s.externalSOCKS5Greeting()); err != nil {
		return &upstreamSOCKS5Error{packetType: Enums.PACKET_SOCKS5_UPSTREAM_UNAVAILABLE, err: err}
	}

	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return &upstreamSOCKS5Error{packetType: Enums.PACKET_SOCKS5_UPSTREAM_UNAVAILABLE, err: err}
	}
	if greeting[0] != 0x05 {
		return &upstreamSOCKS5Error{
			packetType: Enums.PACKET_SOCKS5_UPSTREAM_UNAVAILABLE,
			err:        errors.New("upstream proxy is not a valid SOCKS5 server"),
		}
	}
	if err := s.handleExternalSOCKS5Auth(conn, greeting[1]); err != nil {
		return err
	}

	// Reset deadline — a primed connection sitting in the pool must not
	// inherit the handshake timeout. The dialer that subsequently runs
	// CONNECT installs its own deadline.
	_ = conn.SetDeadline(time.Time{})
	return nil
}

// buildSOCKS5UpstreamPool wires a pool tailored to this server's
// upstream-SOCKS5 configuration. Returns nil when pooling is disabled.
// Safe to call from New() before the server starts running.
func (s *Server) buildSOCKS5UpstreamPool() *socks5UpstreamPool {
	if s == nil || !s.useExternalSOCKS5 {
		return nil
	}
	maxIdle := s.cfg.SOCKS5PoolMaxIdle
	if maxIdle <= 0 {
		return nil
	}
	idleTTL := time.Duration(s.cfg.SOCKS5PoolIdleTTLSeconds * float64(time.Second))
	if idleTTL <= 0 {
		idleTTL = 30 * time.Second
	}
	return newSOCKS5UpstreamPool(
		maxIdle,
		idleTTL,
		s.cfg.SOCKS5PoolPrewarm,
		s.externalSOCKS5Address,
		s.externalSOCKS5Auth,
		s.externalSOCKS5Greeting,
		func(ctx context.Context) (net.Conn, error) {
			return s.dialTCPTargetContext(ctx, s.externalSOCKS5Address)
		},
		s.performExternalSOCKS5Greeting,
		s.log,
	)
}
