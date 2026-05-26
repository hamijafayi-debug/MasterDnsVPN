// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package udpserver

import (
	"container/heap"
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"masterdnsvpn-go/internal/config"
	dnsCache "masterdnsvpn-go/internal/dnscache"
	domainMatcher "masterdnsvpn-go/internal/domainmatcher"
	fragmentStore "masterdnsvpn-go/internal/fragmentstore"
	"masterdnsvpn-go/internal/logger"
	"masterdnsvpn-go/internal/security"
	VpnProto "masterdnsvpn-go/internal/vpnproto"
)

const (
	mtuProbeModeRaw     = 0
	mtuProbeModeBase64  = 1
	mtuProbeCodeLength  = 4
	mtuProbeMetaLength  = mtuProbeCodeLength + 2
	mtuProbeUpMinSize   = 1 + mtuProbeCodeLength
	mtuProbeDownMinSize = mtuProbeUpMinSize + 2
	mtuProbeMinDownSize = VpnProto.SessionAcceptPayloadSize
	mtuProbeMaxDownSize = 4096
)

var preSessionPacketTypes = buildPreSessionPacketTypes()

type Server struct {
	cfg                      config.ServerConfig
	log                      *logger.Logger
	codec                    *security.Codec
	domainMatcher            *domainMatcher.Matcher
	sessions                 *sessionStore
	deferredDNSSession       *deferredSessionProcessor
	deferredConnectSession   *deferredSessionProcessor
	invalidCookieTracker     *invalidCookieTracker
	dnsCache                 *dnsCache.Store
	dnsResolveInflight       *dnsResolveInflightManager
	dnsUpstreamServers       []string
	dnsUpstreamBufferPool    sync.Pool
	dnsFragments             *fragmentStore.Store[dnsFragmentKey]
	socks5Fragments          *fragmentStore.Store[socks5FragmentKey]
	dnsFragmentTimeout       time.Duration
	resolveDNSQueryFn        func([]byte) ([]byte, error)
	dialStreamUpstreamFn     func(string, string, time.Duration) (net.Conn, error)
	uploadCompressionMask    uint8
	downloadCompressionMask  uint8
	dropLogIntervalNanos     int64
	invalidCookieWindow      time.Duration
	invalidCookieWindowNanos int64
	invalidCookieThreshold   int
	socksConnectTimeout      time.Duration
	useExternalSOCKS5        bool
	externalSOCKS5Address    string
	externalSOCKS5Auth       bool
	externalSOCKS5User       []byte
	externalSOCKS5Pass       []byte
	// Step 17 — pre-authenticated TCP connections to the upstream SOCKS5
	// proxy. nil when pooling is disabled or USE_EXTERNAL_SOCKS5=false.
	// Always check pool.Enabled() before reaching for Get/Put — the
	// pointer can be non-nil for a disabled pool too.
	socks5UpstreamPool      *socks5UpstreamPool
	mtuProbePayloadPool     sync.Pool
	packetPool              sync.Pool
	deferredInflightMu      sync.Mutex
	deferredInflight        map[uint64]struct{}
	deferredInflightIndex   map[uint8]map[uint16]map[uint64]struct{}
	invalidSessionDropLog   throttledLogState
	droppedPackets          atomic.Uint64
	lastDropLogUnix         atomic.Int64
	deferredDroppedPackets  atomic.Uint64
	lastDeferredDropLogUnix atomic.Int64
	pongNonce               atomic.Uint32
	invalidDropMode         atomic.Uint32
	// Step 19 — bookkeeping for background goroutines whose lifetimes
	// are bounded only by ctx.Done(). Run() uses this to guarantee no
	// goroutine outlives the call (deterministic teardown).
	backgroundWG sync.WaitGroup
}

// request is the per-packet message flowing from reader goroutines to DNS
// worker goroutines. Step 26 (SYNC-POOL-NONPTR): `bufPtr` holds the raw
// *[]byte returned by packetPool.Get so the buffer can be released back to
// the pool without re-boxing the slice header (which costs a 24-byte heap
// allocation per packet on the ingress hot path). `buf` remains for legacy
// call sites that read packet contents — it points at the same backing array.
type request struct {
	bufPtr *[]byte
	buf    []byte
	size   int
	addr   *net.UDPAddr
	conn   *net.UDPConn
}

