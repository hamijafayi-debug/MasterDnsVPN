// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
// Step 16 — Adaptive Duplication Policy unit tests.
//
// These tests cover three independent surfaces:
//   1. Balancer.GlobalLossPercent — the lock-free, cached loss estimator
//      used by the adaptive policy on every data packet.
//   2. Client.runtimePacketDuplicationCount — the policy decision itself:
//      suppression when path is healthy, retention when path is lossy,
//      default-disabled when the operator hasn't opted in, and the
//      setup/control packet exemption.
//   3. The metrics surface (AdaptiveDupSuppressed / AdaptiveDupApplied)
//      so observability stays consistent with PLAN.md step 1.
// ==============================================================================
package client

import (
	"testing"
	"time"

	"masterdnsvpn-go/internal/config"
	Enums "masterdnsvpn-go/internal/enums"
	"masterdnsvpn-go/internal/metrics"
)

// ----------------------------------------------------------------------------
// Balancer.GlobalLossPercent
// ----------------------------------------------------------------------------

// seedBalancerStats writes synthetic (sent, acked, lost) totals directly into
// the connectionStats counters for every key the balancer knows about. The
// helper bypasses ReportSend / ReportSuccess / ReportTimeout so the test can
// pin the exact ratio the adaptive policy is being judged against. The stats
// remain accessible through the same lookupSnapshot the production hot path
// consults.
func seedBalancerStats(t *testing.T, c *Client, sent uint64, acked uint64, lost uint64) {
	t.Helper()
	snap := c.balancer.lookupSnap.Load()
	if snap == nil {
		t.Fatalf("balancer snapshot is nil; did SetConnections run?")
	}
	for _, stats := range snap.stats {
		if stats == nil {
			continue
		}
		stats.sent.Store(sent)
		stats.acked.Store(acked)
		stats.lost.Store(lost)
	}
	c.balancer.invalidateGlobalLossCache()
}

func TestGlobalLossPercent_EmptyReturnsZero(t *testing.T) {
	c := buildTestClientWithResolvers(config.ClientConfig{})
	pct, samples := c.balancer.GlobalLossPercent()
	if pct != 0 || samples != 0 {
		t.Fatalf("expected (0,0) for empty balancer, got (%.3f,%d)", pct, samples)
	}
}

func TestGlobalLossPercent_ComputesFromAckedLost(t *testing.T) {
	c := buildTestClientWithResolvers(config.ClientConfig{}, "a", "b", "c")
	// 100 sent, 90 acked, 10 lost per resolver → aggregate 300/270/30 → 10%.
	seedBalancerStats(t, c, 100, 90, 10)

	pct, samples := c.balancer.GlobalLossPercent()
	if samples != 300 {
		t.Fatalf("expected 300 aggregate sent, got %d", samples)
	}
	// Loss = lost / (acked+lost) = 30 / 300 = 10%.
	if pct < 9.99 || pct > 10.01 {
		t.Fatalf("expected ~10%% loss, got %.3f%%", pct)
	}
}

func TestGlobalLossPercent_FallsBackToSentWhenNoFeedback(t *testing.T) {
	c := buildTestClientWithResolvers(config.ClientConfig{}, "a")
	// All sent, nothing acked/lost yet — the estimate should report 0%
	// (lost / sent = 0 / 100) rather than crash on a divide-by-zero.
	seedBalancerStats(t, c, 100, 0, 0)

	pct, samples := c.balancer.GlobalLossPercent()
	if samples != 100 {
		t.Fatalf("expected 100 sent, got %d", samples)
	}
	if pct != 0 {
		t.Fatalf("expected 0%% loss with no feedback, got %.3f%%", pct)
	}
}

func TestGlobalLossPercent_CachedWithinTTL(t *testing.T) {
	c := buildTestClientWithResolvers(config.ClientConfig{}, "a")
	seedBalancerStats(t, c, 100, 90, 10)
	// Warm the cache.
	pct1, _ := c.balancer.GlobalLossPercent()

	// Mutate the underlying counters but DO NOT invalidate the cache.
	snap := c.balancer.lookupSnap.Load()
	snap.stats[0].lost.Store(90) // would push loss to 50% if recomputed
	snap.stats[0].acked.Store(10)

	pct2, _ := c.balancer.GlobalLossPercent()
	if pct1 != pct2 {
		t.Fatalf("cached value diverged within TTL: %.3f → %.3f", pct1, pct2)
	}

	// After invalidation the next call must recompute.
	c.balancer.invalidateGlobalLossCache()
	pct3, _ := c.balancer.GlobalLossPercent()
	if pct3 == pct1 {
		t.Fatalf("expected recomputation after invalidate, got cached %.3f", pct3)
	}
}

