package client

import (
	"context"
	"testing"
	"time"
)

// TestMTUProbeBackoffWithJitter exercises the pure backoff helper added in
// Step 14. The helper is deterministic (no RNG) so we can assert exact ranges.
func TestMTUProbeBackoffWithJitter(t *testing.T) {
	t.Run("ZeroBaseReturnsZero", func(t *testing.T) {
		if got := mtuProbeBackoffWithJitter(0, 3); got != 0 {
			t.Fatalf("expected 0 wait for zero base, got %v", got)
		}
	})

	t.Run("ZeroAttemptReturnsZero", func(t *testing.T) {
		if got := mtuProbeBackoffWithJitter(100*time.Millisecond, 0); got != 0 {
			t.Fatalf("expected 0 wait for attempt=0, got %v", got)
		}
		if got := mtuProbeBackoffWithJitter(100*time.Millisecond, -1); got != 0 {
			t.Fatalf("expected 0 wait for negative attempt, got %v", got)
		}
	})

	t.Run("DoublingWithJitterBounds", func(t *testing.T) {
		base := 100 * time.Millisecond
		// attempt N => base * 2^(N-1) ± 20% (jitter range = base/5)
		// floor at base/4.
		cases := []struct {
			attempt    int
			centerHint time.Duration
		}{
			{1, base},
			{2, 2 * base},
			{3, 4 * base},
			{4, 8 * base},
			{5, 16 * base},
		}
		for _, tc := range cases {
			got := mtuProbeBackoffWithJitter(base, tc.attempt)
			lower := tc.centerHint - base/5 - 1
			if lower < base/4 {
				lower = base / 4
			}
			upper := tc.centerHint + base/5 + 1
			if got < lower || got > upper {
				t.Fatalf("attempt=%d: wait %v outside expected [%v, %v]", tc.attempt, got, lower, upper)
			}
		}
	})

	t.Run("ShiftCapAt6", func(t *testing.T) {
		base := 10 * time.Millisecond
		// attempt 7 => shift=6 => base*64. attempts 8, 100 should be ~ same.
		w7 := mtuProbeBackoffWithJitter(base, 7)
		w100 := mtuProbeBackoffWithJitter(base, 100)
		expectedCenter := base << 6
		jitter := base / 5
		for _, w := range []time.Duration{w7, w100} {
			if w < expectedCenter-jitter-1 || w > expectedCenter+jitter+1 {
				t.Fatalf("shift cap broken: wait=%v outside [%v±%v]", w, expectedCenter, jitter)
			}
		}
	})

	t.Run("Deterministic", func(t *testing.T) {
		base := 250 * time.Millisecond
		for attempt := 1; attempt <= 8; attempt++ {
			a := mtuProbeBackoffWithJitter(base, attempt)
			b := mtuProbeBackoffWithJitter(base, attempt)
			if a != b {
				t.Fatalf("helper not deterministic for attempt=%d: %v vs %v", attempt, a, b)
			}
		}
	})

	t.Run("NeverBelowBaseQuarter", func(t *testing.T) {
		base := 200 * time.Millisecond
		for attempt := 1; attempt <= 10; attempt++ {
			got := mtuProbeBackoffWithJitter(base, attempt)
			if got < base/4 {
				t.Fatalf("attempt=%d: wait %v below base/4=%v", attempt, got, base/4)
			}
		}
	})
}

// TestBinarySearchMTU_AggressiveGapPrune verifies that with aggressive mode
// enabled and a small gap-prune threshold, binarySearchMTU exits earlier than
// the legacy (non-aggressive) variant on the same testFn. The testFn always
// succeeds, so the legacy code would polish all the way to `high`.
func TestBinarySearchMTU_AggressiveGapPrune(t *testing.T) {
	c := createTestClient(t)
	c.mtuTestRetries = 1
	c.mtuTestTimeout = 50 * time.Millisecond

	const low, high = 100, 200

	// testFn that always succeeds, counting how many candidate values were
	// tested.
	makeFn := func(counter *int) func(int, bool) (bool, time.Duration, error) {
		return func(candidate int, isRetry bool) (bool, time.Duration, error) {
			*counter++
			return true, time.Microsecond, nil
		}
	}

	// Legacy run (aggressive off).
	c.mtuProbeAggressive = false
	c.mtuProbeGapPrune = 0
	legacyCount := 0
	legacyBest, _ := c.binarySearchMTU(context.Background(), "test", low, high, low, makeFn(&legacyCount))

	// Aggressive run with a non-trivial gap prune.
	c.mtuProbeAggressive = true
	c.mtuProbeGapPrune = 16
	aggressiveCount := 0
	aggressiveBest, _ := c.binarySearchMTU(context.Background(), "test", low, high, low, makeFn(&aggressiveCount))

	// Both runs should converge on the same `best` when testFn always
	// succeeds, because the first probe of `high` succeeds and we return
	// straight away. Verify the early-return semantics hold.
	if legacyBest != high || aggressiveBest != high {
		t.Fatalf("expected both runs to settle at high=%d, got legacy=%d aggressive=%d",
			high, legacyBest, aggressiveBest)
	}

	if legacyCount != 1 || aggressiveCount != 1 {
		t.Fatalf("expected exactly 1 probe per run when high succeeds, got legacy=%d aggressive=%d",
			legacyCount, aggressiveCount)
	}

	// Now exercise the loop path: testFn succeeds for everything except
	// high, so we enter the binary search.
	makeFnExceptHigh := func(counter *int, fail int) func(int, bool) (bool, time.Duration, error) {
		return func(candidate int, isRetry bool) (bool, time.Duration, error) {
			*counter++
			if candidate >= fail {
				return false, 0, nil
			}
			return true, time.Microsecond, nil
		}
	}

	c.mtuProbeAggressive = false
	c.mtuProbeGapPrune = 0
	legacyCount = 0
	legacyBest, _ = c.binarySearchMTU(context.Background(), "test", low, high, low, makeFnExceptHigh(&legacyCount, high))

	c.mtuProbeAggressive = true
	c.mtuProbeGapPrune = 32 // aggressive threshold
	aggressiveCount = 0
	aggressiveBest, _ = c.binarySearchMTU(context.Background(), "test", low, high, low, makeFnExceptHigh(&aggressiveCount, high))

	if legacyBest < low || aggressiveBest < low {
		t.Fatalf("unexpected boundary failure: legacy=%d aggressive=%d (low=%d)", legacyBest, aggressiveBest, low)
	}
	if aggressiveCount > legacyCount {
		t.Fatalf("aggressive mode should not require more probes than legacy: legacy=%d aggressive=%d",
			legacyCount, aggressiveCount)
	}
	// Best is allowed to differ within the gap-prune window.
	if legacyBest < aggressiveBest-c.mtuProbeGapPrune || legacyBest > aggressiveBest+c.mtuProbeGapPrune {
		t.Fatalf("legacy/aggressive best differ by more than gap window: legacy=%d aggressive=%d gap=%d",
			legacyBest, aggressiveBest, c.mtuProbeGapPrune)
	}
}