type postSessionValidation struct {
	record   *sessionRuntimeView
	response []byte
	ok       bool
}

func New(cfg config.ServerConfig, log *logger.Logger, codec *security.Codec) *Server {
	invalidCookieWindow := cfg.InvalidCookieWindow()
	if invalidCookieWindow <= 0 {
		invalidCookieWindow = 2 * time.Second
	}
	dnsFragmentTimeout := cfg.DNSFragmentAssemblyTimeout()
	if dnsFragmentTimeout <= 0 {
		dnsFragmentTimeout = 5 * time.Minute
	}
	dropLogInterval := cfg.DropLogInterval()
	if dropLogInterval <= 0 {
		dropLogInterval = 2 * time.Second
	}
	socksConnectTimeout := cfg.SOCKSConnectTimeout()
	if socksConnectTimeout <= 0 {
		socksConnectTimeout = 8 * time.Second
	}
	dnsDeferredWorkers, connectDeferredWorkers, dnsDeferredQueue, connectDeferredQueue := splitDeferredSessionPools(cfg.EffectiveDeferredSessionWorkers(), cfg.EffectiveDeferredSessionQueueLimit())
	sessions := newSessionStore(cfg.EffectiveSessionOrphanQueueInitialCap(), cfg.EffectiveStreamQueueInitialCapacity(), cfg.SessionInitReuseTTL(), cfg.RecentlyClosedStreamTTL(), cfg.RecentlyClosedStreamCap)
	sessions.maxActiveSessions = cfg.MaxAllowedClientActiveSessions
	sessions.maxActiveStreams = cfg.MaxAllowedClientActiveStreams
	srv := &Server{
		cfg:                    cfg,
		log:                    log,
		codec:                  codec,
		domainMatcher:          domainMatcher.New(cfg.Domain, cfg.MinVPNLabelLength),
		sessions:               sessions,
		deferredDNSSession:     newDeferredSessionProcessor(dnsDeferredWorkers, dnsDeferredQueue, log),
		deferredConnectSession: newDeferredSessionProcessor(connectDeferredWorkers, connectDeferredQueue, log),
		invalidCookieTracker:   newInvalidCookieTracker(),
		dnsCache: func() *dnsCache.Store {
			st := dnsCache.New(
				cfg.EffectiveDNSCacheMaxRecords(),
				time.Duration(cfg.DNSCacheTTLSeconds*float64(time.Second)),
				dnsFragmentTimeout,
			)
			// Step 18 — opt-in hot tier. 0 leaves it disabled, preserving
			// the legacy single-tier behaviour.
			if cfg.DNSCacheHotTierSize > 0 {
				st.EnableHotTier(cfg.DNSCacheHotTierSize)
			}
			return st
		}(),
		dnsResolveInflight: newDNSResolveInflightManager(dnsFragmentTimeout),
		dnsUpstreamServers: append([]string(nil), cfg.DNSUpstreamServers...),
		dnsFragments:       fragmentStore.New[dnsFragmentKey](cfg.EffectiveDNSFragmentStoreCapacity()),
		socks5Fragments:    fragmentStore.New[socks5FragmentKey](cfg.EffectiveSOCKS5FragmentStoreCapacity()),
		dnsFragmentTimeout: dnsFragmentTimeout,
		dnsUpstreamBufferPool: sync.Pool{
			// Step 26 — pool stores *[]byte (pointer-like) to dodge the slice
			// header heap-alloc that SA6002 warns about. Sites Get -> *[]byte,
			// Put returns the same pointer.
			New: func() any {
				buf := make([]byte, 65535)
				return &buf
			},
		},
		dialStreamUpstreamFn: func(network string, address string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout(network, address, timeout)
		},
		uploadCompressionMask:    buildCompressionMask(cfg.SupportedUploadCompressionTypes),
		downloadCompressionMask:  buildCompressionMask(cfg.SupportedDownloadCompressionTypes),
		dropLogIntervalNanos:     dropLogInterval.Nanoseconds(),
		invalidCookieWindow:      invalidCookieWindow,
		invalidCookieWindowNanos: invalidCookieWindow.Nanoseconds(),
		invalidCookieThreshold:   cfg.InvalidCookieErrorThreshold,
		socksConnectTimeout:      socksConnectTimeout,
		useExternalSOCKS5:        cfg.UseExternalSOCKS5,
		externalSOCKS5Address:    net.JoinHostPort(cfg.ForwardIP, strconv.Itoa(cfg.ForwardPort)),
		externalSOCKS5Auth:       cfg.SOCKS5Auth,
		externalSOCKS5User:       []byte(cfg.SOCKS5User),
		externalSOCKS5Pass:       []byte(cfg.SOCKS5Pass),
		mtuProbePayloadPool: sync.Pool{
			// Step 26 — pool stores *[]byte (pointer-like). See SYNC-POOL-NONPTR.
			New: func() any {
				buf := make([]byte, mtuProbeMaxDownSize)
				return &buf
			},
		},
		deferredInflight:      make(map[uint64]struct{}, 128),
		deferredInflightIndex: make(map[uint8]map[uint16]map[uint64]struct{}, 64),
		packetPool: sync.Pool{
			// Step 26 — pool stores *[]byte (pointer-like). This is the hottest
			// pool on the server (one Get/Put per inbound packet on the ingress
			// fast path); the SYNC-POOL-NONPTR refactor eliminates a slice-
			// header heap allocation per packet on multi-Mpps workloads.
			New: func() any {
				buf := make([]byte, cfg.MaxPacketSize)
				return &buf
			},
		},
	}
	// Step 17 — build the SOCKS5 upstream pool now that the server fields
	// it depends on (dialer fn, address, auth flag) are populated. The
	// pool is wired but not started; Run() invokes startReaper(ctx).
	srv.socks5UpstreamPool = srv.buildSOCKS5UpstreamPool()
	return srv
}

