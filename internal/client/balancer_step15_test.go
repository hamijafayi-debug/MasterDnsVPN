package client

import (
	"testing"
	"time"
)

// helper to create a balancer with N active resolvers for Step 15 tests.
func newStep15Balancer(t *testing.T, n int, strategy int) *Balancer {
	t.Helper()
	b := NewBalancer(strategy, nil)
	conns := make([]*Connection, n)
	for i := 0; i < n; i++ {
		key := string(rune('a' + i))
		conns[i] = &Connection{Key: key, IsValid: true}
	}
	b.SetConnections(conns)
	for i := 0; i < n; i++ {
		key := string(rune('a' + i))
		_ = b.SetConnectionValidity(key, true)
	}
	return b
}

// TestCircuitBreaker_FastDisableAfterConsecutiveTimeouts asserts that with
// the breaker enabled, a resolver gets disabled the moment its timeout
// streak hits the configured threshold — well before the statistical
// window would normally fire.
func TestCircuitBreaker_FastDisableAfterConsecutiveTimeouts(t *testing.T) {
	b := newStep15Balancer(t, 5, BalancingRoundRobin)
	b.SetAutoDisableConfig(true, 30*time.Second)
	b.SetResolverHealthConfig(4, 0)

	// First 3 timeouts should NOT disable (under threshold AND under stat
	// window minObservations).
	for i := 0; i < 3; i++ {
		if disabled := b.ReportTimeout("a", time.Now(), 30*time.Second, 1); disabled {
			t.Fatalf("ReportTimeout #%d unexpectedly disabled resolver", i+1)
		}
	}
	if b.ActiveCount() != 5 {
		t.Fatalf("active count dropped before breaker tripped: %d", b.ActiveCount())
	}

	// 4th consecutive timeout trips the breaker.
	if disabled := b.ReportTimeout("a", time.Now(), 30*time.Second, 1); !disabled {
		t.Fatal("circuit breaker did not fire on 4th consecutive timeout")
	}
	if b.ActiveCount() != 4 {
		t.Fatalf("expected active count 4 after breaker, got %d", b.ActiveCount())
	}
}

// TestCircuitBreaker_SuccessResetsCounter verifies that a single successful
// reply clears the consecutive-timeout streak so the breaker doesn't fire
// on a sporadically slow resolver.
func TestCircuitBreaker_SuccessResetsCounter(t *testing.T) {
	b := newStep15Balancer(t, 5, BalancingRoundRobin)
	b.SetAutoDisableConfig(true, 30*time.Second)
	b.SetResolverHealthConfig(3, 0)

	b.ReportTimeout("a", time.Now(), 30*time.Second, 1)
	b.ReportTimeout("a", time.Now(), 30*time.Second, 1)
	// A success should clear the counter.
	b.ReportSuccess("a", 5*time.Millisecond)
	// Two more timeouts should not trip the breaker now (streak restarted).
	for i := 0; i < 2; i++ {
		if disabled := b.ReportTimeout("a", time.Now(), 30*time.Second, 1); disabled {
			t.Fatalf("breaker fired after success reset, iteration %d", i+1)
		}
	}
	if b.ActiveCount() != 5 {
		t.Fatalf("expected no resolver disabled, got active=%d", b.ActiveCount())
	}
}

