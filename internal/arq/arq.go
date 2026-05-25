// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
// Package arq provides a high-performance, QUIC-inspired reliable transport
// overlay specifically designed to operate over DNS/UDP architectures.
// ==============================================================================
package arq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	Enums "masterdnsvpn-go/internal/enums"
	"masterdnsvpn-go/internal/metrics"
	"masterdnsvpn-go/internal/streamutil"
)

// StreamState mirrors Python's Stream_State enum
type StreamState int

const (
	StateOpen StreamState = iota
	StateHalfClosedLocal
	StateHalfClosedRemote
	StateClosing
	StateReset
	StateClosed
	StateDraining
	StateTimeWait
)

// PacketEnqueuer abstracts the transmission layer (Client or Server stream)
type PacketEnqueuer interface {
	PushTXPacket(priority int, packetType uint8, sequenceNum uint16, fragmentID uint8, totalFragments uint8, compressionType uint8, ttl time.Duration, payload []byte) bool
}

type terminalOwner interface {
	OnARQClosed(reason string)
}

type queuedDataRemover interface {
	RemoveQueuedData(sequenceNum uint16) bool
}

type queuedDataNackRemover interface {
	RemoveQueuedDataNack(sequenceNum uint16) bool
}

type Logger interface {
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

type DummyLogger struct{}

func (d *DummyLogger) Debugf(f string, a ...any) {}
func (d *DummyLogger) Infof(f string, a ...any)  {}
func (d *DummyLogger) Errorf(f string, a ...any) {}

type arqDataItem struct {
	Data            []byte
	CreatedAt       time.Time
	LastSentAt      time.Time
	Dispatched      bool
	LastNackSentAt  time.Time
	Retries         int
	CurrentRTO      time.Duration
	SampleEligible  bool
	CompressionType uint8
	TTL             time.Duration
	// OosAckCount tracks how many ACKs for *higher* sequence numbers have
	// arrived while this packet remained un-acked. Once it reaches the
	// fast-retransmit threshold (analogous to TCP's 3-dup-ACK rule, RFC
	// 5681), the segment is retransmitted immediately without waiting for
	// the RTO timer. Reset after a fast retransmit so the same packet is
	// not retransmitted on every subsequent ACK.
	OosAckCount uint8
	// FastRetransmitted is set after the segment has been fast-retransmitted
	// once. It guards against the same packet being fast-retransmitted again
	// before any new ACK information arrives.
	FastRetransmitted bool
}

type arqControlItem struct {
	PacketType     uint8
	SequenceNum    uint16
	FragmentID     uint8
	TotalFragments uint8
	AckType        uint8
	Payload        []byte
	Priority       int
	CreatedAt      time.Time
	LastSentAt     time.Time
	Dispatched     bool
	Retries        int
	CurrentRTO     time.Duration
	SampleEligible bool
	TTL            time.Duration
}

type adaptiveRTOState struct {
	srtt        time.Duration
	rttvar      time.Duration
	currentBase time.Duration
	initialized bool
}

type rtxJob struct {
	sn              uint16
	data            []byte
	compressionType uint8
}
type rxPayload struct {
	sn   uint16
	data []byte
}

var setupControlPacketTypes = map[uint8]bool{
	Enums.PACKET_STREAM_SYN: true,
	Enums.PACKET_SOCKS5_SYN: true,
}

type ARQ struct {
	mu sync.RWMutex

	streamID             uint16
	sessionID            uint8
	ioReady              bool
	streamWorkersStarted bool
	enqueuer             PacketEnqueuer
	localConn            io.ReadWriteCloser
	logger               Logger

	mtu             int
	compressionType uint8

	// Sequence and buffers
	sndNxt        uint16
	rcvNxt        uint16
	sndBuf        map[uint16]*arqDataItem
	rcvBuf        map[uint16][]byte
	controlSndBuf map[uint32]*arqControlItem // key: ptype << 24 | sn << 8 | fragID

	// Stream lifecycle and flags
	state        StreamState
	closed       bool
	closeReason  string
	lastActivity time.Time

	closeReadSent     bool
	closeReadReceived bool
	closeReadAcked    bool
	closeReadSeqSent  *uint16

	closeWriteSent     bool
	closeWriteReceived bool
	closeWriteAcked    bool
	closeWriteSeqSent  *uint16

	rstReceived bool
	rstSent     bool
	rstAcked    bool
	rstSeqSent  *uint16

	localWriteClosed   bool
	localWriterBroken  bool
	localWritePending  bool
	stopLocalRead      bool
	deferredClose      bool
	deferredReason     string
	deferredDeadline   time.Time
	deferredPacket     uint8
	clientEOFAt        time.Time
	closeReadAckedAt   time.Time
	lastDuplicateAckAt time.Time
	waitingAck         bool
	waitingAckFor      uint8
	ackWaitDeadline    time.Time
	drainProgressAt    time.Time
	drainQueueFailAt   time.Time
	drainQueueFails    int
	drainStallLogged   bool

	IsClient bool

	// Backpressure
	windowSize        int
	receiveWindowSize int
	limit             int
	windowNotFull     chan struct{} // Acts as asyncio.Event
	writeLock         sync.Mutex    // equivalent to asyncio.Lock for writer

	// Tuning Configuration
	rto                  time.Duration
	maxRTO               time.Duration
	inactivityTimeout    time.Duration
	dataPacketTTL        time.Duration
	maxDataRetries       int
	terminalDrainTimeout time.Duration
	terminalAckWait      time.Duration

	// Control-plane tuning
	enableControlReliability bool
	controlRto               time.Duration
	controlMaxRto            time.Duration
	controlMaxRetries        int
	controlPacketTTL         time.Duration
	dataAdaptiveRTO          adaptiveRTOState
	controlAdaptiveRTO       adaptiveRTOState
	dataNackMaxGap           int
	dataNackInitialDelay     time.Duration
	dataNackRepeatInterval   time.Duration

	// Step 5: fast-retransmit and retx budget. Both are local-only — they
	// do not appear in any wire packet.
	fastRetxThreshold uint8 // 0 = disabled; otherwise # of OOS-ACKs before fast retx
	// retx budget — token-bucket-like accounting using a 1-second sliding
	// window. retxWindowStart marks the start of the current second;
	// retxWindowCount is the number of retx (RTO + fast) emitted within it.
	// retxBudgetPerSec is the cap (≤0 means unlimited). All four fields are
	// protected by a.mu.
	retxBudgetPerSec int
	retxWindowStart  time.Time
	retxWindowCount  int
	// sndLoBoundSN caches the lowest sequence number currently in sndBuf.
	// sndLoBoundValid says whether the cached value is up-to-date (a delete
	// of the minimum invalidates the cache; a fresh recompute revalidates
	// it). This lets the fast-retransmit walk in ReceiveAck short-circuit
	// in the common in-order case where every ACK targets the minimum SN
	// and there is no older pending entry to bump. Both fields are
	// protected by a.mu.
	sndLoBoundSN    uint16
	sndLoBoundValid bool

	// Virtual streams do not emit local close side effects.
	isVirtual bool

	firstDataNackSeen map[uint16]time.Time
	lastDataNackSent  map[uint16]time.Time

	// Concurrency
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	flushSignal    chan struct{}
	rxChan         chan rxPayload
	pendingInbound int
}

type closeWriter interface {
	CloseWrite() error
}

type writeDeadlineSetter interface {
	SetWriteDeadline(time.Time) error
}

type ioErrorClass int

const (
	ioErrorFatal ioErrorClass = iota
	ioErrorTimeout
	ioErrorEOF
	ioErrorClosed
	ioErrorTransient
)

const (
	ioRetryBackoff         = 100 * time.Millisecond
	ioTransientReadBudget  = 3 * time.Second
	ioTransientWriteBudget = 3
)

func classifyIOError(err error) ioErrorClass {
	if err == nil {
		return ioErrorFatal
	}
	if errors.Is(err, io.EOF) {
		return ioErrorEOF
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return ioErrorClosed
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ioErrorTimeout
		}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, syscall.EAGAIN) || errors.Is(opErr.Err, syscall.EWOULDBLOCK) || errors.Is(opErr.Err, syscall.EINTR) {
			return ioErrorTransient
		}
	}
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
		return ioErrorTransient
	}
	return ioErrorFatal
}

// Config represents the extensive ARQ tuning configuration identically ported from Python
type Config struct {
	WindowSize                  int
	RTO                         float64
	MaxRTO                      float64
	IsVirtual                   bool
	StartPaused                 bool
	EnableControlReliability    bool
	ControlRTO                  float64
	ControlMaxRTO               float64
	ControlMaxRetries           int
	InactivityTimeout           float64
	DataPacketTTL               float64
	MaxDataRetries              int
	ControlPacketTTL            float64
	DataNackMaxGap              int
	DataNackInitialDelaySeconds float64
	DataNackRepeatSeconds       float64
	TerminalDrainTimeout        float64
	TerminalAckWaitTimeout      float64
	CompressionType             uint8
	IsClient                    bool
	InboundQueueSize            int
	// FastRetxThreshold is the number of out-of-order ACKs (ACKs for
	// sequence numbers > the oldest un-acked segment) that must accumulate
	// before fast-retransmit kicks in for that older segment. 0 (default)
	// = use the canonical RFC 5681 value of 3. Negative values disable
	// fast-retransmit entirely (RTO timer is the only retx trigger). This
	// knob is *local-only* and never crosses the wire, preserving the
	// existing wire protocol.
	FastRetxThreshold int
	// RetxBudgetPerSec caps the total number of data retransmissions
	// (RTO-triggered *or* fast-triggered) this ARQ instance will emit per
	// second. 0 (default) = use a sensible value derived from window size.
	// Negative = unlimited. The cap prevents pathological retx storms when
	// the path is genuinely lossy without disabling retx altogether — the
	// dropped retransmissions are recorded in metrics.ArqRetxBudgetDropped.
	// Local-only knob; not on the wire.
	RetxBudgetPerSec int
}

type CloseOptions struct {
	Force          bool
	SendRST        bool
	SendCloseWrite bool
	SendCloseRead  bool
	AfterDrain     bool
	TTL            time.Duration
}

func (a *ARQ) IsClosed() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.closed
}

func (a *ARQ) State() StreamState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state
}

func (a *ARQ) HasPendingSequence(sn uint16) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.sndBuf[sn]
	return ok
}

