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
	"testing"
	"time"

	"masterdnsvpn-go/internal/goroutineleak"
)

// TestSOCKS5UpstreamPool_NoGoroutineLeak boots the reaper, exercises a
// Close cycle, and asserts that no reaper goroutine survives. Before
// Step 19 the reaper had no wg, so callers had to sleep-and-pray.
func TestSOCKS5UpstreamPool_NoGoroutineLeak(t *testing.T) {
	if leakDetectorSkipUnderCount() {
		t.Skip("leak detector intentionally restricted to -count=1 — see PLAN.md Step 19 bug ARQ-LIFECYCLE-1")
	}
	defer goroutineleak.Check(t)

	pool := newSOCKS5UpstreamPool(
		4,             // maxIdle
		1*time.Second, // idleTTL
		0,             // prewarm (off — we don't want background dials)
		"127.0.0.1:0", // dummy addr
		false,
		func() []byte { return nil },
		func(ctx context.Context) (net.Conn, error) {
			return nil, context.Canceled // refuse all dial attempts
		},
		func(conn net.Conn) error { return nil },
		nil,
	)
	if pool == nil || !pool.Enabled() {
		t.Fatal("expected enabled pool")
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool.startReaper(ctx)
	time.Sleep(20 * time.Millisecond) // let reaper park on ticker

	cancel()
	pool.Close()
	if ok := pool.WaitForShutdown(2 * time.Second); !ok {
		t.Fatal("reaper failed to exit within 2s after cancel+Close")
	}
}

// TestSOCKS5UpstreamPool_DisabledPoolNoLeak ensures the disabled-pool
// short-circuit doesn't spawn anything (startReaper must be a no-op).
func TestSOCKS5UpstreamPool_DisabledPoolNoLeak(t *testing.T) {
	if leakDetectorSkipUnderCount() {
		t.Skip("leak detector intentionally restricted to -count=1 — see PLAN.md Step 19 bug ARQ-LIFECYCLE-1")
	}
	defer goroutineleak.Check(t)

	disabled := newSOCKS5UpstreamPool(0, 0, 0, "", false, nil, nil, nil, nil)
	if disabled == nil {
		t.Fatal("disabled pool should still be non-nil for ergonomic API")
	}
	if disabled.Enabled() {
		t.Fatal("expected disabled pool")
	}

	disabled.startReaper(context.Background())
	if ok := disabled.WaitForShutdown(50 * time.Millisecond); !ok {
		t.Fatal("disabled pool should return true immediately from WaitForShutdown")
	}
}
