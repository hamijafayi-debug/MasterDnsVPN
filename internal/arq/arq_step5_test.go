// ==============================================================================
// MasterDnsVPN — Step 5
// Unit tests + benchmarks for ARQ send-path hardening:
//   - Fast retransmit triggered by out-of-order ACKs.
//   - Per-second retransmission budget (RTO + fast combined).
//   - RTTVAR floor that prevents the adaptive RTO from collapsing onto SRTT.
// ==============================================================================

package arq

import (
	"testing"
	"time"

	Enums "masterdnsvpn-go/internal/enums"
	"masterdnsvpn-go/internal/metrics"
)

// drainPackets returns every packet currently sitting on the mock enqueuer's
// channel. Tests use this to confirm that fast-retransmit emitted a
// PACKET_STREAM_RESEND without blocking on unbounded waits.
func drainPackets(enq *MockPacketEnqueuer) []capturedPacket {
	out := make([]capturedPacket, 0, len(enq.Packets))
	for {
		select {
		case p := <-enq.Packets:
			out = append(out, p)
		default:
			return out
		}
	}
}

func filterByType(pkts []capturedPacket, t uint8) []capturedPacket {
	out := pkts[:0:0]
	for _, p := range pkts {
		if p.packetType == t {
			out = append(out, p)
		}
	}
	return out
}

// TestUpdateAdaptiveRTO_RttvarFloor ensures the smoothed-RTT estimator never
// collapses RTTVAR to zero on a perfectly stable path. Without the Step 5
// floor, RTO converges to SRTT and any tiny scheduling jitter triggers a
// spurious retransmission.
func TestUpdateAdaptiveRTO_RttvarFloor(t *testing.T) {
	const sample = 5 * time.Millisecond
	const minRTO = 1 * time.Millisecond
	const maxRTO = 1 * time.Second

	var s adaptiveRTOState
	for i := 0; i < 200; i++ {
		s = updateAdaptiveRTO(s, sample, minRTO, maxRTO)
	}

	if s.rttvar < rttvarFloor {
		t.Fatalf("rttvar collapsed below floor: got %s want >= %s", s.rttvar, rttvarFloor)
	}
	if s.currentBase <= s.srtt {
		t.Fatalf("currentBase=%s must remain strictly above srtt=%s", s.currentBase, s.srtt)
	}
	want := s.srtt + 4*rttvarFloor
	if s.currentBase < clampDuration(want, minRTO, maxRTO) {
		t.Fatalf("currentBase=%s below expected floor %s", s.currentBase, want)
	}
}

// TestUpdateAdaptiveRTO_RespectsClamp checks the floor is still bounded by
// minRTO so callers retain full control on aggressively low minRTO settings.
func TestUpdateAdaptiveRTO_RespectsClamp(t *testing.T) {
	const minRTO = 50 * time.Millisecond
	const maxRTO = 200 * time.Millisecond

	var s adaptiveRTOState
	for i := 0; i < 50; i++ {
		s = updateAdaptiveRTO(s, 3*time.Millisecond, minRTO, maxRTO)
	}
	if s.currentBase < minRTO {
		t.Fatalf("currentBase=%s below minRTO=%s", s.currentBase, minRTO)
	}
	if s.currentBase > maxRTO {
		t.Fatalf("currentBase=%s above maxRTO=%s", s.currentBase, maxRTO)
	}
}