// NewARQ instantiates a pristine reliable streaming overlay suitable for client or server
func NewARQ(streamID uint16, sessionID uint8, enqueuer PacketEnqueuer, localConn io.ReadWriteCloser, mtu int, logger Logger, cfg Config) *ARQ {
	if logger == nil {
		logger = &DummyLogger{}
	}

	windowSize := max(cfg.WindowSize, 300)
	receiveWindowSize := max(windowSize, windowSize*2)

	limit := max(int(float64(windowSize)*0.8), 50)

	a := &ARQ{
		streamID:  streamID,
		sessionID: sessionID,
		ioReady:   !cfg.StartPaused,
		enqueuer:  enqueuer,
		localConn: localConn,
		logger:    logger,
		mtu:       mtu,

		sndBuf:        make(map[uint16]*arqDataItem),
		rcvBuf:        make(map[uint16][]byte),
		controlSndBuf: make(map[uint32]*arqControlItem),

		state:        StateOpen,
		lastActivity: time.Now(),

		windowSize:        windowSize,
		receiveWindowSize: receiveWindowSize,
		limit:             limit,
		windowNotFull:     make(chan struct{}, 1),
		writeLock:         sync.Mutex{},
		flushSignal:       make(chan struct{}, 1),

		inactivityTimeout:    time.Duration(maxF(120.0, cfg.InactivityTimeout) * float64(time.Second)),
		dataPacketTTL:        time.Duration(maxF(120.0, cfg.DataPacketTTL) * float64(time.Second)),
		maxDataRetries:       maxI(60, cfg.MaxDataRetries),
		terminalDrainTimeout: time.Duration(maxF(60.0, cfg.TerminalDrainTimeout) * float64(time.Second)),
		terminalAckWait:      time.Duration(maxF(30.0, cfg.TerminalAckWaitTimeout) * float64(time.Second)),

		enableControlReliability: cfg.EnableControlReliability,
		controlMaxRetries:        maxI(5, cfg.ControlMaxRetries),
		controlPacketTTL:         time.Duration(maxF(120.0, cfg.ControlPacketTTL) * float64(time.Second)),
		dataNackMaxGap:           maxI(0, cfg.DataNackMaxGap),
		dataNackInitialDelay:     time.Duration(maxF(0.0, cfg.DataNackInitialDelaySeconds) * float64(time.Second)),
		dataNackRepeatInterval:   time.Duration(maxF(0.1, cfg.DataNackRepeatSeconds) * float64(time.Second)),

		isVirtual:         cfg.IsVirtual,
		compressionType:   cfg.CompressionType,
		firstDataNackSeen: make(map[uint16]time.Time),
		lastDataNackSent:  make(map[uint16]time.Time),

		rxChan: make(chan rxPayload, func() int {
			if cfg.InboundQueueSize > 0 {
				return cfg.InboundQueueSize
			}

			if windowSize <= 512 {
				return windowSize * 4
			} else if windowSize <= 2048 {
				return windowSize * 2
			}

			return windowSize + (windowSize / 2)
		}()),
	}

	a.streamWorkersStarted = false

	// Apply Event unblock state
	a.signalWindowNotFull()

	userMaxRto := maxF(0.05, cfg.MaxRTO)
	a.maxRTO = time.Duration(userMaxRto * float64(time.Second))
	a.rto = time.Duration(minF(maxF(0.05, cfg.RTO), userMaxRto) * float64(time.Second))

	userControlMaxRto := maxF(0.05, cfg.ControlMaxRTO)
	a.controlMaxRto = time.Duration(userControlMaxRto * float64(time.Second))
	a.controlRto = time.Duration(minF(maxF(0.05, cfg.ControlRTO), userControlMaxRto) * float64(time.Second))
	a.dataAdaptiveRTO = adaptiveRTOState{currentBase: a.rto}
	a.controlAdaptiveRTO = adaptiveRTOState{currentBase: a.controlRto}
	a.IsClient = cfg.IsClient

	// Step 5 — fast-retransmit threshold.
	//   0  → DISABLED (default). Keeps the ACK fast-path at Step-4 cost: no
	//        OOS-ACK bookkeeping, no extra sndBuf walks. Users who care about
	//        lossy paths can opt-in by setting this to 3 (RFC 5681 dup-ACK rule).
	//   <0 → disabled (explicit; same internal state as 0).
	//   >0 → user value, clamped to [1, 255] so it fits in a uint8.
	switch {
	case cfg.FastRetxThreshold <= 0:
		a.fastRetxThreshold = 0
	case cfg.FastRetxThreshold > 255:
		a.fastRetxThreshold = 255
	default:
		a.fastRetxThreshold = uint8(cfg.FastRetxThreshold)
	}

	// Step 5 — retx budget per second.
	//   0  → derive from window: 4 × windowSize, floor 256, ceiling 65535.
	//   <0 → unlimited (stored as -1 sentinel internally).
	//   >0 → user value.
	switch {
	case cfg.RetxBudgetPerSec == 0:
		derived := 4 * windowSize
		if derived < 256 {
			derived = 256
		}
		if derived > 65535 {
			derived = 65535
		}
		a.retxBudgetPerSec = derived
	case cfg.RetxBudgetPerSec < 0:
		a.retxBudgetPerSec = -1
	default:
		a.retxBudgetPerSec = cfg.RetxBudgetPerSec
	}

	a.ctx, a.cancel = context.WithCancel(context.Background())
	return a
}

// Start launches the core background loops for IO multiplexing and retransmission
func (a *ARQ) Start() {
	a.wg.Add(1)
	go a.retransmitLoop()

	a.wg.Add(1)
	go a.rxLoop()

	if a.ioReady {
		a.startStreamWorkers()
	}
}

func (a *ARQ) startStreamWorkers() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.streamWorkersStarted {
		return
	}

	if a.localConn == nil {
		return
	}

	a.streamWorkersStarted = true

	a.wg.Add(1)
	go a.ioLoop()

	a.wg.Add(1)
	go a.writeLoop()

	a.signalFlushReady()
}

func (a *ARQ) SetLocalConn(conn io.ReadWriteCloser) {
	a.mu.Lock()
	if a.localConn != nil {
		a.mu.Unlock()
		return
	}
	a.localConn = conn
	shouldStart := a.ctx != nil && a.ctx.Err() == nil && a.ioReady
	a.mu.Unlock()

	if shouldStart {
		a.startStreamWorkers()
		a.signalFlushReady()
	}
}

func (a *ARQ) SetIOReady(ready bool) {
	a.mu.Lock()
	changed := a.ioReady != ready
	a.ioReady = ready
	a.mu.Unlock()

	if !changed {
		return
	}

	if ready {
		a.startStreamWorkers()
		a.signalFlushReady()
	}
}

// Done returns a channel that is closed when the ARQ context is cancelled or the stream is closed.
func (a *ARQ) Done() <-chan struct{} {
	return a.ctx.Done()
}

// ---------------------------------------------------------------------
// Small Utilities
// ---------------------------------------------------------------------

func minF(x, y float64) float64 {
	if x < y {
		return x
	}
	return y
}

func maxF(x, y float64) float64 {
	if x > y {
		return x
	}
	return y
}

