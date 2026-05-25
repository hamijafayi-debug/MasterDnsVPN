// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package vpnproto

import (
	"testing"

	Enums "masterdnsvpn-go/internal/enums"
	"masterdnsvpn-go/internal/streamutil"
)

// BenchmarkBuildRaw_alloc measures the legacy allocate-every-call path that
// callers still use in cold spots. It is the baseline for the BuildRawInto +
// pool-backed scratch comparison below.
func BenchmarkBuildRaw_alloc(b *testing.B) {
	payload := make([]byte, 1200) // typical MTU-ish payload
	for i := range payload {
		payload[i] = byte(i)
	}
	opts := BuildOptions{
		SessionID:   42,
		PacketType:  Enums.PACKET_STREAM_DATA,
		StreamID:    7,
		SequenceNum: 9,
		Payload:     payload,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw, err := BuildRaw(opts)
		if err != nil {
			b.Fatal(err)
		}
		_ = raw
	}
}

// BenchmarkBuildRawInto_pool measures the new BuildRawInto entry point fed
// with a pooled scratch buffer (basic Get/Put API). The slice-header escape
// is the only remaining allocation here — see _poolPtr below for the
// zero-alloc variant.
func BenchmarkBuildRawInto_pool(b *testing.B) {
	payload := make([]byte, 1200)
	for i := range payload {
		payload[i] = byte(i)
	}
	opts := BuildOptions{
		SessionID:   42,
		PacketType:  Enums.PACKET_STREAM_DATA,
		StreamID:    7,
		SequenceNum: 9,
		Payload:     payload,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scratch := streamutil.Get(MaxHeaderRawSize() + len(payload))
		raw, err := BuildRawInto(scratch[:0], opts)
		if err != nil {
			b.Fatal(err)
		}
		_ = raw
		streamutil.Put(scratch)
	}
}

// BenchmarkBuildRawInto_poolPtr is the zero-alloc form actually used by the
// client send hot path (tunnel_query.go). It must hit 0 allocs/op once the
// pool is warm — that is the canonical "before vs after" win for step 2.
func BenchmarkBuildRawInto_poolPtr(b *testing.B) {
	payload := make([]byte, 1200)
	for i := range payload {
		payload[i] = byte(i)
	}
	opts := BuildOptions{
		SessionID:   42,
		PacketType:  Enums.PACKET_STREAM_DATA,
		StreamID:    7,
		SequenceNum: 9,
		Payload:     payload,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scratchPtr := streamutil.GetPtr(MaxHeaderRawSize() + len(payload))
		raw, err := BuildRawInto((*scratchPtr)[:0], opts)
		if err != nil {
			b.Fatal(err)
		}
		_ = raw
		streamutil.PutPtr(scratchPtr)
	}
}

// BenchmarkBuildRawInto_smallPayload exercises the small-payload tier — the
// session-accept / ARQ control message size — to make sure the pool tier
// selection still wins at the bottom of the distribution.
func BenchmarkBuildRawInto_smallPayload(b *testing.B) {
	payload := make([]byte, 32)
	opts := BuildOptions{
		SessionID:  42,
		PacketType: Enums.PACKET_STREAM_DATA,
		StreamID:   1,
		Payload:    payload,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scratch := streamutil.Get(MaxHeaderRawSize() + len(payload))
		raw, err := BuildRawInto(scratch[:0], opts)
		if err != nil {
			b.Fatal(err)
		}
		_ = raw
		streamutil.Put(scratch)
	}
}
