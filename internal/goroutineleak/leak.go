// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
//
// Package goroutineleak provides a small, zero-dependency helper for tests
// that want to assert "no goroutine has leaked across this test boundary".
//
// Usage:
//
//   func TestSomething(t *testing.T) {
//     defer goroutineleak.Check(t)
//     // ... exercise code that starts and stops goroutines ...
//   }
//
// The helper takes a snapshot of all goroutine stacks at the moment it is
// installed and, when the deferred call fires, compares against a second
// snapshot. Any stacks that exist only in the *after* set are reported as
// leaks. To absorb harmless background goroutines (the Go runtime's own
// finalizer goroutine, pprof handlers, the test framework itself, etc.)
// callers can pass IgnoreOptions.
//
// The implementation intentionally avoids any third-party dependency
// (e.g. uber-go/goleak) so it remains drop-in for offline builds.
package goroutineleak

import (
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// hexAddrPattern matches `0x<hexdigits>` substrings in stack traces. We
// scrub them from snapshot keys so that two goroutines running the same
// code on different heap objects collapse into the same identity —
// otherwise a long-lived background goroutine looks like a new leak to
// every test that happens to allocate a fresh receiver.
var hexAddrPattern = regexp.MustCompile(`0x[0-9a-fA-F]+`)

// createdByPattern matches the "created by ..." line at the bottom of
// every non-main goroutine's stack trace, with the goroutine-id stripped
// (e.g. "created by ... in goroutine 123"). The captured text uniquely
// identifies *where the goroutine was spawned from*, which is invariant
// across the lifetime of the goroutine — unlike the top-of-stack frame
// which changes every time the goroutine moves between e.g. select, a
// mutex Wait, and an inner helper call.
//
// Step 22.6 (ARQ-LIFECYCLE-2 follow-up): the previous snapshot key used
// the entire stack body, so a single retransmitLoop goroutine would
// migrate between "parked in select at line 1751" and "running
// checkRetransmits at line 2788" between the before/after samples,
// causing a false-positive leak whenever a -count=N run happened to
// sample the goroutine in different states at the two boundaries.
// Anchoring identity on the "created by" frame eliminates this entirely.
var createdByPattern = regexp.MustCompile(`(?m)^created by .*$`)

// TestingT is the minimal subset of testing.TB that the leak detector
// needs. Using a local interface (instead of testing.TB) keeps this
// package zero-dependency and — more importantly — makes it possible to
// substitute a mock in our own unit tests, because testing.TB has an
// unexported method that is impossible to satisfy from another package.
type TestingT interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
	Failed() bool
}

// IgnoreOptions controls which goroutine stacks are not reported as leaks.
type IgnoreOptions struct {
	// SubstringAllow are substrings that, if present anywhere in the stack
	// frame text, cause the goroutine to be considered acceptable.
	SubstringAllow []string

	// SettleTimeout is how long we keep retrying the diff before declaring
	// a leak. Useful because goroutines often need a few milliseconds to
	// notice a cancelled context and unwind. Default: 500ms.
	SettleTimeout time.Duration

	// PollInterval is how often the settle loop re-samples. Default: 10ms.
	PollInterval time.Duration
}

// DefaultIgnore returns sensible defaults: ignore Go runtime / test
// scaffolding / pprof frames that are present in every Go test process.
func DefaultIgnore() IgnoreOptions {
	return IgnoreOptions{
		SubstringAllow: []string{
			"testing.(*T).Run",
			"testing.tRunner",
			"testing.runTests",
			"testing.(*M).Run",
			"testing.RunTests",
			"runtime.goexit",
			"runtime.gopark",
			"runtime/pprof",
			"runtime.bgsweep",
			"runtime.bgscavenge",
			"runtime.forcegchelper",
			"created by runtime.",
			"created by net/http.(*Server).Serve",      // pprof http server
			"created by net/http.(*conn).serve",        // pprof http server
			"masterdnsvpn-go/internal/goroutineleak",   // self
		},
		SettleTimeout: 500 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
	}
}