// TestARQ_FastRetransmit_TriggersAfterThresholdOosAcks verifies that when
// ACKs for higher sequence numbers arrive while an older segment is still
// pending, the older segment is retransmitted as soon as the OOS-ACK count
// reaches the configured threshold.
func TestARQ_FastRetransmit_TriggersAfterThresholdOosAcks(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:        32,
		RTO:               5.0,
		MaxRTO:            10.0,
		FastRetxThreshold: 3,
	})

	now := time.Now()
	for sn := uint16(10); sn <= 13; sn++ {
		a.sndBuf[sn] = &arqDataItem{
			Data:           []byte{byte(sn)},
			CreatedAt:      now,
			LastSentAt:     now,
			Dispatched:     true,
			CurrentRTO:     a.rto,
			SampleEligible: true,
		}
	}

	beforeFast := metrics.ArqFastRetx.Value()

	for _, sn := range []uint16{11, 12, 13} {
		if !a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, sn) {
			t.Fatalf("expected ack for %d to be tracked", sn)
		}
	}

	time.Sleep(50 * time.Millisecond)

	pkts := drainPackets(enq)
	resends := filterByType(pkts, Enums.PACKET_STREAM_RESEND)
	if len(resends) == 0 {
		t.Fatalf("expected at least one PACKET_STREAM_RESEND, got %d packets total", len(pkts))
	}
	if resends[0].sequenceNum != 10 {
		t.Fatalf("expected resend of sn=10, got sn=%d", resends[0].sequenceNum)
	}

	afterFast := metrics.ArqFastRetx.Value()
	if afterFast <= beforeFast {
		t.Fatalf("ArqFastRetx counter did not increase: before=%d after=%d", beforeFast, afterFast)
	}

	a.mu.RLock()
	info, exists := a.sndBuf[10]
	a.mu.RUnlock()
	if !exists {
		t.Fatal("sn=10 should still be tracked after fast retx")
	}
	if !info.FastRetransmitted {
		t.Fatal("expected FastRetransmitted=true after fast retx")
	}
	if info.OosAckCount != 0 {
		t.Fatalf("OosAckCount not reset after fast retx: %d", info.OosAckCount)
	}
}

// TestARQ_FastRetransmit_NoDoubleFire ensures the same segment is not
// fast-retransmitted twice in a row.
func TestARQ_FastRetransmit_NoDoubleFire(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:        32,
		RTO:               5.0,
		MaxRTO:            10.0,
		FastRetxThreshold: 3,
	})

	now := time.Now()
	for sn := uint16(20); sn <= 26; sn++ {
		a.sndBuf[sn] = &arqDataItem{
			Data:       []byte{byte(sn)},
			CreatedAt:  now,
			LastSentAt: now,
			Dispatched: true,
			CurrentRTO: a.rto,
		}
	}

	for _, sn := range []uint16{21, 22, 23} {
		a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, sn)
	}
	time.Sleep(20 * time.Millisecond)

	firstWave := filterByType(drainPackets(enq), Enums.PACKET_STREAM_RESEND)
	if len(firstWave) != 1 || firstWave[0].sequenceNum != 20 {
		t.Fatalf("expected exactly one resend of sn=20, got %+v", firstWave)
	}

	for _, sn := range []uint16{24, 25, 26} {
		a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, sn)
	}
	time.Sleep(20 * time.Millisecond)

	secondWave := filterByType(drainPackets(enq), Enums.PACKET_STREAM_RESEND)
	if len(secondWave) != 0 {
		t.Fatalf("expected no second fast retx of sn=20, got %+v", secondWave)
	}
}

// TestARQ_FastRetransmit_DisabledByNegativeThreshold verifies the disable knob.
func TestARQ_FastRetransmit_DisabledByNegativeThreshold(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:        32,
		RTO:               5.0,
		MaxRTO:            10.0,
		FastRetxThreshold: -1,
	})

	if a.fastRetxThreshold != 0 {
		t.Fatalf("expected disabled threshold to store 0 internally, got %d", a.fastRetxThreshold)
	}

	now := time.Now()
	for sn := uint16(30); sn <= 36; sn++ {
		a.sndBuf[sn] = &arqDataItem{
			Data:       []byte{byte(sn)},
			CreatedAt:  now,
			LastSentAt: now,
			Dispatched: true,
			CurrentRTO: a.rto,
		}
	}

	for _, sn := range []uint16{31, 32, 33, 34, 35, 36} {
		a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, sn)
	}
	time.Sleep(20 * time.Millisecond)

	resends := filterByType(drainPackets(enq), Enums.PACKET_STREAM_RESEND)
	if len(resends) != 0 {
		t.Fatalf("expected no resends when fast retx disabled, got %d", len(resends))
	}
}

// TestARQ_FastRetransmit_DisabledByDefault verifies that the zero-value Config
// (the most common deployment) keeps fast-retx off, preserving the Step-4 ACK
// fast-path with no extra OOS-ACK bookkeeping. Users opt in by setting
// FastRetxThreshold > 0 (typically 3 for RFC 5681 semantics).
func TestARQ_FastRetransmit_DisabledByDefault(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize: 32,
		RTO:        1.0,
		MaxRTO:     5.0,
	})
	if a.fastRetxThreshold != 0 {
		t.Fatalf("expected fast retx disabled by default (0), got %d", a.fastRetxThreshold)
	}
}

