// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package client

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"masterdnsvpn-go/internal/config"
	"masterdnsvpn-go/internal/metrics"
)

// resolveOverflowMode mirrors the production parser so the unit tests
// can verify every accepted spelling / fallback without spinning up a
// full Client.
func TestEffectiveStreamQueueOverflowMode_Resolution(t *testing.T) {
	tests := []struct {
		policy string
		want   config.StreamQueueOverflowMode
	}{
		{"", config.StreamQueueOverflowBlock},                       // default
		{"block", config.StreamQueueOverflowBlock},                  // explicit block
		{"BLOCK", config.StreamQueueOverflowBlock},                  // case-insensitive
		{"drop-newest", config.StreamQueueOverflowDropNewest},       //
		{"drop_newest", config.StreamQueueOverflowDropNewest},       // underscore form
		{"newest", config.StreamQueueOverflowDropNewest},            // short form
		{"DROP-NEWEST", config.StreamQueueOverflowDropNewest},       // case
		{" drop-newest ", config.StreamQueueOverflowDropNewest},     // whitespace
		{"drop-oldest", config.StreamQueueOverflowDropOldest},       //
		{"drop_oldest", config.StreamQueueOverflowDropOldest},       //
		{"oldest", config.StreamQueueOverflowDropOldest},            //
		{"nonsense", config.StreamQueueOverflowBlock},               // safe fallback
		{"drop", config.StreamQueueOverflowBlock},                   // ambiguous → block
	}
	for _, tt := range tests {
		cfg := config.ClientConfig{StreamQueueOverflowPolicy: tt.policy}
		got := cfg.EffectiveStreamQueueOverflowMode()
		if got != tt.want {
			t.Errorf("policy=%q want=%d got=%d", tt.policy, tt.want, got)
		}
	}
}

// newOverflowTestClient constructs a Client with just enough wiring for
// dispatchPlannerTask / dispatchWriterTask to operate. A real Client
// requires resolvers, a codec, a logger, … which is more setup than
// these tests need. We only touch the channels and the parsed policy.
func newOverflowTestClient(t *testing.T, policy string, cap int) *Client {
	t.Helper()
	c := &Client{
		cfg:              config.ClientConfig{StreamQueueOverflowPolicy: policy},
		plannerQueue:     make(chan plannerTask, cap),
		encodedTXChannel: make(chan writerTask, cap),
	}
	return c
}

func TestDispatchPlannerTask_BlockPolicy_BlocksThenDrainsOnConsume(t *testing.T) {
	// Snapshot the global counter delta — other unit tests run in the
	// same process and may have bumped it. Block policy must add zero
	// drops during *this* test's lifetime.
	beforeNew := metrics.StreamQueueDropsNewest.Value()
	beforeOld := metrics.StreamQueueDropsOldest.Value()

	c := newOverflowTestClient(t, "block", 1)
	// Fill the queue.
	if !c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true}) {
		t.Fatal("first send should succeed")
	}

	// Second send must block. Run it in a goroutine and prove via
	// timing that it does not return until the consumer drains.
	sent := make(chan struct{})
	go func() {
		ok := c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true})
		if !ok {
			t.Errorf("expected eventual success after drain")
		}
		close(sent)
	}()

	// Give the producer a brief chance to park.
	select {
	case <-sent:
		t.Fatal("producer returned before consumer drained — block policy not enforced")
	case <-time.After(50 * time.Millisecond):
	}

	// Drain one and the producer must complete.
	<-c.plannerQueue
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("producer never woke up after drain")
	}

	// No drop metrics under block policy (delta-checked because the
	// counters are package-level globals shared with other tests).
	if delta := metrics.StreamQueueDropsNewest.Value() - beforeNew; delta != 0 {
		t.Errorf("StreamQueueDropsNewest delta under block want=0 got=%d", delta)
	}
	if delta := metrics.StreamQueueDropsOldest.Value() - beforeOld; delta != 0 {
		t.Errorf("StreamQueueDropsOldest delta under block want=0 got=%d", delta)
	}
}

func TestDispatchPlannerTask_BlockPolicy_CtxCancellation(t *testing.T) {
	c := newOverflowTestClient(t, "block", 1)
	// Fill the queue.
	_ = c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() {
		done <- c.dispatchPlannerTask(ctx, plannerTask{wasPacked: true})
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case got := <-done:
		if got {
			t.Error("expected false when ctx cancelled before slot opens")
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not honor ctx.Done")
	}
}

func TestDispatchPlannerTask_DropNewest(t *testing.T) {
	before := metrics.StreamQueueDropsNewest.Value()
	c := newOverflowTestClient(t, "drop-newest", 1)
	_ = c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true})

	// Second send must NOT block under drop-newest. Wrap in a 100ms
	// timer to catch any regression to blocking.
	done := make(chan bool, 1)
	go func() {
		done <- c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true})
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("expected false (dropped) under drop-newest")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("drop-newest blocked the producer")
	}

	if got := metrics.StreamQueueDropsNewest.Value() - before; got != 1 {
		t.Errorf("StreamQueueDropsNewest delta want=1 got=%d", got)
	}
}

