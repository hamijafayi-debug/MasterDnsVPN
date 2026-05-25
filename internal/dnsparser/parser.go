// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
package dnsparser

import (
	"encoding/binary"
	"errors"
)

var (
	ErrPacketTooShort  = errors.New("dns packet too short")
	ErrInvalidName     = errors.New("invalid dns name")
	ErrInvalidQuestion = errors.New("invalid dns question section")
	ErrInvalidAnswer   = errors.New("invalid dns resource record section")
	ErrNotDNSRequest   = errors.New("packet does not look like a supported dns request")
)

const (
	dnsHeaderSize = 12
	maxNameJumps  = 10
	// maxDNSName is the RFC 1035 hard limit on a wire-format domain name
	// (255 bytes including length octets and the trailing root). Names
	// fit on the stack comfortably at this size.
	maxDNSName = 255
)

type Header struct {
	ID      uint16
	Flags   uint16
	QR      uint8
	OpCode  uint8
	AA      uint8
	TC      uint8
	RD      uint8
	RA      uint8
	Z       uint8
	RCode   uint8
	QDCount uint16
	ANCount uint16
	NSCount uint16
	ARCount uint16
}

type Question struct {
	Name  string
	Type  uint16
	Class uint16
}

type ResourceRecord struct {
	Name  string
	Type  uint16
	Class uint16
	TTL   uint32
	RDLen uint16
	RData []byte
}

type Packet struct {
	Header      Header
	Questions   []Question
	Answers     []ResourceRecord
	Authorities []ResourceRecord
	Additional  []ResourceRecord
}

type LitePacket struct {
	Header            Header
	Questions         []Question
	FirstQuestion     Question
	HasQuestion       bool
	QuestionEndOffset int
}

func ParsePacketLite(data []byte) (LitePacket, error) {
	if len(data) < dnsHeaderSize {
		return LitePacket{}, ErrPacketTooShort
	}

	header := parseHeader(data)
	return parsePacketLiteWithHeader(data, header)
}

func ParseDNSRequestLite(data []byte) (LitePacket, error) {
	if len(data) < dnsHeaderSize {
		return LitePacket{}, ErrPacketTooShort
	}

	header := parseHeader(data)
	if !isLikelyDNSRequestHeader(header) {
		return LitePacket{}, ErrNotDNSRequest
	}

	return parsePacketLiteWithHeader(data, header)
}

func parsePacketLiteWithHeader(data []byte, header Header) (LitePacket, error) {
	packet := LitePacket{Header: header}
	if header.QDCount == 0 {
		packet.QuestionEndOffset = dnsHeaderSize
		return packet, nil
	}

	// Fast path: the overwhelmingly common shape on the server hot path
	// is a single-question query. Parse only the first question (no
	// []Question slice allocation) and fast-skip the remaining ones.
	if header.QDCount == 1 {
		first, offset, err := parseFirstQuestion(data, dnsHeaderSize)
		if err != nil {
			return LitePacket{}, ErrInvalidQuestion
		}
		packet.FirstQuestion = first
		packet.HasQuestion = true
		packet.QuestionEndOffset = offset
		return packet, nil
	}

	// Multi-question slow path: keep populating the full slice for
	// callers that need it (currently only response_test exercises it).
	questions, offset, err := parseQuestions(data, dnsHeaderSize, int(header.QDCount))
	if err != nil {
		return LitePacket{}, ErrInvalidQuestion
	}

	packet.Questions = questions
	packet.QuestionEndOffset = offset
	packet.HasQuestion = len(questions) > 0
	if packet.HasQuestion {
		packet.FirstQuestion = questions[0]
	}
	return packet, nil
}

// parseFirstQuestion parses exactly one DNS question starting at offset,
// returning the parsed Question and the offset just past it. It allocates
// only the name string (no []Question slice).
func parseFirstQuestion(data []byte, offset int) (Question, int, error) {
	name, nextOffset, err := parseName(data, offset)
	if err != nil {
		return Question{}, offset, ErrInvalidQuestion
	}
	if nextOffset+4 > len(data) {
		return Question{}, nextOffset, ErrInvalidQuestion
	}
	q := Question{
		Name:  name,
		Type:  binary.BigEndian.Uint16(data[nextOffset : nextOffset+2]),
		Class: binary.BigEndian.Uint16(data[nextOffset+2 : nextOffset+4]),
	}
	return q, nextOffset + 4, nil
}

func ParsePacket(data []byte) (Packet, error) {
	if len(data) < dnsHeaderSize {
		return Packet{}, ErrPacketTooShort
	}

	header := parseHeader(data)
	offset := dnsHeaderSize

	questions, nextOffset, err := parseQuestions(data, offset, int(header.QDCount))
	if err != nil {
		return Packet{}, err
	}
	offset = nextOffset

	answers, nextOffset, err := parseResourceRecords(data, offset, int(header.ANCount))
	if err != nil {
		return Packet{}, err
	}
	offset = nextOffset

	authorities, nextOffset, err := parseResourceRecords(data, offset, int(header.NSCount))
	if err != nil {
		return Packet{}, err
	}
	offset = nextOffset

	additional, _, err := parseResourceRecords(data, offset, int(header.ARCount))
	if err != nil {
		return Packet{}, err
	}

	return Packet{
		Header:      header,
		Questions:   questions,
		Answers:     answers,
		Authorities: authorities,
		Additional:  additional,
	}, nil
}

