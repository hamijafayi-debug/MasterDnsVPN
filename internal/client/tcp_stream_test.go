// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
package client

import (
	"context"
	"net"
	"testing"
	"time"

	"masterdnsvpn-go/internal/arq"
	"masterdnsvpn-go/internal/config"
	Enums "masterdnsvpn-go/internal/enums"
	VpnProto "masterdnsvpn-go/internal/vpnproto"
)

func buildTCPTestClient(t *testing.T) *Client {
	c := buildTestClientWithResolvers(config.ClientConfig{
		ProtocolType:                "TCP",
		ARQWindowSize:               64,
		ARQInitialRTOSeconds:        0.2,
		ARQMaxRTOSeconds:            1.0,
		ARQControlInitialRTOSeconds: 0.2,
		ARQControlMaxRTOSeconds:     1.0,
	}, "resolver-a")
	// Force-close every stream that is still active when the test ends so
	// the ARQ retransmit goroutine started by new_stream → ARQ.Start does
	// not outlive the test body and write into reused memory in the next
	// test of the same package. We only force-close streams that are NOT
	// already marked terminal: closing a stream twice triggers a known
	// production-code race in ARQ.Close()'s isVirtual read/write pair
	// (logged in PLAN.md bug log for a dedicated fix step). Test-only
	// safety net — production code untouched.
	if t != nil {
		t.Cleanup(func() {
			c.streamsMu.Lock()
			streams := make([]*Stream_client, 0, len(c.active_streams))
			for _, s := range c.active_streams {
				streams = append(streams, s)
			}
			c.streamsMu.Unlock()

			// Step 7: replaced the 20ms settling sleep with a
			// deterministic WaitForShutdown call. ARQ.Close cancels the
			// internal context but does NOT join the retransmit /
			// write-loop goroutines before returning, so without an
			// explicit wait those goroutines can still write into the
			// stream's state after the test body returns and race
			// detector flags them against the next test's reads.
			// 2 seconds is a comfortable ceiling: in practice clean
			// shutdown takes <5ms on loopback.
			for _, s := range streams {
				if s == nil {
					continue
				}
				if s.TerminalSince().IsZero() {
					s.Close()
				}
				if a, ok := s.Stream.(*arq.ARQ); ok && a != nil {
					_ = a.WaitForShutdown(2 * time.Second)
				}
			}
		})
	}
	return c
}

func TestHandleTCPConnectQueuesStreamSyn(t *testing.T) {
	c := buildTCPTestClient(t)
	c.syncedUploadMTU = 64

	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	c.HandleTCPConnect(context.Background(), local)

	if len(c.active_streams) != 1 {
		t.Fatalf("expected one active stream, got %d", len(c.active_streams))
	}

	var stream *Stream_client
	for _, s := range c.active_streams {
		stream = s
	}
	if stream == nil {
		t.Fatal("expected created stream")
	}

	if got := stream.StatusValue(); got != streamStatusConnecting {
		t.Fatalf("expected stream status %q, got %q", streamStatusConnecting, got)
	}

	packet, _, ok := stream.PopNextTXPacket()
	if !ok || packet == nil {
		t.Fatal("expected queued STREAM_SYN packet")
	}
	defer stream.ReleaseTXPacket(packet)

	if packet.PacketType != Enums.PACKET_STREAM_SYN {
		t.Fatalf("expected packet type STREAM_SYN, got %d", packet.PacketType)
	}

	if len(packet.Payload) != 0 {
		t.Fatalf("expected raw STREAM_SYN without payload, got %d payload bytes", len(packet.Payload))
	}
}