type throttledLogState struct {
	mu   sync.Mutex
	last map[string]int64
	heap throttledLogHeap
}

type throttledLogEntry struct {
	key  string
	seen int64
}

type throttledLogHeap []throttledLogEntry

func (h throttledLogHeap) Len() int { return len(h) }

func (h throttledLogHeap) Less(i, j int) bool {
	return h[i].seen < h[j].seen
}

func (h throttledLogHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *throttledLogHeap) Push(x any) {
	*h = append(*h, x.(throttledLogEntry))
}

func (h *throttledLogHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

const (
	throttledLogSoftCap = 1024
	throttledLogHardCap = 1536
)

func (s *throttledLogState) allow(key string, now time.Time, interval time.Duration) bool {
	if s == nil {
		return true
	}
	if interval <= 0 {
		interval = time.Second
	}

	nowUnixNano := now.UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.last == nil {
		s.last = make(map[string]int64, 64)
	}

	last := s.last[key]

	if last != 0 && nowUnixNano-last < interval.Nanoseconds() {
		return false
	}

	s.last[key] = nowUnixNano
	heap.Push(&s.heap, throttledLogEntry{key: key, seen: nowUnixNano})

	if len(s.last) > 0 {
		s.pruneLocked(nowUnixNano, interval)
	}

	return true
}

func (s *throttledLogState) pruneLocked(nowUnixNano int64, interval time.Duration) {
	if s == nil || len(s.last) == 0 {
		return
	}

	cutoff := nowUnixNano - interval.Nanoseconds()
	for len(s.heap) > 0 {
		entry := s.heap[0]
		last, ok := s.last[entry.key]
		if !ok || last != entry.seen {
			heap.Pop(&s.heap)
			continue
		}
		if entry.seen > cutoff && len(s.last) <= throttledLogHardCap {
			break
		}
		delete(s.last, entry.key)
		heap.Pop(&s.heap)
	}

	for len(s.last) > throttledLogSoftCap && len(s.heap) > 0 {
		entry := heap.Pop(&s.heap).(throttledLogEntry)
		last, ok := s.last[entry.key]
		if !ok || last != entry.seen {
			continue
		}
		delete(s.last, entry.key)
	}
}

func splitDeferredSessionPools(totalWorkers int, totalQueue int) (dnsWorkers int, connectWorkers int, dnsQueue int, connectQueue int) {
	if totalWorkers <= 0 {
		totalWorkers = 1
	}
	if totalQueue <= 0 {
		totalQueue = 256
	}

	// DNS queries use a dedicated lightweight pool so connect-heavy work keeps
	// the full user-configured deferred capacity.
	dnsWorkers = 1
	connectWorkers = totalWorkers

	connectQueue = totalQueue
	dnsQueue = min(max(totalQueue/4, 64), 256)

	return dnsWorkers, connectWorkers, dnsQueue, connectQueue
}

func (s *Server) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	conns, err := s.openUDPListeners()
	if err != nil {
		return err
	}
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()

	s.log.Infof(
		"\U0001F4E1 <green>UDP Listener Ready, Addr: <cyan>%s</cyan>, Readers: <cyan>%d</cyan>, Workers: <cyan>%d</cyan>, Queue: <cyan>%d</cyan>, Sockets: <cyan>%d</cyan></green>",
		s.cfg.Address(),
		s.cfg.EffectiveUDPReaders(),
		s.cfg.EffectiveDNSRequestWorkers(),
		s.cfg.EffectiveMaxConcurrentRequests(),
		len(conns),
	)

	reqCh := make(chan request, s.cfg.EffectiveMaxConcurrentRequests())
	var workerWG sync.WaitGroup
	cleanupDone := make(chan struct{})

	go func() {
		defer close(cleanupDone)
		s.sessionCleanupLoop(runCtx)
	}()

	s.deferredDNSSession.Start(runCtx)
	s.deferredConnectSession.Start(runCtx)
	// Step 17 — kick the SOCKS5 upstream pool reaper. No-op when the
	// pool is disabled (Enabled() == false). The reaper exits when
	// runCtx is cancelled OR socks5UpstreamPool.Close is invoked below.
	if s.socks5UpstreamPool != nil {
		s.socks5UpstreamPool.startReaper(runCtx)
		defer s.socks5UpstreamPool.Close()
	}
	// Step 18 — DNS cache amortized pruner. 0 interval keeps the
	// legacy lazy-on-read behaviour. The pruner exits cleanly when
	// runCtx is cancelled below. Step 19 wires it into backgroundWG
	// so Run() can join on its exit deterministically.
	if s.cfg.DNSCachePruneIntervalSeconds > 0 && s.dnsCache != nil {
		interval := time.Duration(s.cfg.DNSCachePruneIntervalSeconds * float64(time.Second))
		maxScan := s.cfg.DNSCachePruneMaxScanPerShard
		s.backgroundWG.Add(1)
		go func() {
			defer s.backgroundWG.Done()
			s.runDNSCachePruner(runCtx, interval, maxScan)
		}()
	}
	s.startDNSWorkers(runCtx, conns[0], reqCh, &workerWG)

	go func() {
		<-runCtx.Done()
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()

	readErrCh := make(chan error, max(1, len(conns)))
	var readerWG sync.WaitGroup
	s.startReaders(runCtx, conns, reqCh, readErrCh, &readerWG)

	readerWG.Wait()
	close(reqCh)
	workerWG.Wait()
	cancel()
	<-cleanupDone

	// Step 19 — drain the deferred-session workers so the server's Run
	// returns only after every goroutine it started has exited. Previously
	// these workers were ctx-only and could outlive Run() by a few ms,
	// which broke goroutine-leak assertions and made tests racy when the
	// server was restarted.
	if s.deferredDNSSession != nil {
		s.deferredDNSSession.WaitForShutdown(2 * time.Second)
	}
	if s.deferredConnectSession != nil {
		s.deferredConnectSession.WaitForShutdown(2 * time.Second)
	}
	if s.socks5UpstreamPool != nil {
		// reaper was started above and Close() was deferred — by the
		// time we get here both ctx is cancelled AND Close has run
		// (deferred runs in LIFO so it fires before openUDPListeners'
		// cleanup). We just need to join the reaper goroutine.
		s.socks5UpstreamPool.WaitForShutdown(2 * time.Second)
	}
	// Hard-stop budget for any other ctx-driven background goroutines
	// (DNSCachePruner, future additions). We give them up to 2s to drain;
	// if they overshoot, log and continue rather than block forever — the
	// caller is already shutting down so leaking briefly is preferable to
	// hanging Run() indefinitely.
	bgDone := make(chan struct{})
	go func() {
		s.backgroundWG.Wait()
		close(bgDone)
	}()
	select {
	case <-bgDone:
	case <-time.After(2 * time.Second):
		if s.log != nil {
			s.log.Warnf("⏱️  <yellow>Background goroutines failed to drain within 2s — proceeding with shutdown</yellow>")
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	select {
	case err := <-readErrCh:
		return err
	default:
		return nil
	}
}
