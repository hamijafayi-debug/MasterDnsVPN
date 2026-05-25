// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package security

import (
	"bytes"
	"sync"
	"testing"
)

func TestNonceGenUniqueAcrossCalls(t *testing.T) {
	g, err := newNonceGen(4)
	if err != nil {
		t.Fatalf("newNonceGen: %v", err)
	}
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		buf := make([]byte, 12)
		g.Fill(buf, 12)
		key := string(buf)
		if seen[key] {
			t.Fatalf("nonce collision after %d iterations", i)
		}
		seen[key] = true
	}
}

func TestNonceGenPrefixStableCounterAdvances(t *testing.T) {
	g, err := newNonceGen(4)
	if err != nil {
		t.Fatalf("newNonceGen: %v", err)
	}
	buf1 := make([]byte, 12)
	buf2 := make([]byte, 12)
	g.Fill(buf1, 12)
	g.Fill(buf2, 12)

	// Prefix bytes (the first 4) must be identical across calls.
	if !bytes.Equal(buf1[:4], buf2[:4]) {
		t.Fatalf("prefix changed between calls: %x vs %x", buf1[:4], buf2[:4])
	}
	// Counter portion (bytes 4..12) must differ.
	if bytes.Equal(buf1[4:], buf2[4:]) {
		t.Fatalf("counter did not advance: %x", buf1[4:])
	}
}

func TestNonceGenConcurrentDistinct(t *testing.T) {
	g, err := newNonceGen(8)
	if err != nil {
		t.Fatalf("newNonceGen: %v", err)
	}
	const goroutines = 16
	const perG = 1000
	var (
		mu   sync.Mutex
		seen = make(map[string]struct{}, goroutines*perG)
		wg   sync.WaitGroup
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([][16]byte, perG)
			for j := 0; j < perG; j++ {
				var buf [16]byte
				g.Fill(buf[:], 16)
				local[j] = buf
			}
			mu.Lock()
			for _, b := range local {
				seen[string(b[:])] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != goroutines*perG {
		t.Fatalf("expected %d distinct nonces, got %d", goroutines*perG, len(seen))
	}
}

func TestNonceGenReseedChangesPrefix(t *testing.T) {
	g, err := newNonceGen(4)
	if err != nil {
		t.Fatalf("newNonceGen: %v", err)
	}
	buf1 := make([]byte, 12)
	g.Fill(buf1, 12)

	prevPrefix := append([]byte(nil), buf1[:4]...)
	if err := g.Reseed(); err != nil {
		t.Fatalf("Reseed: %v", err)
	}
	buf2 := make([]byte, 12)
	g.Fill(buf2, 12)

	// The prefix should almost certainly have changed; even if it
	// collides by chance (1 in 2^32), the counter must restart at 1 so
	// the post-reseed nonce can't equal the pre-reseed one.
	if bytes.Equal(buf1, buf2) {
		t.Fatalf("post-reseed nonce equals pre-reseed nonce (prefix=%x)", prevPrefix)
	}
}

func TestCodecRoundTripAllMethodsWithCounterNonce(t *testing.T) {
	// Sanity: each AEAD/cipher method round-trips after we replaced the
	// rand.Read nonce path with the counter generator. Repeats per method
	// guarantee at least two distinct nonces hit the same Open call.
	for _, method := range []int{0, 1, 2, 3, 4, 5} {
		codec, err := NewCodec(method, benchKey)
		if err != nil {
			t.Fatalf("NewCodec(%d): %v", method, err)
		}
		for iter := 0; iter < 4; iter++ {
			pt := []byte("step-13-counter-nonce-roundtrip-payload")
			ct, err := codec.Encrypt(pt)
			if err != nil {
				t.Fatalf("method=%d Encrypt iter=%d: %v", method, iter, err)
			}
			out, err := codec.Decrypt(ct)
			if err != nil {
				t.Fatalf("method=%d Decrypt iter=%d: %v", method, iter, err)
			}
			if !bytes.Equal(out, pt) {
				t.Fatalf("method=%d iter=%d round-trip mismatch", method, iter)
			}
		}
	}
}

func TestUseRandFallbackInteropRoundTrip(t *testing.T) {
	// Encrypt with fallback enabled (legacy rand.Read), Decrypt with it
	// disabled (counter nonce). Should still round-trip because the
	// receiver only consumes the nonce bytes, not the way they were
	// generated.
	for _, method := range []int{2, 3, 5} {
		codec, err := NewCodec(method, benchKey)
		if err != nil {
			t.Fatalf("NewCodec(%d): %v", method, err)
		}
		useRandFallback(true)
		ct, err := codec.Encrypt([]byte("interop-payload"))
		useRandFallback(false)
		if err != nil {
			t.Fatalf("method=%d Encrypt: %v", method, err)
		}
		out, err := codec.Decrypt(ct)
		if err != nil {
			t.Fatalf("method=%d Decrypt: %v", method, err)
		}
		if string(out) != "interop-payload" {
			t.Fatalf("method=%d interop mismatch", method)
		}
	}
}

// FuzzCodecDecryptDoesNotPanic ensures the Decrypt path is robust to garbage
// input — adversarial ciphertext should never crash the codec.
func FuzzCodecDecryptDoesNotPanic(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		{0},
		bytes.Repeat([]byte{0xFF}, 16),
		bytes.Repeat([]byte{0xAA}, 64),
		bytes.Repeat([]byte{0x55}, 4096),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	codecs := []int{0, 1, 2, 3, 4, 5}
	f.Fuzz(func(t *testing.T, input []byte) {
		for _, method := range codecs {
			codec, err := NewCodec(method, benchKey)
			if err != nil {
				t.Fatalf("NewCodec(%d): %v", method, err)
			}
			// We don't care about success/failure; we care that the call
			// returns rather than panics.
			_, _ = codec.Decrypt(input)
		}
	})
}
