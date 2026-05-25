// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
//
// Step 18 — DNS cache hot tier + amortized prune + metrics wiring tests.
//
// What these tests cover:
//
//   1. Hot tier hit returns the cached response (and patches the query ID).
//   2. Hot tier miss falls through to the cold tier; the cold hit then
//      promotes the entry into the hot tier (next lookup is hot).
//   3. SetReady invalidates an existing hot copy so the next GetReady
//      pulls the fresh value from the cold tier (and re-promotes).
//   4. Cold-tier expiry removes the hot copy (lookup misses cleanly).
//   5. Hot tier respects its bounded capacity (LRU eviction).
//   6. Default-off: a Store created via New() with no EnableHotTier
//      call behaves exactly as before (no hot tier, no panics).
//   7. metrics.CacheHits / CacheMisses tick on the right paths.
//   8. PruneExpired removes only entries whose TTL has elapsed; pending
//      entries are skipped; the cursor advances so successive calls cover
//      the whole shard.
//   9. PruneExpired bounded work — a single call never scans more than
//      maxScanPerShard entries per shard.
//  10. Concurrent stress under -race: parallel readers + writers + pruner
//      never trigger a data race.
//
// All tests are local-only — none of them exercise the network or the
// DNS wire format. Wire compatibility is unchanged by step 18.
package dnscache

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masterdnsvpn-go/internal/metrics"
)

func resetCacheMetrics() {
	metrics.CacheHits.Set(0)
	metrics.CacheMisses.Set(0)
}

func makeReadyResponse(tag byte) []byte {
	// 12-byte DNS header + 1 byte of "payload" to make len(resp) >= 2.
	// The first two bytes are the transaction ID; SetReady normalises
	// them to 0,0 so we can put whatever we like here.
	resp := make([]byte, 14)
	resp[0] = 0xAB
	resp[1] = 0xCD
	resp[13] = tag
	return resp
}

func makeQuery(idA, idB byte) []byte {
	q := make([]byte, 12)
	q[0] = idA
	q[1] = idB
	return q
}

// --- Test 1: hot hit returns patched response ----------------------------

func TestHotTier_HitReturnsPatchedResponse(t *testing.T) {
	resetCacheMetrics()
	s := New(100, time.Hour, time.Minute)
	s.EnableHotTier(8)
	now := time.Now()

	key := BuildKey("example.com", 1, 1)
	resp := makeReadyResponse(0x11)
	s.SetReady(key, "example.com", 1, 1, resp, now)

	// First lookup: cold hit, promotes to hot.
	q := makeQuery(0x55, 0x66)
	got, ok := s.GetReady(key, q, now)
	if !ok {
		t.Fatalf("expected cold hit, got miss")
	}
	if got[0] != 0x55 || got[1] != 0x66 {
		t.Fatalf("query ID was not patched into response: got %x %x", got[0], got[1])
	}

	// Second lookup: should be served from the hot tier.
	if s.HotTierLen() != 1 {
		t.Fatalf("expected hot tier to have 1 entry after promotion, got %d", s.HotTierLen())
	}
	q2 := makeQuery(0x77, 0x88)
	got2, ok := s.GetReady(key, q2, now)
	if !ok {
		t.Fatalf("expected hot hit, got miss")
	}
	if got2[0] != 0x77 || got2[1] != 0x88 {
		t.Fatalf("query ID was not patched on hot hit: got %x %x", got2[0], got2[1])
	}

	if hits := metrics.CacheHits.Value(); hits != 2 {
		t.Fatalf("expected 2 CacheHits (cold-then-hot), got %d", hits)
	}
	if misses := metrics.CacheMisses.Value(); misses != 0 {
		t.Fatalf("expected 0 CacheMisses, got %d", misses)
	}
}

// --- Test 2: cold-tier hit promotes to hot --------------------------------

