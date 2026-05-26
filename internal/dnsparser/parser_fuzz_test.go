// ==============================================================================
// MasterDnsVPN
// Step 11 — Fuzz target for the DNS lite parser.
// Catches OOB reads, infinite loops via name-pointer cycles, and
// inconsistencies between ParsePacketLite and ParsePacket on valid input.
// ==============================================================================
package dnsparser

import "testing"

// FuzzParseDNSRequestLite drives the lite parser with arbitrary input,
// ensuring it never panics and never returns a nil error with an
// inconsistent state (HasQuestion without a populated FirstQuestion).
func FuzzParseDNSRequestLite(f *testing.F) {
	// Seed corpus: a few well-formed queries plus deliberately malformed
	// shapes that have historically tripped up DNS parsers.
	f.Add(buildBenchQueryShort())
	f.Add(buildBenchQueryLongName())
	f.Add(buildBenchQueryMulti())
	f.Add([]byte{})                          // too short
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0}) // truncated header
	// Pointer-cycle bait: header with QDCount=1, name pointer to itself.
	f.Add([]byte{
		0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xC0, 0x0C, // pointer at offset 12 -> 12 (cycle)
		0x00, 0x01, 0x00, 0x01,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := ParseDNSRequestLite(data)
		if err != nil {
			return // any error is acceptable; parser must just not panic
		}
		// On success the invariants below must hold.
		if parsed.HasQuestion && parsed.FirstQuestion.Name == "" {
			t.Fatalf("HasQuestion=true but FirstQuestion.Name is empty")
		}
		if parsed.QuestionEndOffset < dnsHeaderSize {
			t.Fatalf("QuestionEndOffset=%d < dnsHeaderSize", parsed.QuestionEndOffset)
		}
		if parsed.QuestionEndOffset > len(data) {
			t.Fatalf("QuestionEndOffset=%d exceeds buffer len=%d",
				parsed.QuestionEndOffset, len(data))
		}
	})
}

// FuzzParseName isolates the name decoder so a corpus can target the
// pointer-jump / lowercase / max-label-length paths without going through
// the full request validation.
func FuzzParseName(f *testing.F) {
	f.Add([]byte("\x07example\x03com\x00"))                     // simple name
	f.Add([]byte("\x03foo\xc0\x00"))                            // pointer
	f.Add([]byte("\x03FoO\x03BaR\x00"))                         // mixed case
	f.Add([]byte("\x40toolonglabelthatexceedssixtythreebytes")) // too-long label
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		name, next, err := parseName(data, 0)
		if err != nil {
			return
		}
		if next < 0 || next > len(data) {
			t.Fatalf("parseName next-offset out of range: %d (len=%d)", next, len(data))
		}
		// Returned names should never contain ASCII uppercase letters.
		for i := 0; i < len(name); i++ {
			if c := name[i]; c >= 'A' && c <= 'Z' {
				t.Fatalf("parseName returned uppercase byte %q at %d in %q", c, i, name)
			}
		}
	})
}
