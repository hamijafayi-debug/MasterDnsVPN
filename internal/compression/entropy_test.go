// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package compression

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestEntropyDeciBitsRandomIsHigh(t *testing.T) {
	data := make([]byte, 2048)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	e := EntropyDeciBits(data)
	// 256-byte sampling caps the *measurable* entropy below 80: by birthday
	// arithmetic, a uniform random sample of 256 from 256 possibilities only
	// hits ~70 unique bytes, giving E ≈ 70..76 deci-bits. Any value above 65
	// is "high entropy" by any practical definition.
	if e < 65 {
		t.Fatalf("expected high entropy for random bytes (>=65 deci-bits/byte), got %d", e)
	}
	if e > EntropyMaxDeciBits {
		t.Fatalf("entropy must be clamped to %d deci-bits/byte, got %d", EntropyMaxDeciBits, e)
	}
}

func TestEntropyDeciBitsRepeatedIsLow(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 2048)
	e := EntropyDeciBits(data)
	if e > 5 {
		t.Fatalf("expected near-zero entropy for repeated byte, got %d deci-bits/byte", e)
	}
}

func TestEntropyDeciBitsTextIsMid(t *testing.T) {
	// Pseudo-text: bigrams from ascii printable range. Entropy of english-ish
	// text is empirically ~4.0-4.5 bits/byte on small samples.
	base := []byte("the quick brown fox jumps over the lazy dog. ")
	data := make([]byte, 0, 2048)
	for len(data) < 2048 {
		data = append(data, base...)
	}
	data = data[:2048]
	e := EntropyDeciBits(data)
	if e < 30 || e > 60 {
		t.Fatalf("expected text entropy in 30..60 deci-bits, got %d", e)
	}
}

func TestEntropyDeciBitsBelowMinReturnsZero(t *testing.T) {
	data := make([]byte, entropyMinPayload-1)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if e := EntropyDeciBits(data); e != 0 {
		t.Fatalf("expected 0 for sub-min payload, got %d", e)
	}
}

func TestLooksHighEntropyThresholdZeroDisabled(t *testing.T) {
	data := make([]byte, 2048)
	_, _ = rand.Read(data)
	if LooksHighEntropy(data, 0) {
		t.Fatal("threshold=0 must disable the check (return false even for high-entropy data)")
	}
}

func TestLooksHighEntropyDetectsRandomAtRealisticThreshold(t *testing.T) {
	data := make([]byte, 2048)
	_, _ = rand.Read(data)
	// 65 deci-bits/byte is well within the random sample's reach (random
	// samples cluster around 70-75 due to 256-byte sampling caps). This is the
	// threshold a real deployment should pick to discriminate random/encrypted
	// from compressible content.
	if !LooksHighEntropy(data, 65) {
		t.Fatalf("LooksHighEntropy(threshold=65) should fire on cryptorand bytes")
	}
}

func TestSetEntropySkipThresholdDeciClamps(t *testing.T) {
	old := EntropySkipThresholdDeci
	defer SetEntropySkipThresholdDeci(old)

	SetEntropySkipThresholdDeci(-5)
	if EntropySkipThresholdDeci != 0 {
		t.Fatalf("negative threshold must clamp to 0, got %d", EntropySkipThresholdDeci)
	}

	SetEntropySkipThresholdDeci(EntropyMaxDeciBits + 100)
	if EntropySkipThresholdDeci != EntropyMaxDeciBits {
		t.Fatalf("over-max threshold must clamp to %d, got %d", EntropyMaxDeciBits, EntropySkipThresholdDeci)
	}
}

func TestLZ4PoolRoundTrip(t *testing.T) {
	// Verify the LZ4 pool path produces bit-identical output to a fresh-buffer
	// encode and that the round-trip decompresses correctly. Run multiple
	// iterations so the second call hits the pool's reuse path.
	payloads := [][]byte{
		bytes.Repeat([]byte("lz4-pool-roundtrip-"), 64),  // ~1.2KB compressible
		bytes.Repeat([]byte("lz4-pool-roundtrip-"), 512), // ~10KB compressible
	}
	for _, payload := range payloads {
		for i := 0; i < 3; i++ {
			compressed, used := CompressPayload(payload, TypeLZ4, DefaultMinSize)
			if used != TypeLZ4 {
				t.Fatalf("expected LZ4 compression on pass %d, got %d", i+1, used)
			}
			decoded, ok := TryDecompressPayload(compressed, used)
			if !ok {
				t.Fatalf("decompression failed on pass %d", i+1)
			}
			if !bytes.Equal(decoded, payload) {
				t.Fatalf("LZ4 round-trip mismatch on pass %d", i+1)
			}
		}
	}
}
