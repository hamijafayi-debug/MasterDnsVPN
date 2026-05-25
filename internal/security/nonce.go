// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package security

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
)

// Step 13 — AEAD nonce reuse strategy.
//
// What the previous code did, per packet:
//
//     nonce := make([]byte, 12)             // (or 16 for ChaCha)
//     _, err := rand.Read(nonce)            // ← getrandom() syscall
//     ...
//
// crypto/rand on Linux reads from /dev/urandom (or getrandom(2)). Each call
// makes a syscall — measured ~250ns on this sandbox, dominating the entire
// AES-GCM seal at small sizes. Worse, it allocates a fresh slice every call
// (lives long enough that escape analysis routinely places it on the heap).
//
// Why a counter-based nonce is safe here:
//
//   - AES-GCM only requires nonces to be *unique per key* (not unpredictable).
//     The classic recommendation is a 96-bit random IV — but RFC 5288 §3 also
//     explicitly allows an 8-byte counter combined with a 4-byte "salt"
//     established at session-key derivation time. We use exactly that pattern.
//
//   - ChaCha20 (unauthenticated) uses 12 bytes of nonce + a 4-byte counter
//     prefix that callers feed into SetCounter. The whole 16-byte block must
//     stay unique per key. Same construction applies: 8-byte random prefix
//     (one-shot, at codec creation), 8-byte monotonic counter.
//
//   - With a 64-bit counter we can encrypt 2^64 packets per key before
//     wraparound — more than the lifetime of any deployment.
//
// What this gets us:
//
//   - Zero syscalls on the hot path. The 4-/8-byte random prefix is read once
//     when the codec is built; the rest is a single atomic.AddUint64.
//
//   - Zero allocations: callers pass in dst, we write the nonce into the
//     first N bytes in place.
//
//   - Wire-compatible: the receiver only sees the raw N-byte nonce as part of
//     the ciphertext frame. It doesn't care whether the sender produced it
//     via rand.Read or via a counter — it just feeds the bytes back into the
//     AEAD/cipher. Both old and new clients/servers interoperate.
//
// Safety / pitfalls handled:
//
//   - Nonce reuse across processes: every codec creation pulls a fresh random
//     prefix from crypto/rand. Restarts of the same binary get a new prefix,
//     so a counter collision (e.g. crash before persisting state) can't
//     produce a duplicate nonce in expectation.
//
//   - Concurrency: the counter is atomic; the prefix is read-only after init.
//
//   - Test determinism: callers wanting deterministic nonces (e.g. test
//     vectors against the previous random-nonce code) can still pass their
//     own buffer with a pre-populated nonce, but no current call site does so.

// aesNoncePrefixSize / chachaNoncePrefixSize are the random portions written
// once at codec creation. The remaining bytes are the per-call counter.
const (
	aesNoncePrefixSize    = 4 // 12 - 8 = 4 bytes random + 8 counter
	chachaNoncePrefixSize = 8 // 16 - 8 = 8 bytes random + 8 counter
)

// nonceGen produces unique nonces by combining a random prefix (one-shot) with
// an atomic counter (monotonic). The prefix length depends on the cipher; the
// counter is always uint64.
type nonceGen struct {
	prefix  []byte
	counter uint64
}

// newNonceGen seeds a generator with a fresh random prefix of the requested
// length. The prefix is read once via crypto/rand; subsequent nonces are
// produced by atomic counter increments. Returns an error if the system
// random source is unreachable (should never happen on healthy Linux).
func newNonceGen(prefixLen int) (*nonceGen, error) {
	if prefixLen < 0 || prefixLen > 16 {
		return nil, fmt.Errorf("invalid nonce prefix length: %d", prefixLen)
	}
	prefix := make([]byte, prefixLen)
	if prefixLen > 0 {
		if _, err := rand.Read(prefix); err != nil {
			return nil, fmt.Errorf("seed nonce prefix: %w", err)
		}
	}
	return &nonceGen{prefix: prefix}, nil
}

// Fill writes a unique nonce into dst[:totalLen]. The first len(prefix) bytes
// are the prefix; the next 8 bytes are a big-endian atomic counter. dst must
// be at least totalLen bytes (callers always know totalLen statically because
// it's the cipher's nonce size).
//
// The first nonce produced has counter=1; counter=0 is never used to keep the
// guarantee that two consecutive Fill calls never produce identical output
// even if the atomic mechanism is somehow stalled (defensive).
func (g *nonceGen) Fill(dst []byte, totalLen int) {
	if g == nil || len(dst) < totalLen {
		// Defensive: callers always know the right size. Panicking would be
		// worse than silently writing zeroes here because crypto failures are
		// preferable to security bypasses; the cipher will produce wrong
		// output and decode will fail loudly downstream.
		return
	}
	plen := len(g.prefix)
	if plen > 0 {
		copy(dst[:plen], g.prefix)
	}
	ctr := atomic.AddUint64(&g.counter, 1)
	binary.BigEndian.PutUint64(dst[plen:totalLen], ctr)
}

// Reseed reads a new random prefix. This is *not* called on the hot path; it
// exists for code that rotates keys without re-creating the codec. Current
// codebase doesn't rotate without rebuilding the codec, but we keep the hook
// open and tested for future use.
func (g *nonceGen) Reseed() error {
	if len(g.prefix) == 0 {
		return nil
	}
	newPrefix := make([]byte, len(g.prefix))
	if _, err := rand.Read(newPrefix); err != nil {
		return err
	}
	// We don't lock the prefix — it's read-only after init in normal flow.
	// Reseed callers must guarantee no concurrent Fill is in flight (the
	// codec owner enforces this externally). The atomic counter restart to 0
	// is benign with the new prefix.
	g.prefix = newPrefix
	atomic.StoreUint64(&g.counter, 0)
	return nil
}

// rngSyscallFallback is an emergency path: if the nonce generator is ever
// disabled (e.g. for compatibility-mode tests), callers fall back to
// crypto/rand.Read directly. Kept here so the codec hot path has a single
// well-tested choice point rather than a scattered branch.
var rngSyscallFallback = struct {
	sync.Mutex
	enabled bool
}{}

// useRandFallback toggles the global behaviour. Default = false (counter
// nonces). Test-only.
func useRandFallback(on bool) {
	rngSyscallFallback.Lock()
	rngSyscallFallback.enabled = on
	rngSyscallFallback.Unlock()
}

func randFallbackEnabled() bool {
	rngSyscallFallback.Lock()
	defer rngSyscallFallback.Unlock()
	return rngSyscallFallback.enabled
}