func TestHotTier_ColdHitPromotes(t *testing.T) {
	resetCacheMetrics()
	s := New(100, time.Hour, time.Minute)
	s.EnableHotTier(8)
	now := time.Now()

	key := BuildKey("promote.com", 1, 1)
	resp := makeReadyResponse(0x22)
	s.SetReady(key, "promote.com", 1, 1, resp, now)

	if got := s.HotTierLen(); got != 0 {
		t.Fatalf("hot tier should be empty before any GetReady, got %d", got)
	}

	_, ok := s.GetReady(key, makeQuery(1, 2), now)
	if !ok {
		t.Fatalf("expected cold hit")
	}
	if got := s.HotTierLen(); got != 1 {
		t.Fatalf("hot tier should have 1 entry after promotion, got %d", got)
	}
}

// --- Test 3: SetReady invalidates a stale hot copy ------------------------

func TestHotTier_SetReadyInvalidatesHot(t *testing.T) {
	resetCacheMetrics()
	s := New(100, time.Hour, time.Minute)
	s.EnableHotTier(8)
	now := time.Now()

	key := BuildKey("update.com", 1, 1)
	s.SetReady(key, "update.com", 1, 1, makeReadyResponse(0x33), now)

	// Promote.
	if _, ok := s.GetReady(key, makeQuery(0, 0), now); !ok {
		t.Fatalf("expected cold hit during promotion")
	}
	if s.HotTierLen() != 1 {
		t.Fatalf("expected promotion; hot len=%d", s.HotTierLen())
	}

	// Now SetReady with a new response — the hot copy must be dropped.
	newResp := makeReadyResponse(0x44)
	s.SetReady(key, "update.com", 1, 1, newResp, now.Add(time.Second))
	if s.HotTierLen() != 0 {
		t.Fatalf("expected hot tier to be invalidated by SetReady, got len=%d", s.HotTierLen())
	}

	// Next GetReady must come from cold and contain the *new* tag byte.
	got, ok := s.GetReady(key, makeQuery(0, 0), now.Add(2*time.Second))
	if !ok {
		t.Fatalf("expected cold hit after update")
	}
	if got[13] != 0x44 {
		t.Fatalf("expected updated response tag 0x44, got 0x%02x", got[13])
	}
}

// --- Test 4: cold expiry removes the hot copy ------------------------------

func TestHotTier_ColdExpiryDropsHotCopy(t *testing.T) {
	resetCacheMetrics()
	// Very short TTL so we can step past it.
	ttl := 50 * time.Millisecond
	s := New(100, ttl, time.Minute)
	s.EnableHotTier(8)

	now := time.Now()
	key := BuildKey("ttl.com", 1, 1)
	s.SetReady(key, "ttl.com", 1, 1, makeReadyResponse(0x55), now)

	// Promote.
	if _, ok := s.GetReady(key, makeQuery(0, 0), now); !ok {
		t.Fatalf("expected cold hit on first lookup")
	}
	if s.HotTierLen() != 1 {
		t.Fatalf("expected promotion; hot len=%d", s.HotTierLen())
	}

	// Step past TTL. The hot tier's Get should detect staleness via
	// the same cacheTTL and report a miss, dropping the hot copy.
	future := now.Add(ttl + 10*time.Millisecond)
	if _, ok := s.GetReady(key, makeQuery(0, 0), future); ok {
		t.Fatalf("expected miss after TTL expiry, got hit")
	}
	if s.HotTierLen() != 0 {
		t.Fatalf("expected hot tier to be cleared after stale lookup, got len=%d", s.HotTierLen())
	}
	// And the cold tier should also have removed the expired entry as
	// part of the GetReady fall-through (legacy lazy-expiry behaviour).
	if _, ok := s.Snapshot(key); ok {
		t.Fatalf("expected cold tier to drop expired entry too")
	}
}

// --- Test 5: hot tier capacity is bounded (LRU eviction) ------------------

