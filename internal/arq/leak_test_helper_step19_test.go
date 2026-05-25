// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package arq

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// leakDetectorSkipUnderCount historically skipped the leak detector when
// a preexisting ARQ.retransmitLoop goroutine was observed in the runtime
// stack — this happened under `go test -count > 1` because pre-Step-19
// fixtures created ARQ instances without joining their workers, so
// every iteration accumulated one more leaked goroutine.
//
// Step 19.5 (ARQ-LIFECYCLE-1 fix): every test ARQ instance in this
// package now goes through newTestARQ (see arq_test_fixture_step19_5_test.go),
// which registers a t.Cleanup hook that force-closes and joins the
// instance. The leak detector therefore runs unconditionally by default;
// the env-var escape hatches are kept solely as emergency overrides for
// environments where loopback ports are flaky or for forensic debugging.
//
// Environment overrides:
//   - LEAK_DETECTOR_FORCE_RUN=1  → never skip (default behaviour now).
//   - LEAK_DETECTOR_SKIP=1       → always skip (emergency escape hatch).
func leakDetectorSkipUnderCount() bool {
	if v := os.Getenv("LEAK_DETECTOR_FORCE_RUN"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil && b {
			return false
		}
	}
	if v := os.Getenv("LEAK_DETECTOR_SKIP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	// After Step 19.5 the fixture leak is eliminated, so we no longer
	// need to probe runtime stacks to decide whether to skip.
	return false
}

// countARQRetransmitLoopsAlive samples runtime.Stack and returns the
// number of goroutines currently parked inside ARQ.retransmitLoop.
func countARQRetransmitLoopsAlive() int {
	buf := make([]byte, 64*1024)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "internal/arq.(*ARQ).retransmitLoop")
		}
		buf = make([]byte, 2*len(buf))
	}
}
