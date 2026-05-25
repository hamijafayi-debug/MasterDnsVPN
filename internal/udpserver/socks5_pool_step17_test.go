// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
//
// Step 17 — Upstream SOCKS5 connection pool unit tests.
//
// These tests are deliberately self-contained: they spin up a tiny in-process
// "fake SOCKS5 proxy" that the pool dials into, exercise every edge of the
// pool API (Get / Put / TTL eviction / Close / overflow / prewarm), and
// validate the end-to-end CONNECT path through dialExternalSOCKS5TargetContext.
// No real network or external dependencies are touched.
// ==============================================================================

package udpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ----------------------------------------------------------------------------
// In-process fake SOCKS5 proxy
// ----------------------------------------------------------------------------

// fakeSOCKS5Proxy handles the upstream side of the SOCKS5 handshake for the
// tests. It supports no-auth, accepts every CONNECT, and lets the caller
// inject specific behaviours per accepted connection (close-after-greeting,
// stall, etc.) via the optional handlerHook.
type fakeSOCKS5Proxy struct {
	t        *testing.T
	listener net.Listener
	addr     string
	closing  atomic.Bool

	// Per-connection counters surfaced for assertions.
	greetingsServed atomic.Int64
	connectsServed  atomic.Int64
	rawAccepted     atomic.Int64

	// Optional hook invoked after greeting but before CONNECT parsing.
	// Returning true tells the proxy to abort this connection (close).
	// Lets a test simulate a "stale primed conn closed by upstream".
	afterGreetingHook func(idx int64) bool
}

func newFakeSOCKS5Proxy(t *testing.T) *fakeSOCKS5Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &fakeSOCKS5Proxy{
		t:        t,
		listener: ln,
		addr:     ln.Addr().String(),
	}
	go p.serve()
	t.Cleanup(p.Close)
	return p
}

func (p *fakeSOCKS5Proxy) Close() {
	if p.closing.Swap(true) {
		return
	}
	_ = p.listener.Close()
}

func (p *fakeSOCKS5Proxy) serve() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		idx := p.rawAccepted.Add(1)
		go p.handle(conn, idx)
	}
}

func (p *fakeSOCKS5Proxy) handle(conn net.Conn, idx int64) {
	defer conn.Close()
	// Greeting: read 3 bytes [VER=5, NMETHODS=1, METHOD=0].
	greeting := make([]byte, 3)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return
	}
	if greeting[0] != 0x05 {
		return
	}
	// Reply: VER=5, METHOD=0 (no auth).
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	p.greetingsServed.Add(1)

	if p.afterGreetingHook != nil && p.afterGreetingHook(idx) {
		return // close after greeting — simulates a stale pool entry
	}

	// CONNECT request: VER=5, CMD=1, RSV=0, ATYP, addr..., port(2)
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	if header[0] != 0x05 || header[1] != 0x01 {
		return
	}
	switch header[3] {
	case 0x01: // IPv4
		extra := make([]byte, 4+2)
		if _, err := io.ReadFull(conn, extra); err != nil {
			return
		}
	case 0x03: // domain
		var dlen [1]byte
		if _, err := io.ReadFull(conn, dlen[:]); err != nil {
			return
		}
		extra := make([]byte, int(dlen[0])+2)
		if _, err := io.ReadFull(conn, extra); err != nil {
			return
		}
	case 0x04: // IPv6
		extra := make([]byte, 16+2)
		if _, err := io.ReadFull(conn, extra); err != nil {
			return
		}
	default:
		return
	}
	// Reply success: VER=5, REP=0, RSV=0, ATYP=1, 0.0.0.0:0
	reply := []byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if _, err := conn.Write(reply); err != nil {
		return
	}
	p.connectsServed.Add(1)

	// Block until the test caller closes the connection.
	io.Copy(io.Discard, conn)
}