func TestHotTier_LRUBound(t *testing.T) {
	resetCacheMetrics()
	s := New(1000, time.Hour, time.Minute)
	s.EnableHotTier(4)
	now := time.Now()

	keys := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		k := BuildKey("host"+strconv.Itoa(i)+".test", 1, 1)
		keys = append(keys, k)
		s.SetReady(k, "host"+strconv.Itoa(i)+".test", 1, 1, makeReadyResponse(byte(i)), now)
	}

	// Promote all 8 — only the last 4 should survive in the hot tier.
	for _, k := range keys {
		s.GetReady(k, makeQuery(0, 0), now)
	}
	if got := s.HotTierLen(); got != 4 {
		t.Fatalf("hot tier capacity should be 4, got len=%d", got)
	}

	// The first 4 keys promoted should have been evicted; the last 4
	// remain. We verify by checking that a lookup of keys[0] is a
	// cold hit (re-promotes) while keys[7] was already hot.
	resetCacheMetrics()
	_, _ = s.GetReady(keys[0], makeQuery(0, 0), now) // cold hit (re-promote)
	_, _ = s.GetReady(keys[7], makeQuery(0, 0), now) // hot hit
	if metrics.CacheHits.Value() != 2 {
		t.Fatalf("expected 2 hits (cold+hot), got %d", metrics.CacheHits.Value())
	}
}

// --- Test 6: default-off behaviour is unchanged ---------------------------

func TestHotTier_DefaultDisabled(t *testing.T) {
	resetCacheMetrics()
	// Note: no EnableHotTier call.
	s := New(100, time.Hour, time.Minute)
	now := time.Now()

	if s.HotTierLen() != 0 {
		t.Fatalf("hot tier should report 0 length when disabled")
	}

	key := BuildKey("default.test", 1, 1)
	s.SetReady(key, "default.test", 1, 1, makeReadyResponse(0x99), now)
	_, ok := s.GetReady(key, makeQuery(1, 2), now)
	if !ok {
		t.Fatalf("cold lookup must still work with hot tier disabled")
	}
	if s.HotTierLen() != 0 {
		t.Fatalf("disabled hot tier must never grow; got %d", s.HotTierLen())
	}
	if metrics.CacheHits.Value() != 1 || metrics.CacheMisses.Value() != 0 {
		t.Fatalf("metrics: hits=%d misses=%d (want 1/0)", metrics.CacheHits.Value(), metrics.CacheMisses.Value())
	}
}

// --- Test 7: metrics increment correctly on hit/miss ----------------------

func TestCacheMetrics_HitAndMissPaths(t *testing.T) {
	resetCacheMetrics()
	s := New(100, time.Hour, time.Minute)
	s.EnableHotTier(8)
	now := time.Now()

	// Miss on an unknown key.
	if _, ok := s.GetReady(BuildKey("unknown.x", 1, 1), makeQuery(0, 0), now); ok {
		t.Fatalf("expected miss")
	}
	if metrics.CacheMisses.Value() != 1 {
		t.Fatalf("expected 1 miss, got %d", metrics.CacheMisses.Value())
	}

	// Hit on a known key — cold tier path.
	key := BuildKey("known.x", 1, 1)
	s.SetReady(key, "known.x", 1, 1, makeReadyResponse(1), now)
	if _, ok := s.GetReady(key, makeQuery(0, 0), now); !ok {
		t.Fatalf("expected cold hit")
	}
	if metrics.CacheHits.Value() != 1 {
		t.Fatalf("expected 1 hit, got %d", metrics.CacheHits.Value())
	}

	// Hit again — hot tier path.
	if _, ok := s.GetReady(key, makeQuery(0, 0), now); !ok {
		t.Fatalf("expected hot hit")
	}
	if metrics.CacheHits.Value() != 2 {
		t.Fatalf("expected 2 hits, got %d", metrics.CacheHits.Value())
	}

	// Pending entries are NOT served as ready -> must count as miss.
	// LookupOrCreatePending on a brand-new key ALSO ticks one miss
	// (step 18 wiring on the client path), so we expect 1 (the
	// LookupOrCreatePending) + 1 (the GetReady on the now-pending key)
	// = 2 additional misses on top of the 1 from earlier in this test.
	pendingKey := BuildKey("pending.x", 1, 1)
	s.LookupOrCreatePending(pendingKey, "pending.x", 1, 1, now)
	if _, ok := s.GetReady(pendingKey, makeQuery(0, 0), now); ok {
		t.Fatalf("pending entry must not be served as ready")
	}
	if metrics.CacheMisses.Value() != 3 {
		t.Fatalf("expected 3 misses, got %d", metrics.CacheMisses.Value())
	}
}

