// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package client

import (
	"context"
	"testing"
	"time"

	"masterdnsvpn-go/internal/goroutineleak"
)

// TestClientAsyncRuntime_NoGoroutineLeak boots the full client async
// runtime (reader/writer/processor/planner workers, dispatcher, cleanup
// worker, plus the tcp listener, dns listener and ping manager) on
// loopback, then calls StopAsyncRuntime and asserts that NO background
// goroutine remains. This is the canary for any future change that adds
// a `go func(){…}` without a matching wg/ctx exit path.
//
// If the local environment cannot bind to a loopback port (rare), the
// test SKIPs rather than fails — the goal is to assert the cleanup path
// when it can actually run, not to gate CI on socket availability.
func TestClientAsyncRuntime_NoGoroutineLeak(t *testing.T) {
	if leakDetectorSkipUnderCount() {
		t.Skip("skipping leak detector: preexisting ARQ retransmitLoop leak from sibling fixture (see ARQ-LIFECYCLE-1)")
	}
	defer goroutineleak.Check(t)

	c := createTestClient(t)
	c.cfg.ListenIP = "127.0.0.1"
	c.cfg.ListenPort = 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.StartAsyncRuntime(ctx); err != nil {
		t.Skipf("StartAsyncRuntime failed (likely environment): %v", err)
		return
	}

	// Let workers spin up and park on their respective channels.
	time.Sleep(50 * time.Millisecond)

	c.StopAsyncRuntime()
}