func TestHandleStreamPacketConnectedEnablesTCPStreamIO(t *testing.T) {
	c := buildTCPTestClient(t)
	c.syncedUploadMTU = 64

	local, remote := net.Pipe()
	defer remote.Close()

	stream := c.new_stream(1, local, nil)
	arqObj, ok := stream.Stream.(*arq.ARQ)
	if !ok || arqObj == nil {
		t.Fatal("expected ARQ-backed stream")
	}

	packet := VpnProto.Packet{
		PacketType:  Enums.PACKET_STREAM_CONNECTED,
		StreamID:    1,
		HasStreamID: true,
	}
	if err := c.HandleStreamPacket(packet); err != nil {
		t.Fatalf("HandleStreamPacket returned error: %v", err)
	}

	if got := stream.StatusValue(); got != streamStatusActive {
		t.Fatalf("expected stream status %q, got %q", streamStatusActive, got)
	}

	arqObj.ReceiveData(0, []byte("ok"))

	_ = remote.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 2)
	n, err := remote.Read(buf)
	if err != nil {
		t.Fatalf("expected TCP stream IO to become ready, read failed: %v", err)
	}
	if string(buf[:n]) != "ok" {
		t.Fatalf("unexpected data through stream: %q", string(buf[:n]))
	}
}