func TestGlobalLossPercent_SetConnectionsInvalidatesCache(t *testing.T) {
	c := buildTestClientWithResolvers(config.ClientConfig{}, "a")
	seedBalancerStats(t, c, 100, 90, 10)
	pct1, samples1 := c.balancer.GlobalLossPercent()
	if pct1 == 0 {
		t.Fatalf("expected nonzero loss seed, got 0")
	}

	// Build a brand-new resolver set with no stats — the cache must
	// reflect zero rather than the stale 10% value.
	fresh := Connection{Key: "b", Domain: "b.example.com", Resolver: "127.0.0.1", ResolverPort: 6000}
	c.balancer.SetConnections([]*Connection{&fresh})
	pct2, samples2 := c.balancer.GlobalLossPercent()
	if pct2 != 0 || samples2 != 0 {
		t.Fatalf("expected (0,0) after SetConnections, got (%.3f,%d); pre-state was (%.3f,%d)",
			pct2, samples2, pct1, samples1)
	}
}

func TestGlobalLossPercent_ClampedTo100(t *testing.T) {
	c := buildTestClientWithResolvers(config.ClientConfig{}, "a")
	// Pathological state — more lost than acked+lost would imply if our
	// math were sloppy. Guarantee the return value never exceeds 100%.
	seedBalancerStats(t, c, 100, 0, 1000)
	pct, _ := c.balancer.GlobalLossPercent()
	if pct < 99 || pct > 100 {
		t.Fatalf("expected clamp to ~100%%, got %.3f", pct)
	}
}

// ----------------------------------------------------------------------------
// Client.runtimePacketDuplicationCount — policy decisions
// ----------------------------------------------------------------------------

func mkAdaptiveCfg(enabled bool, lossThreshold float64, minSamples int) config.ClientConfig {
	return config.ClientConfig{
		PacketDuplicationCount:         3,
		SetupPacketDuplicationCount:    4,
		AdaptiveDuplication:            enabled,
		AdaptiveDuplicationLossPercent: lossThreshold,
		AdaptiveDuplicationMinSamples:  minSamples,
	}
}

func TestRuntimeDup_AdaptiveDisabled_KeepsConfigured(t *testing.T) {
	// adaptive off → the old behaviour: full PacketDuplicationCount regardless
	// of path health. This is the default-off backward-compat case PLAN.md
	// step 16 explicitly calls out.
	c := buildTestClientWithResolvers(mkAdaptiveCfg(false, 2.0, 10), "a")
	seedBalancerStats(t, c, 1000, 999, 1) // ~0.1% loss — would suppress if on

	got := c.runtimePacketDuplicationCount(Enums.PACKET_STREAM_DATA)
	if got != 3 {
		t.Fatalf("expected dup=3 with adaptive off, got %d", got)
	}
}

func TestRuntimeDup_AdaptiveLowLoss_SuppressesToOne(t *testing.T) {
	c := buildTestClientWithResolvers(mkAdaptiveCfg(true, 2.0, 10), "a")
	seedBalancerStats(t, c, 1000, 999, 1) // 0.1% loss → below 2% threshold

	beforeSuppressed := metrics.AdaptiveDupSuppressed.Value()
	got := c.runtimePacketDuplicationCount(Enums.PACKET_STREAM_DATA)
	if got != 1 {
		t.Fatalf("expected dup=1 (suppressed) on low-loss path, got %d", got)
	}
	if metrics.AdaptiveDupSuppressed.Value() != beforeSuppressed+1 {
		t.Fatalf("expected AdaptiveDupSuppressed to increment by 1, got delta %d",
			metrics.AdaptiveDupSuppressed.Value()-beforeSuppressed)
	}
}

func TestRuntimeDup_AdaptiveHighLoss_KeepsConfigured(t *testing.T) {
	c := buildTestClientWithResolvers(mkAdaptiveCfg(true, 2.0, 10), "a")
	seedBalancerStats(t, c, 1000, 900, 100) // 10% loss → above 2% threshold

	beforeApplied := metrics.AdaptiveDupApplied.Value()
	got := c.runtimePacketDuplicationCount(Enums.PACKET_STREAM_DATA)
	if got != 3 {
		t.Fatalf("expected dup=3 (applied) on lossy path, got %d", got)
	}
	if metrics.AdaptiveDupApplied.Value() != beforeApplied+1 {
		t.Fatalf("expected AdaptiveDupApplied to increment by 1, got delta %d",
			metrics.AdaptiveDupApplied.Value()-beforeApplied)
	}
}

func TestRuntimeDup_AdaptiveExactlyAtThreshold_KeepsConfigured(t *testing.T) {
	// The threshold comparison is strict-less-than (lossPct < threshold) so
	// "exactly at threshold" means "no suppression". Pin that behaviour.
	c := buildTestClientWithResolvers(mkAdaptiveCfg(true, 2.0, 10), "a")
	seedBalancerStats(t, c, 1000, 980, 20) // exactly 2% loss

	got := c.runtimePacketDuplicationCount(Enums.PACKET_STREAM_DATA)
	if got != 3 {
		t.Fatalf("expected dup=3 at exact threshold (strict-less-than semantics), got %d", got)
	}
}

