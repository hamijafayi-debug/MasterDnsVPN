// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

//go:build !linux

package udpserver

import (
	"context"
	"net"
)

// batchReadSupported reports whether the platform supports an optimized
// batched recvmmsg path. Only Linux exposes recvmmsg via the kernel; on every
// other GOOS the ipv4.PacketConn.ReadBatch implementation falls back to a
// single ReadFrom, which would actually hurt throughput due to the extra
// allocations the wrapper does. So on these platforms we return false and the
// dispatcher in server_ingress.go takes the per-packet path directly.
func batchReadSupported() bool { return false }

// batchReadLoop is provided as a no-op on non-Linux platforms; it is never
// invoked because batchReadSupported() returns false. It exists so the
// compiler does not have to special-case call sites.
func (s *Server) batchReadLoop(ctx context.Context, conn *net.UDPConn, reqCh chan<- request, readerID int) error {
	return s.readLoop(ctx, conn, reqCh, readerID)
}