func TestHandleStreamPacketConnectFailClosesTCPStream(t *testing.T) {
	c := buildTCPTestClient(t)
	c.syncedUploadMTU = 64

	local, remote := net.Pipe()
	defer remote.Close()

	stream := c.new_stream(2, local, nil)
	arqObj, ok := stream.Stream.(*arq.ARQ)
	if !ok || arqObj == nil {
		t.Fatal("expected ARQ-backed stream")
	}

	packet := VpnProto.Packet{
		PacketType:  Enums.PACKET_STREAM_CONNECT_FAIL,
		StreamID:    2,
		HasStreamID: true,
	}
	if err := c.HandleStreamPacket(packet); err != nil {
		t.Fatalf("HandleStreamPacket returned error: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for !arqObj.IsClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !arqObj.IsClosed() {
		t.Fatal("expected ARQ stream to be closed after connect failure")
	}

	if got := stream.StatusValue(); got != streamStatusClosed {
		t.Fatalf("expected stream status %q, got %q", streamStatusClosed, got)
	}

	c.streamsMu.RLock()
	_, stillActive := c.active_streams[stream.StreamID]
	c.streamsMu.RUnlock()
	if stillActive {
		t.Fatal("expected closed stream to be removed from active_streams")
	}
}

func TestRecentlyClosedCloseReadStreamSuppressesLateOrphanReset(t *testing.T) {
	c := buildTCPTestClient(t)
	stream := c.new_stream(32, nil, nil)

	stream.OnARQClosed("close handshake completed")

	c.streamsMu.RLock()
	_, stillActive := c.active_streams[stream.StreamID]
	c.streamsMu.RUnlock()
	if stillActive {
		t.Fatal("expected recently closed stream to be removed from active_streams")
	}

	handled := c.preprocessInboundPacket(VpnProto.Packet{
		PacketType:  Enums.PACKET_STREAM_CLOSE_READ,
		StreamID:    stream.StreamID,
		HasStreamID: true,
	})
	if !handled {
		t.Fatal("expected late CLOSE_READ for recently closed stream to be consumed")
	}
	packet, _, ok := c.orphanQueue.Pop()
	if !ok {
		t.Fatal("expected CLOSE_READ_ACK to be queued for recently closed stream")
	}
	if packet.PacketType != Enums.PACKET_STREAM_CLOSE_READ_ACK {
		t.Fatalf("expected packet type %s, got %s", Enums.PacketTypeName(Enums.PACKET_STREAM_CLOSE_READ_ACK), Enums.PacketTypeName(packet.PacketType))
	}
}

func TestRecentlyClosedResetStreamSuppressesLateOrphanReset(t *testing.T) {
	c := buildTCPTestClient(t)
	stream := c.new_stream(33, nil, nil)

	stream.OnARQClosed("peer reset received")
	c.rememberClosedStream(stream.StreamID, "RST acknowledged", time.Now())

	c.streamsMu.RLock()
	_, stillActive := c.active_streams[stream.StreamID]
	c.streamsMu.RUnlock()
	if stillActive {
		t.Fatal("expected recently closed reset stream to be removed from active_streams")
	}

	handled := c.preprocessInboundPacket(VpnProto.Packet{
		PacketType:  Enums.PACKET_STREAM_CLOSE_READ,
		StreamID:    stream.StreamID,
		HasStreamID: true,
	})
	if !handled {
		t.Fatal("expected late CLOSE_READ for reset-closed stream to be consumed")
	}
	packet, _, ok := c.orphanQueue.Pop()
	if !ok {
		t.Fatal("expected CLOSE_READ_ACK to be queued for reset-closed stream")
	}
	if packet.PacketType != Enums.PACKET_STREAM_CLOSE_READ_ACK {
		t.Fatalf("expected packet type %s, got %s", Enums.PacketTypeName(Enums.PACKET_STREAM_CLOSE_READ_ACK), Enums.PacketTypeName(packet.PacketType))
	}
}

func TestRecentlyClosedStreamStillAcksLateSocksConnected(t *testing.T) {
	c := buildTCPTestClient(t)
	stream := c.new_stream(41, nil, nil)

	stream.OnARQClosed("close handshake completed")

	handled := c.preprocessInboundPacket(VpnProto.Packet{
		PacketType:  Enums.PACKET_SOCKS5_CONNECTED,
		StreamID:    stream.StreamID,
		HasStreamID: true,
	})
	if !handled {
		t.Fatal("expected late SOCKS5_CONNECTED for recently closed stream to be consumed")
	}

	packet, _, ok := c.orphanQueue.Pop()
	if !ok {
		t.Fatal("expected SOCKS5_CONNECTED_ACK to be queued for recently closed stream")
	}
	if packet.PacketType != Enums.PACKET_SOCKS5_CONNECTED_ACK {
		t.Fatalf("expected packet type %s, got %s", Enums.PacketTypeName(Enums.PACKET_SOCKS5_CONNECTED_ACK), Enums.PacketTypeName(packet.PacketType))
	}
}

func TestMissingUnknownStreamStillQueuesOrphanReset(t *testing.T) {
	c := buildTCPTestClient(t)

	handled := c.preprocessInboundPacket(VpnProto.Packet{
		PacketType:  Enums.PACKET_STREAM_CLOSE_READ,
		StreamID:    777,
		HasStreamID: true,
	})
	if !handled {
		t.Fatal("expected missing stream packet to be handled")
	}
	if size := c.orphanQueue.Size(); size != 1 {
		t.Fatalf("expected orphan reset for unknown stream, got queue size %d", size)
	}
}

func TestTerminalStreamDataQueuesRST(t *testing.T) {
	c := buildTCPTestClient(t)
	stream := c.new_stream(34, nil, nil)

	stream.MarkTerminal(time.Now())
	stream.SetStatus(streamStatusTimeWait)

	packet := VpnProto.Packet{
		PacketType:     Enums.PACKET_STREAM_DATA,
		StreamID:       stream.StreamID,
		HasStreamID:    true,
		SequenceNum:    1,
		HasSequenceNum: true,
		Payload:        []byte("late"),
	}
	if err := c.HandleStreamPacket(packet); err != nil {
		t.Fatalf("HandleStreamPacket returned error: %v", err)
	}
	if size := c.orphanQueue.Size(); size != 1 {
		t.Fatalf("expected queued response for terminal stream data, got queue size %d", size)
	}

	queued, _, ok := c.orphanQueue.Pop()
	if !ok {
		t.Fatal("expected STREAM_RST for terminal stream data")
	}
	if queued.PacketType != Enums.PACKET_STREAM_RST {
		t.Fatalf("expected packet type %s, got %s", Enums.PacketTypeName(Enums.PACKET_STREAM_RST), Enums.PacketTypeName(queued.PacketType))
	}
}

func TestRecentlyClosedStreamDataQueuesRST(t *testing.T) {
	c := buildTCPTestClient(t)
	stream := c.new_stream(42, nil, nil)
	stream.OnARQClosed("close handshake completed")

	handled := c.preprocessInboundPacket(VpnProto.Packet{
		PacketType:     Enums.PACKET_STREAM_DATA,
		StreamID:       stream.StreamID,
		HasStreamID:    true,
		SequenceNum:    9,
		HasSequenceNum: true,
		Payload:        []byte("late"),
	})
	if !handled {
		t.Fatal("expected late DATA for recently closed stream to be consumed")
	}

	packet, _, ok := c.orphanQueue.Pop()
	if !ok {
		t.Fatal("expected STREAM_RST queued for late data on recently closed stream")
	}
	if packet.PacketType != Enums.PACKET_STREAM_RST {
		t.Fatalf("expected packet type %s, got %s", Enums.PacketTypeName(Enums.PACKET_STREAM_RST), Enums.PacketTypeName(packet.PacketType))
	}
}

func TestForceCloseStreamQueuesRST(t *testing.T) {
	c := buildTCPTestClient(t)
	local, remote := net.Pipe()
	defer remote.Close()

	stream := c.new_stream(35, local, nil)

	c.CloseStream(stream.StreamID, true, 0)

	if got := stream.StatusValue(); got != streamStatusCancelled {
		t.Fatalf("expected stream status %q after force close, got %q", streamStatusCancelled, got)
	}
	if stream.TerminalSince().IsZero() {
		t.Fatal("expected force-closed stream to be marked terminal")
	}

	packet, _, ok := stream.PopNextTXPacket()
	if !ok || packet == nil {
		t.Fatal("expected queued STREAM_RST packet after force close")
	}
	defer stream.ReleaseTXPacket(packet)

	if packet.PacketType != Enums.PACKET_STREAM_RST {
		t.Fatalf("expected packet type %s, got %s", Enums.PacketTypeName(Enums.PACKET_STREAM_RST), Enums.PacketTypeName(packet.PacketType))
	}
}

func TestGracefulCloseStreamQueuesCloseRead(t *testing.T) {
	c := buildTCPTestClient(t)
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	stream := c.new_stream(36, local, nil)

	c.CloseStream(stream.StreamID, false, 0)

	packet, _, ok := stream.PopNextTXPacket()
	if !ok || packet == nil {
		t.Fatal("expected queued STREAM_CLOSE_READ packet after graceful close")
	}
	defer stream.ReleaseTXPacket(packet)

	if packet.PacketType != Enums.PACKET_STREAM_CLOSE_READ {
		t.Fatalf("expected packet type %s, got %s", Enums.PacketTypeName(Enums.PACKET_STREAM_CLOSE_READ), Enums.PacketTypeName(packet.PacketType))
	}
}

func TestLateSocksConnectedAfterCancellationQueuesRST(t *testing.T) {
	c := buildTCPTestClient(t)
	local, remote := net.Pipe()
	defer remote.Close()

	stream := c.new_stream(37, local, nil)
	stream.MarkTerminal(time.Now())
	stream.SetStatus(streamStatusCancelled)

	packet := VpnProto.Packet{
		PacketType:  Enums.PACKET_SOCKS5_CONNECTED,
		StreamID:    stream.StreamID,
		HasStreamID: true,
	}
	if err := c.HandleSocksConnected(packet); err != nil {
		t.Fatalf("HandleSocksConnected returned error: %v", err)
	}

	queued, _, ok := stream.PopNextTXPacket()
	if !ok || queued == nil {
		t.Fatal("expected late SOCKS success after cancellation to queue STREAM_RST")
	}
	defer stream.ReleaseTXPacket(queued)

	if queued.PacketType != Enums.PACKET_STREAM_RST {
		t.Fatalf("expected packet type %s, got %s", Enums.PacketTypeName(Enums.PACKET_STREAM_RST), Enums.PacketTypeName(queued.PacketType))
	}
}

func TestLateStreamConnectedAfterCancellationQueuesRST(t *testing.T) {
	c := buildTCPTestClient(t)
	local, remote := net.Pipe()
	defer remote.Close()

	stream := c.new_stream(38, local, nil)
	stream.MarkTerminal(time.Now())
	stream.SetStatus(streamStatusCancelled)

	packet := VpnProto.Packet{
		PacketType:  Enums.PACKET_STREAM_CONNECTED,
		StreamID:    stream.StreamID,
		HasStreamID: true,
	}
	if err := c.HandleStreamPacket(packet); err != nil {
		t.Fatalf("HandleStreamPacket returned error: %v", err)
	}

	queued, _, ok := stream.PopNextTXPacket()
	if !ok || queued == nil {
		t.Fatal("expected late STREAM_CONNECTED after cancellation to queue STREAM_RST")
	}
	defer stream.ReleaseTXPacket(queued)

	if queued.PacketType != Enums.PACKET_STREAM_RST {
		t.Fatalf("expected packet type %s, got %s", Enums.PacketTypeName(Enums.PACKET_STREAM_RST), Enums.PacketTypeName(queued.PacketType))
	}
}

func TestCloseAllStreamsFinalizesLocally(t *testing.T) {
	c := buildTCPTestClient(t)

	localA, remoteA := net.Pipe()
	defer remoteA.Close()
	streamA := c.new_stream(39, localA, nil)

	localB, remoteB := net.Pipe()
	defer remoteB.Close()
	streamB := c.new_stream(40, localB, nil)

	c.CloseAllStreams()

	for _, stream := range []*Stream_client{streamA, streamB} {
		arqObj, ok := stream.Stream.(*arq.ARQ)
		if !ok || arqObj == nil {
			t.Fatalf("expected ARQ-backed stream %d", stream.StreamID)
		}
		if !arqObj.IsClosed() {
			t.Fatalf("expected stream %d ARQ to be closed after CloseAllStreams", stream.StreamID)
		}
		if size := stream.txQueue.Size(); size != 0 {
			t.Fatalf("expected stream %d TX queue to be cleared after CloseAllStreams, got %d", stream.StreamID, size)
		}
	}
}

func TestFakeConnReadUnblocksOnClose(t *testing.T) {
	conn := newFakeConn()
	errCh := make(chan error, 1)

	go func() {
		_, err := conn.Read(make([]byte, 1))
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)

	if err := conn.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected fakeConn.Read to return an error after Close")
		}
		if err != net.ErrClosed {
			t.Fatalf("expected net.ErrClosed after Close, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected fakeConn.Read to unblock after Close")
	}
}