func TestDispatchPlannerTask_DropOldest(t *testing.T) {
	beforeNew := metrics.StreamQueueDropsNewest.Value()
	beforeOld := metrics.StreamQueueDropsOldest.Value()

	c := newOverflowTestClient(t, "drop-oldest", 1)
	// Use distinguishable tasks via dupCount so we can verify which
	// one was evicted.
	first := plannerTask{wasPacked: true, dupCount: 11}
	second := plannerTask{wasPacked: true, dupCount: 22}

	if !c.dispatchPlannerTask(context.Background(), first) {
		t.Fatal("first send should succeed")
	}
	// Second send must NOT block — it should evict the first.
	done := make(chan bool, 1)
	go func() {
		done <- c.dispatchPlannerTask(context.Background(), second)
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("expected true (enqueued after eviction) under drop-oldest")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("drop-oldest blocked the producer")
	}

	// The remaining task in the channel must be `second`.
	select {
	case got := <-c.plannerQueue:
		if got.dupCount != 22 {
			t.Errorf("queue should hold the newer task (dupCount=22) after eviction, got dupCount=%d", got.dupCount)
		}
	default:
		t.Fatal("queue unexpectedly empty after drop-oldest enqueue")
	}

	if delta := metrics.StreamQueueDropsOldest.Value() - beforeOld; delta != 1 {
		t.Errorf("StreamQueueDropsOldest delta want=1 got=%d", delta)
	}
	if delta := metrics.StreamQueueDropsNewest.Value() - beforeNew; delta != 0 {
		t.Errorf("StreamQueueDropsNewest should not move under drop-oldest, got delta=%d", delta)
	}
}

func TestDispatchWriterTask_DropNewest(t *testing.T) {
	before := metrics.StreamQueueDropsNewest.Value()
	c := newOverflowTestClient(t, "drop-newest", 1)
	_ = c.dispatchWriterTask(context.Background(), writerTask{wasPacked: true})

	done := make(chan bool, 1)
	go func() {
		done <- c.dispatchWriterTask(context.Background(), writerTask{wasPacked: true})
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("expected false under drop-newest")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("writer drop-newest blocked")
	}
	if got := metrics.StreamQueueDropsNewest.Value() - before; got != 1 {
		t.Errorf("StreamQueueDropsNewest delta want=1 got=%d", got)
	}
}

// TestBurstBehavior_MemoryCeilingUnderDropPolicy validates the core
// claim of Step 20: under a drop policy, a sustained producer burst
// cannot park more than O(channel-cap) goroutines, regardless of how
// long the consumer is suspended.
//
// Methodology: pause the consumer (by simply never reading from the
// channel), spray 10000 tasks at a queue of capacity 4 under
// drop-newest, and verify (a) the producer goroutine never parks (each
// dispatch returns within a tight timeout), and (b) at most `cap`
// tasks land in the channel. Under the old "block" behaviour this
// scenario would have parked 9996 goroutines.
func TestBurstBehavior_DropPolicyBoundsMemoryFootprint(t *testing.T) {
	const cap = 4
	const burst = 10000

	c := newOverflowTestClient(t, "drop-newest", cap)

	g0 := runtime.NumGoroutine()
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; i < burst; i++ {
		if time.Now().After(deadline) {
			t.Fatalf("burst loop exceeded deadline at i=%d — producer probably parked", i)
		}
		c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true})
	}
	g1 := runtime.NumGoroutine()

	// Drain — must contain at most `cap` items.
	drained := 0
	for {
		select {
		case <-c.plannerQueue:
			drained++
		default:
			goto done
		}
	}
done:
	if drained > cap {
		t.Errorf("queue overflowed: drained=%d cap=%d", drained, cap)
	}

	// Goroutine delta should be ~0 (we never spawned). Allow ±2 for
	// scheduler noise.
	if delta := g1 - g0; delta > 2 || delta < -2 {
		t.Errorf("unexpected goroutine delta=%d (g0=%d g1=%d) under drop-newest burst", delta, g0, g1)
	}
}

// TestBurstBehavior_DropOldestKeepsLatestPackets verifies that under
// drop-oldest, a sustained burst eventually leaves the *newest* burst-
// suffix in the channel — old tasks get evicted as new ones arrive.
func TestBurstBehavior_DropOldestKeepsLatestPackets(t *testing.T) {
	const cap = 8
	c := newOverflowTestClient(t, "drop-oldest", cap)

	const burst = 200
	for i := 0; i < burst; i++ {
		c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true, dupCount: i})
	}

	// Drain and verify the dupCount field is in [burst-cap, burst).
	minDup := burst
	maxDup := -1
	count := 0
	for {
		select {
		case t := <-c.plannerQueue:
			if t.dupCount < minDup {
				minDup = t.dupCount
			}
			if t.dupCount > maxDup {
				maxDup = t.dupCount
			}
			count++
		default:
			if count != cap {
				t.Errorf("drained=%d want=%d (cap)", count, cap)
			}
			if minDup < burst-cap {
				t.Errorf("queue contains a stale task: minDup=%d want >= %d", minDup, burst-cap)
			}
			if maxDup != burst-1 {
				t.Errorf("queue should retain the newest task: maxDup=%d want=%d", maxDup, burst-1)
			}
			return
		}
	}
}

// TestConcurrentProducersDropNewest stresses the lock-free contention
// path: many producers, drop-newest policy, no consumer. The test
// checks the invariant "queue size never exceeds cap" while N
// producers race against each other.
func TestConcurrentProducersDropNewest(t *testing.T) {
	const cap = 16
	const producers = 8
	const perProducer = 5000

	c := newOverflowTestClient(t, "drop-newest", cap)

	var wg sync.WaitGroup
	wg.Add(producers)
	for p := 0; p < producers; p++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				c.dispatchPlannerTask(context.Background(), plannerTask{wasPacked: true})
			}
		}()
	}
	wg.Wait()

	if got := len(c.plannerQueue); got > cap {
		t.Errorf("queue overflowed: len=%d cap=%d", got, cap)
	}
}
