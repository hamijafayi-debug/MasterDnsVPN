package client

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestBalancerLeastLossFallsBackToRoundRobinWithoutStats(t *testing.T) {
	b := NewBalancer(BalancingLeastLoss, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
		{Key: "c", IsValid: true},
	}
	b.SetConnections(connections)
	_ = b.SetConnectionValidity("a", true)
	_ = b.SetConnectionValidity("b", true)
	_ = b.SetConnectionValidity("c", true)

	first, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected first connection")
	}
	second, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected second connection")
	}
	third, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected third connection")
	}

	if first.Key != "a" || second.Key != "b" || third.Key != "c" {
		t.Fatalf("expected round-robin a,b,c before stats, got %q,%q,%q", first.Key, second.Key, third.Key)
	}
}

func TestBalancerLowestLatencyUsesRuntimeStats(t *testing.T) {
	b := NewBalancer(BalancingLowestLatency, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
	}
	b.SetConnections(connections)
	_ = b.SetConnectionValidity("a", true)
	_ = b.SetConnectionValidity("b", true)

	for i := 0; i < 6; i++ {
		b.ReportSend("a")
		b.ReportSuccess("a", 8*time.Millisecond)
		b.ReportSend("b")
		b.ReportSuccess("b", 2*time.Millisecond)
	}

	best, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected best connection")
	}
	if best.Key != "b" {
		t.Fatalf("expected lower-latency resolver b, got %q", best.Key)
	}
}

func TestBalancerHybridPrefersLowerLossWhenLatencyIsClose(t *testing.T) {
	b := NewBalancer(BalancingHybridScore, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
	}
	b.SetConnections(connections)
	_ = b.SetConnectionValidity("a", true)
	_ = b.SetConnectionValidity("b", true)

	for i := 0; i < 10; i++ {
		b.ReportSend("a")
		b.ReportSuccess("a", 12*time.Millisecond)
		b.ReportSend("b")
		b.ReportSuccess("b", 8*time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		b.ReportSend("a")
		b.ReportTimeout("a", time.Now(), 10*time.Second, 1)
	}

	best, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected best connection")
	}
	if best.Key != "b" {
		t.Fatalf("expected hybrid mode to prefer lower-loss resolver b, got %q", best.Key)
	}
}

func TestBalancerHybridPrefersLowerLatencyWhenLossIsEqual(t *testing.T) {
	b := NewBalancer(BalancingHybridScore, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
	}
	b.SetConnections(connections)
	_ = b.SetConnectionValidity("a", true)
	_ = b.SetConnectionValidity("b", true)

	for i := 0; i < 6; i++ {
		b.ReportSend("a")
		b.ReportSuccess("a", 12*time.Millisecond)
		b.ReportSend("b")
		b.ReportSuccess("b", 3*time.Millisecond)
	}

	best, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected best connection")
	}
	if best.Key != "b" {
		t.Fatalf("expected hybrid mode to prefer lower-latency resolver b when loss is equal, got %q", best.Key)
	}
}

func TestBalancerHybridFallsBackToRoundRobinWithoutStats(t *testing.T) {
	b := NewBalancer(BalancingHybridScore, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
		{Key: "c", IsValid: true},
	}
	b.SetConnections(connections)
	_ = b.SetConnectionValidity("a", true)
	_ = b.SetConnectionValidity("b", true)
	_ = b.SetConnectionValidity("c", true)

	first, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected first connection")
	}
	second, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected second connection")
	}
	third, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected third connection")
	}

	if first.Key != "a" || second.Key != "b" || third.Key != "c" {
		t.Fatalf("expected round-robin a,b,c before hybrid stats, got %q,%q,%q", first.Key, second.Key, third.Key)
	}
}

