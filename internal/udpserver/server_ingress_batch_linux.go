// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

//go:build linux

package udpserver

import (
	"context"
	"errors"
	"net"

	"golang.org/x/net/ipv4"

	"masterdnsvpn-go/internal/metrics"
)

// batchReadSupported reports whether the platform supports an optimized batched
// recvmmsg path. On Linux this is true unconditionally; the helper provides a
// uniform entry point for the dispatcher in server_ingress.go.
func batchReadSupported() bool { return true }

// batchReadBurst is the maximum number of datagrams pulled from the kernel per
// recvmmsg(2) syscall. Higher values amortize syscall overhead under heavy
// load but inflate per-loop latency for idle paths. 32 is a conservative
// sweet-spot used by other Go UDP servers (e.g. quic-go) and keeps the
// per-reader live buffer footprint at 32 * MaxPacketSize ≈ 2 MiB worst case
// when MaxPacketSize is 65535. We tune down to the queue depth so we never
// dispatch a batch larger than the configured queue.
const batchReadBurst = 32

// batchReadLoop is the Linux fast-path that replaces the default per-packet
// ReadFromUDP loop in readLoop. It uses ipv4.PacketConn.ReadBatch to pull up
// to batchReadBurst datagrams per syscall, then dispatches each one into the
// shared request channel. Behavior on the channel side (drop policy, drop
// counter, drop log throttling) is identical to the single-packet path so we
// stay wire-compat with the rest of the server.
//
// If creating the ipv4.PacketConn or the very first ReadBatch fails for any
// reason (a rare condition; ipv4.NewPacketConn only fails on conn type
// mismatch which cannot happen for *net.UDPConn), we fall back to the
// per-packet path. This preserves end-to-end correctness on exotic kernels.
func (s *Server) batchReadLoop(ctx context.Context, conn *net.UDPConn, reqCh chan<- request, readerID int) error {
	pc := ipv4.NewPacketConn(conn)

	burst := batchReadBurst
	if cap(reqCh) > 0 && burst > cap(reqCh) {
		burst = cap(reqCh)
	}
	if burst < 1 {
		burst = 1
	}

	// Pre-allocate the message and addr slots. Buffers come from the packet
	// pool every iteration so the dispatch into reqCh can retain ownership
	// (same lifecycle contract as the single-packet readLoop).
	msgs := make([]ipv4.Message, burst)
	for i := range msgs {
		msgs[i].Buffers = make(net.Buffers, 1)
	}

	useBatch := true
	for {
		if useBatch {
			// Rehydrate each Buffers[0] with a fresh pooled slice. We do
			// this before every ReadBatch so the kernel writes directly
			// into pooled memory and there is zero copy on the success
			// path.
			for i := range msgs {
				buf := s.packetPool.Get().([]byte)
				msgs[i].Buffers[0] = buf[:cap(buf)]
				msgs[i].N = 0
				msgs[i].Addr = nil
			}

			n, err := pc.ReadBatch(msgs, 0)
			if err != nil {
				// Release every prepared buffer — none of them is valid.
				for i := range msgs {
					if msgs[i].Buffers[0] != nil {
						s.packetPool.Put(msgs[i].Buffers[0])
						msgs[i].Buffers[0] = nil
					}
				}
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return nil
				}
				// Permanent unsupported / not-implemented sentinel on this
				// kernel: switch to per-packet path for the lifetime of
				// this reader. Other errors are propagated up.
				if errors.Is(err, errBatchReadUnsupported) {
					useBatch = false
					continue
				}
				s.log.Debugf(
					"\U0001F4A5 <yellow>UDP Batch Read Error, Reader: <cyan>%d</cyan>, Error: <cyan>%v</cyan></yellow>",
					readerID,
					err,
				)
				return err
			}

			// Dispatch successful messages and release any that were
			// pre-allocated but not filled (n < burst).
			for i := 0; i < n; i++ {
				m := &msgs[i]
				addr, ok := m.Addr.(*net.UDPAddr)
				if !ok || m.N <= 0 {
					if m.Buffers[0] != nil {
						s.packetPool.Put(m.Buffers[0])
						m.Buffers[0] = nil
					}
					continue
				}
				metrics.PacketsIn.Add(1)
				metrics.BytesIn.Add(int64(m.N))
				buf := m.Buffers[0]
				m.Buffers[0] = nil

				select {
				case reqCh <- request{buf: buf, size: m.N, addr: addr, conn: conn}:
				case <-ctx.Done():
					s.packetPool.Put(buf)
					// Release remaining prepared buffers.
					for j := i + 1; j < len(msgs); j++ {
						if msgs[j].Buffers[0] != nil {
							s.packetPool.Put(msgs[j].Buffers[0])
							msgs[j].Buffers[0] = nil
						}
					}
					return nil
				default:
					s.packetPool.Put(buf)
					s.onDrop(addr, len(reqCh), cap(reqCh))
				}
			}
			for i := n; i < len(msgs); i++ {
				if msgs[i].Buffers[0] != nil {
					s.packetPool.Put(msgs[i].Buffers[0])
					msgs[i].Buffers[0] = nil
				}
			}
			continue
		}

		// Fallback path — same logic as readLoopSingle, kept inline so we
		// don't pay a function-pointer dispatch cost per packet.
		buffer := s.packetPool.Get().([]byte)
		nRead, addr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			s.packetPool.Put(buffer)
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Debugf(
				"\U0001F4A5 <yellow>UDP Read Error, Reader: <cyan>%d</cyan>, Error: <cyan>%v</cyan></yellow>",
				readerID,
				err,
			)
			return err
		}
		metrics.PacketsIn.Add(1)
		metrics.BytesIn.Add(int64(nRead))

		select {
		case reqCh <- request{buf: buffer, size: nRead, addr: addr, conn: conn}:
		case <-ctx.Done():
			s.packetPool.Put(buffer)
			return nil
		default:
			s.packetPool.Put(buffer)
			s.onDrop(addr, len(reqCh), cap(reqCh))
		}
	}
}

// errBatchReadUnsupported is a sentinel some kernels may surface; reserved for
// future use. Today ipv4.PacketConn never returns this on Linux, but keeping
// the symbol lets the dispatch logic be future-proof.
var errBatchReadUnsupported = errors.New("batch read unsupported on this kernel")