func maxI(x, y int) int {
	if x > y {
		return x
	}
	return y
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func clampDuration(v, minV, maxV time.Duration) time.Duration {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

// rttvarFloor prevents the smoothed RTT variance from collapsing to zero on
// extremely stable paths (e.g. loopback). Without a floor, the RFC 6298
// formula RTO = SRTT + K*RTTVAR converges to ≈ SRTT, which is the precise
// boundary where a tiny scheduling jitter (or one momentarily slow ACK) makes
// the timer fire spuriously and triggers a retransmission storm. 1ms is the
// industry-standard granularity used by Linux/BSD TCP stacks.
const rttvarFloor = time.Millisecond

// updateAdaptiveRTO refines the smoothed RTT estimator (RFC 6298) using a
// fresh RTT sample. The estimator tracks SRTT (smoothed RTT) and RTTVAR
// (smoothed RTT variance) with α=1/8 and β=1/4, and derives the next base RTO
// as SRTT + 4·RTTVAR clamped into [minRTO, maxRTO].
//
// Step 5 hardening: RTTVAR is clamped to ≥ rttvarFloor (1ms) so paths with
// near-constant RTT (loopback, lan, low-jitter tunnels) cannot drive the base
// RTO down to exactly SRTT — that boundary is where spurious retransmissions
// originate.
func updateAdaptiveRTO(state adaptiveRTOState, sample, minRTO, maxRTO time.Duration) adaptiveRTOState {
	sample = clampDuration(sample, minRTO, maxRTO)

	if !state.initialized {
		state.srtt = sample
		state.rttvar = sample / 2
		state.initialized = true
	} else {
		delta := absDuration(state.srtt - sample)
		state.rttvar = time.Duration((3*state.rttvar + delta) / 4)
		state.srtt = time.Duration((7*state.srtt + sample) / 8)
	}

	if state.rttvar < rttvarFloor {
		state.rttvar = rttvarFloor
	}

	state.currentBase = clampDuration(state.srtt+4*state.rttvar, minRTO, maxRTO)
	return state
}

const (
	dataRetransmitRTOGrowthFactor    = 1.35
	controlRetransmitRTOGrowthFactor = 1.25
	setupControlRTOGrowthFactor      = 1.15
)

// ---------------------------------------------------------------------
// Flow Control & Shared State Helpers
// ---------------------------------------------------------------------

func (a *ARQ) signalWindowNotFull() {
	select {
	case a.windowNotFull <- struct{}{}:
	default:
	}
}

func (a *ARQ) waitWindowNotFull() {
	timer := time.NewTimer(200 * time.Millisecond)
	waitStarted := time.Time{}
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	for {
		a.mu.RLock()
		sndBufLen := len(a.sndBuf)
		if sndBufLen < a.limit || a.closed {
			a.mu.RUnlock()
			return
		}
		a.mu.RUnlock()

		now := time.Now()
		if waitStarted.IsZero() {
			waitStarted = now
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(200 * time.Millisecond)

		select {
		case <-a.windowNotFull:
		case <-timer.C:
		case <-a.ctx.Done():
			return
		}
	}
}

func (a *ARQ) signalFlushReady() {
	select {
	case a.flushSignal <- struct{}{}:
	default:
	}
}

// IsReset checks whether stream is explicitly in reset path
func (a *ARQ) IsReset() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state == StateReset || a.rstReceived || a.rstSent
}

// setState atomically transitions the stream
func (a *ARQ) setState(newState StreamState) {
	a.state = newState
}

func (a *ARQ) closeReadReceivedLocked() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.closeReadReceived
}

func (a *ARQ) isClosed() bool {
	return a.IsClosed()
}

// releaseRcvBufLocked returns every pool-backed buffer currently stored in
// rcvBuf back to streamutil before the map is replaced or cleared. Caller
// must hold a.mu in write mode.
func (a *ARQ) releaseRcvBufLocked() {
	for sn, buf := range a.rcvBuf {
		streamutil.Put(buf)
		delete(a.rcvBuf, sn)
	}
}

// clearAllQueues is used to wipe state instantly (RST / Abort semantics)
// Caller must hold a.mu.
func (a *ARQ) clearAllQueues(clearControl bool) {
	a.sndBuf = make(map[uint16]*arqDataItem)
	a.invalidateSndLoBoundLocked()
	a.releaseRcvBufLocked()
	a.rcvBuf = make(map[uint16][]byte)
	if clearControl {
		a.controlSndBuf = make(map[uint32]*arqControlItem)
	}
	a.clearDataNackStateLocked()

	a.signalWindowNotFull()
}

// clearDataNackStateLocked clears data NACK tracking maps.
// Caller must hold a.mu.
func (a *ARQ) clearDataNackStateLocked() {
	clear(a.firstDataNackSeen)
	clear(a.lastDataNackSent)
}

func (a *ARQ) clearOutboundStateLocked(clearControl bool) {
	if remover, ok := a.enqueuer.(queuedDataRemover); ok {
		for sn := range a.sndBuf {
			remover.RemoveQueuedData(sn)
		}
	}

	if nackRemover, ok := a.enqueuer.(queuedDataNackRemover); ok {
		for sn := range a.lastDataNackSent {
			nackRemover.RemoveQueuedDataNack(sn)
		}
	}

	a.sndBuf = make(map[uint16]*arqDataItem)
	a.invalidateSndLoBoundLocked()
	if clearControl {
		a.controlSndBuf = make(map[uint32]*arqControlItem)
	}
	a.clearDataNackStateLocked()
	a.signalWindowNotFull()
}

func (a *ARQ) contiguousReadyLocked() int {
	ready := 0
	for sn := a.rcvNxt; ; sn++ {
		if _, exists := a.rcvBuf[sn]; !exists {
			break
		}
		ready++
	}
	return ready
}

func formatAgoFrom(now time.Time, ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	return now.Sub(ts).Round(time.Millisecond).String()
}

func formatDeadlineDelta(now time.Time, deadline time.Time) string {
	if deadline.IsZero() {
		return "-"
	}
	delta := deadline.Sub(now).Round(time.Millisecond)
	return delta.String()
}

func (a *ARQ) currentDataBaseRTO() time.Duration {
	base := a.dataAdaptiveRTO.currentBase
	if base <= 0 {
		return a.rto
	}
	return clampDuration(base, a.rto, a.maxRTO)
}

func (a *ARQ) currentControlBaseRTO() time.Duration {
	base := a.controlAdaptiveRTO.currentBase
	if base <= 0 {
		return a.controlRto
	}
	return clampDuration(base, a.controlRto, a.controlMaxRto)
}

func (a *ARQ) noteSuccessfulDataSample(sample time.Duration) {
	a.mu.Lock()
	a.dataAdaptiveRTO = updateAdaptiveRTO(a.dataAdaptiveRTO, sample, a.rto, a.maxRTO)
	a.mu.Unlock()
}

// trackInsertedSndSNLocked maintains the cached lowest-pending-SN hint
// when a new segment is inserted into sndBuf. The hint is a strict lower
// bound: a fresh insert may shrink it (if the new sn is older modulo 2^16
// than the current cached value), but never grow it. Caller must hold a.mu.
func (a *ARQ) trackInsertedSndSNLocked(sn uint16) {
	if !a.sndLoBoundValid {
		a.sndLoBoundSN = sn
		a.sndLoBoundValid = true
		return
	}
	// sn is "older" than the cached value when (cached - sn) ∈ (0, 32768).
	diff := uint16(a.sndLoBoundSN - sn)
	if diff > 0 && diff < 32768 {
		a.sndLoBoundSN = sn
	}
}

// trackDeletedSndSNLocked maintains the cached lowest-pending-SN hint when
// a segment is removed from sndBuf. Removing any sn other than the current
// minimum leaves the hint valid (still a lower bound). Removing the
// minimum either advances the hint to the next contiguous sn (the
// dominant in-order-ACK case, O(1)) or invalidates the cache when there
// is a gap (forcing the next reader to recompute). Caller must hold a.mu.
func (a *ARQ) trackDeletedSndSNLocked(sn uint16) {
	if !a.sndLoBoundValid || a.sndLoBoundSN != sn {
		return
	}
	if len(a.sndBuf) == 0 {
		a.sndLoBoundValid = false
		a.sndLoBoundSN = 0
		return
	}
	// Fast-path: under in-order ACK arrival, the next minimum is exactly
	// sn+1. A single map probe avoids the full O(N) re-scan and keeps the
	// hint perfectly accurate. On a gap (out-of-order ACK), we degrade to
	// invalidating the cache; the next ReceiveAck that sees an OOS path
	// will recompute via the walk anyway.
	next := sn + 1
	if _, ok := a.sndBuf[next]; ok {
		a.sndLoBoundSN = next
		return
	}
	a.sndLoBoundValid = false
	a.sndLoBoundSN = 0
}

// invalidateSndLoBoundLocked is the explicit reset used by bulk wipes
// (clearAllQueues, clearOutboundStateLocked). Caller must hold a.mu.
func (a *ARQ) invalidateSndLoBoundLocked() {
	a.sndLoBoundValid = false
	a.sndLoBoundSN = 0
}

// consumeRetxBudgetLocked is the gating function for every data
// retransmission emitted by this ARQ. It implements a coarse 1-second
// sliding window: the first retx in a new second resets the counter, and
// subsequent retx within the same second are admitted only while the count
// stays below the configured cap.
//
// Returns true when the caller may proceed with the retransmission, false
// when the budget is exhausted (in which case the caller should record
// metrics.ArqRetxBudgetDropped and skip the retx). Caller must hold a.mu.
//
// Budget = -1 means unlimited; budget = 0 should never reach here because
// NewARQ promotes it to a derived positive value.
func (a *ARQ) consumeRetxBudgetLocked(now time.Time) bool {
	if a.retxBudgetPerSec < 0 {
		return true
	}
	if a.retxBudgetPerSec == 0 {
		// Defensive — never expected because NewARQ rewrites zero.
		return true
	}
	if a.retxWindowStart.IsZero() || now.Sub(a.retxWindowStart) >= time.Second {
		a.retxWindowStart = now
		a.retxWindowCount = 0
	}
	if a.retxWindowCount >= a.retxBudgetPerSec {
		return false
	}
	a.retxWindowCount++
	return true
}

// emitFastRetransmits sends one PACKET_STREAM_RESEND for each candidate
// produced by the per-ACK fast-retransmit detection in ReceiveAck, respecting
// the per-second retransmission budget. Called with a.mu *unlocked* —
// re-acquires the lock for each per-item bookkeeping update.
func (a *ARQ) emitFastRetransmits(jobs []rtxJob, now time.Time) {
	if len(jobs) == 0 {
		return
	}
	for _, j := range jobs {
		a.mu.Lock()
		allowed := a.consumeRetxBudgetLocked(now)
		a.mu.Unlock()
		if !allowed {
			metrics.ArqRetxBudgetDropped.Add(1)
			continue
		}

		ok := a.enqueuer.PushTXPacket(
			Enums.DefaultPacketPriority(Enums.PACKET_STREAM_RESEND),
			Enums.PACKET_STREAM_RESEND,
			j.sn, 0, 0, j.compressionType, 0, j.data,
		)
		if !ok {
			// Roll back the budget consumption so the next attempt is
			// not unfairly throttled.
			a.mu.Lock()
			if a.retxBudgetPerSec > 0 && a.retxWindowCount > 0 {
				a.retxWindowCount--
			}
			a.mu.Unlock()
			a.noteDrainQueueFailure(now)
			continue
		}

		metrics.ArqRetx.Add(1)
		metrics.ArqFastRetx.Add(1)

		a.mu.Lock()
		if info, exists := a.sndBuf[j.sn]; exists {
			info.LastSentAt = now
			info.Dispatched = false
			info.Retries++
			info.SampleEligible = false
			info.OosAckCount = 0
			info.FastRetransmitted = true
			// Grow CurrentRTO like the RTO-driven path so the timer
			// does not fire on the heels of a fast retx.
			grownRTO := time.Duration(float64(info.CurrentRTO) * dataRetransmitRTOGrowthFactor)
			info.CurrentRTO = clampDuration(grownRTO, a.currentDataBaseRTO(), a.maxRTO)
		}
		a.mu.Unlock()
	}
}

func (a *ARQ) noteSuccessfulControlSample(sample time.Duration) {
	a.mu.Lock()
	a.controlAdaptiveRTO = updateAdaptiveRTO(a.controlAdaptiveRTO, sample, a.controlRto, a.controlMaxRto)
	a.mu.Unlock()
}

func (a *ARQ) NoteTXPacketDequeued(packetType uint8, sequenceNum uint16, fragmentID uint8) {
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()

	switch packetType {
	case Enums.PACKET_STREAM_DATA, Enums.PACKET_STREAM_RESEND:
		if info, exists := a.sndBuf[sequenceNum]; exists {
			info.LastSentAt = now
			info.Dispatched = true
		}
	default:
		if !a.enableControlReliability {
			return
		}
		key := uint32(packetType)<<24 | uint32(sequenceNum)<<8 | uint32(fragmentID)
		if info, exists := a.controlSndBuf[key]; exists {
			info.LastSentAt = now
			info.Dispatched = true
		}
	}
}

// ---------------------------------------------------------------------
// Transitions & Hooks
// ---------------------------------------------------------------------
func (a *ARQ) MarkCloseReadSent() {
	a.mu.Lock()
	a.closeReadSent = true

	if a.closeReadReceived {
		a.setState(StateClosing)
	} else {
		a.setState(StateHalfClosedLocal)
	}
	a.mu.Unlock()

	a.tryFinalizeRemoteEOF()
}

func (a *ARQ) MarkCloseReadReceived() {
	a.mu.Lock()
	if a.isVirtual {
		a.mu.Unlock()
		return
	}

	a.closeReadReceived = true

	if a.closeReadSent {
		a.setState(StateClosing)
	} else {
		a.setState(StateHalfClosedRemote)
	}
	a.mu.Unlock()

	a.signalFlushReady()
	a.tryFinalizeRemoteEOF()
}

func (a *ARQ) markCloseReadAcked() {
	a.mu.Lock()
	a.closeReadAcked = true
	a.closeReadAckedAt = time.Now()

	if a.closeReadReceived {
		a.setState(StateClosing)
	}

	a.mu.Unlock()
}

func (a *ARQ) MarkCloseWriteSent() {
	a.mu.Lock()
	a.closeWriteSent = true
	a.localWriterBroken = true
	a.localWriteClosed = true
	a.releaseRcvBufLocked()
	a.rcvBuf = make(map[uint16][]byte)
	if a.closeReadReceived {
		a.setState(StateClosing)
	}
	a.mu.Unlock()
}

func (a *ARQ) MarkCloseWriteReceived() {
	a.mu.Lock()
	if a.isVirtual {
		a.mu.Unlock()
		return
	}
	a.closeWriteReceived = true
	a.stopLocalRead = true

	if remover, ok := a.enqueuer.(queuedDataRemover); ok {
		for sn := range a.sndBuf {
			remover.RemoveQueuedData(sn)
		}
	}
	a.sndBuf = make(map[uint16]*arqDataItem)
	a.invalidateSndLoBoundLocked()
	a.signalWindowNotFull()
	a.mu.Unlock()

	// A peer close-write can empty outbound state without passing through ReceiveAck.
	// If we were draining toward a deferred terminal packet, re-evaluate it now.
	a.settleTerminalDrain()
	a.tryFinalizeRemoteEOF()
}

func (a *ARQ) markCloseWriteAcked() {
	a.mu.Lock()
	a.closeWriteAcked = true
	a.localWriterBroken = true
	a.localWriteClosed = true
	a.mu.Unlock()
}

func (a *ARQ) maybeInitiateClientCloseReadAfterWriterBreak() {
	a.mu.Lock()
	shouldInitiate := a.IsClient &&
		a.localWriterBroken &&
		!a.closed &&
		!a.rstSent &&
		!a.rstReceived &&
		!a.closeReadSent &&
		!a.closeReadReceived
	pendingOutbound := len(a.sndBuf) > 0 || a.localWritePending
	a.mu.Unlock()

	if !shouldInitiate {
		return
	}

	a.Close("Client local endpoint disconnected after write side closed", CloseOptions{
		SendCloseRead: true,
		AfterDrain:    pendingOutbound,
	})
}

func (a *ARQ) tryFinalizeClientLocalDisconnect() {
	a.mu.Lock()
	shouldClose := a.IsClient &&
		!a.closed &&
		a.localWriterBroken &&
		a.closeWriteAcked &&
		a.closeReadSent &&
		a.closeReadAcked &&
		len(a.sndBuf) == 0 &&
		len(a.rcvBuf) == 0 &&
		a.pendingInbound == 0 &&
		!a.localWritePending &&
		!a.waitingAck &&
		!a.deferredClose
	a.mu.Unlock()

	if shouldClose {
		a.finalizeClose("client local disconnect completed")
	}
}

func (a *ARQ) markLocalWriterBroken(reason string) {
	a.mu.Lock()
	a.localWriterBroken = true
	a.localWritePending = false
	a.releaseRcvBufLocked()
	a.rcvBuf = make(map[uint16][]byte)
	a.mu.Unlock()
}

func (a *ARQ) noteClientEOF(now time.Time) {
	a.mu.Lock()
	if a.IsClient && a.clientEOFAt.IsZero() {
		a.clientEOFAt = now
	}
	a.mu.Unlock()
}

func (a *ARQ) halfCloseLocalWriter() {
	a.mu.Lock()
	if a.localWriteClosed || a.closed {
		a.mu.Unlock()
		return
	}

	a.localWriteClosed = true
	conn := a.localConn
	a.mu.Unlock()

	if conn == nil {
		return
	}

	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func (a *ARQ) clearWaitingAck(packetType uint8) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.waitingAck && a.waitingAckFor == packetType {
		a.waitingAck = false
		a.waitingAckFor = 0
		a.ackWaitDeadline = time.Time{}
	}
}

func (a *ARQ) resetDrainTrackingLocked(now time.Time) {
	a.drainProgressAt = now
	a.drainQueueFailAt = time.Time{}
	a.drainQueueFails = 0
	a.drainStallLogged = false
}

func (a *ARQ) noteDrainProgressLocked(now time.Time) {
	a.resetDrainTrackingLocked(now)
}

func (a *ARQ) noteDrainQueueFailure(now time.Time) {
	a.mu.Lock()
	if a.deferredClose || a.state == StateDraining {
		a.drainQueueFailAt = now
		a.drainQueueFails++
	}
	a.mu.Unlock()
}

func (a *ARQ) runFinalAckWatchdog(now time.Time) {
	a.mu.Lock()
	shouldAck := !a.closed &&
		!a.rstSent &&
		!a.rstReceived &&
		a.IsClient &&
		a.closeReadSent &&
		a.closeReadAcked &&
		!a.closeReadReceived &&
		!a.closeWriteSent &&
		!a.closeWriteAcked &&
		!a.closeWriteReceived &&
		!a.localWriterBroken &&
		len(a.rcvBuf) == 0 &&
		a.pendingInbound == 0 &&
		!a.localWritePending &&
		a.rcvNxt > 0 &&
		now.Sub(a.lastActivity) >= 2*time.Second &&
		(a.lastDuplicateAckAt.IsZero() || now.Sub(a.lastDuplicateAckAt) >= 2*time.Second)
	if !shouldAck {
		a.mu.Unlock()
		return
	}

	ackSN := a.rcvNxt - 1
	lastActivityAgo := now.Sub(a.lastActivity).Round(time.Millisecond)
	a.lastDuplicateAckAt = now
	a.mu.Unlock()

	a.logger.Debugf(
		"ARQ Final ACK Watchdog | Session: %d | Stream: %d | AckSeq: %d | LastActivityAgo: %s",
		a.sessionID,
		a.streamID,
		ackSN,
		lastActivityAgo,
	)

	a.enqueuer.PushTXPacket(
		Enums.DefaultPacketPriority(Enums.PACKET_STREAM_DATA_ACK),
		Enums.PACKET_STREAM_DATA_ACK,
		ackSN, 0, 0, 0, 0, nil,
	)
}

func (a *ARQ) clearTrackedControlPacket(packetType uint8, sequenceNum uint16, fragmentID uint8) {
	a.mu.Lock()
	delete(a.controlSndBuf, uint32(packetType)<<24|uint32(sequenceNum)<<8|uint32(fragmentID))
	a.mu.Unlock()
}

func (a *ARQ) tryFinalizeRemoteEOF() {
	a.mu.Lock()
	waitingForCloseReadAck := a.waitingAck && a.waitingAckFor == Enums.PACKET_STREAM_CLOSE_READ
	receiveDrained := (len(a.rcvBuf) == 0 && a.pendingInbound == 0) || a.localWriterBroken
	writeSideSettled := (!a.localWriterBroken && (!a.closeWriteSent || a.closeWriteAcked)) ||
		(a.localWriterBroken && (a.closeWriteAcked || a.closeWriteReceived))
	shouldClose := !a.closed &&
		a.closeReadReceived &&
		receiveDrained &&
		(!a.localWritePending || a.localWriterBroken) &&
		(a.closeReadAcked || (a.closeReadSent && !waitingForCloseReadAck)) &&
		writeSideSettled
	a.mu.Unlock()

	if shouldClose {
		a.finalizeClose("close handshake completed")
		return
	}

	a.tryFinalizeClientLocalDisconnect()
}

func (a *ARQ) tryFinalizePeerResetDrain() bool {
	a.mu.Lock()
	if !a.rstReceived || a.closed {
		a.mu.Unlock()
		return false
	}

	contiguousReady := a.contiguousReadyLocked()
	rcvBufLen := len(a.rcvBuf)
	pendingInbound := a.pendingInbound
	localWritePending := a.localWritePending
	a.mu.Unlock()

	if contiguousReady > 0 {
		a.signalFlushReady()
		return false
	}

	if localWritePending || pendingInbound > 0 {
		return false
	}

	if rcvBufLen > 0 {
		a.finalizeClose("peer reset received with non-contiguous buffered data")
		return true
	}

	a.finalizeClose("peer reset received")
	return true
}

func (a *ARQ) MarkRstSent() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rstSent = true
	a.clearAllQueues(true)
	a.setState(StateReset)
}

