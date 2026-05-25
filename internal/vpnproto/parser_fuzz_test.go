// ==============================================================================
// MasterDnsVPN
// Step 22 — Fuzz targets for the VPN protocol parser.
//
// These targets stress the four untrusted-input entry points that touch
// network bytes directly:
//   - Parse / ParseAtOffset                — base header + extension decoding
//   - ForEachPackedControlBlock            — Type 14 packed-block iteration
//   - DescribePackedControlBlocks          — debug-render path (must never panic)
//
// We do not assert on parse success — the goal is to guarantee that no input,
// however malformed, can crash the parser. Crashes auto-save to
// internal/vpnproto/testdata/fuzz/<TargetName>/<sha> and become permanent
// regression seeds.
// ==============================================================================

package vpnproto

import (
	"bytes"
	"testing"
)

// validMinimalPacket builds the smallest legal packet of type 1 (PACKET_DATA-ish:
// no extensions). Used as a seed so the fuzzer starts near the valid surface.
func validMinimalPacket() []byte {
	// Layout (no extensions): [sid][ptype][cookie][check]
	buf := make([]byte, 4)
	buf[0] = 0x42  // session id
	buf[1] = 0x01  // packet type with no extensions (depends on packetFlags table)
	buf[2] = 0x77  // session cookie
	buf[3] = computeHeaderCheckByte(buf[:3])
	return buf
}

// validStreamPacket builds a legal packet that carries the stream extension.
func validStreamPacket() []byte {
	// Find a packet type that has the stream extension set in the table.
	// Use packetType=2 by convention; fall back to a search if it doesn't.
	ptype := uint8(0)
	for i := 0; i < 256; i++ {
		f := packetFlags[i]
		if f&packetFlagValid != 0 && f&packetFlagStream != 0 &&
			f&packetFlagSequence == 0 && f&packetFlagFragment == 0 &&
			f&packetFlagCompression == 0 {
			ptype = uint8(i)
			break
		}
	}
	if ptype == 0 {
		return validMinimalPacket()
	}
	buf := make([]byte, 6)
	buf[0] = 0x10
	buf[1] = ptype
	buf[2] = 0x12 // streamID high
	buf[3] = 0x34 // streamID low
	buf[4] = 0x77 // cookie
	buf[5] = computeHeaderCheckByte(buf[:5])
	return buf
}

// FuzzParse exercises the full packet-parse entry point with random bytes,
// short bytes, and a couple of valid seeds. The contract is: never panic.
func FuzzParse(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		{0x00},
		{0xFF, 0xFF},
		{0x01, 0x02, 0x03, 0x04},
		bytes.Repeat([]byte{0x00}, 16),
		bytes.Repeat([]byte{0xFF}, 32),
		bytes.Repeat([]byte{0xAA}, 4096),
		validMinimalPacket(),
		validStreamPacket(),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// We don't care about success/failure — only that Parse returns.
		_, _ = Parse(data)
	})
}

// FuzzParseAtOffset adds a second integer dimension to catch off-by-ones
// around the bounded-buffer entry point.
func FuzzParseAtOffset(f *testing.F) {
	seeds := []struct {
		data   []byte
		offset int
	}{
		{nil, 0},
		{[]byte{}, 0},
		{validMinimalPacket(), 0},
		{append([]byte{0xCC, 0xDD}, validMinimalPacket()...), 2},
		{bytes.Repeat([]byte{0xFF}, 64), 0},
		{bytes.Repeat([]byte{0xFF}, 64), 63},
	}
	for _, s := range seeds {
		f.Add(s.data, s.offset)
	}

	f.Fuzz(func(t *testing.T, data []byte, offset int) {
		_, _ = ParseAtOffset(data, offset)
	})
}

// FuzzForEachPackedControlBlock exercises the Type-14 iterator. The block
// size is a constant, so the iterator must tolerate inputs of any length,
// including non-multiples of PackedControlBlockSize.
func FuzzForEachPackedControlBlock(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		bytes.Repeat([]byte{0x00}, PackedControlBlockSize),
		bytes.Repeat([]byte{0xFF}, PackedControlBlockSize*4),
		bytes.Repeat([]byte{0xAA}, PackedControlBlockSize*4+3), // misaligned
		bytes.Repeat([]byte{0x55}, PackedControlBlockSize-1),   // shorter than one block
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		// Yield always returns true so the loop completes.
		ForEachPackedControlBlock(payload, func(_ uint8, _ uint16, _ uint16, _ uint8, _ uint8) bool {
			return true
		})
	})
}

// FuzzDescribePackedControlBlocks exercises the debug-rendering path which
// must remain panic-free even when fed completely random bytes.
func FuzzDescribePackedControlBlocks(f *testing.F) {
	seeds := []struct {
		payload  []byte
		maxKinds int
	}{
		{nil, 4},
		{[]byte{}, 4},
		{bytes.Repeat([]byte{0xFF}, PackedControlBlockSize*8), 4},
		{bytes.Repeat([]byte{0x01}, PackedControlBlockSize*2+1), 1},
		{bytes.Repeat([]byte{0x00}, 1), 0}, // maxKinds<=0 default branch
	}
	for _, s := range seeds {
		f.Add(s.payload, s.maxKinds)
	}

	f.Fuzz(func(t *testing.T, payload []byte, maxKinds int) {
		// Clamp maxKinds to a sane range to keep the fuzzer's mutation surface
		// useful — extreme values are not interesting and just slow execution.
		if maxKinds < -8 {
			maxKinds = -8
		}
		if maxKinds > 64 {
			maxKinds = 64
		}
		_ = DescribePackedControlBlocks(payload, maxKinds)
	})
}