func TestBalancerLossThenLatencyPrefersLowerLossFirst(t *testing.T) {
	b := NewBalancer(BalancingLossThenLatency, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
	}
	b.SetConnections(connections)
	_ = b.SetConnectionValidity("a", true)
	_ = b.SetConnectionValidity("b", true)

	for i := 0; i < 10; i++ {
		b.ReportSend("a")
		b.ReportSuccess("a", 4*time.Millisecond)
		b.ReportSend("b")
		b.ReportSuccess("b", 10*time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		b.ReportSend("a")
		b.ReportTimeout("a", time.Now(), 10*time.Second, 1)
	}

	best, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected best connection")
	}
	if best.Key != "b" {
		t.Fatalf("expected loss-then-latency mode to prefer lower-loss resolver b, got %q", best.Key)
	}
}

func TestBalancerLossThenLatencyUsesLatencyInsideLossTier(t *testing.T) {
	b := NewBalancer(BalancingLossThenLatency, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
	}
	b.SetConnections(connections)
	_ = b.SetConnectionValidity("a", true)
	_ = b.SetConnectionValidity("b", true)

	for i := 0; i < 8; i++ {
		b.ReportSend("a")
		b.ReportSuccess("a", 15*time.Millisecond)
		b.ReportSend("b")
		b.ReportSuccess("b", 4*time.Millisecond)
	}

	best, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected best connection")
	}
	if best.Key != "b" {
		t.Fatalf("expected lower-latency resolver b inside equal-loss tier, got %q", best.Key)
	}
}

func TestBalancerLossThenLatencyRoundRobinsAcrossNearTopCandidates(t *testing.T) {
	b := NewBalancer(BalancingLossThenLatency, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
	}
	b.SetConnections(connections)
	_ = b.SetConnectionValidity("a", true)
	_ = b.SetConnectionValidity("b", true)

	for i := 0; i < 8; i++ {
		b.ReportSend("a")
		b.ReportSuccess("a", 10*time.Millisecond)
		b.ReportSend("b")
		b.ReportSuccess("b", 12*time.Millisecond)
	}

	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		best, ok := b.GetBestConnection()
		if !ok {
			t.Fatal("expected best connection")
		}
		seen[best.Key] = true
	}

	if !seen["a"] || !seen["b"] {
		t.Fatalf("expected round-robin across near-top candidates, seen=%v", seen)
	}
}

func TestBalancerLeastLossTopRandomFallsBackToRoundRobinWithoutStats(t *testing.T) {
	b := NewBalancer(BalancingLeastLossTopRandom, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
		{Key: "c", IsValid: true},
	}
	b.SetConnections(connections)
	_ = b.SetConnectionValidity("a", true)
	_ = b.SetConnectionValidity("b", true)
	_ = b.SetConnectionValidity("c", true)

	first, _ := b.GetBestConnection()
	second, _ := b.GetBestConnection()
	third, _ := b.GetBestConnection()
	if first.Key != "a" || second.Key != "b" || third.Key != "c" {
		t.Fatalf("expected round-robin a,b,c before loss-top-random stats, got %q,%q,%q", first.Key, second.Key, third.Key)
	}
}

func TestBalancerLeastLossTopRandomUsesTopLossTier(t *testing.T) {
	b := NewBalancer(BalancingLeastLossTopRandom, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
		{Key: "c", IsValid: true},
		{Key: "d", IsValid: true},
	}
	b.SetConnections(connections)
	for _, key := range []string{"a", "b", "c", "d"} {
		_ = b.SetConnectionValidity(key, true)
	}

	for i := 0; i < 10; i++ {
		for _, key := range []string{"a", "b", "c", "d"} {
			b.ReportSend(key)
			b.ReportSuccess(key, 5*time.Millisecond)
		}
	}
	for i := 0; i < 1; i++ {
		b.ReportSend("c")
		b.ReportTimeout("c", time.Now(), 10*time.Second, 1)
		b.ReportSend("d")
		b.ReportTimeout("d", time.Now(), 10*time.Second, 1)
	}

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		best, ok := b.GetBestConnection()
		if !ok {
			t.Fatal("expected best connection")
		}
		seen[best.Key] = true
		if best.Key == "c" || best.Key == "d" {
			t.Fatalf("expected picks only from lower-loss top tier, got %q", best.Key)
		}
	}

	if !seen["a"] || !seen["b"] {
		t.Fatalf("expected random selection among top loss tier, seen=%v", seen)
	}
}

