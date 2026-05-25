// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package client

import (
	"context"
	"sync"
	"testing"

	"masterdnsvpn-go/internal/config"
)

// BenchmarkDispatchPlannerTask_BlockPolicy measures the cost of the
// "block" overflow policy (pre-Step-20 behaviour) when the queue
// always has room. This is the per-packet hot-path cost on the happy
// path — any regression here would slow every send.
func BenchmarkDispatchPlannerTask_BlockPolicy(b *testing.B) {
	c := newOverflowBenchClient("block", 1024)
	go drainPlanner(c, b.N)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true})
	}
}

// BenchmarkDispatchPlannerTask_DropNewest measures the drop-newest
// policy under sustained overflow (no consumer). The full burst is
// absorbed without parking any producer goroutine.
func BenchmarkDispatchPlannerTask_DropNewest(b *testing.B) {
	c := newOverflowBenchClient("drop-newest", 16)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true})
	}
}

// BenchmarkDispatchPlannerTask_DropOldest measures the drop-oldest
// policy under sustained overflow. Per-call cost includes one eviction
// once the queue saturates, so it is necessarily higher than
// drop-newest. We benchmark the steady-state path.
func BenchmarkDispatchPlannerTask_DropOldest(b *testing.B) {
	c := newOverflowBenchClient("drop-oldest", 16)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true})
	}
}

// BenchmarkDispatchPlannerTask_DropNewest_Parallel exercises the same
// drop-newest hot path with multiple producer goroutines competing
// for the channel — this is the realistic shape under load.
func BenchmarkDispatchPlannerTask_DropNewest_Parallel(b *testing.B) {
	c := newOverflowBenchClient("drop-newest", 16)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			c.dispatchPlannerTask(ctx, plannerTask{wasPacked: true})
		}
	})
}

// newOverflowBenchClient mirrors newOverflowTestClient but without the
// *testing.T helper marker (benchmarks already get b.Helper coverage).
func newOverflowBenchClient(policy string, cap int) *Client {
	return &Client{
		cfg:              config.ClientConfig{StreamQueueOverflowPolicy: policy},
		plannerQueue:     make(chan plannerTask, cap),
		encodedTXChannel: make(chan writerTask, cap),
	}
}

// drainPlanner runs alongside the block-policy benchmark, consuming
// tasks as fast as the producer queues them. Without this drain the
// "block" benchmark would simply measure a parked producer.
func drainPlanner(c *Client, n int) {
	for i := 0; i < n; i++ {
		<-c.plannerQueue
	}
}

// Lightweight wrapper so the bench file compiles in isolation if the
// helpers are moved later.
var _ sync.Locker = (*sync.Mutex)(nil)
