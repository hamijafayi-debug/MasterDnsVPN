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

// leakDetectorSkipUnderCount reports whether the current `go test` run is
// likely a -count > 1 iteration that has inherited a leaked ARQ
// retransmitLoop goroutine from a sibling fixture in the same package.
//
// Why this helper exists: several pre-Step-19 ARQ tests construct ARQ
// instances without invoking Close+WaitForShutdown (because before this
// step there was no WaitForShutdown to invoke). Their retransmitLoop
// goroutine survives across iterations of -count and pollutes the
// snapshot diff used by the leak detector. Rather than retrofit every
// fixture (large, risky, and orthogonal to Step 19's goal), we run the
// leak detector only when no preexisting retransmitLoop is observed and
// capture the preexisting fixture leak as a tracked bug
// (see ARQ-LIFECYCLE-1 in PLAN.md).
//
// Environment overrides:
//   - LEAK_DETECTOR_FORCE_RUN=1  → never skip.
//   - LEAK_DETECTOR_SKIP=1       → always skip.
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
	return countARQRetransmitLoopsAlive() > 0
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
