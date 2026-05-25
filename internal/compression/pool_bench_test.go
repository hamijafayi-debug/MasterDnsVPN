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

// Step 12 benchmarks — measure before/after across compressible vs
// incompressible payloads. The "Random" variants are the killer: with the
// entropy heuristic enabled they should drop to a near-zero-cost early
// return; with the heuristic disabled they show the full encoder cost.

func makeText(size int) []byte {
	// Pseudo-text payload: repeating English-ish bigrams. ZSTD/ZLIB compress
	// this ~4-5× ; LZ4 typically ~2-3× .
	base := []byte("the quick brown fox jumps over the lazy dog. ")
	out := make([]byte, 0, size)
	for len(out) < size {
		out = append(out, base...)
	}
	return out[:size]
}

func makeBinary(size int) []byte {
	// Semi-random binary with some structure: 16-byte runs of the same value.
	out := make([]byte, size)
	for i := 0; i < size; i += 16 {
		v := byte(i / 16)
		end := i + 16
		if end > size {
			end = size
		}
		for j := i; j < end; j++ {
			out[j] = v
		}
	}
	return out
}

func makeRandom(size int) []byte {
	out := make([]byte, size)
	_, _ = rand.Read(out)
	return out
}

func benchmarkCompress(b *testing.B, data []byte, compType uint8) {
	b.Helper()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, used := CompressPayload(data, compType, DefaultMinSize)
		_ = out
		_ = used
	}
}

func BenchmarkCompressLZ4_Text1200(b *testing.B) {
	benchmarkCompress(b, makeText(1200), TypeLZ4)
}

func BenchmarkCompressLZ4_Binary1200(b *testing.B) {
	benchmarkCompress(b, makeBinary(1200), TypeLZ4)
}

func BenchmarkCompressLZ4_Random1200(b *testing.B) {
	benchmarkCompress(b, makeRandom(1200), TypeLZ4)
}

func BenchmarkCompressZSTD_Text1200(b *testing.B) {
	benchmarkCompress(b, makeText(1200), TypeZSTD)
}

func BenchmarkCompressZSTD_Random1200(b *testing.B) {
	benchmarkCompress(b, makeRandom(1200), TypeZSTD)
}

func BenchmarkCompressZLIB_Text1200(b *testing.B) {
	benchmarkCompress(b, makeText(1200), TypeZLIB)
}

func BenchmarkCompressZLIB_Random1200(b *testing.B) {
	benchmarkCompress(b, makeRandom(1200), TypeZLIB)
}

// Entropy-skip benchmarks — exercise the heuristic path. We toggle the global
// threshold around each subtest. Note that the package-level threshold mutation
// is documented as start-time-only, but is safe in single-test isolation
// because no other goroutines are calling CompressPayload during the bench.
func BenchmarkCompressLZ4_Random1200_EntropySkip(b *testing.B) {
	old := EntropySkipThresholdDeci
	SetEntropySkipThresholdDeci(65) // 6.5 bits/byte — sampling-noise-safe threshold for random data
	defer SetEntropySkipThresholdDeci(old)
	benchmarkCompress(b, makeRandom(1200), TypeLZ4)
}

func BenchmarkCompressZSTD_Random1200_EntropySkip(b *testing.B) {
	old := EntropySkipThresholdDeci
	SetEntropySkipThresholdDeci(65)
	defer SetEntropySkipThresholdDeci(old)
	benchmarkCompress(b, makeRandom(1200), TypeZSTD)
}

func BenchmarkCompressZLIB_Random1200_EntropySkip(b *testing.B) {
	old := EntropySkipThresholdDeci
	SetEntropySkipThresholdDeci(65)
	defer SetEntropySkipThresholdDeci(old)
	benchmarkCompress(b, makeRandom(1200), TypeZLIB)
}

func BenchmarkEntropyDeciBits_1200(b *testing.B) {
	data := makeRandom(1200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EntropyDeciBits(data)
	}
}

func BenchmarkEntropyDeciBits_Text1200(b *testing.B) {
	data := makeText(1200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EntropyDeciBits(data)
	}
}

// Sanity check: round-trip with the entropy guard enabled still works for
// text (compressed) and random (skipped → raw bytes returned).
func TestEntropySkipPreservesRoundTrip(t *testing.T) {
	old := EntropySkipThresholdDeci
	SetEntropySkipThresholdDeci(65)
	defer SetEntropySkipThresholdDeci(old)

	text := makeText(2048)
	compressed, used := CompressPayload(text, TypeZSTD, DefaultMinSize)
	if used != TypeZSTD {
		t.Fatalf("expected text payload to be compressed: used=%d", used)
	}
	decoded, ok := TryDecompressPayload(compressed, used)
	if !ok || !bytes.Equal(decoded, text) {
		t.Fatalf("text round-trip failed")
	}

	random := makeRandom(2048)
	out, used := CompressPayload(random, TypeZSTD, DefaultMinSize)
	if used != TypeOff {
		t.Fatalf("expected random payload to skip compression: used=%d", used)
	}
	if !bytes.Equal(out, random) {
		t.Fatalf("random payload must pass through unchanged when skipped")
	}
}
