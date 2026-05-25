// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package streamutil

import "sync"

// Sized byte-slice pools that callers can use as scratch space in hot paths.
// The intent of this file (Step 2 in PLAN.md) is to give the rest of the code
// base a single, well-tested allocator that recycles []byte across goroutines
// without each package having to define its own sync.Pool.
//
// Design notes:
//
//   - Four tiers were picked to cover the realistic packet sizes observed in
//     the DNS / ARQ hot paths:
//
//     |  tier name |  cap  |  typical user                                |
//     |------------|-------|-----------------------------------------------|
//     |  small     |  512  | session-accept payloads, error responses     |
//     |  medium    |  2048 | typical ARQ data segment (≤ MTU)             |
//     |  large     |  8192 | base64-expanded TXT chunks, batched encodes  |
//     |  jumbo     | 65536 | upstream resolver responses, fragment join   |
//
//   - Get(n) always returns a buffer whose len == n; its cap is at least n
//     and matches one of the tier sizes. Callers may sub-slice without losing
//     poolability — Put() looks at cap() to route the buffer back to the
//     correct tier.
//
//   - Requests larger than the jumbo tier fall back to a plain make. Put on
//     such buffers is a no-op so callers can stay uniform.
//
//   - The pools store *[]byte (pointer-to-slice) rather than []byte directly
//     to avoid the heap allocation that happens when storing a slice header
//     in sync.Pool's `any` slot (see Go vet's `SA6002`).
//
// Benchmarks live in bufpool_test.go and serve as the before/after baseline
// for the wider sync.Pool roll-out planned in PLAN.md step 2.

// Pool size tiers. Exported so callers (or future steps) can introspect
// pool boundaries.
const (
	BufSmall  = 512
	BufMedium = 2 * 1024
	BufLarge  = 8 * 1024
	BufJumbo  = 64 * 1024
)

var (
	smallPool  = sizedPool(BufSmall)
	mediumPool = sizedPool(BufMedium)
	largePool  = sizedPool(BufLarge)
	jumboPool  = sizedPool(BufJumbo)
)

// sizedPool returns a sync.Pool that allocates pointer-to-byte-slice buffers
// with the given capacity. Pointer storage avoids the per-Get allocation that
// the linter (and the runtime) warn about when sync.Pool holds slice values
// directly.
func sizedPool(size int) *sync.Pool {
	return &sync.Pool{
		New: func() any {
			buf := make([]byte, size)
			return &buf
		},
	}
}

// poolFor selects the smallest tier whose capacity is ≥ size. nil is returned
// for sizes larger than the jumbo tier — callers fall back to plain make().
func poolFor(size int) (*sync.Pool, int) {
	switch {
	case size <= BufSmall:
		return smallPool, BufSmall
	case size <= BufMedium:
		return mediumPool, BufMedium
	case size <= BufLarge:
		return largePool, BufLarge
	case size <= BufJumbo:
		return jumboPool, BufJumbo
	default:
		return nil, 0
	}
}

// Get returns a byte slice whose len equals size and whose capacity is at
// least size. The buffer is drawn from the smallest tier that fits, or
// freshly allocated for over-jumbo requests.
//
// The returned slice's contents are NOT zeroed — callers must overwrite the
// region they intend to read.
func Get(size int) []byte {
	if size <= 0 {
		return nil
	}
	pool, tierCap := poolFor(size)
	if pool == nil {
		return make([]byte, size)
	}
	bufPtr := pool.Get().(*[]byte)
	// The pool was primed with `tierCap`-sized slices, but a malicious or
	// resized buffer might come back smaller. Re-allocate defensively.
	if cap(*bufPtr) < size {
		return make([]byte, size, tierCap)
	}
	return (*bufPtr)[:size]
}

// Put returns a previously-acquired buffer to the pool. It is safe to pass a
// re-sliced or appended slice — capacity is used to pick the correct tier
// and the buffer is reset to its full length before being stored so the next
// Get sees the intended capacity.
//
// Buffers whose capacity does not match a known tier (e.g. allocated outside
// the pool) are silently dropped.
//
// Allocation note: the slice header is wrapped in a fresh *[]byte so it can
// be stored in sync.Pool's `any` slot without re-boxing on every Put. This
// costs one 24-byte pointer allocation per call site that loses track of the
// original *[]byte handed out by Get — see GetPtr/PutPtr below for the
// zero-alloc variant used in the very hottest paths.
func Put(buf []byte) {
	if cap(buf) == 0 {
		return
	}
	// Always normalise the slice so the pool stores a full-cap value.
	full := buf[:cap(buf)]

	switch cap(full) {
	case BufSmall:
		smallPool.Put(&full)
	case BufMedium:
		mediumPool.Put(&full)
	case BufLarge:
		largePool.Put(&full)
	case BufJumbo:
		jumboPool.Put(&full)
	default:
		// Out-of-tier buffer; let it be garbage-collected. We could route
		// it to the nearest larger tier, but that would inflate memory
		// over time for callers that purposely build oversized slices.
	}
}

// GetPtr / PutPtr are the zero-alloc twins of Get / Put. They return the
// raw *[]byte the sync.Pool stores internally, which lets the caller hand
// it back without ever causing the slice header to escape to the heap.
//
// Usage:
//
//	bufPtr := streamutil.GetPtr(size)
//	defer streamutil.PutPtr(bufPtr)
//	raw := (*bufPtr)[:size]
//	... use raw ...
//
// The contents of *bufPtr have len == cap == tier size; the caller is
// expected to slice down to its desired length.
func GetPtr(size int) *[]byte {
	if size <= 0 {
		return nil
	}
	pool, tierCap := poolFor(size)
	if pool == nil {
		buf := make([]byte, size)
		return &buf
	}
	bufPtr := pool.Get().(*[]byte)
	if cap(*bufPtr) < size {
		// Defensive — should not happen with a fresh pool slot.
		buf := make([]byte, tierCap)
		return &buf
	}
	return bufPtr
}

// PutPtr returns the pointer fetched by GetPtr back to its tier. The slice
// length is restored to the full tier capacity so the next GetPtr caller
// sees the expected size.
func PutPtr(bufPtr *[]byte) {
	if bufPtr == nil {
		return
	}
	full := (*bufPtr)[:cap(*bufPtr)]
	*bufPtr = full

	switch cap(full) {
	case BufSmall:
		smallPool.Put(bufPtr)
	case BufMedium:
		mediumPool.Put(bufPtr)
	case BufLarge:
		largePool.Put(bufPtr)
	case BufJumbo:
		jumboPool.Put(bufPtr)
	default:
		// Drop oversized buffers — same policy as Put.
	}
}

// GetCap is a convenience variant that asks for a slice with a specific
// initial length AND requests pool capacity to be at least minCap. Used by
// callers that want headroom for in-place growth (e.g. building a packet
// header before knowing the final payload length).
func GetCap(initialLen, minCap int) []byte {
	if minCap < initialLen {
		minCap = initialLen
	}
	buf := Get(minCap)
	if buf == nil {
		return nil
	}
	return buf[:initialLen]
}