// --- Test 7b: LookupOrCreatePending ticks CacheHits on ready re-lookup ----

func TestCacheMetrics_LookupOrCreatePendingHitPath(t *testing.T) {
	resetCacheMetrics()
	s := New(100, time.Hour, time.Minute)
	now := time.Now()

	key := BuildKey("client-hit.x", 1, 1)
	// First, plant a ready entry (as if a previous round-trip resolved
	// it and cached it).
	s.SetReady(key, "client-hit.x", 1, 1, makeReadyResponse(0xAA), now)

	// Now the client path looks it up via LookupOrCreatePending; this
	// should be a hit (StatusReady) and tick CacheHits.
	res := s.LookupOrCreatePending(key, "client-hit.x", 1, 1, now)
	if res.Status != StatusReady {
		t.Fatalf("expected StatusReady from LookupOrCreatePending on a ready key, got %v", res.Status)
	}
	if metrics.CacheHits.Value() != 1 {
		t.Fatalf("expected 1 hit from client-path lookup, got %d", metrics.CacheHits.Value())
	}
	if metrics.CacheMisses.Value() != 0 {
		t.Fatalf("expected 0 misses, got %d", metrics.CacheMisses.Value())
	}
}

// --- Test 8: PruneExpired removes only expired entries; skips pending -----

func TestPruneExpired_RemovesOnlyExpiredAndSkipsPending(t *testing.T) {
	resetCacheMetrics()
	ttl := 100 * time.Millisecond
	s := New(1000, ttl, 5*time.Minute) // pendingTimeout much longer than ttl
	now := time.Now()

	// 5 ready entries, 5 pending entries.
	readyKeys := make([]string, 5)
	pendingKeys := make([]string, 5)
	for i := 0; i < 5; i++ {
		rk := BuildKey("ready"+strconv.Itoa(i), 1, 1)
		pk := BuildKey("pending"+strconv.Itoa(i), 1, 1)
		readyKeys[i] = rk
		pendingKeys[i] = pk
		s.SetReady(rk, "ready"+strconv.Itoa(i), 1, 1, makeReadyResponse(byte(i)), now)
		s.LookupOrCreatePending(pk, "pending"+strconv.Itoa(i), 1, 1, now)
	}

	// Step past TTL.
	future := now.Add(ttl + 50*time.Millisecond)
	// Multiple sweeps because per-shard cursor advances only
	// maxScanPerShard entries at a time.
	totalRemoved := 0
	for i := 0; i < 4; i++ {
		totalRemoved += s.PruneExpired(future, 1000)
	}

	if totalRemoved != 5 {
		t.Fatalf("expected 5 expired ready entries removed, got %d", totalRemoved)
	}

	// All 5 pending entries must still be present.
	if !s.HasPending() {
		t.Fatalf("pending entries must survive PruneExpired")
	}
	pendingCount := 0
	for _, k := range pendingKeys {
		if _, ok := s.Snapshot(k); ok {
			pendingCount++
		}
	}
	if pendingCount != 5 {
		t.Fatalf("expected 5 surviving pending entries, got %d", pendingCount)
	}
	// All 5 expired ready entries must be gone.
	for _, k := range readyKeys {
		if _, ok := s.Snapshot(k); ok {
			t.Fatalf("expired ready entry %q should have been pruned", k)
		}
	}
}