func TestBalancerLeastLossTopRoundRobinUsesTopLossTier(t *testing.T) {
	b := NewBalancer(BalancingLeastLossTopRoundRobin, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
		{Key: "b", IsValid: true},
		{Key: "c", IsValid: true},
		{Key: "d", IsValid: true},
	}
	b.SetConnections(connections)
	for _, key := range []string{"a", "b", "c", "d"} {
		_ = b.SetConnectionValidity(key, true)
	}

	for i := 0; i < 10; i++ {
		for _, key := range []string{"a", "b", "c", "d"} {
			b.ReportSend(key)
			b.ReportSuccess(key, 5*time.Millisecond)
		}
	}
	for i := 0; i < 1; i++ {
		b.ReportSend("c")
		b.ReportTimeout("c", time.Now(), 10*time.Second, 1)
		b.ReportSend("d")
		b.ReportTimeout("d", time.Now(), 10*time.Second, 1)
	}

	first, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected best connection")
	}
	second, ok := b.GetBestConnection()
	if !ok {
		t.Fatal("expected best connection")
	}
	if (first.Key != "a" && first.Key != "b") || (second.Key != "a" && second.Key != "b") {
		t.Fatalf("expected picks only from lower-loss top tier, got %q then %q", first.Key, second.Key)
	}
	if first.Key == second.Key {
		t.Fatalf("expected round-robin across top loss tier, got %q then %q", first.Key, second.Key)
	}
}

func TestBalancerStatsHalfLifeAlsoAppliesOnSend(t *testing.T) {
	b := NewBalancer(BalancingLeastLoss, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
	}
	b.SetConnections(connections)

	for i := 0; i < connectionStatsHalfLifeThreshold+1; i++ {
		b.ReportSend("a")
	}

	stats := b.statsForKey("a")
	if stats == nil {
		t.Fatal("expected stats for resolver a")
	}

	sent, acked, _, sum, count := stats.snapshot()
	if sent != (connectionStatsHalfLifeThreshold+1)/2 {
		t.Fatalf("expected send-triggered half-life to bound sent, got sent=%d acked=%d sum=%d count=%d", sent, acked, sum, count)
	}
	if acked != 0 || sum != 0 || count != 0 {
		t.Fatalf("expected send-triggered half-life to preserve zero success stats, got acked=%d sum=%d count=%d", acked, sum, count)
	}
}

func TestBalancerStatsHalfLifePreservesRelativeSuccessSignal(t *testing.T) {
	b := NewBalancer(BalancingLeastLoss, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true},
	}
	b.SetConnections(connections)

	for i := 0; i < 800; i++ {
		b.ReportSend("a")
	}
	for i := 0; i < 400; i++ {
		b.ReportSuccess("a", 5*time.Millisecond)
	}
	for i := 0; i < 401; i++ {
		b.ReportSend("a")
	}

	stats := b.statsForKey("a")
	if stats == nil {
		t.Fatal("expected stats for resolver a")
	}

	sent, acked, _, sum, count := stats.snapshot()
	if sent != 700 || acked != 200 || count != 200 {
		t.Fatalf("expected balanced half-life after crossing threshold, got sent=%d acked=%d count=%d", sent, acked, count)
	}
	if sum != uint64(time.Millisecond/time.Microsecond)*5*200 {
		t.Fatalf("expected RTT signal to decay proportionally, got sum=%d", sum)
	}
}

func TestBalancerSetConnectionsCopiesSourceDomain(t *testing.T) {
	b := NewBalancer(BalancingRoundRobinDefault, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true, Domain: "a.example.com"},
	}
	b.SetConnections(connections)

	connections[0].Domain = "mutated.example.com"

	got, ok := b.GetConnectionByKey("a")
	if !ok {
		t.Fatal("expected resolver a in balancer snapshot")
	}
	if got.Domain != "a.example.com" {
		t.Fatalf("expected balancer to keep copied domain after source mutation, got %q", got.Domain)
	}
}

