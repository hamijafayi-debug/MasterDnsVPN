// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package vpnproto

import (
	"bytes"
	"testing"

	Enums "masterdnsvpn-go/internal/enums"
)

// TestBuildRawIntoMatchesBuildRaw guarantees byte-for-byte parity between the
// new pool-aware entry point and the legacy allocate-every-call form. Any
// drift here would silently break wire compatibility.
func TestBuildRawIntoMatchesBuildRaw(t *testing.T) {
	payload := []byte("hello world payload bytes")
	opts := BuildOptions{
		SessionID:   17,
		PacketType:  Enums.PACKET_STREAM_DATA,
		StreamID:    0xBEEF,
		SequenceNum: 0xCAFE,
		Payload:     payload,
	}

	legacy, err := BuildRaw(opts)
	if err != nil {
		t.Fatalf("BuildRaw: %v", err)
	}

	scratch := make([]byte, 0, 8192)
	pooled, err := BuildRawInto(scratch, opts)
	if err != nil {
		t.Fatalf("BuildRawInto: %v", err)
	}

	if !bytes.Equal(legacy, pooled) {
		t.Fatalf("mismatch:\nlegacy=%x\npooled=%x", legacy, pooled)
	}
}

// TestBuildRawIntoFallsBackToAlloc covers the case where the caller supplies
// a too-small scratch slice (or nil) — BuildRawInto must still produce a
// correct frame by allocating a fresh slice.
func TestBuildRawIntoFallsBackToAlloc(t *testing.T) {
	payload := []byte("payload")
	opts := BuildOptions{
		SessionID:  3,
		PacketType: Enums.PACKET_STREAM_DATA,
		StreamID:   1,
		Payload:    payload,
	}

	legacy, err := BuildRaw(opts)
	if err != nil {
		t.Fatalf("BuildRaw: %v", err)
	}

	// nil dst — must allocate
	got, err := BuildRawInto(nil, opts)
	if err != nil {
		t.Fatalf("BuildRawInto(nil): %v", err)
	}
	if !bytes.Equal(legacy, got) {
		t.Fatalf("nil-dst mismatch")
	}

	// too-small cap — must allocate, not panic
	tiny := make([]byte, 0, 1)
	got, err = BuildRawInto(tiny, opts)
	if err != nil {
		t.Fatalf("BuildRawInto(tiny): %v", err)
	}
	if !bytes.Equal(legacy, got) {
		t.Fatalf("tiny-dst mismatch")
	}
}

// TestBuildRawIntoReturnsSubsliceWhenDstFits ensures the happy-path returns a
// slice aliased onto the caller's buffer when there is enough capacity. This
// is what enables the pool-Put-after-use pattern in the client hot path.
func TestBuildRawIntoReturnsSubsliceWhenDstFits(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAA}, 100)
	opts := BuildOptions{
		SessionID:  9,
		PacketType: Enums.PACKET_STREAM_DATA,
		StreamID:   1,
		Payload:    payload,
	}

	scratch := make([]byte, 0, 4096)
	raw, err := BuildRawInto(scratch, opts)
	if err != nil {
		t.Fatalf("BuildRawInto: %v", err)
	}
	if &raw[:1][0] != &scratch[:1][0] {
		t.Fatalf("BuildRawInto did not alias the supplied dst slice")
	}
}