func parseHeader(data []byte) Header {
	flags := binary.BigEndian.Uint16(data[2:4])
	return Header{
		ID:      binary.BigEndian.Uint16(data[0:2]),
		Flags:   flags,
		QR:      uint8((flags >> 15) & 0x1),
		OpCode:  uint8((flags >> 11) & 0xF),
		AA:      uint8((flags >> 10) & 0x1),
		TC:      uint8((flags >> 9) & 0x1),
		RD:      uint8((flags >> 8) & 0x1),
		RA:      uint8((flags >> 7) & 0x1),
		Z:       uint8((flags >> 4) & 0x7),
		RCode:   uint8(flags & 0xF),
		QDCount: binary.BigEndian.Uint16(data[4:6]),
		ANCount: binary.BigEndian.Uint16(data[6:8]),
		NSCount: binary.BigEndian.Uint16(data[8:10]),
		ARCount: binary.BigEndian.Uint16(data[10:12]),
	}
}

func parseQuestions(data []byte, offset int, count int) ([]Question, int, error) {
	if count == 0 {
		return nil, offset, nil
	}

	questions := make([]Question, count)
	for i := range count {
		name, nextOffset, err := parseName(data, offset)
		if err != nil {
			return nil, offset, ErrInvalidQuestion
		}
		offset = nextOffset

		if offset+4 > len(data) {
			return nil, offset, ErrInvalidQuestion
		}

		questions[i] = Question{
			Name:  name,
			Type:  binary.BigEndian.Uint16(data[offset : offset+2]),
			Class: binary.BigEndian.Uint16(data[offset+2 : offset+4]),
		}
		offset += 4
	}

	return questions, offset, nil
}

func parseResourceRecords(data []byte, offset int, count int) ([]ResourceRecord, int, error) {
	if count == 0 {
		return nil, offset, nil
	}

	records := make([]ResourceRecord, count)
	for i := range count {
		name, nextOffset, err := parseName(data, offset)
		if err != nil {
			return nil, offset, ErrInvalidAnswer
		}
		offset = nextOffset

		if offset+10 > len(data) {
			return nil, offset, ErrInvalidAnswer
		}

		rType := binary.BigEndian.Uint16(data[offset : offset+2])
		rClass := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		ttl := binary.BigEndian.Uint32(data[offset+4 : offset+8])
		rdLen := binary.BigEndian.Uint16(data[offset+8 : offset+10])
		offset += 10

		end := offset + int(rdLen)
		if end > len(data) {
			return nil, offset, ErrInvalidAnswer
		}

		records[i] = ResourceRecord{
			Name:  name,
			Type:  rType,
			Class: rClass,
			TTL:   ttl,
			RDLen: rdLen,
			RData: data[offset:end],
		}
		offset = end
	}

	return records, offset, nil
}

func parseName(data []byte, offset int) (string, int, error) {
	dataLen := len(data)
	if offset >= dataLen {
		return "", offset, ErrInvalidName
	}

	// Stack-allocated scratch buffer sized to the RFC 1035 limit.
	// Compiler keeps this in the parseName stack frame (no heap alloc)
	// and we only allocate when converting to a string at the end.
	var (
		scratch  [maxDNSName]byte
		nameLen  int
		jumped   bool
		jumps    int
		origNext = offset
		hasLabel bool
	)

	for {
		if offset >= dataLen {
			return "", origNext, ErrInvalidName
		}

		length := int(data[offset])
		if length == 0 {
			offset++
			if !jumped {
				origNext = offset
			}
			break
		}

		if length >= 192 { // 0xC0
			if offset+1 >= dataLen || jumps >= maxNameJumps {
				return "", origNext, ErrInvalidName
			}
			ptr := int(binary.BigEndian.Uint16(data[offset:offset+2]) & 0x3FFF)
			if ptr >= dataLen {
				return "", origNext, ErrInvalidName
			}
			if !jumped {
				origNext = offset + 2
				jumped = true
			}
			offset = ptr
			jumps++
			continue
		}

		if length > 63 {
			return "", origNext, ErrInvalidName
		}

		offset++
		end := offset + length
		if end > dataLen {
			return "", origNext, ErrInvalidName
		}

		// Append "." separator before every label except the first.
		// Bail out cleanly if the accumulated name would exceed the
		// RFC 1035 hard limit (255 bytes wire-format, which translates
		// to <= 253 presentation-format characters).
		if hasLabel {
			if nameLen+1+length > len(scratch) {
				return "", origNext, ErrInvalidName
			}
			scratch[nameLen] = '.'
			nameLen++
		} else if length > len(scratch) {
			return "", origNext, ErrInvalidName
		}

		copyLowerASCIILabel(scratch[nameLen:nameLen+length], data[offset:end])
		nameLen += length
		hasLabel = true
		offset = end
		if !jumped {
			origNext = offset
		}
	}

	if !hasLabel {
		return ".", origNext, nil
	}
	// Single allocation: convert the scratch slice to a string.
	return string(scratch[:nameLen]), origNext, nil
}

// copyLowerASCIILabel copies src into dst (which MUST be the same length)
// lower-casing ASCII uppercase letters in the process. DNS labels are
// limited to 63 bytes so this is always small.
func copyLowerASCIILabel(dst, src []byte) {
	_ = dst[len(src)-1] // bounds-check elimination hint
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		dst[i] = ch
	}
}