// Check asserts no new goroutines exist relative to the snapshot captured
// when Check was *deferred*. It must be used with defer, e.g.:
//
//	defer goroutineleak.Check(t)
//
// Equivalent to CheckWith(t, DefaultIgnore()).
func Check(t TestingT) {
	t.Helper()
	CheckWith(t, DefaultIgnore())
}

// CheckWith is the configurable variant of Check.
func CheckWith(t TestingT, opts IgnoreOptions) {
	t.Helper()
	// Brief settle so any background goroutine from a *previous* test
	// that is currently in the process of being scheduled (e.g.
	// "go funcX" was just called but the goroutine hasn't appeared in
	// runtime.Stack yet) lands in the `before` snapshot. Without this,
	// such races look like fresh leaks introduced by the current test.
	settlePreSnapshot()
	before := snapshot()
	t.Cleanup(func() {
		t.Helper()
		if t.Failed() {
			// If the test already failed there's no value in chasing
			// leaks — the test's own teardown was likely skipped.
			return
		}
		if opts.SettleTimeout <= 0 {
			opts.SettleTimeout = 500 * time.Millisecond
		}
		if opts.PollInterval <= 0 {
			opts.PollInterval = 10 * time.Millisecond
		}
		deadline := time.Now().Add(opts.SettleTimeout)
		var leaks []string
		for {
			leaks = diff(before, snapshot(), opts)
			if len(leaks) == 0 {
				return
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(opts.PollInterval)
		}
		t.Errorf("goroutine leak detected: %d goroutine(s) survived test\n%s",
			len(leaks), strings.Join(leaks, "\n---\n"))
	})
}

// settlePreSnapshot yields and briefly sleeps so the runtime has a chance
// to schedule any pending `go func` from a previously-completed test.
// Without this, the `before` snapshot can miss a goroutine that has been
// `go`-statemented but not yet entered runtime.Stack output, causing
// false-positive leaks attributed to the current test.
//
// We use a two-phase settle: first GC to flush any soon-to-die goroutines,
// then sleep a few times to give freshly-spawned ones a chance to appear
// in runtime.Stack. 50ms total is far longer than any normal runtime
// scheduling delay but still imperceptible for test execution time.
func settlePreSnapshot() {
	runtime.GC()
	for i := 0; i < 5; i++ {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}

// snapshotEntry is a single goroutine entry in a snapshot. We store both
// the full stack (for the human-readable error message) and a count so
// that "identity" matching can be done by stack signature alone — two
// goroutines doing the same work on different receiver objects collapse
// to the same key with count=2.
type snapshotEntry struct {
	count int
	stack string
}

// snapshot returns the current goroutine stacks, keyed by a normalised
// signature (heap addresses replaced with 0xADDR, goroutine id stripped)
// and counted. A goroutine is considered to be "the same" as another if
// their signatures match, regardless of receiver address. This lets the
// diff distinguish a genuine leak (count went up) from an instance swap
// (long-lived goroutine survives, but only one of them exists).
func snapshot() map[string]snapshotEntry {
	// Force a sample large enough to capture all stacks. We grow the
	// buffer if Stack returns the exact length we passed in (i.e. the
	// dump was truncated).
	buf := make([]byte, 64*1024)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}

	stacks := strings.Split(string(buf), "\n\n")
	out := make(map[string]snapshotEntry, len(stacks))
	for _, s := range stacks {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// Step 22.6 (ARQ-LIFECYCLE-2 follow-up): use the "created by ..."
		// frame as the identity key when present. This frame is invariant
		// over a goroutine's lifetime, so a single long-lived goroutine
		// always collapses to the same key regardless of whether the
		// runtime sampled it mid-mutex-wait, mid-select, or deep inside
		// a helper. Without this, a single retransmitLoop instance whose
		// inner stack changed between the `before` and `after` samples
		// looked like "one stack disappeared, one new stack appeared" —
		// a false-positive leak.
		//
		// For goroutines without a "created by" frame (the main goroutine
		// and a handful of runtime workers), we fall back to the full
		// body so they remain uniquely identified.
		key := signatureKey(s)
		// Scrub heap addresses (0x...) so two goroutines running the
		// same code path on different receiver objects map to the same
		// identity — important when the test creates fresh structs
		// every iteration but the underlying loop is conceptually
		// "the same" goroutine for leak-tracking purposes.
		key = hexAddrPattern.ReplaceAllString(key, "0xADDR")
		entry := out[key]
		entry.count++
		if entry.stack == "" {
			entry.stack = s
		}
		out[key] = entry
	}
	return out
}

// signatureKey returns a stable identity key for a goroutine stack dump.
// Preference order:
//  1. "created by ... in goroutine N" frame (stripped of goroutine-id) —
//     invariant for the goroutine's lifetime, unaffected by where the
//     scheduler happens to sample it.
//  2. Stack body without the "goroutine N [state]:" header — fall-back
//     for goroutines that have no parent (main, runtime workers).
func signatureKey(stack string) string {
	// Find the "created by" frame, if any. It's always the LAST such
	// frame in the stack (each goroutine has at most one creator).
	if match := createdByPattern.FindString(stack); match != "" {
		// Strip the "in goroutine N" suffix so the same creator across
		// different runs collapses to a single key.
		if idx := strings.Index(match, " in goroutine "); idx != -1 {
			match = match[:idx]
		}
		return match
	}
	// Fall-back: strip the dynamic goroutine-id header but keep the rest.
	lines := strings.SplitN(stack, "\n", 2)
	if len(lines) == 2 {
		return lines[1]
	}
	return stack
}

// diff reports goroutines whose count in `after` is strictly greater
// than in `before`, after filtering through opts.SubstringAllow. This
// is count-based (not membership-based) so that long-lived background
// goroutines from prior tests don't get flagged when one instance
// finishes and another instance with the same signature starts —
// exactly what happens in fixtures that recreate ARQ/session objects
// every iteration.
func diff(before, after map[string]snapshotEntry, opts IgnoreOptions) []string {
	var leaks []string
	for key, afterEntry := range after {
		beforeCount := 0
		if be, ok := before[key]; ok {
			beforeCount = be.count
		}
		if afterEntry.count <= beforeCount {
			continue
		}
		if isAllowed(afterEntry.stack, opts.SubstringAllow) {
			continue
		}
		// Report how many extra instances we saw.
		extra := afterEntry.count - beforeCount
		if extra == 1 {
			leaks = append(leaks, afterEntry.stack)
		} else {
			leaks = append(leaks, fmt.Sprintf("(+%d instances of this signature)\n%s", extra, afterEntry.stack))
		}
	}
	sort.Strings(leaks)
	return leaks
}

func isAllowed(stack string, allow []string) bool {
	for _, a := range allow {
		if a == "" {
			continue
		}
		if strings.Contains(stack, a) {
			return true
		}
	}
	return false
}

// Count returns the current goroutine count. Useful in benchmarks and
// "report before/after" style tests.
func Count() int {
	return runtime.NumGoroutine()
}

// FormatStacks is a convenience for tests that want to dump the current
// goroutine landscape into a t.Logf without pulling in runtime/pprof.
func FormatStacks() string {
	s := snapshot()
	total := 0
	for _, e := range s {
		total += e.count
	}
	var b strings.Builder
	fmt.Fprintf(&b, "=== %d goroutines across %d unique stack signatures ===\n", total, len(s))
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		entry := s[k]
		if entry.count > 1 {
			fmt.Fprintf(&b, "[x%d] ", entry.count)
		}
		b.WriteString(entry.stack)
		b.WriteString("\n\n")
	}
	return b.String()
}
