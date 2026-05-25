// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package vpnproto

import "masterdnsvpn-go/internal/security"

type BuildOptions struct {
	SessionID       uint8
	PacketType      uint8
	SessionCookie   uint8
	StreamID        uint16
	SequenceNum     uint16
	FragmentID      uint8
	TotalFragments  uint8
	CompressionType uint8
	Payload         []byte
}

func BuildRaw(opts BuildOptions) ([]byte, error) {
	return BuildRawInto(nil, opts)
}

// BuildRawInto serialises a packet exactly like BuildRaw, but reuses the
// caller-provided dst slice when it has enough capacity. It always returns
// the sub-slice that contains the serialised bytes (length =
// headerLen+len(opts.Payload)).
//
// Pass dst=nil (or a zero-cap slice) to fall back to allocating a fresh
// buffer — the existing BuildRaw call site is exactly this case.
//
// Typical hot-path use:
//
//	scratch := streamutil.Get(vpnproto.MaxHeaderRawSize() + len(payload))
//	raw, _  := vpnproto.BuildRawInto(scratch, opts)
//	... consume raw (e.g. encrypt+encode) ...
//	streamutil.Put(scratch)
//
// The returned slice always aliases dst when dst is large enough — callers
// must not Put dst back to the pool while raw is still in use.
func BuildRawInto(dst []byte, opts BuildOptions) ([]byte, error) {
	flags := packetFlags[opts.PacketType]
	if flags&packetFlagValid == 0 {
		return nil, ErrInvalidPacketType
	}

	headerLen := 2 + integrityLength // SessionID + PacketType + SessionCookie + Integrity
	if flags&packetFlagStream != 0 {
		headerLen += 2
	}
	if flags&packetFlagSequence != 0 {
		headerLen += 2
	}
	if flags&packetFlagFragment != 0 {
		headerLen += 2
	}
	if flags&packetFlagCompression != 0 {
		headerLen++
	}

	total := headerLen + len(opts.Payload)
	var raw []byte
	if cap(dst) >= total {
		raw = dst[:total]
	} else {
		raw = make([]byte, total)
	}
	raw[0] = opts.SessionID
	raw[1] = opts.PacketType
	offset := 2

	if flags&packetFlagStream != 0 {
		raw[offset] = byte(opts.StreamID >> 8)
		raw[offset+1] = byte(opts.StreamID)
		offset += 2
	}
	if flags&packetFlagSequence != 0 {
		raw[offset] = byte(opts.SequenceNum >> 8)
		raw[offset+1] = byte(opts.SequenceNum)
		offset += 2
	}
	if flags&packetFlagFragment != 0 {
		raw[offset] = opts.FragmentID
		raw[offset+1] = opts.TotalFragments
		offset += 2
	}
	if flags&packetFlagCompression != 0 {
		raw[offset] = opts.CompressionType
		offset++
	}

	raw[offset] = opts.SessionCookie
	offset++
	raw[offset] = computeHeaderCheckByte(raw[:offset])
	offset++
	copy(raw[offset:], opts.Payload)
	return raw, nil
}

func BuildEncoded(opts BuildOptions, codec *security.Codec) (string, error) {
	raw, err := BuildRaw(opts)
	if err != nil {
		return "", err
	}
	if codec == nil {
		return "", ErrCodecUnavailable
	}
	return codec.EncryptAndEncode(raw)
}

func HeaderRawSize(packetType uint8) int {
	flags := packetFlags[packetType]
	if flags&packetFlagValid == 0 {
		return 0
	}

	size := 2 + integrityLength
	if flags&packetFlagStream != 0 {
		size += 2
	}
	if flags&packetFlagSequence != 0 {
		size += 2
	}
	if flags&packetFlagFragment != 0 {
		size += 2
	}
	if flags&packetFlagCompression != 0 {
		size++
	}
	return size
}

func MaxHeaderRawSize() int {
	maxSize := 0
	for packetType := range len(packetFlags) {
		size := HeaderRawSize(uint8(packetType))
		if size > maxSize {
			maxSize = size
		}
	}
	return maxSize
}

func MaxHeaderPacketType() uint8 {
	var bestType uint8
	maxSize := 0
	for packetType := range len(packetFlags) {
		size := HeaderRawSize(uint8(packetType))
		if size > maxSize {
			maxSize = size
			bestType = uint8(packetType)
		}
	}
	return bestType
}