func TestRuntimeDup_AdaptiveBelowMinSamples_KeepsConfigured(t *testing.T) {
	// Not enough samples yet → don't gamble on suppression. This is the
	// "fresh session" gate that protects against a single-packet timeout
	// flipping the policy on cold connections.
	c := buildTestClientWithResolvers(mkAdaptiveCfg(true, 2.0, 100), "a")
	seedBalancerStats(t, c, 50, 49, 1) // only 50 sent — below min=100

	got := c.runtimePacketDuplicationCount(Enums.PACKET_STREAM_DATA)
	if got != 3 {
		t.Fatalf("expected dup=3 below min samples, got %d", got)
	}
}

func TestRuntimeDup_AdaptiveDoesNotAffectSetupPackets(t *testing.T) {
	// Setup/control packets must NEVER be suppressed by the adaptive policy
	// because each one costs a stream RTT when lost. Walk all five
	// exempt packet types so this contract is enforced going forward.
	c := buildTestClientWithResolvers(mkAdaptiveCfg(true, 100.0, 1), "a")
	seedBalancerStats(t, c, 1000, 999, 1) // health screams "suppress"

	for _, pt := range []uint8{
		Enums.PACKET_STREAM_SYN,
		Enums.PACKET_PACKED_CONTROL_BLOCKS,
		Enums.PACKET_SOCKS5_SYN,
		Enums.PACKET_STREAM_CLOSE_READ,
		Enums.PACKET_STREAM_CLOSE_WRITE,
	} {
		got := c.runtimePacketDuplicationCount(pt)
		// Setup dup count is 4, packet dup count is 3 → setup branch wins.
		if got != 4 {
			t.Fatalf("packet type %d: expected setup-dup=4 even with adaptive on, got %d", pt, got)
		}
	}
}

func TestRuntimeDup_AdaptiveDoesNotForcePingAboveTwo(t *testing.T) {
	// Pings are clamped to min(count,2). Whether or not adaptive runs first,
	// this clamp must still apply. (Adaptive suppression to 1 is also fine
	// for pings; we just verify the cap is intact.)
	c := buildTestClientWithResolvers(mkAdaptiveCfg(true, 100.0, 1), "a")
	seedBalancerStats(t, c, 1000, 900, 100) // 10% loss → adaptive would keep dup

	got := c.runtimePacketDuplicationCount(Enums.PACKET_PING)
	if got != 2 {
		t.Fatalf("expected ping dup clamped to 2, got %d", got)
	}
}

func TestRuntimeDup_AdaptiveConfiguredOneCannotIncrease(t *testing.T) {
	// Edge: when PacketDuplicationCount is already 1, the adaptive code path
	// must early-exit (count > 1 guard) and never accidentally count a
	// metric increment for a no-op decision.
	cfg := mkAdaptiveCfg(true, 2.0, 10)
	cfg.PacketDuplicationCount = 1
	c := buildTestClientWithResolvers(cfg, "a")
	seedBalancerStats(t, c, 1000, 999, 1)

	beforeS := metrics.AdaptiveDupSuppressed.Value()
	beforeA := metrics.AdaptiveDupApplied.Value()
	got := c.runtimePacketDuplicationCount(Enums.PACKET_STREAM_DATA)
	if got != 1 {
		t.Fatalf("expected dup=1 when configured to 1, got %d", got)
	}
	if metrics.AdaptiveDupSuppressed.Value() != beforeS {
		t.Fatalf("no-op decision incremented AdaptiveDupSuppressed: delta=%d",
			metrics.AdaptiveDupSuppressed.Value()-beforeS)
	}
	if metrics.AdaptiveDupApplied.Value() != beforeA {
		t.Fatalf("no-op decision incremented AdaptiveDupApplied: delta=%d",
			metrics.AdaptiveDupApplied.Value()-beforeA)
	}
}

// ----------------------------------------------------------------------------
// Concurrency smoke test — the hot path must remain free of races even when
// loss feedback is updated by one goroutine while a fleet of "senders" is
// polling the policy decision.
// ----------------------------------------------------------------------------

func TestRuntimeDup_AdaptiveConcurrentReadersAndUpdater(t *testing.T) {
	c := buildTestClientWithResolvers(mkAdaptiveCfg(true, 2.0, 10), "a", "b", "c")
	seedBalancerStats(t, c, 1000, 990, 10)

	stop := make(chan struct{})
	var readers []chan struct{}

	for i := 0; i < 8; i++ {
		done := make(chan struct{})
		readers = append(readers, done)
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.runtimePacketDuplicationCount(Enums.PACKET_STREAM_DATA)
			}
		}()
	}

	updater := make(chan struct{})
	go func() {
		defer close(updater)
		snap := c.balancer.lookupSnap.Load()
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			for _, s := range snap.stats {
				if s == nil {
					continue
				}
				s.lost.Add(1)
				s.acked.Add(10)
			}
			c.balancer.invalidateGlobalLossCache()
			time.Sleep(time.Millisecond)
		}
	}()

	<-updater
	close(stop)
	for _, done := range readers {
		<-done
	}
}
