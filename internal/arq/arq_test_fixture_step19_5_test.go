// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package arq

import (
	"io"
	"testing"
	"time"
)

// newTestARQ is the canonical fixture for ARQ unit tests. It mirrors
// NewARQ exactly but additionally registers a t.Cleanup hook that:
//
//  1. Calls Close(reason, CloseOptions{Force: true}) on the instance —
//     idempotent if the test already invoked Close itself.
//  2. Blocks until WaitForShutdown returns or 2 seconds elapse, so the
//     retransmitLoop / writeLoop / ioLoop / dispatcher goroutines have
//     definitively exited before the test ends.
//
// Why this exists (Step 19.5 — fix ARQ-LIFECYCLE-1):
//
// Before Step 19, ARQ had no way for external callers to wait for its
// internal goroutines to exit. Many pre-existing unit tests called
// NewARQ + sometimes Close, but never joined the workers. Under
// `go test -count > 1` those goroutines leaked across iterations and
// the Step 19 leak detector had to disengage itself to avoid false
// positives. By routing every test ARQ instance through newTestARQ,
// the lifecycle becomes deterministic and the leak detector can run
// unconditionally.
//
// Tests that intentionally do NOT call Close (because they assert on
// the "still-running" state) are still safe: this cleanup always
// force-closes, which is the desired teardown.
func newTestARQ(tb testing.TB, streamID uint16, sessionID uint8, enqueuer PacketEnqueuer, localConn io.ReadWriteCloser, mtu int, logger Logger, cfg Config) *ARQ {
	tb.Helper()
	a := NewARQ(streamID, sessionID, enqueuer, localConn, mtu, logger, cfg)
	registerARQCleanup(tb, a)
	return a
}

// registerARQCleanup attaches the standard teardown to an existing ARQ.
// Exposed separately so tests that need NewARQ directly (e.g. to verify
// a constructor invariant before Start is called) can still opt into
// the cleanup contract. Accepts testing.TB so benchmarks (*testing.B)
// and regular tests (*testing.T) share the same lifecycle.
func registerARQCleanup(tb testing.TB, a *ARQ) {
	if tb == nil || a == nil {
		return
	}
	tb.Cleanup(func() {
		// Force-close is idempotent: ARQ.Close internally guards on
		// the `closed` flag, so calling it on an already-closed
		// instance is cheap (one mutex acquisition, one boolean read,
		// early return). Note: closeAlreadyClosed is gated by both
		// `closed` *and* the close kind, so calling Force on an
		// instance the test already closed gracefully is also safe.
		a.Close("test cleanup (Step 19.5)", CloseOptions{Force: true})
		// 2s is a generous ceiling. On loopback / mocked enqueuers
		// the workers exit in <5ms once the context is cancelled.
		// Failing to exit within 2s indicates a real bug — we log it
		// instead of failing because a t.Cleanup that fails the test
		// can mask the actual test failure underneath.
		if !a.WaitForShutdown(2 * time.Second) {
			tb.Logf("WARNING [Step 19.5 fixture cleanup]: ARQ streamID=%d sessionID=%d did not shut down within 2s", a.streamID, a.sessionID)
		}
	})
}