func (a *ARQ) MarkRstReceived() {
	a.mu.Lock()
	if a.isVirtual {
		a.mu.Unlock()
		return
	}

	a.rstReceived = true
	a.stopLocalRead = true
	a.clearOutboundStateLocked(true)
	a.setState(StateReset)
	a.mu.Unlock()
	a.signalFlushReady()
}

func (a *ARQ) markRstAcked() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rstAcked = true
	a.clearAllQueues(true)
	a.setState(StateReset)
}

// ---------------------------------------------------------------------
// Core Loops
// ---------------------------------------------------------------------

// ioLoop reads from local socket data and enqueues reliable outbound packets
func (a *ARQ) ioLoop() {
	defer a.wg.Done()

	resetRequired := false
	resetAfterDrain := false
	gracefulEOF := false
	alreadyHandled := false
	var errorReason string
	var transientReadSince time.Time

	buf := make([]byte, max(a.mtu, 1))
	ioReadyTimer := time.NewTimer(100 * time.Millisecond)
	defer func() {
		if !ioReadyTimer.Stop() {
			select {
			case <-ioReadyTimer.C:
			default:
			}
		}
	}()

	for !a.isClosed() {
		a.waitWindowNotFull()

		a.mu.Lock()
		if a.stopLocalRead || a.closed {
			a.mu.Unlock()
			alreadyHandled = true
			break
		}

		if !a.ioReady {
			a.mu.Unlock()
			if !ioReadyTimer.Stop() {
				select {
				case <-ioReadyTimer.C:
				default:
				}
			}
			ioReadyTimer.Reset(100 * time.Millisecond)
			select {
			case <-a.ctx.Done():
				return
			case <-ioReadyTimer.C:
				continue
			}
		}

		if a.localConn == nil {
			a.mu.Unlock()
			errorReason = "Local connection missing"
			resetRequired = true
			break
		}
		localConn := a.localConn
		a.mu.Unlock()

		if c, ok := localConn.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		}

		n, err := localConn.Read(buf)
		if n > 0 {
			transientReadSince = time.Time{}
			raw := append([]byte(nil), buf[:n]...)

			now := time.Now()
			a.mu.Lock()
			a.lastActivity = now
			sn := a.sndNxt
			a.sndNxt++
			currentRTO := a.currentDataBaseRTO()
			a.sndBuf[sn] = &arqDataItem{
				Data:            raw,
				CreatedAt:       now,
				LastSentAt:      time.Time{},
				Dispatched:      false,
				Retries:         0,
				CurrentRTO:      currentRTO,
				SampleEligible:  true,
				CompressionType: a.compressionType,
				TTL:             0,
			}
			a.trackInsertedSndSNLocked(sn)
			a.mu.Unlock()

			ok := a.enqueuer.PushTXPacket(
				Enums.DefaultPacketPriority(Enums.PACKET_STREAM_DATA),
				Enums.PACKET_STREAM_DATA,
				sn, 0, 0, a.compressionType, 0, raw,
			)
			if !ok {
				a.mu.Lock()
				if info, exists := a.sndBuf[sn]; exists {
					info.Dispatched = true
					info.LastSentAt = time.Now().Add(-info.CurrentRTO)
				}
				a.mu.Unlock()
			}
		}

		if err != nil {
			switch classifyIOError(err) {
			case ioErrorTimeout:
				transientReadSince = time.Time{}
				continue
			case ioErrorTransient:
				now := time.Now()
				if transientReadSince.IsZero() {
					transientReadSince = now
				} else if now.Sub(transientReadSince) > ioTransientReadBudget {
					errorReason = "Repeated transient read errors: " + err.Error()
					resetRequired = true
					resetAfterDrain = n > 0
					break
				}
				time.Sleep(ioRetryBackoff)
				continue
			case ioErrorEOF:
				transientReadSince = time.Time{}
				errorReason = "Local App Closed Connection (EOF)"
				a.noteClientEOF(time.Now())
				gracefulEOF = true
			case ioErrorClosed:
				transientReadSince = time.Time{}
				if a.isGracefulCloseInProgress() {
					alreadyHandled = true
					break
				}
				errorReason = "Local connection closed"
				resetRequired = true
				resetAfterDrain = n > 0
			default:
				transientReadSince = time.Time{}
				errorReason = "Read Error: " + err.Error()
				resetRequired = true
				resetAfterDrain = n > 0
			}
			break
		}

		if n <= 0 {
			continue
		}
	}

	if a.isClosed() || alreadyHandled {
		return
	}

	if resetRequired {
		a.Close(errorReason, CloseOptions{SendRST: true, AfterDrain: resetAfterDrain})
		return
	}

	if gracefulEOF {
		a.Close(errorReason, CloseOptions{SendCloseRead: true, AfterDrain: true})
		return
	}
}

// ---------------------------------------------------------------------
// Terminal Emit / Drain Helpers
// ---------------------------------------------------------------------

// deferTerminalPacket arms a drain-before-terminal phase.
// It stops new local reads, waits for pending outbound data to drain,
// then `settleTerminalDrain` decides whether to emit the requested close packet or fall back to RST.
func (a *ARQ) deferTerminalPacket(reason string, packetType uint8) {
	a.mu.Lock()
	if a.closed || a.isVirtual {
		a.mu.Unlock()
		return
	}

	if a.state != StateReset && a.state != StateClosed {
		a.setState(StateDraining)
	}

	a.stopLocalRead = true
	a.deferredClose = true
	a.deferredReason = reason
	a.deferredPacket = packetType

	deadline := time.Now().Add(a.terminalDrainTimeout)
	if a.deferredDeadline.IsZero() || deadline.After(a.deferredDeadline) {
		a.deferredDeadline = deadline
	}
	a.resetDrainTrackingLocked(time.Now())

	sndBufLen := len(a.sndBuf)
	a.mu.Unlock()

	if sndBufLen == 0 {
		a.settleTerminalDrain()
	}
}

// settleTerminalDrain completes a previously deferred terminal close.
func (a *ARQ) settleTerminalDrain() {
	var (
		packetType uint8
		shouldEmit bool
		reason     string
	)

	a.mu.Lock()
	if a.closed || !a.deferredClose {
		a.mu.Unlock()
		return
	}

	switch {
	case len(a.sndBuf) == 0:
		shouldEmit = true
		packetType = a.deferredPacket
		reason = a.deferredReason
	case !a.deferredDeadline.IsZero() && time.Now().After(a.deferredDeadline):
		shouldEmit = true
		packetType = Enums.PACKET_STREAM_RST
		reason = a.deferredReason + " but drain timeout expired"
	default:
		a.mu.Unlock()
		return
	}

	a.deferredClose = false
	a.deferredReason = ""
	a.deferredDeadline = time.Time{}
	a.deferredPacket = 0
	a.mu.Unlock()
	if shouldEmit {
		a.Close(reason, CloseOptions{
			SendCloseRead:  packetType == Enums.PACKET_STREAM_CLOSE_READ,
			SendCloseWrite: packetType == Enums.PACKET_STREAM_CLOSE_WRITE,
			SendRST:        packetType == Enums.PACKET_STREAM_RST,
		})
	}
}

func (a *ARQ) emitTerminalPacketWithTTL(packetType uint8, reason string, ttl time.Duration) {
	a.mu.Lock()
	if a.closed || a.isVirtual {
		a.mu.Unlock()
		return
	}

	a.closeReason = reason
	a.stopLocalRead = true
	a.deferredClose = false
	a.deferredReason = ""
	a.deferredDeadline = time.Time{}
	a.deferredPacket = 0

	if a.waitingAck && a.waitingAckFor == packetType {
		a.mu.Unlock()
		return
	}

	switch packetType {
	case Enums.PACKET_STREAM_CLOSE_READ:
		if a.rstSent || a.rstReceived || a.closeReadSent {
			a.mu.Unlock()
			return
		}
		if a.closeReadSeqSent == nil {
			seq := uint16(0)
			a.closeReadSeqSent = &seq
		}
		seq := *a.closeReadSeqSent
		a.waitingAck = true
		a.waitingAckFor = packetType
		a.ackWaitDeadline = time.Now().Add(a.terminalAckWait)
		a.mu.Unlock()

		a.MarkCloseReadSent()
		ackType := uint8(Enums.PACKET_STREAM_CLOSE_READ_ACK)
		a.SendControlPacketWithTTL(Enums.PACKET_STREAM_CLOSE_READ, seq, 0, 0, nil, Enums.DefaultPacketPriority(Enums.PACKET_STREAM_CLOSE_READ), a.enableControlReliability, &ackType, ttl)
	case Enums.PACKET_STREAM_CLOSE_WRITE:
		if a.rstReceived || a.rstSent || a.closeWriteSent {
			a.mu.Unlock()
			return
		}
		if a.closeWriteSeqSent == nil {
			seq := uint16(0)
			a.closeWriteSeqSent = &seq
		}
		seq := *a.closeWriteSeqSent
		a.waitingAck = true
		a.waitingAckFor = packetType
		a.ackWaitDeadline = time.Now().Add(a.terminalAckWait)
		a.mu.Unlock()

		a.MarkCloseWriteSent()
		ackType := uint8(Enums.PACKET_STREAM_CLOSE_WRITE_ACK)
		a.SendControlPacketWithTTL(Enums.PACKET_STREAM_CLOSE_WRITE, seq, 0, 0, nil, Enums.DefaultPacketPriority(Enums.PACKET_STREAM_CLOSE_WRITE), a.enableControlReliability, &ackType, ttl)
	case Enums.PACKET_STREAM_RST:
		if a.rstReceived || a.rstSent {
			a.mu.Unlock()
			return
		}
		if a.rstSeqSent == nil {
			seq := uint16(0)
			a.rstSeqSent = &seq
		}
		rstSeq := *a.rstSeqSent
		a.clearAllQueues(true)
		a.waitingAck = true
		a.waitingAckFor = packetType
		a.ackWaitDeadline = time.Now().Add(a.terminalAckWait)
		a.mu.Unlock()

		a.MarkRstSent()
		ackType := uint8(Enums.PACKET_STREAM_RST_ACK)
		a.SendControlPacketWithTTL(Enums.PACKET_STREAM_RST, rstSeq, 0, 0, nil, Enums.DefaultPacketPriority(Enums.PACKET_STREAM_RST), a.enableControlReliability, &ackType, ttl)
	default:
		a.mu.Unlock()
	}
}

// ---------------------------------------------------------------------
// Retransmit Scheduler
// ---------------------------------------------------------------------

func (a *ARQ) retransmitLoop() {
	defer a.wg.Done()

	timer := time.NewTimer(100 * time.Millisecond)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	for {
		a.mu.Lock()
		rtoFactor := a.rto
		if a.enableControlReliability && a.controlRto < rtoFactor {
			rtoFactor = a.controlRto
		}

		baseInterval := max(rtoFactor/3, 50*time.Millisecond)

		hasPending := len(a.sndBuf) > 0 || (a.enableControlReliability && len(a.controlSndBuf) > 0)
		a.mu.Unlock()

		interval := baseInterval
		if !hasPending {
			interval = max(baseInterval*4, 100*time.Millisecond)
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(interval)
		select {
		case <-a.ctx.Done():
			return
		case <-timer.C:
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					a.logger.Debugf("Retransmit check panic on stream %d: %v", a.streamID, r)
				}
			}()
			a.checkRetransmits()
		}()
	}
}

// ---------------------------------------------------------------------
// Data Plane
// ---------------------------------------------------------------------

