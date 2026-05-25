// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package goroutineleak

import (
	"context"
	"sync"
	"testing"
	"time"
)

// _ static interface assertions
var _ TestingT = (*testing.T)(nil)
var _ TestingT = (*recorderT)(nil)

func TestCheck_NoLeak(t *testing.T) {
	defer Check(t)
	// Start and properly stop a goroutine — must not be flagged.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
	}()
	cancel()
	wg.Wait()
}

func TestCheck_DetectsLeak(t *testing.T) {
	// We use a synthetic inner T because we expect the leak detector to
	// FAIL — but we want the outer test to pass.
	inner := &recorderT{}
	leakStop := make(chan struct{})
	CheckWith(inner, IgnoreOptions{
		SettleTimeout: 100 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
	})
	// Start a goroutine that never exits during the inner test scope.
	go func() {
		<-leakStop
	}()

	// Simulate the test ending — fire the registered cleanup.
	inner.runCleanups()
	if !inner.Errored() {
		t.Fatal("expected leak detector to record an error")
	}
	close(leakStop)
}

func TestCheck_AllowsRespawn(t *testing.T) {
	defer Check(t)
	// Spawn a worker, let it exit, spawn a new instance with the same
	// stack text. The snapshot key (which strips the goroutine id) must
	// recognise that this is the same kind of work, so no leak.
	done := make(chan struct{})
	go func() {
		<-done
	}()
	close(done)
	// brief pause to let it unwind
	time.Sleep(20 * time.Millisecond)

	done2 := make(chan struct{})
	go func() {
		<-done2
	}()
	close(done2)
	time.Sleep(20 * time.Millisecond)
}

func TestCount(t *testing.T) {
	n := Count()
	if n <= 0 {
		t.Fatalf("expected at least one goroutine, got %d", n)
	}
}

// recorderT is a stand-in for *testing.T used by TestCheck_DetectsLeak.
// It only needs to implement the local TestingT interface, not the full
// testing.TB (which has unexported methods we cannot satisfy).
type recorderT struct {
	mu       sync.Mutex
	errored  bool
	cleanups []func()
}

func (r *recorderT) runCleanups() {
	r.mu.Lock()
	cs := r.cleanups
	r.cleanups = nil
	r.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
}

func (r *recorderT) Errored() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.errored
}

func (r *recorderT) Helper() {}
func (r *recorderT) Cleanup(f func()) {
	r.mu.Lock()
	r.cleanups = append(r.cleanups, f)
	r.mu.Unlock()
}
func (r *recorderT) Errorf(format string, args ...any) {
	r.mu.Lock()
	r.errored = true
	r.mu.Unlock()
}
func (r *recorderT) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.errored
}

// silence unused-import warnings when running with race detector noise
var _ = context.Background
var _ = time.Second