// TestCircuitBreaker_DisabledWhenThresholdZero verifies legacy behaviour is
// preserved when the breaker knob is left at 0 (the default).
func TestCircuitBreaker_DisabledWhenThresholdZero(t *testing.T) {
	b := newStep15Balancer(t, 5, BalancingRoundRobin)
	b.SetAutoDisableConfig(true, 30*time.Second)
	// Threshold 0 = circuit breaker disabled.
	b.SetResolverHealthConfig(0, 0)

	// 100 consecutive timeouts and the resolver should still be valid
	// because the statistical window's minObservations gate will hold
	// (window is fresh, totalSent inside window starts at 0 each call).
	// We can't easily reach minObservations from outside, but verify the
	// breaker never fires by checking ActiveCount stays at 5.
	for i := 0; i < 100; i++ {
		b.ReportTimeout("a", time.Now(), 30*time.Second, 1)
	}
	// Note: the window-based path *can* fire with enough timeouts in a
	// window, so we just assert the breaker itself isn't the cause —
	// active count may legitimately drop. The key assertion is that with
	// threshold=0, an isolated burst of timeouts at a fresh window does
	// not produce the breaker log path. We assert "no immediate disable
	// after just 4 timeouts" instead:
	b2 := newStep15Balancer(t, 5, BalancingRoundRobin)
	b2.SetAutoDisableConfig(true, 30*time.Second)
	b2.SetResolverHealthConfig(0, 0)
	for i := 0; i < 4; i++ {
		if disabled := b2.ReportTimeout("a", time.Now(), 30*time.Second, 1); disabled {
			t.Fatalf("threshold=0 should disable breaker, but disabled fired at iter %d", i+1)
		}
	}
	if b2.ActiveCount() != 5 {
		t.Fatalf("threshold=0: expected ActiveCount=5, got %d", b2.ActiveCount())
	}
}

// TestCircuitBreaker_RespectsMinActive guarantees the breaker won't push
// active count below the minActive floor.
func TestCircuitBreaker_RespectsMinActive(t *testing.T) {
	b := newStep15Balancer(t, 3, BalancingRoundRobin)
	b.SetAutoDisableConfig(true, 30*time.Second)
	b.SetResolverHealthConfig(2, 0)

	// Trip the breaker repeatedly across all three resolvers. The minActive
	// floor (passed in as the 4th arg to ReportTimeout) is 2, so at most
	// one resolver should be removed.
	const minActive = 2
	for _, key := range []string{"a", "b", "c"} {
		for i := 0; i < 5; i++ {
			b.ReportTimeout(key, time.Now(), 30*time.Second, minActive)
		}
	}
	if got := b.ActiveCount(); got < minActive {
		t.Fatalf("breaker dropped active count below minActive: got=%d want>=%d", got, minActive)
	}
}

// TestReactivationProbation_DeprioritizesInRoundRobin verifies that with a
// non-zero probation window, the RR selection skips the probation entry
// when an alternative is available, then includes it once probation ends.
func TestReactivationProbation_DeprioritizesInRoundRobin(t *testing.T) {
	b := newStep15Balancer(t, 3, BalancingRoundRobin)
	b.SetResolverHealthConfig(0, 50*time.Millisecond)

	// Disable then reactivate "b" so it lands in probation.
	if !b.SetConnectionValidity("b", false) {
		t.Fatal("setValidity false failed")
	}
	if !b.SetConnectionValidityWithLog("b", true, false) {
		t.Fatal("setValidity true failed")
	}
	if !b.IsOnProbation("b") {
		t.Fatal("b should be on probation immediately after reactivation")
	}

	// Hit GetBestConnection 30 times; "b" should never come back while it
	// is still on probation, because "a" and "c" are valid alternatives.
	gotB := 0
	for i := 0; i < 30; i++ {
		conn, ok := b.GetBestConnection()
		if !ok {
			t.Fatalf("GetBestConnection failed at iter %d", i)
		}
		if conn.Key == "b" {
			gotB++
		}
	}
	if gotB != 0 {
		t.Fatalf("probation skipped only partially: got %d picks of probation resolver", gotB)
	}

	// Wait past the probation window; now "b" should appear roughly 1/3 of
	// the time on a round-robin strategy.
	time.Sleep(70 * time.Millisecond)
	if b.IsOnProbation("b") {
		t.Fatal("b should be off probation after window elapsed")
	}

	gotB = 0
	for i := 0; i < 60; i++ {
		conn, ok := b.GetBestConnection()
		if !ok {
			t.Fatalf("GetBestConnection failed at iter %d (post-probation)", i)
		}
		if conn.Key == "b" {
			gotB++
		}
	}
	if gotB == 0 {
		t.Fatal("post-probation: b never selected on RR strategy over 60 picks")
	}
}