// buildPooledServer creates a Server configured for upstream SOCKS5 with the
// pool set to maxIdle/idleTTL/prewarm and pointed at the supplied fake proxy.
// The returned Server is NOT Running — tests drive the dial path directly.
func buildPooledServer(t *testing.T, proxy *fakeSOCKS5Proxy, maxIdle int, idleTTL time.Duration, prewarm int) *Server {
	t.Helper()
	s := &Server{
		useExternalSOCKS5:     true,
		externalSOCKS5Address: proxy.addr,
		externalSOCKS5Auth:    false,
		socksConnectTimeout:   2 * time.Second,
		dialStreamUpstreamFn: func(network, address string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout(network, address, timeout)
		},
	}
	s.socks5UpstreamPool = newSOCKS5UpstreamPool(
		maxIdle,
		idleTTL,
		prewarm,
		s.externalSOCKS5Address,
		s.externalSOCKS5Auth,
		s.externalSOCKS5Greeting,
		func(ctx context.Context) (net.Conn, error) {
			return s.dialTCPTargetContext(ctx, s.externalSOCKS5Address)
		},
		s.performExternalSOCKS5Greeting,
		nil,
	)
	t.Cleanup(func() {
		if s.socks5UpstreamPool != nil {
			s.socks5UpstreamPool.Close()
		}
	})
	return s
}

// ----------------------------------------------------------------------------
// Pool API — Get / Put / TTL / Close
// ----------------------------------------------------------------------------

func TestPool_DisabledByDefault(t *testing.T) {
	var p *socks5UpstreamPool = &socks5UpstreamPool{}
	if p.Enabled() {
		t.Fatalf("zero-value pool should be disabled")
	}
	if conn, ok := p.Get(); ok || conn != nil {
		t.Fatalf("Get on disabled pool returned (%v,%v)", conn, ok)
	}
}

func TestPool_ConstructorReturnsDisabledWhenMaxIdleZero(t *testing.T) {
	p := newSOCKS5UpstreamPool(0, time.Second, 0, "x", false, nil, nil, nil, nil)
	if p.Enabled() {
		t.Fatalf("maxIdle=0 must produce disabled pool")
	}
}