// ReceiveData handles inbound STREAM_DATA and emit STREAM_DATA_ACK.
func (a *ARQ) ReceiveData(sn uint16, data []byte) bool {
	a.mu.Lock()
	if a.closed || a.rstReceived || a.rstSent {
		a.mu.Unlock()
		return false
	}

	if a.localWriterBroken {
		needCloseWrite := a.localWriterBroken &&
			!a.closeWriteSent &&
			!(a.waitingAck && a.waitingAckFor == Enums.PACKET_STREAM_CLOSE_WRITE) &&
			!a.closed &&
			!a.rstReceived &&
			!a.rstSent
		a.mu.Unlock()
		if needCloseWrite {
			a.Close("Inbound data received after local writer closed", CloseOptions{SendCloseWrite: true})
		}
		return false
	}

	a.pendingInbound++
	a.mu.Unlock()

	// Pool-backed copy of the inbound payload. Lifetime is bounded:
	//   1. enqueued onto a.rxChan
	//   2. processed by processReceivedData, stored into a.rcvBuf[sn]
	//   3. consumed by writeLoop, which calls streamutil.Put(data)
	// On channel-full or duplicate, we release immediately.
	safeData := streamutil.Get(len(data))
	copy(safeData, data)

	select {
	case a.rxChan <- rxPayload{sn: sn, data: safeData}:
		return true
	default:
		streamutil.Put(safeData)
		a.mu.Lock()
		if a.pendingInbound > 0 {
			a.pendingInbound--
		}
		a.mu.Unlock()
		return false
	}
}

func (a *ARQ) rxLoop() {
	defer a.wg.Done()

	for {
		select {
		case <-a.ctx.Done():
			drained := 0
			for {
				select {
				case <-a.rxChan:
					drained++
				default:
					if drained > 0 {
						a.mu.Lock()
						a.pendingInbound -= drained
						if a.pendingInbound < 0 {
							a.pendingInbound = 0
						}
						a.mu.Unlock()
					}
					return
				}
			}
		case payload := <-a.rxChan:
			a.processReceivedData(payload.sn, payload.data)
		}
	}
}

// processReceivedData handles inbound data on the original per-packet path so
// ACK/NACK/flush timing stays conservative under heavy loss and reordering.
func (a *ARQ) processReceivedData(sn uint16, data []byte) {
	now := time.Now()

	a.mu.Lock()
	if a.pendingInbound > 0 {
		a.pendingInbound--
	}

	if a.localWriterBroken || a.closeWriteSent || a.closeWriteAcked {
		needCloseWrite := a.localWriterBroken &&
			!a.closeWriteSent &&
			!(a.waitingAck && a.waitingAckFor == Enums.PACKET_STREAM_CLOSE_WRITE) &&
			!a.closed &&
			!a.rstReceived &&
			!a.rstSent
		a.mu.Unlock()
		if needCloseWrite {
			a.Close("Inbound data received after local writer closed", CloseOptions{SendCloseWrite: true})
		}
		return
	}

	a.lastActivity = now
	diff := sn - a.rcvNxt

	if diff >= 32768 {
		// Already-delivered sequence number. Count as duplicate, release
		// the pooled buffer, and re-ACK so the peer stops retransmitting.
		a.mu.Unlock()
		streamutil.Put(data)
		metrics.ArqDuplicateRx.Add(1)
		a.enqueuer.PushTXPacket(
			Enums.DefaultPacketPriority(Enums.PACKET_STREAM_DATA_ACK),
			Enums.PACKET_STREAM_DATA_ACK,
			sn, 0, 0, 0, 0, nil,
		)
		return
	}

	if int(diff) > a.receiveWindowSize {
		// Beyond the advertised window — drop, but release pool buffer.
		a.mu.Unlock()
		streamutil.Put(data)
		return
	}

	_, exists := a.rcvBuf[sn]
	if !exists && len(a.rcvBuf) >= a.receiveWindowSize && sn != a.rcvNxt {
		a.mu.Unlock()
		streamutil.Put(data)
		return
	}

	if !exists {
		a.rcvBuf[sn] = data
	} else {
		// Duplicate inside window — release the new copy, keep the
		// already-buffered original so writeLoop's release is balanced.
		metrics.ArqDuplicateRx.Add(1)
	}
	a.mu.Unlock()

	if exists {
		streamutil.Put(data)
	}

	a.enqueuer.PushTXPacket(
		Enums.DefaultPacketPriority(Enums.PACKET_STREAM_DATA_ACK),
		Enums.PACKET_STREAM_DATA_ACK,
		sn, 0, 0, 0, 0, nil,
	)

	if !exists {
		a.clearSentDataNack(sn)
	}
	a.maybeSendDataNacks(sn)
	a.signalFlushReady()
}

func (a *ARQ) processReceivedDataBatch(batch []rxPayload) {
	for _, payload := range batch {
		a.processReceivedData(payload.sn, payload.data)
	}
}

func (a *ARQ) writeLoop() {
	defer a.wg.Done()

	const maxRetainedMergeBuf = 256 * 1024

	var mergeBuf []byte              // reusable merge buffer across iterations
	toWrite := make([][]byte, 0, 16) // reusable slice for contiguous chunks
	// pooledChunks tracks the original per-segment buffers that were pulled
	// from rcvBuf in this iteration. They were allocated by streamutil in
	// ReceiveData and must be returned to the pool once the write attempt
	// (success or failure) finishes. The merge path replaces toWrite[0]
	// with the merged buffer, so we keep the originals tracked separately.
	pooledChunks := make([][]byte, 0, 16)
	// oversizedMerged holds a pool-backed merged buffer used for the rare
	// branch where the coalesced size exceeds maxRetainedMergeBuf. We
	// release it back to streamutil after the write so we don't retain a
	// jumbo allocation across iterations.
	var oversizedMerged []byte

	for {
		// Check rcvBuf before blocking — signals may have been coalesced
		// while we were writing, so data can be ready without a new signal.
		a.mu.RLock()
		hasReady := false
		if _, ok := a.rcvBuf[a.rcvNxt]; ok {
			hasReady = true
		}
		a.mu.RUnlock()

		if !hasReady {
			select {
			case <-a.ctx.Done():
				return
			case <-a.flushSignal:
			}
		}

		for {
			if a.isClosed() {
				return
			}

			a.mu.Lock()
			if !a.ioReady || a.closed {
				a.mu.Unlock()
				break
			}

			if a.localConn == nil {
				a.mu.Unlock()
				a.Close("Local connection missing for writer", CloseOptions{SendRST: true, AfterDrain: true})
				return
			}

			toWrite = toWrite[:0]
			pooledChunks = pooledChunks[:0]
			for {
				data, exists := a.rcvBuf[a.rcvNxt]
				if !exists {
					break
				}
				toWrite = append(toWrite, data)
				pooledChunks = append(pooledChunks, data)
				delete(a.rcvBuf, a.rcvNxt)
				a.rcvNxt++
			}
			a.localWritePending = len(toWrite) > 0
			conn := a.localConn
			a.mu.Unlock()

			if len(toWrite) == 0 {
				a.mu.Lock()
				cr := a.closeReadReceived
				rstReceived := a.rstReceived
				pendingInbound := a.pendingInbound
				a.mu.Unlock()
				if cr && pendingInbound == 0 {
					a.halfCloseLocalWriter()
				}
				if rstReceived && a.tryFinalizePeerResetDrain() {
					return
				}
				a.tryFinalizeRemoteEOF()
				break
			}

			// Coalesce contiguous chunks into a single write to reduce syscalls.
			if len(toWrite) > 1 {
				totalSize := 0
				for _, chunk := range toWrite {
					totalSize += len(chunk)
				}

				oversizedMerged = nil
				merged := mergeBuf
				if totalSize <= maxRetainedMergeBuf {
					if cap(merged) >= totalSize {
						merged = merged[:0]
					} else {
						// Allocate fresh (kept retained across iterations,
						// so it is intentionally outside the pool — pool
						// hits would otherwise discard the cap we want
						// to preserve for the next burst).
						merged = make([]byte, 0, totalSize)
					}
					mergeBuf = merged
				} else {
					// Rare jumbo path — draw from the pool so the spike
					// doesn't pin a multi-MiB allocation on the GC heap.
					merged = streamutil.GetCap(0, totalSize)
					oversizedMerged = merged
				}
				for _, chunk := range toWrite {
					merged = append(merged, chunk...)
				}
				if oversizedMerged != nil {
					oversizedMerged = merged
				}
				toWrite = toWrite[:1]
				toWrite[0] = merged
			}

			shouldExit := false
			recheckClose := false
			func() {
				defer func() {
					// Release the per-segment pool buffers pulled from
					// rcvBuf this iteration. Safe regardless of write
					// success — the data has either been delivered or
					// the stream is about to close.
					for i, chunk := range pooledChunks {
						streamutil.Put(chunk)
						pooledChunks[i] = nil
					}
					if oversizedMerged != nil {
						streamutil.Put(oversizedMerged)
						oversizedMerged = nil
					}
					a.mu.Lock()
					a.localWritePending = false
					a.mu.Unlock()
					if recheckClose {
						a.tryFinalizeRemoteEOF()
					}
				}()

				for _, chunk := range toWrite {
					remaining := chunk
					transientRetries := 0
					for len(remaining) > 0 {
						if wd, ok := conn.(writeDeadlineSetter); ok {
							_ = wd.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
						}
						a.writeLock.Lock()
						n, err := conn.Write(remaining)
						a.writeLock.Unlock()
						if n > 0 {
							remaining = remaining[n:]
						}
						if err == nil {
							continue
						}

						class := classifyIOError(err)
						if class == ioErrorTimeout || class == ioErrorTransient {
							if transientRetries >= ioTransientWriteBudget {
								a.markLocalWriterBroken("local app write timeout/transient budget exceeded: " + err.Error())
								if a.isGracefulCloseInProgress() {
									a.Close("Local App Write Error during graceful close: "+err.Error(), CloseOptions{SendCloseWrite: true})
									shouldExit = true
									return
								}
								a.Close("Local App Write Error: "+err.Error(), CloseOptions{SendCloseWrite: true})
								shouldExit = true
								return
							}
							transientRetries++
							time.Sleep(ioRetryBackoff)
							continue
						}

						if class == ioErrorEOF || class == ioErrorClosed {
							a.markLocalWriterBroken("local app writer closed: " + err.Error())
							if a.isGracefulCloseInProgress() {
								a.Close("Local App Closed Connection (writer closed during graceful close)", CloseOptions{SendCloseWrite: true})
								shouldExit = true
								return
							}
							a.Close("Local App Closed Connection (writer closed)", CloseOptions{SendCloseWrite: true})
							shouldExit = true
							return
						}

						if a.isGracefulCloseInProgress() {
							a.markLocalWriterBroken("local app write error during graceful close: " + err.Error())
							a.Close("Local App Write Error during graceful close: "+err.Error(), CloseOptions{SendCloseWrite: true})
							shouldExit = true
							return
						}
						a.markLocalWriterBroken("local app write error: " + err.Error())
						a.Close("Local App Write Error: "+err.Error(), CloseOptions{SendCloseWrite: true})
						shouldExit = true
						return
					}
				}
			}()
			if shouldExit {
				return
			}
			if a.tryFinalizePeerResetDrain() {
				return
			}
			a.tryFinalizeRemoteEOF()
		}
	}
}

func (a *ARQ) isGracefulCloseInProgress() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return true
	}

	if a.waitingAck && (a.waitingAckFor == Enums.PACKET_STREAM_CLOSE_READ || a.waitingAckFor == Enums.PACKET_STREAM_CLOSE_WRITE) {
		return true
	}

	if a.deferredClose && (a.deferredPacket == Enums.PACKET_STREAM_CLOSE_READ || a.deferredPacket == Enums.PACKET_STREAM_CLOSE_WRITE) {
		return true
	}

	switch a.state {
	case StateHalfClosedLocal, StateHalfClosedRemote, StateClosing, StateDraining, StateTimeWait:
		return true
	}

	return a.closeReadSent || a.closeReadReceived || a.closeWriteSent || a.closeWriteReceived
}