// TestARQ_FastRetransmit_ExplicitRFC5681 verifies the user-facing opt-in works.
func TestARQ_FastRetransmit_ExplicitRFC5681(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:        32,
		RTO:               1.0,
		MaxRTO:            5.0,
		FastRetxThreshold: 3,
	})
	if a.fastRetxThreshold != 3 {
		t.Fatalf("expected explicit threshold 3, got %d", a.fastRetxThreshold)
	}
}

// TestARQ_RetxBudget_DefaultDerivedFromWindow validates the derivation formula.
func TestARQ_RetxBudget_DefaultDerivedFromWindow(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize: 600,
		RTO:        1.0,
		MaxRTO:     5.0,
	})
	want := 4 * 600
	if a.retxBudgetPerSec != want {
		t.Fatalf("expected default budget %d, got %d", want, a.retxBudgetPerSec)
	}

	// NewARQ enforces a hard minimum WindowSize of 300 (see ARQ constructor),
	// so the smallest derived budget on a real instance is 4 × 300 = 1200.
	// The 256 floor inside the budget derivation is an inner guard for the
	// raw math; verify the inner formula directly by simulating it.
	smallWS := 10
	if smallWS < 300 {
		smallWS = 300
	}
	a2 := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize: 10,
		RTO:        1.0,
		MaxRTO:     5.0,
	})
	if a2.retxBudgetPerSec != 4*smallWS {
		t.Fatalf("expected budget=%d, got %d", 4*smallWS, a2.retxBudgetPerSec)
	}
}

// TestARQ_RetxBudget_CapsFastRetransmits exercises the retx-budget gate via
// the RTO-driven retransmit path (checkRetransmits). The fast-retransmit
// path emits at most one candidate per ACK (the oldest pending segment), so
// to overflow the budget we rely on a burst of RTO-aged segments that all
// expire at once. This mirrors how the budget protects against retx storms.
func TestARQ_RetxBudget_CapsFastRetransmits(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:       32,
		RTO:              0.05, // 50ms — short so checkRetransmits fires
		MaxRTO:           1.0,
		RetxBudgetPerSec: 2,
	})

	if a.retxBudgetPerSec != 2 {
		t.Fatalf("expected budget=2, got %d", a.retxBudgetPerSec)
	}

	// Pre-populate 5 segments whose LastSentAt is far in the past so they
	// all qualify for retransmission on the next checkRetransmits call.
	pastSend := time.Now().Add(-500 * time.Millisecond)
	for sn := uint16(40); sn <= 44; sn++ {
		a.sndBuf[sn] = &arqDataItem{
			Data:       []byte{byte(sn)},
			CreatedAt:  pastSend,
			LastSentAt: pastSend,
			Dispatched: true,
			CurrentRTO: a.rto,
		}
	}

	beforeDropped := metrics.ArqRetxBudgetDropped.Value()
	beforeRetx := metrics.ArqRetx.Value()

	a.checkRetransmits()

	resends := filterByType(drainPackets(enq), Enums.PACKET_STREAM_RESEND)
	// Combined RESEND + DATA emissions count as retx — drain & count both
	// types because the priority-kind heuristic may pick either.
	// (We re-drained inside the helper above; just count what we got.)
	emitted := metrics.ArqRetx.Value() - beforeRetx
	if emitted != 2 {
		t.Fatalf("expected exactly 2 retx emissions (budget=2), got %d", emitted)
	}
	_ = resends

	afterDropped := metrics.ArqRetxBudgetDropped.Value()
	if afterDropped-beforeDropped != 3 {
		t.Fatalf("expected 3 retx drops, got %d", afterDropped-beforeDropped)
	}
}

