// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package udpserver

import (
	"context"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masterdnsvpn-go/internal/config"
	"masterdnsvpn-go/internal/logger"
	"masterdnsvpn-go/internal/metrics"
)

// TestUDPBatchReadEnabledTriState exercises the three states of the
// UDP_BATCH_READ knob: auto (0), force-on (1), force-off (2). Anything
// outside this range falls back to auto-on, which matches the documented
// semantics.
func TestUDPBatchReadEnabledTriState(t *testing.T) {
	cases := []struct {
		name    string
		value   int
		wantOn  bool
	}{
		{"auto-default", 0, true},
		{"force-on", 1, true},
		{"force-off", 2, false},
		{"unknown-positive-falls-back-to-auto", 5, true},
		{"unknown-negative-falls-back-to-auto", -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.ServerConfig{UDPBatchRead: tc.value}
			if got := cfg.UDPBatchReadEnabled(); got != tc.wantOn {
				t.Fatalf("UDPBatchReadEnabled(%d) = %v, want %v", tc.value, got, tc.wantOn)
			}
		})
	}
}

// TestEffectiveUDPReadersHonorsGOMAXPROCS verifies the Step 9 change that
// caps the auto-sized reader count by min(NumCPU, GOMAXPROCS). The previous
// implementation hit NumCPU only, so containerized deployments with a CPU
// cap would over-provision readers.
func TestEffectiveUDPReadersHonorsGOMAXPROCS(t *testing.T) {
	cpus := runtime.NumCPU()
	if cpus < 2 {
		t.Skipf("need at least 2 CPUs to test GOMAXPROCS clamp, have %d", cpus)
	}

	prev := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	// Force GOMAXPROCS down to 1 so the formula
	//   min(cores/2, 8) with cores=min(NumCPU,GOMAXPROCS)=1
	// collapses to 1. This proves the cap is wired.
	runtime.GOMAXPROCS(1)
	cfg := config.ServerConfig{ProtocolType: "TCP", MaxConcurrentRequests: 1024}
	if got := cfg.EffectiveUDPReaders(); got != 1 {
		t.Fatalf("expected 1 reader under GOMAXPROCS=1, got=%d", got)
	}
}

// readLoopFixture spins up a tiny server-like harness exposing only what the
// reader loops need (packetPool, log, dropLog throttling, onDrop). It binds
// to an ephemeral localhost UDP socket so we can drive real packets through
// both ingress paths.
type readLoopFixture struct {
	t       *testing.T
	srv     *Server
	conn    *net.UDPConn
	clientC *net.UDPConn
	reqCh   chan request
	ctx     context.Context
	cancel  context.CancelFunc
}

func newReadLoopFixture(t *testing.T) *readLoopFixture {
	t.Helper()

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
	srvConn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	cli, err := net.DialUDP("udp", nil, srvConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		_ = srvConn.Close()
		t.Fatalf("dial: %v", err)
	}

	srv := &Server{
		cfg:                  config.ServerConfig{MaxPacketSize: 2048, UDPBatchRead: 0},
		log:                  logger.New("step9-test", "ERROR"),
		dropLogIntervalNanos: int64(2 * time.Second),
	}
	srv.packetPool = sync.Pool{
		New: func() any {
			return make([]byte, srv.cfg.MaxPacketSize)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	f := &readLoopFixture{
		t:       t,
		srv:     srv,
		conn:    srvConn,
		clientC: cli,
		reqCh:   make(chan request, 32),
		ctx:     ctx,
		cancel:  cancel,
	}

	t.Cleanup(func() {
		f.cancel()
		_ = cli.Close()
		_ = srvConn.Close()
	})

	return f
}

// drainPackets receives `want` packets (or times out) from the fixture's
// request channel and returns the decoded payloads in arrival order.
func (f *readLoopFixture) drainPackets(want int, timeout time.Duration) [][]byte {
	out := make([][]byte, 0, want)
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case req := <-f.reqCh:
			out = append(out, append([]byte(nil), req.buf[:req.size]...))
			f.srv.packetPool.Put(req.buf)
		case <-deadline:
			return out
		}
	}
	return out
}

