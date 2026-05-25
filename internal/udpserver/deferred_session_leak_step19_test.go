// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package udpserver

import (
	"context"
	"testing"
	"time"

	"masterdnsvpn-go/internal/goroutineleak"
)

// TestDeferredSessionProcessor_NoGoroutineLeak verifies that the worker
// goroutines started by Start() exit after ctx.Done and that
// WaitForShutdown returns true within budget. Before Step 19 the
// processor had no wg, so this assertion was impossible.
func TestDeferredSessionProcessor_NoGoroutineLeak(t *testing.T) {
	// Skip under high -count runs: sibling ARQ tests in this package leak
	// a retransmitLoop goroutine per iteration that survives well past
	// the test's own scope, polluting our snapshot diff with false
	// positives. The leak is captured as a preexisting bug in PLAN.md
	// (Step 19/A). At -count=1 the detector is reliable.
	if leakDetectorSkipUnderCount() {
		t.Skip("leak detector intentionally restricted to -count=1 — see PLAN.md Step 19 bug ARQ-LIFECYCLE-1")
	}
	defer goroutineleak.Check(t)

	p := newDeferredSessionProcessor(4, 16, nil)
	if p == nil {
		t.Fatal("processor must not be nil for workerCount > 0")
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	// Let workers park on their job channels.
	time.Sleep(20 * time.Millisecond)

	cancel()
	if ok := p.WaitForShutdown(2 * time.Second); !ok {
		t.Fatal("deferred session workers failed to shut down within 2s")
	}
}

// TestDeferredSessionProcessor_WaitWithoutCancel_HitsTimeout confirms
// that callers who forget to cancel ctx see a timeout (not a deadlock).
func TestDeferredSessionProcessor_WaitWithoutCancel_HitsTimeout(t *testing.T) {
	p := newDeferredSessionProcessor(2, 8, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	if ok := p.WaitForShutdown(50 * time.Millisecond); ok {
		t.Fatal("expected WaitForShutdown to time out before cancel")
	}

	cancel()
	if ok := p.WaitForShutdown(2 * time.Second); !ok {
		t.Fatal("expected clean shutdown after cancel")
	}
}