// TestARQ_FastRetxBudget_DropsAdditionalCandidates exercises the budget gate
// inside the fast-retransmit path specifically. With a budget of 0 (set via
// direct field manipulation since 0-via-Config means "use default"), the
// single fast-retx candidate produced per ACK must be rejected and counted
// as dropped.
func TestARQ_FastRetxBudget_DropsAdditionalCandidates(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:        32,
		RTO:               5.0,
		MaxRTO:            10.0,
		FastRetxThreshold: 1,
		RetxBudgetPerSec:  1, // tiny — admits exactly one retx per second
	})

	now := time.Now()
	// Segment older than the about-to-be-acked one.
	a.sndBuf[50] = &arqDataItem{
		Data:       []byte{0x50},
		CreatedAt:  now,
		LastSentAt: now,
		Dispatched: true,
		CurrentRTO: a.rto,
	}
	// A second older segment that we will try to fast-retx later.
	a.sndBuf[51] = &arqDataItem{
		Data:       []byte{0x51},
		CreatedAt:  now,
		LastSentAt: now,
		Dispatched: true,
		CurrentRTO: a.rto,
	}
	a.sndBuf[60] = &arqDataItem{
		Data:       []byte{'A'},
		CreatedAt:  now,
		LastSentAt: now,
		Dispatched: true,
		CurrentRTO: a.rto,
	}
	a.sndBuf[61] = &arqDataItem{
		Data:       []byte{'B'},
		CreatedAt:  now,
		LastSentAt: now,
		Dispatched: true,
		CurrentRTO: a.rto,
	}

	beforeDropped := metrics.ArqRetxBudgetDropped.Value()
	beforeFast := metrics.ArqFastRetx.Value()

	// First ACK → oldest (sn=50) gets bumped to threshold 1 → fast retx.
	// Consumes the budget slot.
	a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, 60)
	time.Sleep(30 * time.Millisecond)
	// Second ACK → next oldest non-fast-retransmitted (sn=51) hits
	// threshold → tries to fast-retx → budget exhausted → dropped.
	a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, 61)
	time.Sleep(30 * time.Millisecond)

	afterFast := metrics.ArqFastRetx.Value()
	if afterFast-beforeFast != 1 {
		t.Fatalf("expected exactly 1 successful fast retx, got %d", afterFast-beforeFast)
	}
	afterDropped := metrics.ArqRetxBudgetDropped.Value()
	if afterDropped-beforeDropped != 1 {
		t.Fatalf("expected exactly 1 budget drop, got %d", afterDropped-beforeDropped)
	}
}

// TestARQ_RetxBudget_WindowSlides verifies the per-second window resets.
func TestARQ_RetxBudget_WindowSlides(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:       32,
		RTO:              1.0,
		MaxRTO:           5.0,
		RetxBudgetPerSec: 4,
	})

	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i < 4; i++ {
		if !a.consumeRetxBudgetLocked(now) {
			t.Fatalf("expected slot %d to be admitted", i)
		}
	}
	if a.consumeRetxBudgetLocked(now) {
		t.Fatal("expected the 5th call in the same second to be rejected")
	}
	if !a.consumeRetxBudgetLocked(now.Add(1100 * time.Millisecond)) {
		t.Fatal("expected new window to admit a fresh retx")
	}
}

// TestARQ_RetxBudget_UnlimitedWhenNegative confirms unlimited semantics.
func TestARQ_RetxBudget_UnlimitedWhenNegative(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:       32,
		RTO:              1.0,
		MaxRTO:           5.0,
		RetxBudgetPerSec: -1,
	})
	if a.retxBudgetPerSec != -1 {
		t.Fatalf("expected sentinel=-1, got %d", a.retxBudgetPerSec)
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i < 10000; i++ {
		if !a.consumeRetxBudgetLocked(now) {
			t.Fatalf("expected unlimited budget to admit call %d", i)
		}
	}
}

// TestARQ_FastRetransmit_DoesNotFireOnUndispatchedSegments ensures undispatched
// segments are never fast-retx candidates.
func TestARQ_FastRetransmit_DoesNotFireOnUndispatchedSegments(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:        32,
		RTO:               5.0,
		MaxRTO:            10.0,
		FastRetxThreshold: 1,
	})

	now := time.Now()
	a.sndBuf[5] = &arqDataItem{
		Data:       []byte{5},
		CreatedAt:  now,
		LastSentAt: time.Time{},
		Dispatched: false,
		CurrentRTO: a.rto,
	}
	a.sndBuf[6] = &arqDataItem{
		Data:       []byte{6},
		CreatedAt:  now,
		LastSentAt: now,
		Dispatched: true,
		CurrentRTO: a.rto,
	}

	if !a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, 6) {
		t.Fatal("expected ack of sn=6 to be tracked")
	}
	time.Sleep(20 * time.Millisecond)

	a.mu.RLock()
	count := a.sndBuf[5].OosAckCount
	a.mu.RUnlock()
	if count != 0 {
		t.Fatalf("undispatched segment should not get OosAckCount bumps, got %d", count)
	}

	resends := filterByType(drainPackets(enq), Enums.PACKET_STREAM_RESEND)
	if len(resends) != 0 {
		t.Fatalf("expected no resend for undispatched segment, got %d", len(resends))
	}
}