// ReceiveAck resolves inbound STREAM_DATA_ACK and frees SEND_WINDOW backpressure buffer slots.
// It returns true only when this ARQ instance was actually tracking the data packet.
//
// Step 5 — fast-retransmit hook: every ACK that arrives also increments the
// OosAckCount of every *older* still-pending segment in sndBuf. The closest
// MasterDnsVPN analogue to TCP's duplicate-ACK is "ACK for a higher SN while
// an older SN is still un-acked". Once that count reaches a.fastRetxThreshold
// for a given older segment, we retransmit it immediately without waiting
// for the RTO timer. The fast-retransmit candidates are collected under the
// lock and emitted afterwards to keep the critical section short.
func (a *ARQ) ReceiveAck(packetType uint8, sn uint16) bool {
	a.mu.Lock()
	now := time.Now()
	a.lastActivity = now
	handled := false
	shouldSignalWindow := false
	var sample time.Duration
	sampleEligible := false
	var fastRetxJobs []rtxJob

	// Pre-snapshot the lowest pending SN behind the just-acked one. The
	// fast-retransmit heuristic only cares about segments that are "older
	// than the acked SN" — and the most likely loss candidate is the very
	// front of the window. Bumping only the single oldest still-pending
	// segment keeps the per-ACK cost bounded and matches TCP's behaviour
	// where 3 dup-ACKs flag the segment at snd.una for fast retransmit.
	if info, exists := a.sndBuf[sn]; exists {
		if info.SampleEligible && info.Dispatched && !info.LastSentAt.IsZero() {
			sample = now.Sub(info.LastSentAt)
			sampleEligible = true
		}
		// Remember whether the just-acked sn was at (or before) the
		// cached lower-bound BEFORE the delete. If it was, then by
		// definition no older pending segment exists in sndBuf and we
		// can skip the whole fast-retransmit walk — the dominant case
		// for in-order ACK flows. seqBehind tolerates uint16 wrap.
		hintValid := a.sndLoBoundValid
		hintSN := a.sndLoBoundSN
		noOlderPending := hintValid && (sn == hintSN || seqBehind(hintSN, sn))

		delete(a.sndBuf, sn)
		a.trackDeletedSndSNLocked(sn)

		// Bump OOS-ACK on the single oldest still-pending dispatched
		// segment whose sn is strictly older than the just-acked sn.
		// Skipped entirely when the lower-bound hint proves no older
		// segment can exist — which is the typical case under in-order
		// ACK arrival.
		if a.fastRetxThreshold > 0 && !noOlderPending && len(a.sndBuf) > 0 {
			var oldestInfo *arqDataItem
			var oldestSN uint16
			var oldestDist uint16
			for candSN, candInfo := range a.sndBuf {
				if candInfo == nil || !candInfo.Dispatched {
					continue
				}
				diff := uint16(sn - candSN)
				if diff == 0 || diff >= 32768 {
					continue // candSN is not strictly older than sn
				}
				if oldestInfo == nil || diff > oldestDist {
					oldestInfo = candInfo
					oldestSN = candSN
					oldestDist = diff
				}
			}
			if oldestInfo != nil {
				if oldestInfo.OosAckCount < 255 {
					oldestInfo.OosAckCount++
				}
				if !oldestInfo.FastRetransmitted && oldestInfo.OosAckCount >= a.fastRetxThreshold {
					fastRetxJobs = []rtxJob{{
						sn:              oldestSN,
						data:            oldestInfo.Data,
						compressionType: oldestInfo.CompressionType,
					}}
				}
				// Re-validate the lower-bound hint with the freshly
				// computed minimum so the next ACK in this burst can
				// take the fast-path. Without this, every ACK that
				// follows a gap would re-walk the entire sndBuf.
				a.sndLoBoundSN = oldestSN
				a.sndLoBoundValid = true
			}
		}

		if a.deferredClose || a.state == StateDraining {
			a.noteDrainProgressLocked(now)
		}
		if len(a.sndBuf) < a.limit {
			shouldSignalWindow = true
		}
		handled = true
	}
	a.mu.Unlock()

	if shouldSignalWindow {
		a.signalWindowNotFull()
	}

	if handled {
		if sampleEligible {
			a.noteSuccessfulDataSample(sample)
		}
		if remover, ok := a.enqueuer.(queuedDataRemover); ok {
			remover.RemoveQueuedData(sn)
		}

		// Fire fast-retransmits *after* releasing the lock and after the
		// adaptive RTO sample update. This ordering matches the existing
		// checkRetransmits convention (no enqueuer call under the lock).
		if len(fastRetxJobs) > 0 {
			a.emitFastRetransmits(fastRetxJobs, now)
		}

		if a.closeReadReceivedLocked() {
			a.tryFinalizeRemoteEOF()
		}
		a.settleTerminalDrain()
	}
	return handled
}

func (a *ARQ) HandleDataNack(sn uint16) bool {
	if a.isClosed() || a.IsReset() {
		return false
	}

	now := time.Now()
	a.mu.Lock()
	a.lastActivity = now
	info, exists := a.sndBuf[sn]
	if !exists {
		a.mu.Unlock()
		return false
	}
	prevNackSentAt := info.LastNackSentAt
	if !prevNackSentAt.IsZero() && now.Sub(prevNackSentAt) < a.dataNackRepeatInterval {
		a.mu.Unlock()
		return false
	}
	info.LastNackSentAt = now

	data := append([]byte(nil), info.Data...)
	compressionType := info.CompressionType
	ttl := info.TTL
	a.mu.Unlock()

	ok := a.enqueuer.PushTXPacket(
		Enums.DefaultPacketPriority(Enums.PACKET_STREAM_RESEND),
		Enums.PACKET_STREAM_RESEND,
		sn, 0, 0, compressionType, ttl, data,
	)
	if !ok {
		a.mu.Lock()
		if info, exists := a.sndBuf[sn]; exists && info.LastNackSentAt.Equal(now) {
			info.LastNackSentAt = prevNackSentAt
		}
		a.mu.Unlock()
		return false
	}
	a.mu.Lock()
	if info, exists := a.sndBuf[sn]; exists {
		info.SampleEligible = false
		// NACK-driven retransmission is functionally identical to a
		// fast retransmit: the segment is back on the wire because the
		// receiver explicitly asked for it. Reset the OOS-ACK counter
		// so subsequent ACK arrivals can not immediately re-trigger a
		// fast retx for the same packet.
		info.OosAckCount = 0
		info.FastRetransmitted = true
	}
	a.mu.Unlock()
	return true
}

func (a *ARQ) maybeSendDataNacks(sn uint16) {
	if a == nil || a.dataNackMaxGap <= 0 {
		return
	}

	a.mu.RLock()
	rcvNxt := a.rcvNxt
	closed := a.closed
	a.mu.RUnlock()
	if closed {
		return
	}

	diff := sn - rcvNxt
	if diff == 0 || diff >= 32768 {
		return
	}

	a.mu.Lock()
	a.pruneDataNackStateLocked(rcvNxt)
	a.mu.Unlock()

	windowSpan := uint16(a.dataNackMaxGap)
	a.mu.RLock()
	missingSeqs := make([]uint16, 0, a.dataNackMaxGap)
	if diff <= windowSpan {
		for missing := rcvNxt; missing != sn; missing++ {
			if _, buffered := a.rcvBuf[missing]; buffered {
				continue
			}
			missingSeqs = append(missingSeqs, missing)
		}
	} else {
		seen := make(map[uint16]struct{}, maxI(2, a.dataNackMaxGap/20+1))
		sampleCount := maxI(1, (a.dataNackMaxGap+19)/20) // ~5% of configured gap, at least 1

		for missing, added := rcvNxt, 0; missing != sn && added < sampleCount; missing++ {
			if _, buffered := a.rcvBuf[missing]; buffered {
				continue
			}
			missingSeqs = append(missingSeqs, missing)
			seen[missing] = struct{}{}
			added++
		}

		frontier := uint16(uint32(rcvNxt) + uint32(windowSpan) - 1)
		for candidate := frontier; ; candidate-- {
			if _, buffered := a.rcvBuf[candidate]; !buffered {
				if _, exists := seen[candidate]; !exists {
					missingSeqs = append(missingSeqs, candidate)
				}
				break
			}
			if candidate == rcvNxt {
				break
			}
		}
	}
	a.mu.RUnlock()

	now := time.Now()
	for _, missing := range missingSeqs {
		if !a.shouldSendDataNack(missing, now) {
			continue
		}
		if !a.enqueuer.PushTXPacket(
			Enums.DefaultPacketPriority(Enums.PACKET_STREAM_DATA_NACK),
			Enums.PACKET_STREAM_DATA_NACK,
			missing, 0, 0, 0, 0, nil,
		) {
			a.noteDrainQueueFailure(now)
			continue
		}
		a.noteDataNackSent(missing, now)
	}
}

func (a *ARQ) shouldSendDataNack(sn uint16, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	firstSeenAt, exists := a.firstDataNackSeen[sn]
	if !exists {
		a.firstDataNackSeen[sn] = now
		return a.dataNackInitialDelay <= 0
	}
	if a.dataNackInitialDelay > 0 && now.Sub(firstSeenAt) < a.dataNackInitialDelay {
		return false
	}

	lastSentAt, exists := a.lastDataNackSent[sn]
	if !exists {
		return true
	}
	return now.Sub(lastSentAt) >= a.dataNackRepeatInterval
}

func (a *ARQ) noteDataNackSent(sn uint16, now time.Time) {
	a.mu.Lock()
	a.lastDataNackSent[sn] = now
	a.mu.Unlock()
}

func seqBehind(base uint16, candidate uint16) bool {
	return candidate != base && uint16(base-candidate) < 32768
}

func (a *ARQ) pruneDataNackStateLocked(rcvNxt uint16) {
	for sn := range a.firstDataNackSeen {
		if seqBehind(rcvNxt, sn) {
			delete(a.firstDataNackSeen, sn)
		}
	}
	for sn := range a.lastDataNackSent {
		if seqBehind(rcvNxt, sn) {
			delete(a.lastDataNackSent, sn)
		}
	}
}

func (a *ARQ) clearSentDataNack(sn uint16) {
	a.mu.Lock()
	delete(a.firstDataNackSeen, sn)
	delete(a.lastDataNackSent, sn)
	a.mu.Unlock()

	if remover, ok := a.enqueuer.(queuedDataNackRemover); ok {
		remover.RemoveQueuedDataNack(sn)
	}
}

func (a *ARQ) gapRecoveryCandidatesLocked() []uint16 {
	if a.dataNackMaxGap <= 0 || len(a.rcvBuf) == 0 {
		return nil
	}
	if _, exists := a.rcvBuf[a.rcvNxt]; exists {
		return nil
	}

	maxGap := uint16(a.dataNackMaxGap)
	missingSeqs := make([]uint16, 0, a.dataNackMaxGap)
	for candidate := a.rcvNxt; ; candidate++ {
		if uint16(candidate-a.rcvNxt) >= maxGap {
			break
		}
		if _, buffered := a.rcvBuf[candidate]; buffered {
			continue
		}
		missingSeqs = append(missingSeqs, candidate)
	}
	return missingSeqs
}

func (a *ARQ) runGapRecoveryWatchdog(now time.Time) {
	if a == nil || a.dataNackMaxGap <= 0 || a.isClosed() {
		return
	}

	a.mu.RLock()
	closed := a.closed
	lastActivity := a.lastActivity
	rcvNxt := a.rcvNxt
	missingSeqs := a.gapRecoveryCandidatesLocked()
	a.mu.RUnlock()

	if closed || len(missingSeqs) == 0 {
		return
	}

	if now.Sub(lastActivity) < a.dataNackRepeatInterval {
		return
	}

	a.mu.Lock()
	a.pruneDataNackStateLocked(rcvNxt)
	a.mu.Unlock()

	for _, missing := range missingSeqs {
		if !a.shouldSendDataNack(missing, now) {
			continue
		}
		if !a.enqueuer.PushTXPacket(
			Enums.DefaultPacketPriority(Enums.PACKET_STREAM_DATA_NACK),
			Enums.PACKET_STREAM_DATA_NACK,
			missing, 0, 0, 0, 0, nil,
		) {
			continue
		}
		a.noteDataNackSent(missing, now)
	}
}

// ---------------------------------------------------------------------
// Control Plane Verification
// ---------------------------------------------------------------------

func (a *ARQ) SendControlPacketWithTTL(packetType uint8, sequenceNum uint16, fragmentID uint8, totalFragments uint8, payload []byte, priority int, trackForAck bool, customAckType *uint8, ttl time.Duration) bool {
	copyData := append([]byte(nil), payload...)
	priority = Enums.NormalizePacketPriority(packetType, priority)

	if !a.enableControlReliability || !trackForAck {
		return a.enqueuer.PushTXPacket(priority, packetType, sequenceNum, fragmentID, totalFragments, 0, ttl, copyData)
	}

	var expectedAck uint8
	if customAckType != nil {
		expectedAck = *customAckType
	} else {
		val, ok := Enums.ControlAckFor(packetType)
		if !ok {
			return true
		}
		expectedAck = val
	}

	key := uint32(packetType)<<24 | uint32(sequenceNum)<<8 | uint32(fragmentID)
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.controlSndBuf[key]; exists {
		return true
	}

	initialRTO := a.currentControlBaseRTO()
	if setupControlPacketTypes[packetType] {
		altRto := 350 * time.Millisecond
		if altRto < initialRTO {
			initialRTO = altRto
		}
	}

	ok := a.enqueuer.PushTXPacket(priority, packetType, sequenceNum, fragmentID, totalFragments, 0, ttl, copyData)

	dispatchedFlag := false
	lastSentAt := time.Time{}
	if !ok {
		dispatchedFlag = true
		lastSentAt = now.Add(-initialRTO)
	}

	a.controlSndBuf[key] = &arqControlItem{
		PacketType:     packetType,
		SequenceNum:    sequenceNum,
		FragmentID:     fragmentID,
		TotalFragments: totalFragments,
		AckType:        expectedAck,
		Payload:        copyData,
		Priority:       priority,
		CreatedAt:      now,
		LastSentAt:     lastSentAt,
		Dispatched:     dispatchedFlag,
		Retries:        0,
		CurrentRTO:     initialRTO,
		SampleEligible: true,
		TTL:            ttl,
	}

	return ok
}

