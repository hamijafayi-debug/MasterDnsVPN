// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
// Package client provides the core logic for the MasterDnsVPN client.
// This file (tunnel_query.go) handles the construction of DNS tunnel queries.
// ==============================================================================
package client

import (
	DnsParser "masterdnsvpn-go/internal/dnsparser"
	Enums "masterdnsvpn-go/internal/enums"
	"masterdnsvpn-go/internal/streamutil"
	VpnProto "masterdnsvpn-go/internal/vpnproto"
)

type preparedTunnelDomain struct {
	normalized string
	qname      []byte
}

func buildTunnelTXTQuestionBytes(domain string, encoded []byte) ([]byte, error) {
	return DnsParser.BuildTunnelTXTQuestionPacket(domain, encoded, Enums.DNS_RECORD_TYPE_TXT, EDnsSafeUDPSize)
}

func prepareTunnelDomain(domain string) (preparedTunnelDomain, error) {
	normalized, qname, err := DnsParser.PrepareTunnelDomainQname(domain)
	if err != nil {
		return preparedTunnelDomain{}, err
	}
	return preparedTunnelDomain{normalized: normalized, qname: qname}, nil
}

// buildTunnelTXTQueryRaw builds an encoded tunnel query using the provided options and codec.
//
// Allocation note: BuildRawInto is fed a pool-backed scratch slice so the raw
// frame skips the per-call make() in the hot send path. We use GetPtr / PutPtr
// (the zero-alloc twin of Get / Put) so the slice header itself stays on the
// pool's pointer — no per-call header escape.
// EncryptAndEncodeBytes only reads `raw`, so it is safe to recycle the
// scratch immediately after the call returns.
func (c *Client) buildTunnelTXTQueryRaw(domain string, options VpnProto.BuildOptions) ([]byte, error) {
	scratchPtr := streamutil.GetPtr(VpnProto.MaxHeaderRawSize() + len(options.Payload))
	defer streamutil.PutPtr(scratchPtr)

	raw, err := VpnProto.BuildRawInto((*scratchPtr)[:0], options)
	if err != nil {
		return nil, err
	}
	encoded, err := c.codec.EncryptAndEncodeBytes(raw)
	if err != nil {
		return nil, err
	}
	return buildTunnelTXTQuestionBytes(domain, encoded)
}

func (c *Client) buildEncodedAutoWithCompressionTrace(options VpnProto.BuildOptions) ([]byte, error) {
	// Reserve enough headroom for the worst-case header plus the payload
	// before any compression takes place. If the payload compresses smaller
	// the slice simply shrinks via BuildRawAutoInto's sub-slicing.
	scratchPtr := streamutil.GetPtr(VpnProto.MaxHeaderRawSize() + len(options.Payload))
	defer streamutil.PutPtr(scratchPtr)

	raw, err := VpnProto.BuildRawAutoInto((*scratchPtr)[:0], options, c.cfg.CompressionMinSize)
	if err != nil {
		return nil, err
	}

	if c.codec == nil {
		return nil, VpnProto.ErrCodecUnavailable
	}
	return c.codec.EncryptAndEncodeBytes(raw)
}

// buildTunnelTXTQuery builds an encoded tunnel query with automatic option handling.
func (c *Client) buildTunnelTXTQuery(domain string, options VpnProto.BuildOptions) ([]byte, error) {
	encoded, err := c.buildEncodedAutoWithCompressionTrace(options)
	if err != nil {
		return nil, err
	}
	return buildTunnelTXTQuestionBytes(domain, encoded)
}
