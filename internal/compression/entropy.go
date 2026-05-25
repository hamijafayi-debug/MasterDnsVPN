// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package compression

import "math"

// Step 12 — entropy heuristic.
//
// CompressPayload pays the full cost of encoder dispatch + buffer allocation
// even when the payload is incompressible (already encrypted / already
// compressed / pure random). The legacy guard `len(compData) >= len(data)`
// fires too late: the cycles are already spent.
//
// LooksHighEntropy provides a cheap pre-check. It samples up to entropySampleSize
// bytes (head + tail + middle) and computes Shannon entropy in deci-bits/byte
// (range 0..80). Callers compare against a threshold (e.g. 76 means 7.6
// bits/byte, very close to random). If the sample looks highly random, the
// payload is unlikely to compress; skipping the encoder is a clear win.
//
// Correctness:
//   - This is a heuristic, not a guarantee. False negatives are possible (a
//     payload that *would* compress is skipped) but rare and pay no extra cost
//     compared to the legacy "compress then discard" path — net neutral.
//   - False positives (low-entropy data marked high-entropy) require pathological
//     samples; the multi-region sampling drops the probability further.
//   - Wire-compatible: this only changes *whether* a payload is sent compressed
//     or not. Receivers handle both cases natively (CompressionType=TypeOff
//     means raw).

const (
	// entropySampleSize caps the sample we feed into the histogram. 256 bytes is
	// large enough to make the histogram statistically meaningful but small
	// enough to scan in well under a microsecond even on a slow CPU.
	entropySampleSize = 256

	// entropyMinPayload is the smallest payload we bother sampling. Below this
	// size the per-byte cost of compression is dominated by encoder setup, and
	// the legacy "compressed >= original" guard is already a fine filter.
	entropyMinPayload = 512

	// EntropyMaxDeciBits is the maximum value LooksHighEntropy can return:
	// 8 bits/byte × 10 = 80. Used by callers that want to clamp config knobs
	// into the valid range.
	EntropyMaxDeciBits = 80
)

// shannonLog2 is precomputed -p*log2(p) * 10 for the 256 possible probability
// numerators when the sample size is exactly entropySampleSize. We can't keep
// a single table because the actual sample length may be < entropySampleSize
// when the payload is short. To keep the hot path branch-free we compute log2
// on the fly with a small integer-friendly approximation.
//
// Strategy: H = -sum(p_i * log2(p_i)) where p_i = count_i / N.
// Expand: H = log2(N) - (1/N) * sum(count_i * log2(count_i)).
//
// log2(N) and log2(count) are computed via a 256-entry lookup that returns
// log2 in deci-bits (i.e. log2(x) * 10) for x in [1..256]. That keeps the
// entire calculation in integer arithmetic.

var log2DeciTable [257]int32

func init() {
	// log2 lookup in deci-bits. Index 0 is left as 0 (the formula multiplies
	// by count, so a 0 count contributes 0 regardless). We compute once at
	// init via math.Log2 so the hot path stays in pure integer arithmetic.
	for i := 1; i <= 256; i++ {
		log2DeciTable[i] = int32(math.Round(math.Log2(float64(i)) * 10))
	}
}

// EntropyDeciBits returns the Shannon entropy of a sampled portion of data,
// expressed in deci-bits per byte (i.e. entropy * 10, in [0, 80]).
//
// For input shorter than entropyMinPayload, it returns 0 (caller should
// treat that as "don't bother skipping — too small to matter").
//
// The function allocates nothing and is safe for concurrent use.
func EntropyDeciBits(data []byte) int32 {
	n := len(data)
	if n < entropyMinPayload {
		return 0
	}

	// Sample up to entropySampleSize bytes. For payloads larger than 3×
	// entropySampleSize, sample three regions (head/middle/tail) to defeat
	// adversarial layouts where only a small header/footer is entropic.
	var hist [256]int32
	var sampled int32

	switch {
	case n <= entropySampleSize:
		for _, b := range data {
			hist[b]++
		}
		sampled = int32(n)
	case n <= 3*entropySampleSize:
		// One contiguous head sample.
		for _, b := range data[:entropySampleSize] {
			hist[b]++
		}
		sampled = entropySampleSize
	default:
		third := int32(entropySampleSize / 3)
		// Head
		for _, b := range data[:third] {
			hist[b]++
		}
		// Middle
		mid := n/2 - int(third/2)
		for _, b := range data[mid : mid+int(third)] {
			hist[b]++
		}
		// Tail
		tailStart := n - int(third)
		for _, b := range data[tailStart : tailStart+int(third)] {
			hist[b]++
		}
		sampled = 3 * third
	}

	// H = log2(N) - (1/N) * sum(count_i * log2(count_i))   [in bits/byte]
	// Multiply both sides by 10 to stay in deci-bits.
	if sampled == 0 {
		return 0
	}

	// log2(sampled) in deci-bits, computed by lookup for sampled <= 256, else by
	// formula: log2(N) = log2(N/k) + log2(k) where we pick k as a power of two.
	log2NDeci := log2DeciInt(int(sampled))

	var sumCountLogDeci int64
	for _, c := range hist {
		if c == 0 {
			continue
		}
		// c is in [1..sampled], sampled <= entropySampleSize = 256
		sumCountLogDeci += int64(c) * int64(log2DeciTable[c])
	}

	// H_deci = log2NDeci - sumCountLogDeci / sampled.
	hDeci := int32(int64(log2NDeci) - sumCountLogDeci/int64(sampled))
	if hDeci < 0 {
		hDeci = 0
	}
	if hDeci > EntropyMaxDeciBits {
		hDeci = EntropyMaxDeciBits
	}
	return hDeci
}

// log2DeciInt extends the [1..256] table to larger integers by factoring out
// powers of two: log2(N) = bits(N>>k) + ... For our use, sampled is bounded
// by 3*entropySampleSize/3*3 = 256 in the multi-region branch and by n in
// the head/full branches with n <= entropySampleSize, so we only need the
// table. Keep the helper for completeness if we ever raise the sample size.
func log2DeciInt(n int) int32 {
	if n <= 0 {
		return 0
	}
	if n <= 256 {
		return log2DeciTable[n]
	}
	// fall back to scaling: log2(n) = log2(n/256) + log2(256) = log2(n/256) + 80.
	scaled := n
	add := int32(0)
	for scaled > 256 {
		scaled >>= 1
		add += 10
	}
	return log2DeciTable[scaled] + add
}

// LooksHighEntropy returns true when the sampled entropy of data is greater
// than or equal to thresholdDeciBits. A threshold of 0 disables the check
// (callers should treat 0 as "always compress, never skip").
//
// thresholdDeciBits is clamped to [1, EntropyMaxDeciBits]. The historically
// useful range for "almost certainly incompressible" is 75..78 — i.e. 7.5..7.8
// bits/byte. Below 70 the heuristic risks skipping payloads that the encoder
// could still compress 5-15% (e.g. base64 text).
func LooksHighEntropy(data []byte, thresholdDeciBits int32) bool {
	if thresholdDeciBits <= 0 {
		return false
	}
	if thresholdDeciBits > EntropyMaxDeciBits {
		thresholdDeciBits = EntropyMaxDeciBits
	}
	if len(data) < entropyMinPayload {
		return false
	}
	return EntropyDeciBits(data) >= thresholdDeciBits
}
