// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package udpserver

import (
	"testing"

	"masterdnsvpn-go/internal/arq"
	Enums "masterdnsvpn-go/internal/enums"
	"masterdnsvpn-go/internal/goroutineleak"
)

// TestSessionCleanup_NoGoroutineLeak verifies that creating a session with
// active streams (each owning a full ARQ instance with ~4 goroutines) and
// then calling cleanupClosedSession reclaims every goroutine. Reuses the
// same fixtures as TestCleanupClosedSessionClosesStreamsAndClearsQueues.
//
// This guards against future regressions where a refactor of
// closeAllStreams forgets the WaitForShutdown that step 18.5 added.
func TestSessionCleanup_NoGoroutineLeak(t *testing.T) {
	if leakDetectorSkipUnderCount() {
		t.Skip("leak detector intentionally restricted to -count=1 — see PLAN.md Step 19 bug ARQ-LIFECYCLE-1")
	}
	defer goroutineleak.Check(t)

	s := newTestServerForCleanup()
	record := newTestSessionRecord(11)
	record.streamCleanup = s.cleanupStreamArtifacts

	cfg := arq.Config{WindowSize: 32, RTO: 1.0, MaxRTO: 5.0}
	for streamID := uint16(1); streamID <= 4; streamID++ {
		stream := record.getOrCreateStream(streamID, cfg, nil, nil)
		stream.Connected = true
		_ = stream.enqueueInboundData(Enums.PACKET_STREAM_DATA, 1, 0, []byte("inbound"))
		_ = stream.PushTXPacket(
			Enums.DefaultPacketPriority(Enums.PACKET_STREAM_RST),
			Enums.PACKET_STREAM_RST, 12, 0, 0, 0, 0, nil,
		)
	}

	s.cleanupClosedSession(record.ID, record)

	// Sanity: every stream should be CLOSED and queues drained.
	record.StreamsMu.RLock()
	streamCount := len(record.Streams)
	activeCount := len(record.ActiveStreams)
	record.StreamsMu.RUnlock()
	if streamCount != 0 {
		t.Fatalf("expected no streams after cleanup, got %d", streamCount)
	}
	if activeCount != 0 {
		t.Fatalf("expected no active stream ids after cleanup, got %d", activeCount)
	}
}
