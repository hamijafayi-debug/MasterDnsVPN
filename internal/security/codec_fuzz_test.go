// ==============================================================================
// MasterDnsVPN
// Step 22 — Additional fuzz coverage for the security codec.
//
// The pre-existing FuzzCodecDecryptDoesNotPanic in nonce_test.go covers the
// raw-bytes Decrypt entry point. This file adds three companion targets:
//
//   - FuzzCodecEncryptDecryptRoundTrip — property: Decrypt(Encrypt(x)) == x
//     for every codec method, for every plaintext shape.
//   - FuzzCodecDecodeStringAndDecrypt — the on-wire string entry point
//     (base64 + codec) that DNS labels actually feed into the server.
//   - FuzzCodecDecodeAndDecrypt        — same path on raw bytes; defends the
//     binary boundary where pool-buffer reuse happens.
//
// Goal: no input — adversarial, oversized, malformed, or empty — must ever
// crash the codec. Crashes auto-save as permanent regression seeds.
// ==============================================================================

package security

import (
	"bytes"
	"testing"
)

// codecMethodsToFuzz enumerates the codec IDs accepted by NewCodec. We keep
// this here (rather than reaching into an exported list) so the fuzz targets
// remain isolated from any future refactor of the registration map.
var codecMethodsToFuzz = []int{0, 1, 2, 3, 4, 5}

// FuzzCodecEncryptDecryptRoundTrip checks the cryptographic round-trip
// property. If Encrypt succeeds, the resulting ciphertext must Decrypt back
// to the original plaintext bit-for-bit. Holds across all methods.
func FuzzCodecEncryptDecryptRoundTrip(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		{0},
		{0x00, 0xFF, 0xAA, 0x55},
		bytes.Repeat([]byte{0x42}, 64),
		bytes.Repeat([]byte{0x00}, 1500), // ~MTU
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, plaintext []byte) {
		// Cap plaintext size to keep fuzzer execs/sec reasonable. Anything
		// larger than ~64KB is uninteresting for cryptographic correctness.
		if len(plaintext) > 64*1024 {
			plaintext = plaintext[:64*1024]
		}

		for _, method := range codecMethodsToFuzz {
			codec, err := NewCodec(method, benchKey)
			if err != nil {
				t.Fatalf("NewCodec(%d): %v", method, err)
			}

			ct, err := codec.Encrypt(plaintext)
			if err != nil {
				// Encryption failure on valid plaintext is a real bug.
				t.Fatalf("Encrypt(method=%d, len=%d) failed: %v", method, len(plaintext), err)
			}

			pt, err := codec.Decrypt(ct)
			if err != nil {
				t.Fatalf("Decrypt(method=%d) failed: %v", method, err)
			}

			if !bytes.Equal(pt, plaintext) {
				t.Fatalf("round-trip mismatch (method=%d): got %x want %x", method, pt, plaintext)
			}
		}
	})
}

// FuzzCodecDecodeStringAndDecrypt stresses the public string entry point
// (base64-style label decode followed by codec decrypt). This is the surface
// that consumes DNS labels arriving on the public network.
func FuzzCodecDecodeStringAndDecrypt(f *testing.F) {
	seeds := []string{
		"",
		"A",
		"AAAA",
		"!!!invalid!!!",
		"////====",
		string(bytes.Repeat([]byte{'a'}, 256)),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		for _, method := range codecMethodsToFuzz {
			codec, err := NewCodec(method, benchKey)
			if err != nil {
				t.Fatalf("NewCodec(%d): %v", method, err)
			}
			// Contract: returns error or value — never panics.
			_, _ = codec.DecodeStringAndDecrypt(input)
		}
	})
}

// FuzzCodecDecodeAndDecrypt is the bytes-mode sibling. This path exercises
// the pooled buffer reuse plumbing (Step 13 size-class pools).
func FuzzCodecDecodeAndDecrypt(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		{'A'},
		{'A', 'A', 'A', 'A'},
		bytes.Repeat([]byte{'a'}, 256),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		for _, method := range codecMethodsToFuzz {
			codec, err := NewCodec(method, benchKey)
			if err != nil {
				t.Fatalf("NewCodec(%d): %v", method, err)
			}
			_, _ = codec.DecodeAndDecrypt(input)
		}
	})
}