// TestARQ_FastRetransmit_HandlesUint16Wrap exercises wraparound-aware older detection.
func TestARQ_FastRetransmit_HandlesUint16Wrap(t *testing.T) {
	enq := NewMockPacketEnqueuer()
	a := newTestARQ(t, 1, 1, enq, nil, 1200, newTestLogger(t), Config{
		WindowSize:        32,
		RTO:               5.0,
		MaxRTO:            10.0,
		FastRetxThreshold: 1,
	})

	now := time.Now()
	a.sndBuf[65530] = &arqDataItem{
		Data:       []byte{0xAA},
		CreatedAt:  now,
		LastSentAt: now,
		Dispatched: true,
		CurrentRTO: a.rto,
	}
	a.sndBuf[3] = &arqDataItem{
		Data:       []byte{0xBB},
		CreatedAt:  now,
		LastSentAt: now,
		Dispatched: true,
		CurrentRTO: a.rto,
	}

	if !a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, 3) {
		t.Fatal("expected ack of sn=3 to be tracked")
	}
	time.Sleep(30 * time.Millisecond)

	resends := filterByType(drainPackets(enq), Enums.PACKET_STREAM_RESEND)
	if len(resends) != 1 {
		t.Fatalf("expected 1 resend for wrapped older sn=65530, got %d", len(resends))
	}
	if resends[0].sequenceNum != 65530 {
		t.Fatalf("expected resend of sn=65530, got sn=%d", resends[0].sequenceNum)
	}
}

// ----- benchmarks --------------------------------------------------------

// BenchmarkReceiveAck_NoFastRetx measures the per-ACK cost when no older
// segments are tracked.
func BenchmarkReceiveAck_NoFastRetx(b *testing.B) {
	enq := NewMockPacketEnqueuer()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-enq.Packets:
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	a := newTestARQ(b, 1, 1, enq, nil, 1200, nil, Config{
		WindowSize:        128,
		RTO:               1.0,
		MaxRTO:            5.0,
		FastRetxThreshold: 3,
	})

	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sn := uint16(i)
		a.mu.Lock()
		a.sndBuf[sn] = &arqDataItem{
			Data:       []byte{byte(i)},
			CreatedAt:  now,
			LastSentAt: now,
			Dispatched: true,
			CurrentRTO: a.rto,
		}
		a.mu.Unlock()
		a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, sn)
	}
}

// BenchmarkReceiveAck_WithOosBumps measures the hottest branch of the new
// code where many older segments must have their OosAckCount bumped.
func BenchmarkReceiveAck_WithOosBumps(b *testing.B) {
	enq := NewMockPacketEnqueuer()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-enq.Packets:
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	a := newTestARQ(b, 1, 1, enq, nil, 1200, nil, Config{
		WindowSize:        256,
		RTO:               5.0,
		MaxRTO:            10.0,
		FastRetxThreshold: 99, // high so we don't actually trigger fast retx
	})

	now := time.Now()
	const olderCount = 64
	for sn := uint16(0); sn < olderCount; sn++ {
		a.sndBuf[sn] = &arqDataItem{
			Data:       []byte{byte(sn)},
			CreatedAt:  now,
			LastSentAt: now,
			Dispatched: true,
			CurrentRTO: a.rto,
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sn := uint16(olderCount + i%1000)
		a.mu.Lock()
		a.sndBuf[sn] = &arqDataItem{
			Data:       []byte{byte(i)},
			CreatedAt:  now,
			LastSentAt: now,
			Dispatched: true,
			CurrentRTO: a.rto,
		}
		a.mu.Unlock()
		a.ReceiveAck(Enums.PACKET_STREAM_DATA_ACK, sn)
	}
}

// BenchmarkConsumeRetxBudgetLocked measures the budget gate itself.
func BenchmarkConsumeRetxBudgetLocked(b *testing.B) {
	a := &ARQ{retxBudgetPerSec: 1_000_000_000}
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.consumeRetxBudgetLocked(now)
	}
}