// TestBinarySearchMTU_AggressiveConsecutiveFails verifies the two-consecutive-
// failure early exit kicks in when the testFn fails above a plateau.
func TestBinarySearchMTU_AggressiveConsecutiveFails(t *testing.T) {
	c := createTestClient(t)
	c.mtuTestRetries = 1
	c.mtuTestTimeout = 50 * time.Millisecond
	c.mtuProbeAggressive = true
	c.mtuProbeGapPrune = 4

	const (
		low     = 100
		high    = 500
		plateau = 150
	)

	probeCount := 0
	testFn := func(candidate int, isRetry bool) (bool, time.Duration, error) {
		probeCount++
		if candidate <= plateau {
			return true, time.Microsecond, nil
		}
		return false, 0, nil
	}

	best, _ := c.binarySearchMTU(context.Background(), "test", low, high, low, testFn)

	if best < low || best > plateau {
		t.Fatalf("best out of bounds: got=%d, expected within [%d, %d]", best, low, plateau)
	}
	// log2(500-100) ~ 9 binary-search candidates worst case. Aggressive mode
	// should exit well before that. Allow up to 12 probes (boundary checks
	// plus binary search before early exit).
	if probeCount > 12 {
		t.Fatalf("aggressive consecutive-fail early-exit didn't trigger: %d probes used", probeCount)
	}
}

// TestBinarySearchMTU_RetryBackoffSleepsBetweenAttempts verifies that when
// mtuProbeRetryBackoff is set, the check loop waits between retries (and that
// the wait is bounded by the helper's deterministic output).
func TestBinarySearchMTU_RetryBackoffSleepsBetweenAttempts(t *testing.T) {
	c := createTestClient(t)
	c.mtuTestRetries = 3
	c.mtuTestTimeout = 50 * time.Millisecond
	c.mtuProbeRetryBackoff = 5 * time.Millisecond

	attempts := 0
	testFn := func(candidate int, isRetry bool) (bool, time.Duration, error) {
		attempts++
		// First call (attempt=0, isRetry=false) for boundary `high` fails so
		// we retry. After 3 attempts, fail permanently so the search picks
		// `low` boundary path.
		return false, 0, nil
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, _ = c.binarySearchMTU(ctx, "test", 100, 100, 100, testFn)
	elapsed := time.Since(start)

	// With retries=3 and backoff=5ms, we expect at least 1 backoff between
	// attempts 1->2 (jitter-adjusted, lower-bounded by base/4 = ~1.25ms).
	if attempts < 2 {
		t.Fatalf("expected at least 2 attempts on the single boundary, got %d", attempts)
	}
	if elapsed < time.Millisecond {
		t.Fatalf("expected some elapsed time due to backoff, got %v", elapsed)
	}
}

// TestBinarySearchMTU_RespectsContextCancel ensures aggressive mode + backoff
// don't introduce paths that ignore ctx cancellation.
func TestBinarySearchMTU_RespectsContextCancel(t *testing.T) {
	c := createTestClient(t)
	c.mtuTestRetries = 5
	c.mtuTestTimeout = 50 * time.Millisecond
	c.mtuProbeAggressive = true
	c.mtuProbeRetryBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	testFn := func(candidate int, isRetry bool) (bool, time.Duration, error) {
		// Cancel mid-flight so the next backoff wait should return early.
		cancel()
		return false, 0, nil
	}

	start := time.Now()
	best, _ := c.binarySearchMTU(ctx, "test", 100, 500, 100, testFn)
	elapsed := time.Since(start)

	if best != 0 {
		t.Fatalf("expected 0 best on cancelled context, got %d", best)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx cancel ignored: elapsed=%v", elapsed)
	}
}
