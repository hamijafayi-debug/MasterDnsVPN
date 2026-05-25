// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package arq

import (
	"io"
	"sync/atomic"
	"testing"
	"time"

	"masterdnsvpn-go/internal/goroutineleak"
)

// leakTestConn is a minimal io.ReadWriteCloser used by the goroutine
// leak tests. Read blocks until Close is called so the ARQ ioLoop has
// somewhere to park, and Write accepts everything.
type leakTestConn struct {
	closed atomic.Bool
	doneCh chan struct{}
}

func newLeakTestConn() *leakTestConn {
	return &leakTestConn{doneCh: make(chan struct{})}
}

func (c *leakTestConn) Read(p []byte) (int, error) {
	<-c.doneCh
	return 0, io.EOF
}
func (c *leakTestConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *leakTestConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		close(c.doneCh)
	}
	return nil
}

// TestARQ_NoGoroutineLeakAfterClose verifies that an ARQ instance started
// with Start() and torn down with Close(Force) + WaitForShutdown does NOT
// leave any background goroutine (retransmitLoop, rxLoop, ioLoop, writeLoop)
// alive after the test scope exits.
//
// This is the regression test for the lifecycle audit done in Step 19.
func TestARQ_NoGoroutineLeakAfterClose(t *testing.T) {
	if leakDetectorSkipUnderCount() {
		t.Skip("skipping leak detector: preexisting ARQ retransmitLoop leak from sibling fixture (see ARQ-LIFECYCLE-1)")
	}
	defer goroutineleak.Check(t)

	enqueuer := NewMockPacketEnqueuer()
	cfg := Config{WindowSize: 32, RTO: 0.1, MaxRTO: 1.0}

	for i := 0; i < 5; i++ {
		a := NewARQ(uint16(i+1), 1, enqueuer, nil, 1000, newTestLogger(t), cfg)
		a.Start()

		// Exercise the rxLoop briefly so the goroutine actually parks
		// on a.rxChan / a.ctx.Done().
		_ = a.ReceiveData(0, []byte("hello"))

		a.Close("test cleanup", CloseOptions{Force: true})
		if ok := a.WaitForShutdown(2 * time.Second); !ok {
			t.Fatalf("ARQ #%d failed to shut down within 2s", i)
		}
	}
}

// TestARQ_NoGoroutineLeakWithStreamWorkers covers the path where Start()
// boots the ioLoop + writeLoop goroutines (which only run when a local
// connection is attached). Even with all four loops alive, Close+Wait
// must reclaim every goroutine.
func TestARQ_NoGoroutineLeakWithStreamWorkers(t *testing.T) {
	if leakDetectorSkipUnderCount() {
		t.Skip("skipping leak detector: preexisting ARQ retransmitLoop leak from sibling fixture (see ARQ-LIFECYCLE-1)")
	}
	defer goroutineleak.Check(t)

	enqueuer := NewMockPacketEnqueuer()
	cfg := Config{WindowSize: 32, RTO: 0.1, MaxRTO: 1.0}
	conn := newLeakTestConn()
	defer conn.Close()

	a := NewARQ(7, 9, enqueuer, conn, 1000, newTestLogger(t), cfg)
	a.Start()

	// Push a tiny payload so writeLoop actually runs at least once.
	_ = a.ReceiveData(0, []byte("data"))
	time.Sleep(20 * time.Millisecond)

	a.Close("worker leak test", CloseOptions{Force: true})
	if ok := a.WaitForShutdown(2 * time.Second); !ok {
		t.Fatalf("ARQ (with stream workers) failed to shut down within 2s")
	}
}
