package udpserver

import (
	"sync/atomic"
	"testing"
	"time"
)

// Step 10 benchmarks: validate that the lock-free atomic.Pointer
// refactor of sessionStore.byID delivers concurrent read/write
// throughput. Because sessionID is uint8 (256 fixed slots), the
// store uses a 256-slot atomic.Pointer array instead of a hashed
// shard map; per-slot atomics give better cache locality and
// remove RLock contention entirely on the hot path.

func benchPopulateStore(b *testing.B, store *sessionStore, count int) {
	b.Helper()
	if count < 1 {
		count = 1
	}
	if count > maxServerSessionID {
		count = maxServerSessionID
	}
	now := time.Now()
	for i := 1; i <= count; i++ {
		id := uint8(i)
		rec := newTestSessionRecord(b, id)
		rec.Signature[0] = byte(i)
		rec.Cookie = byte(i*7 + 1)
		rec.ResponseMode = 1
		rec.setLastActivity(now)
		store.byID[id].Store(rec)
	}
}

// BenchmarkSessionStoreLookupParallel exercises the lock-free
// fast path: concurrent goroutines hammering Lookup() on a fully
// populated store. With atomic.Pointer.Load() there is no global
// lock contention, so kops/sec should scale ~linearly with cores.
func BenchmarkSessionStoreLookupParallel(b *testing.B) {
	store := newSessionStore(8, 32)
	const populated = 200
	benchPopulateStore(b, store, populated)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var counter uint32
		for pb.Next() {
			counter++
			id := uint8((counter % populated) + 1)
			if _, ok := store.Lookup(id); !ok {
				b.Fatalf("missing session id %d", id)
			}
		}
	})
}

// BenchmarkSessionStoreValidateAndTouchParallel checks the
// fast-path validation under heavy concurrent load. Each call
// performs an atomic load + cookie compare + activity update;
// no global lock is acquired on success.
func BenchmarkSessionStoreValidateAndTouchParallel(b *testing.B) {
	store := newSessionStore(8, 32)
	const populated = 200
	benchPopulateStore(b, store, populated)
	cookies := make([]uint8, populated+1)
	for i := 1; i <= populated; i++ {
		if rec := store.byID[uint8(i)].Load(); rec != nil {
			cookies[i] = rec.Cookie
		}
	}

	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var counter uint32
		for pb.Next() {
			counter++
			idx := int(counter%populated) + 1
			id := uint8(idx)
			res := store.ValidateAndTouch(id, cookies[idx], now)
			if !res.Valid {
				b.Fatalf("validation failed for id %d (known=%v)", id, res.Known)
			}
		}
	})
}

// BenchmarkSessionStoreMixedParallel models a realistic workload:
// many concurrent readers (Lookup) racing with a stream of writers
// updating last-activity. With the atomic.Pointer layout, readers
// and writers do not contend for the global RWMutex on the hot
// active branch.
func BenchmarkSessionStoreMixedParallel(b *testing.B) {
	store := newSessionStore(8, 32)
	const populated = 200
	benchPopulateStore(b, store, populated)

	var writerCounter uint64
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var counter uint32
		for pb.Next() {
			counter++
			id := uint8((counter % populated) + 1)
			if counter%8 == 0 {
				atomic.AddUint64(&writerCounter, 1)
				if rec := store.byID[id].Load(); rec != nil {
					rec.setLastActivity(now)
				}
			} else {
				if _, ok := store.Lookup(id); !ok {
					b.Fatalf("missing session id %d", id)
				}
			}
		}
	})
	_ = atomic.LoadUint64(&writerCounter)
}