// TestReadLoopDispatchesPackets validates the baseline (single-packet) reader
// path: bytes sent on the wire arrive in the request channel intact, with
// matching addr and conn pointers, and the per-ingress metrics are bumped.
func TestReadLoopDispatchesPackets(t *testing.T) {
	f := newReadLoopFixture(t)

	startPackets := metrics.PacketsIn.Value()
	startBytes := metrics.BytesIn.Value()

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = f.srv.readLoop(f.ctx, f.conn, f.reqCh, 1)
	}()

	const N = 8
	for i := 0; i < N; i++ {
		payload := []byte{byte(0x10 + i), 0xAA, 0xBB}
		if _, err := f.clientC.Write(payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	got := f.drainPackets(N, 2*time.Second)
	if len(got) != N {
		t.Fatalf("expected %d packets, got %d", N, len(got))
	}
	for i, p := range got {
		if len(p) != 3 || p[0] != byte(0x10+i) || p[1] != 0xAA || p[2] != 0xBB {
			t.Fatalf("packet %d payload mismatch: %v", i, p)
		}
	}

	deltaP := metrics.PacketsIn.Value() - startPackets
	deltaB := metrics.BytesIn.Value() - startBytes
	if deltaP != int64(N) {
		t.Fatalf("PacketsIn delta = %d, want %d", deltaP, N)
	}
	if deltaB != int64(N*3) {
		t.Fatalf("BytesIn delta = %d, want %d", deltaB, N*3)
	}

	f.cancel()
	_ = f.conn.Close()
	<-loopDone
}

// TestBatchReadLoopDispatchesPackets is the Linux-only counterpart that
// drives the batch ingress path end-to-end. On non-Linux the helper just
// delegates to readLoop, so we still get coverage by re-using the same
// fixture (no need for a skip).
func TestBatchReadLoopDispatchesPackets(t *testing.T) {
	if !batchReadSupported() {
		t.Skipf("batch read not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	f := newReadLoopFixture(t)

	startPackets := metrics.PacketsIn.Value()
	startBytes := metrics.BytesIn.Value()

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = f.srv.batchReadLoop(f.ctx, f.conn, f.reqCh, 1)
	}()

	const N = 16
	for i := 0; i < N; i++ {
		payload := []byte{byte(0x20 + i), 0xCC}
		if _, err := f.clientC.Write(payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	got := f.drainPackets(N, 3*time.Second)
	if len(got) != N {
		t.Fatalf("expected %d packets via batch, got %d", N, len(got))
	}

	// Order is preserved within a single recvmmsg burst on Linux loopback,
	// but a packet stream split across multiple syscalls can in principle
	// reorder under load. So we verify the multi-set instead.
	seen := make(map[byte]bool, N)
	for _, p := range got {
		if len(p) != 2 || p[1] != 0xCC {
			t.Fatalf("malformed payload: %v", p)
		}
		seen[p[0]] = true
	}
	for i := 0; i < N; i++ {
		if !seen[byte(0x20+i)] {
			t.Fatalf("missing payload byte %#x", 0x20+i)
		}
	}

	deltaP := metrics.PacketsIn.Value() - startPackets
	deltaB := metrics.BytesIn.Value() - startBytes
	if deltaP != int64(N) {
		t.Fatalf("batch PacketsIn delta = %d, want %d", deltaP, N)
	}
	if deltaB != int64(N*2) {
		t.Fatalf("batch BytesIn delta = %d, want %d", deltaB, N*2)
	}

	f.cancel()
	_ = f.conn.Close()
	<-loopDone
}

// TestStartReadersPicksFallbackWhenBatchDisabled wires the full
// startReaders() dispatch with UDPBatchRead=2 (force-off) and asserts the
// non-batch path is selected even on Linux. We can't directly observe which
// helper was picked, so we verify it indirectly via the absence of the
// ipv4.PacketConn syscall flow: when forced off, the single-packet loop runs,
// which has the exact same observable behavior. The real assertion here is
// that startReaders does not panic or deadlock with that knob.
func TestStartReadersPicksFallbackWhenBatchDisabled(t *testing.T) {
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
	srvConn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &Server{
		cfg: config.ServerConfig{
			MaxPacketSize:         2048,
			UDPBatchRead:          2, // force-off
			UDPReaders:            1,
			MaxConcurrentRequests: 1024,
			ProtocolType:          "TCP",
		},
		log:                  logger.New("step9-test", "ERROR"),
		dropLogIntervalNanos: int64(2 * time.Second),
	}
	srv.packetPool = sync.Pool{
		New: func() any { return make([]byte, srv.cfg.MaxPacketSize) },
	}

	reqCh := make(chan request, 8)
	readErrCh := make(chan error, 1)
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	srv.startReaders(ctx, []*net.UDPConn{srvConn}, reqCh, readErrCh, &wg)

	cli, err := net.DialUDP("udp", nil, srvConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Write([]byte{0xDE, 0xAD}); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.size != 2 || req.buf[0] != 0xDE || req.buf[1] != 0xAD {
			t.Fatalf("unexpected payload: %v", req.buf[:req.size])
		}
		srv.packetPool.Put(req.buf)
	case <-time.After(2 * time.Second):
		t.Fatal("packet not dispatched within 2s with batch forced off")
	}

	cancel()
	_ = srvConn.Close()
	wg.Wait()
}

// TestReadLoopOnDropCountsIncrement asserts the drop accounting still works
// after the Step 9 changes — both the single-packet and (on Linux) the
// batch path share the same s.onDrop helper, so if PacketsIn rises but the
// queue overflows we expect droppedPackets to track it.
func TestReadLoopOnDropCountsIncrement(t *testing.T) {
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
	srvConn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srvConn.Close()

	srv := &Server{
		cfg:                  config.ServerConfig{MaxPacketSize: 2048, UDPBatchRead: 2},
		log:                  logger.New("step9-test", "ERROR"),
		dropLogIntervalNanos: int64(2 * time.Second),
	}
	srv.packetPool = sync.Pool{New: func() any { return make([]byte, srv.cfg.MaxPacketSize) }}

	// Capacity-zero channel — every send drops.
	reqCh := make(chan request)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = srv.readLoop(ctx, srvConn, reqCh, 1)
	}()

	cli, err := net.DialUDP("udp", nil, srvConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	const N = 4
	for i := 0; i < N; i++ {
		if _, err := cli.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	deadline := time.After(2 * time.Second)
	for {
		if d := srv.droppedPackets.Load(); d >= uint64(N) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected at least %d drops, saw %d", N, srv.droppedPackets.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	_ = srvConn.Close()
	<-loopDone
}

// BenchmarkReadLoopThroughput measures the single-packet ingress rate end to
// end (real localhost UDP socket, real syscalls). It establishes a baseline
// to compare against BenchmarkBatchReadLoopThroughput on Linux.
func BenchmarkReadLoopThroughput(b *testing.B) {
	benchIngress(b, func(srv *Server, ctx context.Context, conn *net.UDPConn, reqCh chan request) error {
		return srv.readLoop(ctx, conn, reqCh, 1)
	})
}

// BenchmarkBatchReadLoopThroughput is the Linux-only counterpart that uses
// the recvmmsg-backed batch ingress path.
func BenchmarkBatchReadLoopThroughput(b *testing.B) {
	if !batchReadSupported() {
		b.Skipf("batch read not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	benchIngress(b, func(srv *Server, ctx context.Context, conn *net.UDPConn, reqCh chan request) error {
		return srv.batchReadLoop(ctx, conn, reqCh, 1)
	})
}

// benchIngress shoves a tight stream of small datagrams at the server and
// counts what made it through. It reports throughput as packets/sec via
// b.SetBytes(int64(payloadSize)) so b.N maps to packets and we get MB/s for
// free in the Go bench output.
func benchIngress(b *testing.B, runLoop func(*Server, context.Context, *net.UDPConn, chan request) error) {
	b.Helper()

	const payloadSize = 256
	srvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	defer srvConn.Close()

	// Generous socket buffer to keep the kernel from dropping under the
	// fast sender loop.
	_ = srvConn.SetReadBuffer(8 * 1024 * 1024)

	srv := &Server{
		cfg:                  config.ServerConfig{MaxPacketSize: 2048},
		log:                  logger.New("step9-test", "ERROR"),
		dropLogIntervalNanos: int64(time.Hour), // disable drop logging noise
	}
	srv.packetPool = sync.Pool{New: func() any { return make([]byte, srv.cfg.MaxPacketSize) }}

	reqCh := make(chan request, 8192)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = runLoop(srv, ctx, srvConn, reqCh)
	}()

	// Consumer: drain reqCh and return buffers to the pool. We count
	// completions so the bench reports how many round-trips succeeded.
	var received int64
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for req := range reqCh {
			atomic.AddInt64(&received, 1)
			srv.packetPool.Put(req.buf)
		}
	}()

	cli, err := net.DialUDP("udp", nil, srvConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	_ = cli.SetWriteBuffer(8 * 1024 * 1024)

	payload := make([]byte, payloadSize)

	b.SetBytes(int64(payloadSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := cli.Write(payload); err != nil {
			// EAGAIN-style backpressure on the send side — back off so
			// the bench doesn't spuriously fail. Use a short wait
			// instead of returning so we still count what we sent.
			time.Sleep(50 * time.Microsecond)
		}
	}

	// Allow late arrivals; give the kernel and the loop a tiny grace
	// window so the receive count is more representative. We don't
	// require 100% delivery here — the benchmark measures sustained rate,
	// not loss-free correctness (see unit tests for that).
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) && atomic.LoadInt64(&received) < int64(b.N) {
		time.Sleep(2 * time.Millisecond)
	}

	b.StopTimer()
	b.ReportMetric(float64(atomic.LoadInt64(&received)), "packets-received")

	cancel()
	_ = srvConn.Close()
	<-loopDone
	close(reqCh)
	<-consumerDone
}