func TestBalancerSetConnectionValidityDoesNotPullSourceMutation(t *testing.T) {
	b := NewBalancer(BalancingRoundRobinDefault, nil)
	connections := []*Connection{
		{Key: "a", IsValid: false, UploadMTUBytes: 140, DownloadMTUBytes: 220},
	}
	b.SetConnections(connections)

	connections[0].UploadMTUBytes = 90
	connections[0].DownloadMTUBytes = 180

	if !b.SetConnectionValidity("a", true) {
		t.Fatal("expected SetConnectionValidity to succeed")
	}

	got, ok := b.GetConnectionByKey("a")
	if !ok {
		t.Fatal("expected resolver a in snapshot")
	}
	if !got.IsValid {
		t.Fatal("expected resolver a to become valid")
	}
	if got.UploadMTUBytes != 0 || got.DownloadMTUBytes != 0 {
		t.Fatalf("expected balancer state to stay independent from source mutation, got up=%d down=%d", got.UploadMTUBytes, got.DownloadMTUBytes)
	}
}

func TestBalancerSetConnectionMTUUpdatesBalancerOnly(t *testing.T) {
	b := NewBalancer(BalancingRoundRobinDefault, nil)
	connections := []*Connection{
		{Key: "a", IsValid: true, UploadMTUBytes: 120, UploadMTUChars: 180, DownloadMTUBytes: 220},
	}
	b.SetConnections(connections)

	if !b.SetConnectionMTU("a", 90, 135, 180) {
		t.Fatal("expected SetConnectionMTU to succeed")
	}

	if connections[0].UploadMTUBytes != 120 || connections[0].UploadMTUChars != 180 || connections[0].DownloadMTUBytes != 220 {
		t.Fatalf("expected source MTUs to remain unchanged, got up=%d chars=%d down=%d", connections[0].UploadMTUBytes, connections[0].UploadMTUChars, connections[0].DownloadMTUBytes)
	}

	got, ok := b.GetConnectionByKey("a")
	if !ok {
		t.Fatal("expected resolver a in snapshot")
	}
	if got.UploadMTUBytes != 90 || got.UploadMTUChars != 135 || got.DownloadMTUBytes != 180 {
		t.Fatalf("expected snapshot MTUs to update, got up=%d chars=%d down=%d", got.UploadMTUBytes, got.UploadMTUChars, got.DownloadMTUBytes)
	}
}

// --- Step 8: Balancer lock granularity & selection fast-path ---

// makeBalancerWithN sets up a balancer with N active resolvers named
// "r0".."r{N-1}". Used by Step 8 benchmarks and invariant tests.
func makeBalancerWithN(strategy int, n int) *Balancer {
	b := NewBalancer(strategy, nil)
	conns := make([]*Connection, n)
	for i := 0; i < n; i++ {
		conns[i] = &Connection{Key: "r" + strconv.Itoa(i), IsValid: true}
	}
	b.SetConnections(conns)
	for i := 0; i < n; i++ {
		_ = b.SetConnectionValidity(conns[i].Key, true)
	}
	return b
}

