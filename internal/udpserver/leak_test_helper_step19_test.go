// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package udpserver

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// leakDetectorSkipUnderCount reports whether the current `go test` run
// is using -count > 1. The standard library doesn't expose this directly,
// but the `GO_TEST_COUNT_HINT` env var lets developers force enable the
// skip; we also probe runtime.NumGoroutine to make an educated guess
// (a fresh process will have <10 goroutines before any tests run; if
// we see significantly more during init, a sibling iteration is in play).
//
// Why this helper exists: leak tests in this package rely on snapshot
// diffing. Sibling fixtures in `*_test.go` create ARQ instances without
// closing them — the resulting retransmitLoop goroutine survives each
// iteration of -count, polluting our diff. Rather than rewrite every
// fixture (large, risky, and orthogonal to Step 19's goal), we run the
// leak detector only under -count=1 and capture the preexisting fixture
// leak as a tracked bug.
func leakDetectorSkipUnderCount() bool {
	if v := os.Getenv("LEAK_DETECTOR_FORCE_RUN"); v != "" {
		// Explicit override: caller wants the detector to run regardless.
		if b, err := strconv.ParseBool(v); err == nil && b {
			return false
		}
	}
	if v := os.Getenv("LEAK_DETECTOR_SKIP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	// Step 19.5 (ARQ-LIFECYCLE-1 fix): newTestSessionRecord(t, ...) now
	// registers a t.Cleanup hook that walks the record's Streams map
	// and force-closes + joins every ARQ instance. Previously this
	// helper had to probe runtime.Stack to decide whether to skip; the
	// fixture leak is gone, so the detector runs unconditionally.
	return false
}

// countARQRetransmitLoopsAlive samples runtime.Stack and returns the
// number of goroutines currently parked inside ARQ.retransmitLoop. Used
// only by leak tests to decide whether they should skip.
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
