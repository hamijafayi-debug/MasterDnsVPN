// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package security

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// Step 13 — micro-benchmarks for Seal/Open across all supported codec methods
// and realistic payload sizes. The interesting metric is ns/op and B/op at
// the 200-byte and 1200-byte sizes — those bracket the DNS-tunneled
// payload range we actually see in production.
//
// The "Fallback" variants toggle the global useRandFallback flag so we can
// quantify the cost of the per-call rand.Read syscall vs the new counter
// nonce. Run them with `-bench=^Bench` for the headline numbers and
// `-bench=Fallback` for the comparison.

var (
	benchKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func benchSeal(b *testing.B, method int, size int) {
	codec, err := NewCodec(method, benchKey)
	if err != nil {
		b.Fatalf("NewCodec(%d): %v", method, err)
	}
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := codec.Encrypt(payload)
		if err != nil {
			b.Fatalf("Encrypt: %v", err)
		}
		if len(out) == 0 {
			b.Fatal("empty ciphertext")
		}
	}
}

func benchSealOpen(b *testing.B, method int, size int) {
	codec, err := NewCodec(method, benchKey)
	if err != nil {
		b.Fatalf("NewCodec(%d): %v", method, err)
	}
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ct, err := codec.Encrypt(payload)
		if err != nil {
			b.Fatalf("Encrypt: %v", err)
		}
		pt, err := codec.Decrypt(ct)
		if err != nil {
			b.Fatalf("Decrypt: %v", err)
		}
		if !bytes.Equal(pt, payload) {
			b.Fatal("round-trip mismatch")
		}
	}
}

// AES-GCM seal benchmarks (the headline path: method 3 / 16-byte key).
func BenchmarkCodecSeal_AES128_200B(b *testing.B)  { benchSeal(b, 3, 200) }
func BenchmarkCodecSeal_AES128_1200B(b *testing.B) { benchSeal(b, 3, 1200) }
func BenchmarkCodecSeal_AES128_8192B(b *testing.B) { benchSeal(b, 3, 8192) }
func BenchmarkCodecSeal_AES256_1200B(b *testing.B) { benchSeal(b, 5, 1200) }

// AES seal+open (round-trip) — relevant for upstream/downstream symmetry.
func BenchmarkCodecSealOpen_AES128_1200B(b *testing.B) { benchSealOpen(b, 3, 1200) }
func BenchmarkCodecSealOpen_AES256_1200B(b *testing.B) { benchSealOpen(b, 5, 1200) }

// ChaCha20 (method 2) — second-most-common method.
func BenchmarkCodecSeal_ChaCha_200B(b *testing.B)  { benchSeal(b, 2, 200) }
func BenchmarkCodecSeal_ChaCha_1200B(b *testing.B) { benchSeal(b, 2, 1200) }
func BenchmarkCodecSealOpen_ChaCha_1200B(b *testing.B) {
	benchSealOpen(b, 2, 1200)
}

// XOR (method 1) — fastest path; included so we can see if pool changes
// regress it.
func BenchmarkCodecSeal_XOR_1200B(b *testing.B) { benchSeal(b, 1, 1200) }

// Rand-fallback comparison — uses the legacy per-call crypto/rand.Read path
// for the nonce. Demonstrates the syscall overhead the counter-nonce removes.
func BenchmarkCodecSeal_AES128_1200B_RandFallback(b *testing.B) {
	useRandFallback(true)
	defer useRandFallback(false)
	benchSeal(b, 3, 1200)
}

func BenchmarkCodecSeal_ChaCha_1200B_RandFallback(b *testing.B) {
	useRandFallback(true)
	defer useRandFallback(false)
	benchSeal(b, 2, 1200)
}

// EncryptAndEncode benchmarks — the end-to-end hot path used by the client
// when building DNS labels.
func BenchmarkCodecEncryptAndEncodeBytes_AES128_1200B(b *testing.B) {
	codec, err := NewCodec(3, benchKey)
	if err != nil {
		b.Fatalf("NewCodec: %v", err)
	}
	payload := make([]byte, 1200)
	_, _ = rand.Read(payload)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := codec.EncryptAndEncodeBytes(payload)
		if err != nil {
			b.Fatalf("EncryptAndEncodeBytes: %v", err)
		}
		if len(out) == 0 {
			b.Fatal("empty output")
		}
	}
}

// Parallel benchmark — verifies the atomic counter scales under contention.
func BenchmarkCodecSeal_AES128_1200B_Parallel(b *testing.B) {
	codec, err := NewCodec(3, benchKey)
	if err != nil {
		b.Fatalf("NewCodec: %v", err)
	}
	payload := make([]byte, 1200)
	_, _ = rand.Read(payload)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := codec.Encrypt(payload); err != nil {
				b.Fatalf("Encrypt: %v", err)
			}
		}
	})
}