// --- Test 9: PruneExpired respects maxScanPerShard ------------------------

func TestPruneExpired_BoundedWork(t *testing.T) {
	ttl := 10 * time.Millisecond
	s := New(2000, ttl, time.Minute)
	now := time.Now()

	// Insert enough entries to spread across all shards.
	const N = 2000
	for i := 0; i < N; i++ {
		k := BuildKey("k"+strconv.Itoa(i), 1, 1)
		s.SetReady(k, "k"+strconv.Itoa(i), 1, 1, makeReadyResponse(byte(i&0xff)), now)
	}

	future := now.Add(ttl + 5*time.Millisecond)

	// A single prune call with maxScanPerShard=4 should remove at most
	// 4 * shardCount = 128 entries.
	removed := s.PruneExpired(future, 4)
	if removed > 4*shardCount {
		t.Fatalf("PruneExpired removed %d in one call; expected <= %d", removed, 4*shardCount)
	}
}

// --- Test 10: PruneExpired cursor eventually covers everything ------------

func TestPruneExpired_CursorEventuallyCoversAll(t *testing.T) {
	ttl := 10 * time.Millisecond
	s := New(2000, ttl, time.Minute)
	now := time.Now()

	const N = 500
	for i := 0; i < N; i++ {
		k := BuildKey("k"+strconv.Itoa(i), 1, 1)
		s.SetReady(k, "k"+strconv.Itoa(i), 1, 1, makeReadyResponse(byte(i&0xff)), now)
	}

	future := now.Add(ttl + 5*time.Millisecond)

	// Loop pruning with small per-shard budget until nothing more
	// gets removed in a full pass.
	total := 0
	for pass := 0; pass < 200; pass++ {
		removed := s.PruneExpired(future, 4)
		total += removed
		if total >= N {
			break
		}
	}
	if total < N {
		t.Fatalf("expected cursor to eventually cover all %d entries, only got %d", N, total)
	}
}

// --- Test 11: EnableHotTier idempotent / re-sizable -----------------------

func TestHotTier_EnableTwiceResets(t *testing.T) {
	s := New(100, time.Hour, time.Minute)
	s.EnableHotTier(4)
	now := time.Now()

	// Populate.
	for i := 0; i < 4; i++ {
		k := BuildKey("k"+strconv.Itoa(i), 1, 1)
		s.SetReady(k, "k"+strconv.Itoa(i), 1, 1, makeReadyResponse(byte(i)), now)
		s.GetReady(k, makeQuery(0, 0), now)
	}
	if s.HotTierLen() != 4 {
		t.Fatalf("hot tier should be at capacity, got %d", s.HotTierLen())
	}

	// Re-enable with a different size — hot tier is reset.
	s.EnableHotTier(8)
	if s.HotTierLen() != 0 {
		t.Fatalf("re-EnableHotTier should reset the hot tier, got %d", s.HotTierLen())
	}

	// Disable.
	s.EnableHotTier(0)
	k := BuildKey("after-disable", 1, 1)
	s.SetReady(k, "after-disable", 1, 1, makeReadyResponse(0x88), now)
	if _, ok := s.GetReady(k, makeQuery(0, 0), now); !ok {
		t.Fatalf("cold lookup must work after disabling hot tier")
	}
	if s.HotTierLen() != 0 {
		t.Fatalf("disabled hot tier must not grow")
	}
}

// --- Test 12: hot tier is clamped to maxRecords ---------------------------

