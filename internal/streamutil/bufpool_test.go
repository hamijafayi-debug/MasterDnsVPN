// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package streamutil

import (
	"sync"
	"testing"
)

func TestGetReturnsRequestedLen(t *testing.T) {
	cases := []int{1, 100, 512, 513, 2048, 2049, 8192, 8193, 65536}
	for _, n := range cases {
		buf := Get(n)
		if len(buf) != n {
			t.Errorf("Get(%d) returned len=%d", n, len(buf))
		}
		if cap(buf) < n {
			t.Errorf("Get(%d) returned cap=%d < %d", n, cap(buf), n)
		}
		Put(buf)
	}
}

func TestGetZeroOrNegativeReturnsNil(t *testing.T) {
	if buf := Get(0); buf != nil {
		t.Errorf("Get(0) returned %v want nil", buf)
	}
	if buf := Get(-1); buf != nil {
		t.Errorf("Get(-1) returned %v want nil", buf)
	}
}

func TestPoolFor(t *testing.T) {
	cases := []struct {
		size    int
		wantCap int
		wantNil bool
	}{
		{1, BufSmall, false},
		{512, BufSmall, false},
		{513, BufMedium, false},
		{2048, BufMedium, false},
		{2049, BufLarge, false},
		{8192, BufLarge, false},
		{8193, BufJumbo, false},
		{65536, BufJumbo, false},
		{65537, 0, true},
	}
	for _, c := range cases {
		pool, capSize := poolFor(c.size)
		if c.wantNil {
			if pool != nil {
				t.Errorf("poolFor(%d) pool != nil; expected fallback", c.size)
			}
			continue
		}
		if pool == nil {
			t.Errorf("poolFor(%d) pool is nil; expected tier %d", c.size, c.wantCap)
		}
		if capSize != c.wantCap {
			t.Errorf("poolFor(%d) cap=%d want %d", c.size, capSize, c.wantCap)
		}
	}
}

func TestPutAcceptsResliced(t *testing.T) {
	buf := Get(BufMedium)
	// Resliced down — Put must still find the tier via cap().
	smaller := buf[:100]
	Put(smaller)

	// Pool should hand back a buffer with the full tier capacity.
	out := Get(50)
	if cap(out) < BufSmall {
		t.Fatalf("recycled buffer cap=%d too small", cap(out))
	}
	Put(out)
}

func TestPutOutOfTierIsNoop(t *testing.T) {
	// Building a slice whose cap does not match any tier should be silently
	// dropped — no panic, no growth of any pool.
	odd := make([]byte, 100, 999)
	Put(odd) // must not panic
}

func TestGetPtrPutPtrRoundtrip(t *testing.T) {
	cases := []int{1, 100, 512, 513, 2048, 2049, 8192, 65536}
	for _, n := range cases {
		p := GetPtr(n)
		if p == nil {
			t.Fatalf("GetPtr(%d) returned nil", n)
		}
		if cap(*p) < n {
			t.Errorf("GetPtr(%d) cap=%d < %d", n, cap(*p), n)
		}
		PutPtr(p)
	}
}

func TestPutPtrNilSafe(t *testing.T) {
	PutPtr(nil) // must not panic
}

func TestGetPtrZeroOrNegativeReturnsNil(t *testing.T) {
	if p := GetPtr(0); p != nil {
		t.Errorf("GetPtr(0) returned %v want nil", p)
	}
	if p := GetPtr(-1); p != nil {
		t.Errorf("GetPtr(-1) returned %v want nil", p)
	}
}

func TestGetCapHonoursMinCap(t *testing.T) {
	buf := GetCap(10, BufLarge)
	if len(buf) != 10 {
		t.Errorf("GetCap returned len=%d want 10", len(buf))
	}
	if cap(buf) < BufLarge {
		t.Errorf("GetCap returned cap=%d want >= %d", cap(buf), BufLarge)
	}
	Put(buf)
}

func TestConcurrentGetPut(t *testing.T) {
	const workers = 32
	const iter = 1000
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < iter; j++ {
				size := (seed + j) % BufJumbo
				if size == 0 {
					size = 1
				}
				buf := Get(size)
				if len(buf) != size {
					t.Errorf("len=%d want %d", len(buf), size)
					return
				}
				// touch the memory so the race detector exercises it
				buf[0] = byte(j)
				if size > 1 {
					buf[size-1] = byte(j)
				}
				Put(buf)
			}
		}(i)
	}
	wg.Wait()
}

func BenchmarkMakeVsPoolSmall(b *testing.B) {
	b.Run("make/512", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := make([]byte, 512)
			_ = buf
		}
	})
	b.Run("pool/512", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := Get(512)
			Put(buf)
		}
	})
}

func BenchmarkMakeVsPoolMedium(b *testing.B) {
	b.Run("make/2048", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := make([]byte, 2048)
			_ = buf
		}
	})
	b.Run("pool/2048", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := Get(2048)
			Put(buf)
		}
	})
}

func BenchmarkMakeVsPoolJumbo(b *testing.B) {
	b.Run("make/65536", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := make([]byte, 65536)
			_ = buf
		}
	})
	b.Run("pool/65536", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := Get(65536)
			Put(buf)
		}
	})
}

// BenchmarkGetPtrPutPtrZeroAlloc is the canonical "is the pool actually
// working?" test. With a warm pool this run should report 0 allocs/op and
// 0 B/op regardless of tier size.
func BenchmarkGetPtrPutPtrZeroAlloc(b *testing.B) {
	for _, size := range []int{512, 2048, 8192, 65536} {
		size := size
		b.Run("size", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p := GetPtr(size)
				_ = (*p)[:size]
				PutPtr(p)
			}
		})
	}
}
