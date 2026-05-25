// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package compression

import "sync"

// Step 12 — pooled scratch buffers for compression hot paths.
//
// Why this file exists:
//
//   - compressLZ4 used to `make([]byte, maxSize+4)` on every call, which for a
//     ~1200-byte payload is ~1.3 KiB of heap churn per packet (~50KB/s of
//     allocations at modest packet rates).
//
//   - compressZLIB / decompressZLIB / decompressZSTD all use a `*bytes.Buffer`
//     pool whose initial capacity was 256 bytes. Any real payload immediately
//     grows the underlying slice, defeating the pool's purpose; growth often
//     copies through two or three intermediate sizes before reaching the
//     ~1200B steady state. We add a sized `[]byte` pool used by encoders that
//     don't need bytes.Buffer's read/write semantics.
//
//   - The pools live inside the compression package (not streamutil) for two
//     reasons:
//       1. Tier sizes here are tuned to compression workloads (lz4 bound is
//          essentially `len + len/255 + 16`, dominated by the input size).
//       2. Keeping the dependency surface flat — streamutil already serves
//          packet-builder hot paths; mixing compression-only buffers there
//          would muddy the eviction policy.
//
// Lifetime contract:
//
//   - getCompressBuf(size) returns a pointer to a buffer with cap ≥ size and
//     len == 0. Caller appends, then either:
//       (a) hands the buffer to putCompressBuf when done, OR
//       (b) returns the buffer to the caller of CompressPayload (which then
//           owns it — pool reuse is not possible in that case).
//
//   - For the LZ4 path we always copy the final slice into a fresh `[]byte`
//     before returning (the LZ4 encoded output is much smaller than the bound,
//     so reusing the bound-sized buffer would be wasteful and confusing for
//     downstream consumers).

const (
	// LZ4 bound is roughly input + input/255 + 16. For our typical 1200-byte
	// DNS-tunneled packet the bound is ~1240; we round up generously.
	lz4BufSmall  = 2 * 1024  // covers up to ~2KB payloads (covers most DNS-tunneled segments)
	lz4BufMedium = 8 * 1024  // covers ARQ chunks up to ~8KB
	lz4BufLarge  = 32 * 1024 // covers large segments / merged frames
	lz4BufJumbo  = 96 * 1024 // covers up to ~88KB input (max practical here is maxDecompressedSize=10MB but most calls stay well below 64KB)
)

var (
	lz4Small  = newSizedSlicePool(lz4BufSmall)
	lz4Medium = newSizedSlicePool(lz4BufMedium)
	lz4Large  = newSizedSlicePool(lz4BufLarge)
	lz4Jumbo  = newSizedSlicePool(lz4BufJumbo)
)

func newSizedSlicePool(size int) *sync.Pool {
	return &sync.Pool{
		New: func() any {
			buf := make([]byte, size)
			return &buf
		},
	}
}

// getLZ4Buf returns a *[]byte with cap ≥ needed. The returned slice has its
// length set to its full capacity (caller will write at most `needed` bytes
// and re-slice). The second return value is the pool to which the buffer
// should be returned via putLZ4Buf; nil for over-jumbo allocations (those are
// dropped on Put).
func getLZ4Buf(needed int) (*[]byte, *sync.Pool) {
	switch {
	case needed <= lz4BufSmall:
		return lz4Small.Get().(*[]byte), lz4Small
	case needed <= lz4BufMedium:
		return lz4Medium.Get().(*[]byte), lz4Medium
	case needed <= lz4BufLarge:
		return lz4Large.Get().(*[]byte), lz4Large
	case needed <= lz4BufJumbo:
		return lz4Jumbo.Get().(*[]byte), lz4Jumbo
	default:
		buf := make([]byte, needed)
		return &buf, nil
	}
}

func putLZ4Buf(bufPtr *[]byte, pool *sync.Pool) {
	if bufPtr == nil || pool == nil {
		return
	}
	pool.Put(bufPtr)
}