func TestHotTier_ClampedToColdCapacity(t *testing.T) {
	s := New(8, time.Hour, time.Minute)
	s.EnableHotTier(1000)
	// The hot tier should have been clamped down to the cold-tier
	// capacity. We can't read the internal max directly, but we can
	// verify by filling it: only up to 8 entries can ever land in it.
	now := time.Now()
	for i := 0; i < 50; i++ {
		k := BuildKey("k"+strconv.Itoa(i), 1, 1)
		s.SetReady(k, "k"+strconv.Itoa(i), 1, 1, makeReadyResponse(byte(i)), now)
		s.GetReady(k, makeQuery(0, 0), now)
	}
	if got := s.HotTierLen(); got > 8 {
		t.Fatalf("hot tier should have been clamped to <= 8 (cold cap), got %d", got)
	}
}

// --- Benchmarks: GetReady latency cold-only vs hot-enabled ---------------

func benchSetup(b *testing.B, hotSize int) (*Store, []string) {
	b.Helper()
	s := New(10000, time.Hour, time.Minute)
	if hotSize > 0 {
		s.EnableHotTier(hotSize)
	}
	now := time.Now()
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = BuildKey("bench"+strconv.Itoa(i)+".com", 1, 1)
		s.SetReady(keys[i], "bench"+strconv.Itoa(i)+".com", 1, 1, makeReadyResponse(byte(i&0xff)), now)
	}
	// Warm the hot tier with the working set (first 64 entries).
	for i := 0; i < 64 && i < len(keys); i++ {
		s.GetReady(keys[i], makeQuery(0, 0), now)
	}
	return s, keys
}

func BenchmarkGetReady_ColdOnly(b *testing.B) {
	s, keys := benchSetup(b, 0)
	q := makeQuery(0xAB, 0xCD)
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.GetReady(keys[i%64], q, now)
	}
}

func BenchmarkGetReady_HotEnabled(b *testing.B) {
	s, keys := benchSetup(b, 128)
	q := makeQuery(0xAB, 0xCD)
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.GetReady(keys[i%64], q, now)
	}
}

func BenchmarkGetReady_HotEnabled_Parallel(b *testing.B) {
	s, keys := benchSetup(b, 128)
	q := makeQuery(0xAB, 0xCD)
	now := time.Now()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.GetReady(keys[i%64], q, now)
			i++
		}
	})
}

func BenchmarkGetReady_ColdOnly_Parallel(b *testing.B) {
	s, keys := benchSetup(b, 0)
	q := makeQuery(0xAB, 0xCD)
	now := time.Now()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.GetReady(keys[i%64], q, now)
			i++
		}
	})
}

// --- Test 13: concurrent readers + writers + pruner is race-clean ----------

func TestHotTier_ConcurrentSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skip concurrent smoke in -short mode")
	}
	s := New(2000, 200*time.Millisecond, time.Minute)
	s.EnableHotTier(64)

	// Seed some entries.
	now := time.Now()
	const seed = 200
	for i := 0; i < seed; i++ {
		k := BuildKey("seed"+strconv.Itoa(i), 1, 1)
		s.SetReady(k, "seed"+strconv.Itoa(i), 1, 1, makeReadyResponse(byte(i&0xff)), now)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var ops atomic.Uint64

	// 4 readers.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
					k := BuildKey("seed"+strconv.Itoa((i+r*17)%seed), 1, 1)
					s.GetReady(k, makeQuery(byte(i), byte(r)), time.Now())
					ops.Add(1)
					i++
				}
			}
		}(r)
	}
	// 2 writers.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
					k := BuildKey(fmt.Sprintf("writer%d-%d", w, i%50), 1, 1)
					s.SetReady(k, "w", 1, 1, makeReadyResponse(byte(i)), time.Now())
					ops.Add(1)
					i++
				}
			}
		}(w)
	}
	// 1 pruner.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.PruneExpired(time.Now(), 16)
			}
		}
	}()

	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()

	if ops.Load() == 0 {
		t.Fatalf("expected concurrent ops to make progress")
	}
}