func TestPool_PutGetRoundTrip(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	s := buildPooledServer(t, proxy, 4, 2*time.Second, 0)

	// Dial + prime a connection manually and put it.
	conn, err := s.dialTCPTargetContext(context.Background(), proxy.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := s.performExternalSOCKS5Greeting(conn); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if !s.socks5UpstreamPool.Put(conn) {
		t.Fatalf("Put rejected on empty pool")
	}
	if s.socks5UpstreamPool.Len() != 1 {
		t.Fatalf("expected len=1, got %d", s.socks5UpstreamPool.Len())
	}
	popped, ok := s.socks5UpstreamPool.Get()
	if !ok || popped != conn {
		t.Fatalf("Get did not return the same conn: ok=%v same=%v", ok, popped == conn)
	}
	_ = conn.Close()
}

func TestPool_TTLEviction(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	// Use a tiny TTL so the entry expires before we try to Get it.
	s := buildPooledServer(t, proxy, 4, 50*time.Millisecond, 0)

	conn, err := s.dialTCPTargetContext(context.Background(), proxy.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := s.performExternalSOCKS5Greeting(conn); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if !s.socks5UpstreamPool.Put(conn) {
		t.Fatalf("Put rejected")
	}

	time.Sleep(120 * time.Millisecond)

	if got, ok := s.socks5UpstreamPool.Get(); ok {
		t.Fatalf("expected expired conn, got non-nil from Get: %v", got)
	}
	stats := s.socks5UpstreamPool.Snapshot()
	if stats.Evicted < 1 {
		t.Fatalf("expected Evicted>=1 after TTL expiry, got %+v", stats)
	}
}

func TestPool_OverflowClosesExtras(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	s := buildPooledServer(t, proxy, 2, time.Second, 0)

	makePrimedConn := func() net.Conn {
		c, err := s.dialTCPTargetContext(context.Background(), proxy.addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := s.performExternalSOCKS5Greeting(c); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		return c
	}
	a, b, c := makePrimedConn(), makePrimedConn(), makePrimedConn()
	if !s.socks5UpstreamPool.Put(a) {
		t.Fatalf("Put a rejected")
	}
	if !s.socks5UpstreamPool.Put(b) {
		t.Fatalf("Put b rejected")
	}
	if s.socks5UpstreamPool.Put(c) {
		t.Fatalf("Put c should be rejected (over cap)")
	}
	if s.socks5UpstreamPool.Len() != 2 {
		t.Fatalf("expected len=2, got %d", s.socks5UpstreamPool.Len())
	}
	stats := s.socks5UpstreamPool.Snapshot()
	if stats.OverflowClosed < 1 {
		t.Fatalf("expected OverflowClosed>=1, got %+v", stats)
	}
}

func TestPool_CloseDrainsAndRejectsLater(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	s := buildPooledServer(t, proxy, 4, time.Second, 0)

	conn, err := s.dialTCPTargetContext(context.Background(), proxy.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := s.performExternalSOCKS5Greeting(conn); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if !s.socks5UpstreamPool.Put(conn) {
		t.Fatalf("Put rejected before close")
	}
	s.socks5UpstreamPool.Close()
	if s.socks5UpstreamPool.Len() != 0 {
		t.Fatalf("expected drained pool, got len=%d", s.socks5UpstreamPool.Len())
	}
	// Closing again must not panic.
	s.socks5UpstreamPool.Close()

	// Put-after-Close must reject the conn.
	c2, err := s.dialTCPTargetContext(context.Background(), proxy.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := s.performExternalSOCKS5Greeting(c2); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if s.socks5UpstreamPool.Put(c2) {
		t.Fatalf("Put accepted after Close")
	}
}

// ----------------------------------------------------------------------------
// End-to-end CONNECT path
// ----------------------------------------------------------------------------

func TestDialExternalSOCKS5_PoolMissDialsFresh(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	s := buildPooledServer(t, proxy, 4, time.Second, 0)

	target := []byte{0x01, 127, 0, 0, 1, 0x00, 0x50} // IPv4 127.0.0.1:80
	conn, err := s.dialExternalSOCKS5TargetContext(context.Background(), target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	if proxy.greetingsServed.Load() != 1 {
		t.Fatalf("expected 1 greeting (cold dial), got %d", proxy.greetingsServed.Load())
	}
	if proxy.connectsServed.Load() != 1 {
		t.Fatalf("expected 1 CONNECT, got %d", proxy.connectsServed.Load())
	}
}

func TestDialExternalSOCKS5_PoolHitSkipsGreeting(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	s := buildPooledServer(t, proxy, 4, time.Second, 0)

	// Pre-prime one connection by hand.
	primed, err := s.dialTCPTargetContext(context.Background(), proxy.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := s.performExternalSOCKS5Greeting(primed); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if !s.socks5UpstreamPool.Put(primed) {
		t.Fatalf("Put primed rejected")
	}
	greetingsBefore := proxy.greetingsServed.Load()
	connectsBefore := proxy.connectsServed.Load()

	target := []byte{0x01, 127, 0, 0, 1, 0x00, 0x50}
	conn, err := s.dialExternalSOCKS5TargetContext(context.Background(), target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	// Greeting count must NOT have advanced (we reused the primed conn).
	if got := proxy.greetingsServed.Load(); got != greetingsBefore {
		t.Fatalf("expected greetings stable at %d, got %d (pool hit must skip greeting)",
			greetingsBefore, got)
	}
	// CONNECT count advanced by exactly one.
	if got := proxy.connectsServed.Load(); got != connectsBefore+1 {
		t.Fatalf("expected CONNECT to advance by 1, before=%d after=%d", connectsBefore, got)
	}
	hits := s.socks5UpstreamPool.Snapshot().Hits
	if hits < 1 {
		t.Fatalf("expected pool.Hits>=1, got %d", hits)
	}
}

func TestDialExternalSOCKS5_StalePoolEntryTriggersRetry(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	// First handled conn (the primed one in the pool) gets closed by the
	// proxy right after greeting; the second handled conn behaves normally.
	var serverClosed sync.Once
	proxy.afterGreetingHook = func(idx int64) bool {
		hit := false
		if idx == 1 {
			serverClosed.Do(func() { hit = true })
		}
		return hit
	}

	s := buildPooledServer(t, proxy, 4, time.Second, 0)

	// Pre-prime — proxy will receive greeting, reply, then close.
	primed, err := s.dialTCPTargetContext(context.Background(), proxy.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := s.performExternalSOCKS5Greeting(primed); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if !s.socks5UpstreamPool.Put(primed) {
		t.Fatalf("Put primed rejected")
	}
	// Give the proxy a tick to close that primed conn from its side.
	time.Sleep(50 * time.Millisecond)

	target := []byte{0x01, 127, 0, 0, 1, 0x00, 0x50}
	conn, err := s.dialExternalSOCKS5TargetContext(context.Background(), target)
	if err != nil {
		t.Fatalf("dial after stale pool entry: %v", err)
	}
	_ = conn.Close()
	// The retry path performs a fresh greeting + CONNECT; the proxy
	// should record at least one CONNECT.
	if proxy.connectsServed.Load() < 1 {
		t.Fatalf("expected proxy to serve CONNECT on retry path, got 0")
	}
}

// ----------------------------------------------------------------------------
// Reaper / prewarm
// ----------------------------------------------------------------------------

func TestPool_ReaperReapsExpiredEntries(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	s := buildPooledServer(t, proxy, 4, 30*time.Millisecond, 0)

	conn, err := s.dialTCPTargetContext(context.Background(), proxy.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := s.performExternalSOCKS5Greeting(conn); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if !s.socks5UpstreamPool.Put(conn) {
		t.Fatalf("Put rejected")
	}

	time.Sleep(80 * time.Millisecond)
	closed := s.socks5UpstreamPool.reapExpired(time.Now())
	if closed != 1 {
		t.Fatalf("expected reapExpired to close 1 entry, got %d", closed)
	}
	if s.socks5UpstreamPool.Len() != 0 {
		t.Fatalf("expected empty pool after reap, got len=%d", s.socks5UpstreamPool.Len())
	}
}

func TestPool_PrewarmFillsToTarget(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	s := buildPooledServer(t, proxy, 4, 2*time.Second, 3)

	added := s.socks5UpstreamPool.refillOnce(context.Background())
	if added != 3 {
		t.Fatalf("expected refillOnce to add 3, got %d", added)
	}
	if s.socks5UpstreamPool.Len() != 3 {
		t.Fatalf("expected len=3 after prewarm, got %d", s.socks5UpstreamPool.Len())
	}
	stats := s.socks5UpstreamPool.Snapshot()
	if stats.PrewarmSucceeded != 3 {
		t.Fatalf("expected PrewarmSucceeded=3, got %+v", stats)
	}
}

func TestPool_PrewarmZeroNoop(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	s := buildPooledServer(t, proxy, 4, 2*time.Second, 0)

	added := s.socks5UpstreamPool.refillOnce(context.Background())
	if added != 0 {
		t.Fatalf("expected 0 with prewarm=0, got %d", added)
	}
	if s.socks5UpstreamPool.Len() != 0 {
		t.Fatalf("expected empty pool, got len=%d", s.socks5UpstreamPool.Len())
	}
}

func TestPool_PrewarmStopsOnDialError(t *testing.T) {
	// Point the pool at an address that nothing is listening on so every
	// dial errors out. PrewarmFailed should increment and the loop must
	// not block forever.
	s := &Server{
		useExternalSOCKS5:     true,
		externalSOCKS5Address: "127.0.0.1:1", // typically blocked / unreachable
		socksConnectTimeout:   100 * time.Millisecond,
		dialStreamUpstreamFn: func(network, address string, timeout time.Duration) (net.Conn, error) {
			return nil, errors.New("synthetic dial failure")
		},
	}
	s.socks5UpstreamPool = newSOCKS5UpstreamPool(
		2, time.Second, 2,
		s.externalSOCKS5Address, false,
		s.externalSOCKS5Greeting,
		func(ctx context.Context) (net.Conn, error) {
			return s.dialTCPTargetContext(ctx, s.externalSOCKS5Address)
		},
		s.performExternalSOCKS5Greeting,
		nil,
	)
	t.Cleanup(s.socks5UpstreamPool.Close)

	added := s.socks5UpstreamPool.refillOnce(context.Background())
	if added != 0 {
		t.Fatalf("expected 0 successful adds on dial failure, got %d", added)
	}
	stats := s.socks5UpstreamPool.Snapshot()
	if stats.PrewarmFailed < 1 {
		t.Fatalf("expected PrewarmFailed>=1, got %+v", stats)
	}
}

// ----------------------------------------------------------------------------
// Concurrency smoke — Get / Put / Close together must not race.
// ----------------------------------------------------------------------------

func TestPool_ConcurrentGetPutClose(t *testing.T) {
	proxy := newFakeSOCKS5Proxy(t)
	s := buildPooledServer(t, proxy, 8, time.Second, 0)

	// Pre-seed a few entries.
	for i := 0; i < 4; i++ {
		c, err := s.dialTCPTargetContext(context.Background(), proxy.addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := s.performExternalSOCKS5Greeting(c); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		s.socks5UpstreamPool.Put(c)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if c, ok := s.socks5UpstreamPool.Get(); ok {
					// Put back without using; the pool's job is to
					// stay consistent under churn.
					if !s.socks5UpstreamPool.Put(c) {
						_ = c.Close()
					}
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Close after the racing readers/writers drained.
	s.socks5UpstreamPool.Close()
	if s.socks5UpstreamPool.Len() != 0 {
		t.Fatalf("expected drained pool, got len=%d", s.socks5UpstreamPool.Len())
	}
}