func (a *ARQ) handleTrackedPacketTTLExpiry(packetType uint8, reason string) {
	if _, ok := Enums.GetPacketCloseStream(packetType); ok &&
		packetType != Enums.PACKET_STREAM_CLOSE_READ &&
		packetType != Enums.PACKET_STREAM_CLOSE_WRITE {
		a.finalizeClose(reason)
		return
	}

	a.Close(reason, CloseOptions{SendRST: true})
}

func (a *ARQ) handleTrackedTerminalAck(originPtype uint8) bool {
	if _, ok := Enums.GetPacketCloseStream(originPtype); ok &&
		originPtype != Enums.PACKET_STREAM_CLOSE_READ &&
		originPtype != Enums.PACKET_STREAM_CLOSE_WRITE &&
		originPtype != Enums.PACKET_STREAM_RST {
		a.finalizeClose(fmt.Sprintf("%s acknowledged", Enums.PacketTypeName(originPtype)))
		return true
	}

	return false
}

func (a *ARQ) handleWaitingTerminalAck(ackPacketType uint8, isWaitingCloseRead bool, isWaitingCloseWrite bool, isWaitingRst bool) bool {
	if ackPacketType == Enums.PACKET_STREAM_CLOSE_READ_ACK && isWaitingCloseRead {
		a.markCloseReadAcked()
		a.clearWaitingAck(Enums.PACKET_STREAM_CLOSE_READ)
		a.tryFinalizeRemoteEOF()
		return true
	}

	if ackPacketType == Enums.PACKET_STREAM_CLOSE_WRITE_ACK && isWaitingCloseWrite {
		a.markCloseWriteAcked()
		a.clearWaitingAck(Enums.PACKET_STREAM_CLOSE_WRITE)
		a.maybeInitiateClientCloseReadAfterWriterBreak()
		a.tryFinalizeRemoteEOF()
		return true
	}

	if ackPacketType == Enums.PACKET_STREAM_RST_ACK && isWaitingRst {
		a.markRstAcked()
		a.finalizeClose("RST acknowledged")
		return true
	}

	return false
}

func (a *ARQ) handleTrackedCloseOrResetAck(originPtype uint8) bool {
	switch originPtype {
	case Enums.PACKET_STREAM_CLOSE_READ:
		a.markCloseReadAcked()
		a.clearWaitingAck(Enums.PACKET_STREAM_CLOSE_READ)
		a.tryFinalizeRemoteEOF()
		return true
	case Enums.PACKET_STREAM_CLOSE_WRITE:
		a.markCloseWriteAcked()
		a.clearWaitingAck(Enums.PACKET_STREAM_CLOSE_WRITE)
		a.maybeInitiateClientCloseReadAfterWriterBreak()
		a.tryFinalizeRemoteEOF()
		return true
	case Enums.PACKET_STREAM_RST:
		a.markRstAcked()
		a.finalizeClose("RST acknowledged")
		return true
	default:
		return false
	}
}

func (a *ARQ) ReceiveControlAck(ackPacketType uint8, sequenceNum uint16, fragmentID uint8) bool {
	a.mu.Lock()
	now := time.Now()
	a.lastActivity = now
	originPtype, ok := Enums.ReverseControlAckFor(ackPacketType)
	if !ok {
		a.mu.Unlock()
		return false
	}

	key := uint32(originPtype)<<24 | uint32(sequenceNum)<<8 | uint32(fragmentID)
	info, tracked := a.controlSndBuf[key]
	_, isCloseStreamPacket := Enums.GetPacketCloseStream(originPtype)
	var sample time.Duration
	sampleEligible := false

	if !tracked && isCloseStreamPacket {
		for _, info := range a.controlSndBuf {
			if info.PacketType == originPtype {
				tracked = true
				break
			}
		}
	}

	waitingFor := a.waitingAckFor
	isWaitingCloseRead := ackPacketType == Enums.PACKET_STREAM_CLOSE_READ_ACK && waitingFor == Enums.PACKET_STREAM_CLOSE_READ
	isWaitingCloseWrite := ackPacketType == Enums.PACKET_STREAM_CLOSE_WRITE_ACK && waitingFor == Enums.PACKET_STREAM_CLOSE_WRITE
	isWaitingRst := ackPacketType == Enums.PACKET_STREAM_RST_ACK && waitingFor == Enums.PACKET_STREAM_RST

	if !tracked && !isWaitingCloseRead && !isWaitingCloseWrite && !isWaitingRst {
		a.mu.Unlock()
		return false
	}

	if tracked {
		if info != nil && info.SampleEligible && info.Dispatched && !info.LastSentAt.IsZero() {
			sample = now.Sub(info.LastSentAt)
			sampleEligible = true
		}
		if isCloseStreamPacket {
			for trackedKey, info := range a.controlSndBuf {
				if info.PacketType == originPtype {
					delete(a.controlSndBuf, trackedKey)
				}
			}
		} else {
			delete(a.controlSndBuf, key)
		}
	}
	a.mu.Unlock()

	if tracked && sampleEligible {
		a.noteSuccessfulControlSample(sample)
	}

	if tracked && a.handleTrackedCloseOrResetAck(originPtype) {
		return true
	}

	if tracked && a.handleTrackedTerminalAck(originPtype) {
		return true
	}

	if a.handleWaitingTerminalAck(ackPacketType, isWaitingCloseRead, isWaitingCloseWrite, isWaitingRst) {
		return true
	}

	return tracked
}

func (a *ARQ) HandleAckPacket(packetType uint8, sequenceNum uint16, fragmentID uint8) bool {
	if packetType == Enums.PACKET_STREAM_DATA_ACK {
		return a.ReceiveAck(packetType, sequenceNum)
	}

	if _, ok := Enums.ReverseControlAckFor(packetType); !ok {
		return false
	}

	return a.ReceiveControlAck(packetType, sequenceNum, fragmentID)
}

// ---------------------------------------------------------------------
// Retransmit Checks
// ---------------------------------------------------------------------

func (a *ARQ) checkRetransmits() {
	if a.isClosed() {
		return
	}

	now := time.Now()
	a.runGapRecoveryWatchdog(now)
	a.runFinalAckWatchdog(now)

	if a.handleTerminalRetransmitState(now) {
		return
	}

	a.mu.RLock()
	var jobs []rtxJob
	var ttlExpired bool
	var retryExceeded bool
	draining := a.deferredClose || a.state == StateDraining
	drainRTOCap := clampDuration(2*time.Second, a.rto, a.maxRTO)

	for sn, info := range a.sndBuf {
		if info.TTL > 0 {
			if now.Sub(info.CreatedAt) >= info.TTL {
				ttlExpired = true
				break
			}
		} else if now.Sub(info.CreatedAt) >= a.dataPacketTTL && info.Retries >= a.maxDataRetries {
			retryExceeded = true
			break
		}

		effectiveRTO := info.CurrentRTO
		if draining && effectiveRTO > drainRTOCap {
			effectiveRTO = drainRTOCap
		}

		if !info.Dispatched || now.Sub(info.LastSentAt) < effectiveRTO {
			continue
		}

		jobs = append(jobs, rtxJob{
			sn:              sn,
			data:            info.Data,
			compressionType: info.CompressionType,
		})
	}
	a.mu.RUnlock()

	if ttlExpired {
		a.handleTrackedPacketTTLExpiry(Enums.PACKET_STREAM_DATA, "Packet TTL expired")
		return
	}
	if retryExceeded {
		a.Close("Max retransmissions exceeded", CloseOptions{SendRST: true})
		return
	}

	priorityKinds := a.retransmitPriorityKinds(jobs)
	for i, j := range jobs {
		// Step 5 — gate every RTO-driven retransmission through the
		// per-second budget. The budget counter is shared with the
		// fast-retransmit path so a single ARQ instance cannot exceed
		// the configured retx-per-second rate via either mechanism.
		a.mu.Lock()
		allowed := a.consumeRetxBudgetLocked(now)
		a.mu.Unlock()
		if !allowed {
			metrics.ArqRetxBudgetDropped.Add(1)
			continue
		}

		priority := Enums.DefaultPacketPriority(Enums.PACKET_STREAM_DATA)
		packetType := uint8(Enums.PACKET_STREAM_DATA)

		if priorityKinds[i] {
			priority = Enums.DefaultPacketPriority(Enums.PACKET_STREAM_RESEND)
			packetType = uint8(Enums.PACKET_STREAM_RESEND)
		}

		ok := a.enqueuer.PushTXPacket(
			priority,
			packetType,
			j.sn, 0, 0, j.compressionType, 0, j.data,
		)
		if !ok {
			// Refund the budget — the enqueue refusal is a queue-full
			// signal, not a path-loss signal.
			a.mu.Lock()
			if a.retxBudgetPerSec > 0 && a.retxWindowCount > 0 {
				a.retxWindowCount--
			}
			a.mu.Unlock()
			a.noteDrainQueueFailure(now)
			continue
		}
		metrics.ArqRetx.Add(1)

		a.mu.Lock()
		info, exists := a.sndBuf[j.sn]
		if exists {
			dataFloor := a.currentDataBaseRTO()
			info.LastSentAt = now
			info.Dispatched = false
			info.Retries++
			info.SampleEligible = false
			// RTO timer fired — the segment is back on the wire as a
			// fresh attempt, so clear the fast-retransmit guard and
			// the OOS-ACK counter that previous ACKs accumulated.
			info.OosAckCount = 0
			info.FastRetransmitted = false
			grownRTO := time.Duration(float64(info.CurrentRTO) * dataRetransmitRTOGrowthFactor)
			maxRTO := a.maxRTO
			if draining && maxRTO > drainRTOCap {
				maxRTO = drainRTOCap
			}
			info.CurrentRTO = clampDuration(grownRTO, dataFloor, maxRTO)
		}
		a.mu.Unlock()
	}

	if a.enableControlReliability {
		a.checkControlRetransmits(now)
	}
}

func (a *ARQ) retransmitPriorityKinds(jobs []rtxJob) []bool {
	if len(jobs) == 0 {
		return nil
	}

	kinds := make([]bool, len(jobs))
	if len(jobs) == 1 {
		kinds[0] = true
		return kinds
	}

	frontBudget := a.windowSize / 10
	if frontBudget < 1 {
		frontBudget = 1
	}
	if frontBudget > 64 {
		frontBudget = 64
	}
	if frontBudget > len(jobs) {
		frontBudget = len(jobs)
	}

	sndNxt := a.sndNxt
	bestIdx := make([]int, 0, frontBudget)
	bestDist := make([]uint16, 0, frontBudget)

	insertBest := func(idx int, dist uint16) {
		pos := len(bestIdx)
		for pos > 0 {
			prev := pos - 1
			prevDist := bestDist[prev]
			prevIdx := bestIdx[prev]
			if prevDist > dist || (prevDist == dist && jobs[prevIdx].sn <= jobs[idx].sn) {
				break
			}
			pos--
		}

		bestIdx = append(bestIdx, 0)
		bestDist = append(bestDist, 0)
		copy(bestIdx[pos+1:], bestIdx[pos:])
		copy(bestDist[pos+1:], bestDist[pos:])
		bestIdx[pos] = idx
		bestDist[pos] = dist

		if len(bestIdx) > frontBudget {
			bestIdx = bestIdx[:frontBudget]
			bestDist = bestDist[:frontBudget]
		}
	}

	for i := range jobs {
		dist := uint16(sndNxt - jobs[i].sn)
		if len(bestIdx) < frontBudget {
			insertBest(i, dist)
			continue
		}

		last := len(bestIdx) - 1
		if dist > bestDist[last] || (dist == bestDist[last] && jobs[i].sn < jobs[bestIdx[last]].sn) {
			insertBest(i, dist)
		}
	}

	for _, idx := range bestIdx {
		kinds[idx] = true
	}

	return kinds
}