// TestBalancerStatsForKeySnapshotInvariant asserts that the lock-free
// statsForKey path returns the SAME *connectionStats pointer as the
// locked fallback for every resolver, and stays consistent under
// concurrent ReportSuccess calls. This protects the Step 8 snapshot
// optimisation from drifting from the canonical state.
func TestBalancerStatsForKeySnapshotInvariant(t *testing.T) {
	const n = 50
	b := makeBalancerWithN(BalancingHybridScore, n)

	for i := 0; i < n; i++ {
		key := "r" + strconv.Itoa(i)
		got := b.statsForKey(key)
		if got == nil {
			t.Fatalf("expected non-nil stats for %s", key)
		}
		// Compare against the locked view.
		b.mu.RLock()
		idx, ok := b.indexByKey[key]
		if !ok {
			b.mu.RUnlock()
			t.Fatalf("indexByKey missing %s", key)
		}
		expected := b.stats[idx]
		b.mu.RUnlock()
		if got != expected {
			t.Fatalf("snapshot drift for %s: got %p, locked %p", key, got, expected)
		}
	}

	// Hammer the snapshot path concurrently with reports.
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				key := "r" + strconv.Itoa((seed*7+i)%n)
				b.ReportSend(key)
				b.ReportSuccess(key, 5*time.Millisecond)
			}
		}(w)
	}
	wg.Wait()

	// Sanity-check that counters survived (sent should equal acked since
	// every send is followed by a success in the loop above).
	for i := 0; i < n; i++ {
		key := "r" + strconv.Itoa(i)
		st := b.statsForKey(key)
		if st == nil {
			t.Fatalf("post-load: nil stats for %s", key)
		}
		sent := st.sent.Load()
		acked := st.acked.Load()
		if sent == 0 || sent != acked {
			t.Fatalf("post-load: %s sent=%d acked=%d (want equal & non-zero)", key, sent, acked)
		}
	}
}

// TestBalancerStatsForKeyMissingKeyReturnsNil locks in the contract
// that a key absent from the snapshot returns nil (not a panic).
func TestBalancerStatsForKeyMissingKeyReturnsNil(t *testing.T) {
	b := makeBalancerWithN(BalancingHybridScore, 4)
	if got := b.statsForKey("nope"); got != nil {
		t.Fatalf("expected nil for absent key, got %p", got)
	}
}

// TestBalancerSetConnectionsRepublishesSnapshot verifies that calling
// SetConnections a second time atomically swaps the lookup snapshot:
// the new key resolves, the old key returns nil.
func TestBalancerSetConnectionsRepublishesSnapshot(t *testing.T) {
	b := NewBalancer(BalancingHybridScore, nil)
	b.SetConnections([]*Connection{{Key: "old", IsValid: true}})
	if b.statsForKey("old") == nil {
		t.Fatal("expected stats for old key on first publish")
	}
	b.SetConnections([]*Connection{{Key: "new", IsValid: true}})
	if b.statsForKey("old") != nil {
		t.Fatal("expected nil for old key after republish")
	}
	if b.statsForKey("new") == nil {
		t.Fatal("expected stats for new key after republish")
	}
}

// BenchmarkBalancerSelect_50 measures GetBestConnection throughput with
// 50 active resolvers under the default hybrid strategy. This is the
// canonical Step 8 selection fast-path bench.
func BenchmarkBalancerSelect_50(b *testing.B) {
	bal := makeBalancerWithN(BalancingHybridScore, 50)
	// Seed some scoring signal so the hybrid scorer is exercised
	// rather than the no-signal round-robin fallback.
	for i := 0; i < 50; i++ {
		key := "r" + strconv.Itoa(i)
		bal.ReportSend(key)
		bal.ReportSuccess(key, time.Duration(5+i)*time.Millisecond)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = bal.GetBestConnection()
	}
}

// BenchmarkBalancerStatsForKey_50 measures the lock-free hot-path
// lookup. This is what every per-packet ReportSend / ReportSuccess
// goes through.
func BenchmarkBalancerStatsForKey_50(b *testing.B) {
	bal := makeBalancerWithN(BalancingHybridScore, 50)
	keys := make([]string, 50)
	for i := range keys {
		keys[i] = "r" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = bal.statsForKey(keys[i%len(keys)])
	}
}

// BenchmarkBalancerReportSuccessParallel_50 stresses the hottest entry
// point under contention. Pre-Step-8 this took the RWMutex on every
// call; post-Step-8 it is lock-free and the parallel scaling should be
// near-linear.
func BenchmarkBalancerReportSuccessParallel_50(b *testing.B) {
	bal := makeBalancerWithN(BalancingHybridScore, 50)
	keys := make([]string, 50)
	for i := range keys {
		keys[i] = "r" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%len(keys)]
			bal.ReportSuccess(key, 7*time.Millisecond)
			i++
		}
	})
	// Touch fmt so we keep the import in case future tweaks need it.
	_ = fmt.Sprintf
}