// TestReactivationProbation_AllOnProbationFallsBack verifies that when every
// active resolver is on probation, the RR path falls back to selecting one
// of them anyway (rather than reporting no connection).
func TestReactivationProbation_AllOnProbationFallsBack(t *testing.T) {
	b := newStep15Balancer(t, 2, BalancingRoundRobin)
	b.SetResolverHealthConfig(0, 100*time.Millisecond)

	for _, key := range []string{"a", "b"} {
		_ = b.SetConnectionValidity(key, false)
		_ = b.SetConnectionValidityWithLog(key, true, false)
	}
	if !b.IsOnProbation("a") || !b.IsOnProbation("b") {
		t.Fatal("both resolvers should be on probation")
	}
	conn, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("GetBestConnection should fall back when all are on probation")
	}
	if conn.Key != "a" && conn.Key != "b" {
		t.Fatalf("unexpected key %q", conn.Key)
	}
}

// TestReactivationProbation_DisabledWhenZero verifies the legacy
// instant-traffic behaviour is preserved when the probation knob is 0.
func TestReactivationProbation_DisabledWhenZero(t *testing.T) {
	b := newStep15Balancer(t, 3, BalancingRoundRobin)
	b.SetResolverHealthConfig(0, 0)

	_ = b.SetConnectionValidity("b", false)
	_ = b.SetConnectionValidityWithLog("b", true, false)

	if b.IsOnProbation("b") {
		t.Fatal("probation=0 should never put resolvers on probation")
	}
}

// TestBlackholeResolver_FastDisableWithCircuitBreaker simulates the
// blackhole scenario: a resolver that swallows every probe. With the
// breaker on, the resolver should be disabled within a small bounded
// number of probe attempts.
func TestBlackholeResolver_FastDisableWithCircuitBreaker(t *testing.T) {
	const breakerThreshold = 6
	b := newStep15Balancer(t, 5, BalancingRoundRobin)
	b.SetAutoDisableConfig(true, 30*time.Second)
	b.SetResolverHealthConfig(breakerThreshold, 0)

	// Simulate the blackhole: "a" never replies, every probe is a timeout.
	probes := 0
	for b.ActiveCount() == 5 && probes < 200 {
		b.ReportTimeout("a", time.Now(), 30*time.Second, 1)
		probes++
	}

	if probes >= 200 {
		t.Fatal("blackhole resolver was not disabled within 200 probes")
	}
	if probes > breakerThreshold {
		t.Fatalf("breaker should fire at %d consecutive timeouts; took %d", breakerThreshold, probes)
	}
	if b.ActiveCount() != 4 {
		t.Fatalf("expected blackhole resolver to be disabled; active=%d", b.ActiveCount())
	}
}

// TestProbationCleared_OnSuccessfulReuse ensures that once probation expires,
// it doesn't re-trigger spuriously on subsequent calls.
func TestProbationCleared_OnSuccessfulReuse(t *testing.T) {
	b := newStep15Balancer(t, 2, BalancingRoundRobin)
	b.SetResolverHealthConfig(0, 10*time.Millisecond)

	_ = b.SetConnectionValidity("a", false)
	_ = b.SetConnectionValidityWithLog("a", true, false)
	if !b.IsOnProbation("a") {
		t.Fatal("a should be on probation")
	}
	time.Sleep(20 * time.Millisecond)
	if b.IsOnProbation("a") {
		t.Fatal("a should be off probation after 20ms")
	}
	// Calling ReportSuccess shouldn't put a back on probation.
	b.ReportSend("a")
	b.ReportSuccess("a", 5*time.Millisecond)
	if b.IsOnProbation("a") {
		t.Fatal("ReportSuccess must not re-arm probation")
	}
}
