// ==============================================================================
// MasterDnsVPN
// Step 11 — DNS Parser Zero-Copy benchmarks (baseline + post-refactor).
// ==============================================================================
package dnsparser

import (
	"testing"

	Enums "masterdnsvpn-go/internal/enums"
)

// Short single-question query — the dominant shape on the server hot path.
func buildBenchQueryShort() []byte {
	return buildDNSQuery(0xABCD, "example.com", Enums.DNS_RECORD_TYPE_A, true)
}

// Longer hostname with many labels (still single question).
func buildBenchQueryLongName() []byte {
	return buildDNSQuery(0x1234, "a1.b2.c3.d4.e5.f6.g7.h8.tunnel.example.com",
		Enums.DNS_RECORD_TYPE_TXT, true)
}

// Multi-question query (rarely seen in the wild, but exercises the slow path).
func buildBenchQueryMulti() []byte {
	return buildMultiQuestionDNSQuery(0xBEEF,
		[]liteQuestionSpec{
			{Name: "alpha.example.com", Type: Enums.DNS_RECORD_TYPE_A, Class: Enums.DNSQ_CLASS_IN},
			{Name: "beta.example.org", Type: Enums.DNS_RECORD_TYPE_AAAA, Class: Enums.DNSQ_CLASS_IN},
		}, true)
}

func BenchmarkParseDNSRequestLiteShort(b *testing.B) {
	query := buildBenchQueryShort()
	b.SetBytes(int64(len(query)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := ParseDNSRequestLite(query)
		if err != nil {
			b.Fatalf("parse: %v", err)
		}
		if !p.HasQuestion {
			b.Fatalf("no question parsed")
		}
	}
}

func BenchmarkParseDNSRequestLiteLongName(b *testing.B) {
	query := buildBenchQueryLongName()
	b.SetBytes(int64(len(query)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := ParseDNSRequestLite(query)
		if err != nil {
			b.Fatalf("parse: %v", err)
		}
		if !p.HasQuestion {
			b.Fatalf("no question parsed")
		}
	}
}

func BenchmarkParsePacketLiteMulti(b *testing.B) {
	query := buildBenchQueryMulti()
	b.SetBytes(int64(len(query)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := ParsePacketLite(query)
		if err != nil {
			b.Fatalf("parse: %v", err)
		}
		if !p.HasQuestion {
			b.Fatalf("no question parsed")
		}
	}
}

func BenchmarkBuildEmptyNoErrorResponseShort(b *testing.B) {
	query := buildBenchQueryShort()
	b.SetBytes(int64(len(query)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := BuildEmptyNoErrorResponse(query)
		if err != nil {
			b.Fatalf("build: %v", err)
		}
		if len(resp) < dnsHeaderSize {
			b.Fatalf("short response: %d", len(resp))
		}
	}
}

func BenchmarkBuildEmptyNoErrorResponseFromLiteShort(b *testing.B) {
	query := buildBenchQueryShort()
	parsed, err := ParseDNSRequestLite(query)
	if err != nil {
		b.Fatalf("parse: %v", err)
	}
	b.SetBytes(int64(len(query)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := BuildEmptyNoErrorResponseFromLite(query, parsed)
		if err != nil {
			b.Fatalf("build: %v", err)
		}
		if len(resp) < dnsHeaderSize {
			b.Fatalf("short response: %d", len(resp))
		}
	}
}