func TestFakeConnReadDeadlineReturnsTimeout(t *testing.T) {
	conn := newFakeConn()

	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline returned error: %v", err)
	}

	start := time.Now()
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expected timeout error from fakeConn.Read")
	}

	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatalf("expected timeout-compatible error, got %v", err)
	}

	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("fakeConn.Read timeout took too long: %v", elapsed)
	}
}

func TestRecentlyClosedHeapStaleEntryGrowth(t *testing.T) {
	c := buildTCPTestClient(t)
	now := time.Now()

	for cycle := 0; cycle < 10; cycle++ {
		for id := uint16(1); id <= 50; id++ {
			c.rememberClosedStream(id, "RST acknowledged", now)
		}
		now = now.Add(100 * time.Millisecond)
	}

	c.recentlyClosedMu.Lock()
	heapSize := len(c.recentlyClosedHeap)
	mapSize := len(c.recentlyClosedStreams)
	c.recentlyClosedMu.Unlock()

	t.Logf("After 10 cycles of 50 stream IDs: heap=%d map=%d ratio=%.2f", heapSize, mapSize, float64(heapSize)/float64(mapSize))

	maxAllowed := mapSize + mapSize/2
	if maxAllowed < mapSize+4 {
		maxAllowed = mapSize + 4
	}
	if heapSize > maxAllowed {
		t.Errorf("heap (%d) exceeded 1.5x the map size (%d, threshold %d), compaction should prevent this", heapSize, mapSize, maxAllowed)
	}
}