func (a *ARQ) handleTerminalRetransmitState(now time.Time) bool {
	a.mu.Lock()
	if a.deferredClose {
		pending := len(a.sndBuf)
		shouldClose := pending == 0
		shouldAbort := !a.deferredDeadline.IsZero() && now.After(a.deferredDeadline)
		stalledFor := time.Duration(0)
		if !a.drainProgressAt.IsZero() {
			stalledFor = now.Sub(a.drainProgressAt)
		}
		queueBlocked := a.drainQueueFails >= 3 &&
			!a.drainQueueFailAt.IsZero() &&
			now.Sub(a.drainQueueFailAt) <= 30*time.Second
		shouldAbortEarly := pending > 0 && queueBlocked && stalledFor >= 20*time.Second
		if shouldAbortEarly && !a.drainStallLogged {
			a.drainStallLogged = true
			a.logger.Debugf(
				"ARQ Drain Stall | Session: %d | Stream: %d | Pending: %d | QueueFails: %d | StalledFor: %s | LastQueueFailAgo: %s | Reason: %s",
				a.sessionID,
				a.streamID,
				pending,
				a.drainQueueFails,
				stalledFor.Round(time.Millisecond),
				now.Sub(a.drainQueueFailAt).Round(time.Millisecond),
				a.deferredReason,
			)
		}
		a.mu.Unlock()

		if shouldClose || shouldAbort {
			a.settleTerminalDrain()
		} else if shouldAbortEarly {
			a.Close("Deferred drain stalled after resend queue pressure", CloseOptions{SendRST: true})
		}

		return a.isClosed()
	}

	if a.waitingAck && !a.ackWaitDeadline.IsZero() && now.After(a.ackWaitDeadline) {
		waitingFor := a.waitingAckFor
		a.mu.Unlock()

		if waitingFor == Enums.PACKET_STREAM_RST {
			a.finalizeClose("Terminal ACK wait timeout")
			return true
		}

		if waitingFor == Enums.PACKET_STREAM_CLOSE_READ || waitingFor == Enums.PACKET_STREAM_CLOSE_WRITE {
			a.Close("Close handshake ACK wait timeout", CloseOptions{SendRST: true})
			return false
		}

		return false
	}

	// Check for peer-signaled reset termination.
	// Only trigger on a.rstReceived (peer sent RST to us). Do NOT use
	// a.state==StateReset here because StateReset is also set by MarkRstSent()
	// (when WE send RST). That would cause every locally-initiated RST to be
	// mis-identified as a peer reset, killing the stream immediately before the
	// RST_ACK arrives.
	if a.rstReceived && !a.closed {
		a.mu.Unlock()
		return a.tryFinalizePeerResetDrain()
	}

	receiveDrainedForCloseWrite := len(a.rcvBuf) == 0 && a.pendingInbound == 0 && !a.localWritePending
	peerFinishedSending := a.closeReadReceived || a.localWriterBroken
	shouldInitiateCloseWriteAfterEOF := a.IsClient &&
		((!a.clientEOFAt.IsZero() && now.Sub(a.clientEOFAt) >= 2*time.Second) ||
			(!a.closeReadAckedAt.IsZero() && now.Sub(a.closeReadAckedAt) >= 2*time.Second)) &&
		!a.closed &&
		!a.rstSent &&
		!a.rstReceived &&
		a.closeReadSent &&
		a.closeReadAcked &&
		!a.closeWriteSent &&
		!a.closeWriteAcked &&
		!a.closeWriteReceived &&
		!(a.waitingAck && a.waitingAckFor == Enums.PACKET_STREAM_CLOSE_WRITE) &&
		receiveDrainedForCloseWrite &&
		peerFinishedSending
	if shouldInitiateCloseWriteAfterEOF {
		a.mu.Unlock()
		a.Close("Client close-read grace elapsed", CloseOptions{SendCloseWrite: true})
		return false
	}

	if now.Sub(a.lastActivity) > a.inactivityTimeout {
		hasPending := len(a.sndBuf) > 0 || (a.enableControlReliability && len(a.controlSndBuf) > 0)
		if hasPending {
			a.lastActivity = now
			a.mu.Unlock()
			return false
		}

		a.mu.Unlock()
		a.Close("Stream Inactivity Timeout (Dead)", CloseOptions{SendRST: true})
		return true
	}

	a.mu.Unlock()
	return false
}

func (a *ARQ) checkControlRetransmits(now time.Time) {
	a.mu.Lock()

	for key, info := range a.controlSndBuf {
		if info.TTL > 0 {
			if now.Sub(info.CreatedAt) >= info.TTL {
				delete(a.controlSndBuf, key)
				a.mu.Unlock()
				a.handleTrackedPacketTTLExpiry(info.PacketType, "Packet TTL expired")
				return
			}
		} else {
			maxRetries := a.controlMaxRetries
			packetTTL := a.controlPacketTTL

			if setupControlPacketTypes[info.PacketType] {
				if maxRetries < 120 {
					maxRetries = 120
				}
				if packetTTL < 300*time.Second {
					packetTTL = 300 * time.Second
				}
			}

			expiredByTTL := now.Sub(info.CreatedAt) >= packetTTL
			exceededRetries := info.Retries >= maxRetries
			if expiredByTTL || exceededRetries {
				delete(a.controlSndBuf, key)
				reason := "Control packet expired"
				if exceededRetries {
					reason = "Control packet max retransmissions exceeded"
				}
				a.mu.Unlock()
				a.handleTrackedPacketTTLExpiry(info.PacketType, reason)
				return
			}
		}

		if info.TTL == 0 {
			// no-op: legacy retry ownership remains active for non-TTL packets
		}

		if !info.Dispatched || now.Sub(info.LastSentAt) < info.CurrentRTO {
			continue
		}

		ok := a.enqueuer.PushTXPacket(info.Priority, info.PacketType, info.SequenceNum, info.FragmentID, info.TotalFragments, 0, info.TTL, info.Payload)
		if !ok {
			continue
		}
		metrics.ArqRetx.Add(1)

		info.LastSentAt = now
		info.Dispatched = false
		info.Retries++
		info.SampleEligible = false
		growth := controlRetransmitRTOGrowthFactor
		floorRto := a.currentControlBaseRTO()

		if setupControlPacketTypes[info.PacketType] {
			growth = setupControlRTOGrowthFactor
			altFloor := 350 * time.Millisecond
			if altFloor < floorRto {
				floorRto = altFloor
			}
		}

		grownRTO := time.Duration(float64(info.CurrentRTO) * growth)
		info.CurrentRTO = clampDuration(grownRTO, floorRto, a.controlMaxRto)
	}
	a.mu.Unlock()
}

// ---------------------------------------------------------------------
// Final Close Path
// ---------------------------------------------------------------------

func (a *ARQ) finalizeClose(reason string) {
	a.mu.Lock()
	if a.closed || a.isVirtual {
		a.mu.Unlock()
		return
	}

	sndBufLen := len(a.sndBuf)
	rcvBufLen := len(a.rcvBuf)
	controlSndBufLen := len(a.controlSndBuf)
	contiguousReady := a.contiguousReadyLocked()
	pendingInbound := a.pendingInbound
	rxQueueLen := len(a.rxChan)
	rxQueueCap := cap(a.rxChan)
	prevState := a.state
	closeReadSent := a.closeReadSent
	closeReadReceived := a.closeReadReceived
	closeReadAcked := a.closeReadAcked
	closeWriteSent := a.closeWriteSent
	closeWriteReceived := a.closeWriteReceived
	closeWriteAcked := a.closeWriteAcked
	rstSent := a.rstSent
	rstReceived := a.rstReceived
	rstAcked := a.rstAcked
	localWritePending := a.localWritePending
	localWriteClosed := a.localWriteClosed
	localWriterBroken := a.localWriterBroken
	waitingAck := a.waitingAck
	waitingAckFor := a.waitingAckFor
	deferredClose := a.deferredClose
	deferredPacket := a.deferredPacket
	rcvNxt := a.rcvNxt
	priorReason := a.closeReason
	ioReady := a.ioReady
	stopLocalRead := a.stopLocalRead
	streamWorkersStarted := a.streamWorkersStarted
	lastActivityAgo := formatAgoFrom(time.Now(), a.lastActivity)
	clientEOFAgo := formatAgoFrom(time.Now(), a.clientEOFAt)
	closeReadAckedAgo := formatAgoFrom(time.Now(), a.closeReadAckedAt)
	ackDeadlineIn := formatDeadlineDelta(time.Now(), a.ackWaitDeadline)
	deferredDeadlineIn := formatDeadlineDelta(time.Now(), a.deferredDeadline)
	a.closeReason = reason
	a.closed = true
	a.deferredClose = false
	a.deferredReason = ""
	a.deferredDeadline = time.Time{}
	a.deferredPacket = 0
	a.waitingAck = false
	a.waitingAckFor = 0
	a.ackWaitDeadline = time.Time{}

	if a.state == StateReset || a.rstReceived || a.rstSent {
		a.setState(StateReset)
	} else if a.closeReadSent || a.closeReadReceived || a.closeWriteSent || a.closeWriteReceived {
		a.setState(StateTimeWait)
	} else {
		a.setState(StateClosing)
	}

	a.cancel()

	if a.localConn != nil {
		_ = a.localConn.Close()
	}

	a.clearAllQueues(true)
	a.mu.Unlock()

	a.logger.Debugf(
		"ARQ Stream Closed | Session: %d | Stream: %d | Reason: %s | PriorReason: %s | PrevState: %d | SndBuf: %d | RcvBuf: %d | ControlSndBuf: %d | ContigRcv: %d | PendingInbound: %d | RxQueue: %d/%d | RcvNxt: %d | LocalWrite: pending=%t closed=%t broken=%t | CloseRead: %t/%t/%t | CloseWrite: %t/%t/%t | WaitingAck: %t/%s/%s | Deferred: %t/%s/%s | IO: ready=%t stopRead=%t workers=%t | RST: %t/%t/%t | Since: lastActivity=%s clientEOF=%s closeReadAcked=%s",
		a.sessionID,
		a.streamID,
		reason,
		priorReason,
		prevState,
		sndBufLen,
		rcvBufLen,
		controlSndBufLen,
		contiguousReady,
		pendingInbound,
		rxQueueLen,
		rxQueueCap,
		rcvNxt,
		localWritePending,
		localWriteClosed,
		localWriterBroken,
		closeReadSent,
		closeReadReceived,
		closeReadAcked,
		closeWriteSent,
		closeWriteReceived,
		closeWriteAcked,
		waitingAck,
		Enums.PacketTypeName(waitingAckFor),
		ackDeadlineIn,
		deferredClose,
		Enums.PacketTypeName(deferredPacket),
		deferredDeadlineIn,
		ioReady,
		stopLocalRead,
		streamWorkersStarted,
		rstSent,
		rstReceived,
		rstAcked,
		lastActivityAgo,
		clientEOFAgo,
		closeReadAckedAgo,
	)

	if owner, ok := a.enqueuer.(terminalOwner); ok {
		owner.OnARQClosed(reason)
	}
}

// Close is the single close entrypoint for this ARQ stream.
// Modes are expressed through options:
// - Force: finalize immediately
// - SendCloseRead: local read side ended; peer should finish draining inbound and close writer
// - SendCloseWrite: local write side ended; peer should stop sending to us
// - SendRST: reset close, optionally after drain
func (a *ARQ) Close(reason string, opts CloseOptions) {
	if a.isVirtual && !opts.Force {
		return
	}

	if opts.Force || (!opts.SendRST && !opts.SendCloseRead && !opts.SendCloseWrite) {
		a.mu.Lock()
		a.isVirtual = false
		a.mu.Unlock()
		a.finalizeClose(reason)
		return
	}

	if opts.SendCloseRead {
		if opts.AfterDrain {
			a.deferTerminalPacket(reason, Enums.PACKET_STREAM_CLOSE_READ)
			return
		}

		a.emitTerminalPacketWithTTL(Enums.PACKET_STREAM_CLOSE_READ, reason, opts.TTL)
		return
	}

	if opts.SendCloseWrite {
		a.emitTerminalPacketWithTTL(Enums.PACKET_STREAM_CLOSE_WRITE, reason, opts.TTL)
		return
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}

	alreadyResetting := a.rstSent || a.rstReceived ||
		(a.waitingAck && a.waitingAckFor == Enums.PACKET_STREAM_RST) ||
		(a.deferredClose && a.deferredPacket == Enums.PACKET_STREAM_RST)

	if alreadyResetting {
		a.mu.Unlock()
		return
	}

	hasPendingData := len(a.sndBuf) > 0
	a.closeReason = reason
	a.setState(StateReset)
	a.deferredClose = false
	a.deferredReason = ""
	a.deferredDeadline = time.Time{}
	a.deferredPacket = 0
	a.mu.Unlock()

	if opts.AfterDrain && hasPendingData {
		a.deferTerminalPacket(reason, Enums.PACKET_STREAM_RST)
		return
	}

	a.emitTerminalPacketWithTTL(Enums.PACKET_STREAM_RST, reason, opts.TTL)
}
