// Package protocol implements uTP (Micro Transport Protocol) per BEP 29.
package protocol

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
)

// Conn represents a uTP connection.
// It operates in two modes:
//   - dialer mode: owns its own UDP socket (recvChan == nil)
//   - acceptor mode: shares the Listener's socket (recvChan != nil)
type Conn struct {
	conn       net.PacketConn
	remoteAddr net.Addr
	ctx        context.Context
	cancel     context.CancelFunc
	state      *State

	// Acceptor mode only.
	recvChan chan []byte // raw UDP payloads dispatched by the Listener
	listener *Listener   // owning listener, for cleanup on Close
}

// uTPHeader represents the uTP packet header (BEP 29: 20 bytes)
type uTPHeader struct {
	Type          uint8  // 4 bits: packet type
	Ver           uint8  // 4 bits: protocol version (1)
	Extension     uint8  // 8 bits: extension type
	ConnectionID  uint16 // 16 bits: connection identifier
	Timestamp     uint32 // 32 bits: timestamp in microseconds
	TimestampDiff uint32 // 32 bits: timestamp difference
	WndSize       uint32 // 32 bits: advertised window size
	SeqNr         uint16 // 16 bits: sequence number
	AckNr         uint16 // 16 bits: acknowledgment number
}

// parseHeader parses a uTP header from bytes (BEP 29 header format)
// Returns the header and the size of the header (including extensions)
func parseHeader(buf []byte) (*uTPHeader, int, error) {
	if len(buf) < 20 {
		return nil, 0, fmt.Errorf("packet too small: %d bytes", len(buf))
	}

	h := &uTPHeader{
		Type:          (buf[0] & 0xF0) >> 4,
		Ver:           buf[0] & 0x0F,
		Extension:     buf[1],
		ConnectionID:  binary.BigEndian.Uint16(buf[2:4]),
		Timestamp:     binary.BigEndian.Uint32(buf[4:8]),
		TimestampDiff: binary.BigEndian.Uint32(buf[8:12]),
		WndSize:       binary.BigEndian.Uint32(buf[12:16]),
		SeqNr:         binary.BigEndian.Uint16(buf[16:18]),
		AckNr:         binary.BigEndian.Uint16(buf[18:20]),
	}

	// Validate version (BEP 29: must be 1)
	if h.Ver != 1 {
		return nil, 0, fmt.Errorf("unsupported uTP version: %d", h.Ver)
	}

	// Skip extensions per BEP 29 - each extension is [next_extension_type:1][len:1][data:len]
	// The extension field in header (buf[1]) is the first extension type
	// A value of 0 means no extensions
	headerSize := 20
	extType := h.Extension
	offset := 20

	for extType != 0 && offset < len(buf) {
		if offset+2 > len(buf) {
			// Malformed extension
			break
		}
		nextExtType := buf[offset]
		extLen := int(buf[offset+1])
		offset += 2 + extLen
		extType = nextExtType
		headerSize = offset
	}

	return h, headerSize, nil
}

// buildHeader builds a uTP header (BEP 29 format)
func (c *Conn) buildHeader(packetType uint8) []byte {
	buf := make([]byte, 20)

	// Type and Version (BEP 29: 4 bits each)
	buf[0] = (packetType << 4) | 1 // Version 1

	// Extension (BEP 29: linked list of extensions)
	buf[1] = EXT_NO_EXTENSION // No extensions for now

	// Connection ID (BEP 29: identifies connection)
	var connID uint16
	if packetType == ST_SYN {
		connID = c.state.connIDRecv
	} else {
		connID = c.state.connIDSend
	}
	binary.BigEndian.PutUint16(buf[2:4], connID)

	// Timestamp (BEP 29: microseconds since epoch)
	timestamp := uint32(time.Now().UnixNano() / 1000)
	binary.BigEndian.PutUint32(buf[4:8], timestamp)

	// Timestamp Difference (BEP 29: reply_micro from last received packet)
	c.state.mu.RLock()
	binary.BigEndian.PutUint32(buf[8:12], c.state.replyMicro)
	c.state.mu.RUnlock()

	// Window Size (BEP 29: bytes left in receive buffer)
	c.state.mu.RLock()
	binary.BigEndian.PutUint32(buf[12:16], c.state.wndSize)
	c.state.mu.RUnlock()

	// Sequence Number (BEP 29: packet sequence number)
	if packetType == ST_SYN {
		binary.BigEndian.PutUint16(buf[16:18], 1)
	} else {
		c.state.mu.RLock()
		binary.BigEndian.PutUint16(buf[16:18], c.state.seqNr)
		c.state.mu.RUnlock()
	}

	// Ack Number (BEP 29: last received sequence number)
	c.state.mu.RLock()
	binary.BigEndian.PutUint16(buf[18:20], c.state.ackNr)
	c.state.mu.RUnlock()

	return buf
}

// DialContext attempts to establish a uTP connection (BEP 29 handshake)
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	log.Debug("uTP DialContext started", "network", network, "addr", addr)

	// Parse address
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Debug("uTP failed to resolve address", "addr", addr, "error", err)
		return nil, fmt.Errorf("failed to resolve UDP address: %w", err)
	}
	log.Debug("uTP resolved address", "addr", addr, "udp_addr", udpAddr.String())

	// Create UDP socket
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		log.Debug("uTP failed to create UDP socket", "error", err)
		return nil, fmt.Errorf("failed to create UDP socket: %w", err)
	}
	log.Debug("uTP created UDP socket", "local_addr", conn.LocalAddr().String())

	// Create context with cancellation
	ctx, cancel := context.WithCancel(ctx)

	// Create uTP connection with proper state
	utpConn := &Conn{
		conn:       conn,
		remoteAddr: udpAddr,
		ctx:        ctx,
		cancel:     cancel,
		state:      NewState(),
	}

	// Perform uTP handshake per BEP 29
	log.Debug("uTP starting handshake", "remote", udpAddr.String(), "local", conn.LocalAddr().String())
	if err := utpConn.performHandshake(ctx); err != nil {
		log.Debug("uTP handshake failed", "remote", udpAddr.String(), "error", err)
		conn.Close()
		cancel()
		return nil, fmt.Errorf("uTP handshake failed: %w", err)
	}

	log.Debug("uTP connection established", "remote", udpAddr.String(), "local", conn.LocalAddr().String())
	return utpConn, nil
}

// performHandshake performs uTP connection setup per BEP 29 section "connection setup"
func (c *Conn) performHandshake(ctx context.Context) error {
	// Set state to CS_SYN_SENT (BEP 29)
	c.state.SetState(CS_SYN_SENT)

	// Build ST_SYN packet (BEP 29: initiates connection)
	synPacket := c.buildHeader(ST_SYN)

	// Log SYN packet details
	connIDRecv := binary.BigEndian.Uint16(synPacket[2:4])
	log.Debug("uTP sending SYN",
		"remote", c.remoteAddr.String(),
		"local", c.conn.LocalAddr().String(),
		"packet_size", len(synPacket),
		"type", synPacket[0]>>4,
		"version", synPacket[0]&0x0F,
		"conn_id_recv", connIDRecv,
		"conn_id_send_expected", connIDRecv+1,
		"seq_nr", binary.BigEndian.Uint16(synPacket[16:18]),
		"ack_nr", binary.BigEndian.Uint16(synPacket[18:20]),
		"wnd_size", binary.BigEndian.Uint32(synPacket[12:16]))

	// Send SYN with timeout
	if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Debug("uTP failed to set write deadline", "error", err)
		return fmt.Errorf("set write deadline failed: %w", err)
	}

	sentAt := time.Now()
	sentBytes, err := c.conn.WriteTo(synPacket, c.remoteAddr)
	if err != nil {
		log.Debug("uTP SYN send failed", "remote", c.remoteAddr.String(), "error", err)
		return fmt.Errorf("send SYN failed: %w", err)
	}
	log.Debug("uTP SYN sent", "remote", c.remoteAddr.String(), "bytes", sentBytes, "time", sentAt)

	// Wait for ST_STATE response (BEP 29: SYN-ACK)
	if err := c.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Debug("uTP failed to set read deadline", "error", err)
		return fmt.Errorf("set read deadline failed: %w", err)
	}

	log.Debug("uTP waiting for SYN-ACK", "remote", c.remoteAddr.String(), "timeout", "5s")
	buf := make([]byte, 1500)
	n, addr, err := c.conn.ReadFrom(buf)
	if err != nil {
		log.Debug("uTP SYN-ACK read failed", "remote", c.remoteAddr.String(), "error", err, "waited", time.Since(sentAt))
		return fmt.Errorf("read SYN-ACK failed: %w", err)
	}
	log.Debug("uTP received response",
		"remote", c.remoteAddr.String(),
		"from", addr.String(),
		"bytes", n,
		"rtt", time.Since(sentAt))

	// Verify address (BEP 29: must be from remote peer)
	if addr.String() != c.remoteAddr.String() {
		return fmt.Errorf("response from unexpected address: %s", addr.String())
	}

	// Parse header
	header, _, err := parseHeader(buf[:n])
	if err != nil {
		log.Debug("uTP failed to parse SYN-ACK",
			"remote", c.remoteAddr.String(),
			"bytes_received", n,
			"error", err)
		return fmt.Errorf("parse SYN-ACK header failed: %w", err)
	}

	log.Debug("uTP parsed SYN-ACK header",
		"remote", c.remoteAddr.String(),
		"type", header.Type,
		"version", header.Ver,
		"conn_id", header.ConnectionID,
		"seq_nr", header.SeqNr,
		"ack_nr", header.AckNr,
		"wnd_size", header.WndSize)

	// Verify it's ST_STATE packet (BEP 29: response to SYN)
	if header.Type != ST_STATE {
		log.Debug("uTP received unexpected packet type",
			"remote", c.remoteAddr.String(),
			"expected", ST_STATE,
			"got", header.Type)
		return fmt.Errorf("expected ST_STATE, got type %d", header.Type)
	}

	// Update state per BEP 29
	c.state.mu.Lock()
	c.state.ackNr = header.SeqNr
	c.state.wndSize = header.WndSize
	c.state.connIDSend = header.ConnectionID
	c.state.mu.Unlock()

	// Calculate RTT (BEP 29: for timeout calculation)
	rttMicros := time.Since(sentAt).Microseconds()
	c.state.UpdateRTT(rttMicros)

	// Update reply_micro (BEP 29: for timestamp_diff in next packet)
	now := uint32(time.Now().UnixNano() / 1000)
	delay := now - header.Timestamp
	c.state.mu.Lock()
	c.state.replyMicro = delay
	c.state.mu.Unlock()

	// Update base_delay for congestion control (BEP 29)
	c.state.UpdateBaseDelay(delay)

	// Send final ACK to complete handshake (BEP 29)
	ackPacket := c.buildHeader(ST_STATE)
	ackBytes, err := c.conn.WriteTo(ackPacket, c.remoteAddr)
	if err != nil {
		log.Debug("uTP failed to send final ACK",
			"remote", c.remoteAddr.String(),
			"error", err)
		return fmt.Errorf("send final ACK failed: %w", err)
	}
	log.Debug("uTP sent final ACK",
		"remote", c.remoteAddr.String(),
		"bytes", ackBytes)

	// Set state to CS_CONNECTED (BEP 29)
	c.state.SetState(CS_CONNECTED)

	log.Debug("uTP handshake complete",
		"remote", c.remoteAddr.String(),
		"local", c.conn.LocalAddr().String(),
		"conn_id_send", c.state.connIDSend,
		"conn_id_recv", c.state.connIDRecv,
		"rtt_us", rttMicros)

	return nil
}

// Read implements net.Conn.
func (c *Conn) Read(b []byte) (int, error) {
	if c.recvChan != nil {
		return c.readFromChan(b)
	}
	return c.readFromSocket(b)
}

// readFromChan is used in acceptor mode: packets arrive via the Listener's readLoop.
func (c *Conn) readFromChan(b []byte) (int, error) {
	for {
		select {
		case <-c.ctx.Done():
			return 0, c.ctx.Err()
		case pkt, ok := <-c.recvChan:
			if !ok {
				return 0, io.EOF
			}
			n, done, err := c.processPacket(b, pkt)
			if done || err != nil {
				return n, err
			}
			// Not done (e.g. ACK-only packet): wait for next packet.
		}
	}
}

// readFromSocket is used in dialer mode: reads directly from the UDP socket.
func (c *Conn) readFromSocket(b []byte) (int, error) {
	deadline, ok := c.ctx.Deadline()
	if ok {
		c.conn.SetReadDeadline(deadline)
	}
	for {
		buf := make([]byte, 1500)
		n, addr, err := c.conn.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		if addr.String() != c.remoteAddr.String() {
			continue
		}
		dataLen, done, err := c.processPacket(b, buf[:n])
		if done || err != nil {
			return dataLen, err
		}
	}
}

// processPacket parses a raw UDP payload and updates connection state.
// Returns (n, done, err): if done=true, caller should return immediately.
func (c *Conn) processPacket(b, pkt []byte) (int, bool, error) {
	header, headerSize, err := parseHeader(pkt)
	if err != nil {
		return 0, false, nil // ignore malformed
	}

	// Delay tracking for LEDBAT congestion control (BEP 29).
	now := uint32(time.Now().UnixNano() / 1000)
	delay := now - header.Timestamp
	c.state.mu.Lock()
	c.state.replyMicro = delay
	c.state.lastRecvTime = time.Now()
	c.state.mu.Unlock()
	c.state.UpdateBaseDelay(delay)
	c.state.mu.RLock()
	ourDelay := delay - c.state.baseDelay
	c.state.mu.RUnlock()
	c.state.AdjustWindow(ourDelay)

	switch header.Type {
	case ST_DATA:
		c.state.mu.Lock()
		c.state.ackNr = header.SeqNr
		c.state.mu.Unlock()
		// Send ACK.
		ack := c.buildHeader(ST_STATE)
		c.conn.WriteTo(ack, c.remoteAddr) //nolint:errcheck
		dataLen := len(pkt) - headerSize
		if dataLen > 0 {
			n := copy(b, pkt[headerSize:])
			return n, true, nil
		}
		return 0, false, nil

	case ST_STATE:
		c.state.mu.Lock()
		c.state.wndSize = header.WndSize
		c.state.mu.Unlock()
		toRetransmit := c.state.ProcessAck(header.AckNr)
		for _, seqNr := range toRetransmit {
			_ = c.retransmitPacket(seqNr)
		}
		c.state.ResetTimeout()
		return 0, false, nil

	case ST_FIN:
		c.state.SetState(CS_CLOSED)
		return 0, true, fmt.Errorf("connection closed by peer")

	case ST_RESET:
		c.state.SetState(CS_CLOSED)
		return 0, true, fmt.Errorf("connection reset by peer")
	}
	return 0, false, nil
}

// Write implements net.Conn
// NOTE: This is a simplified implementation. Full BEP 29 compliance requires:
// - Proper sequence number management
// - Retransmission on packet loss
// - Window-based flow control
// - Packet pacing
func (c *Conn) Write(b []byte) (n int, err error) {
	// Check if connected
	if c.state.GetState() != CS_CONNECTED {
		return 0, fmt.Errorf("not connected")
	}

	// Set write deadline
	deadline, ok := c.ctx.Deadline()
	if ok {
		c.conn.SetWriteDeadline(deadline)
	}

	// Split data into chunks (BEP 29: packet size management)
	const maxDataSize = MAX_PACKET_SIZE
	totalSent := 0

	for totalSent < len(b) {
		chunkSize := len(b) - totalSent
		if chunkSize > maxDataSize {
			chunkSize = maxDataSize
		}

		// Wait until window allows sending (BEP 29: flow control)
		for !c.state.CanSend(uint32(chunkSize + 20)) {
			time.Sleep(10 * time.Millisecond)
		}

		// Get sequence number before incrementing
		seqNr := c.state.IncrementSeqNr()

		// Build DATA packet
		header := c.buildHeader(ST_DATA)
		packet := make([]byte, 20+chunkSize)
		copy(packet[:20], header)
		copy(packet[20:], b[totalSent:totalSent+chunkSize])

		// Track outstanding packet (BEP 29: for ACK processing and retransmission)
		dataCopy := make([]byte, chunkSize)
		copy(dataCopy, b[totalSent:totalSent+chunkSize])
		c.state.AddOutstandingPacket(seqNr, dataCopy)

		// Update cur_window (BEP 29: bytes in flight)
		c.state.mu.Lock()
		c.state.curWindow += uint32(len(packet))
		c.state.mu.Unlock()

		// Send packet
		_, err := c.conn.WriteTo(packet, c.remoteAddr)
		if err != nil {
			return totalSent, err
		}

		// Reset timeout counter on successful send (BEP 29)
		c.state.ResetTimeout()

		totalSent += chunkSize
	}

	return totalSent, nil
}

// retransmitPacket resends a lost packet (BEP 29: packet loss handling)
func (c *Conn) retransmitPacket(seqNr uint16) error {
	pkt := c.state.GetOutstandingPacket(seqNr)
	if pkt == nil {
		return fmt.Errorf("packet %d not found", seqNr)
	}

	// Rebuild packet with same sequence number
	header := c.buildHeader(ST_DATA)
	packet := make([]byte, 20+len(pkt.data))
	copy(packet[:20], header)
	copy(packet[20:], pkt.data)

	// Update sequence number in header to match original
	header[16] = byte(seqNr >> 8)
	header[17] = byte(seqNr)
	copy(packet[:20], header)

	// Resend packet
	if _, err := c.conn.WriteTo(packet, c.remoteAddr); err != nil {
		return fmt.Errorf("retransmit failed: %w", err)
	}

	// Update packet's sent time
	c.state.MarkPacketResent(seqNr)

	return nil
}

// Close implements net.Conn (BEP 29: send ST_FIN).
func (c *Conn) Close() error {
	if c.state.GetState() == CS_CONNECTED {
		fin := c.buildHeader(ST_FIN)
		c.conn.WriteTo(fin, c.remoteAddr) //nolint:errcheck
		c.state.SetState(CS_FIN_SENT)
	}
	c.cancel()
	if c.listener != nil {
		// Acceptor mode: unregister from listener and signal recvChan readers.
		c.listener.removeConn(c)
		return nil
	}
	// Dialer mode: we own the socket.
	return c.conn.Close()
}

// LocalAddr implements net.Conn
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr implements net.Conn
func (c *Conn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// SetDeadline implements net.Conn
func (c *Conn) SetDeadline(t time.Time) error {
	c.ctx, c.cancel = context.WithDeadline(c.ctx, t)
	return nil
}

// SetReadDeadline implements net.Conn
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline implements net.Conn
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// ============================================================
// Listener — incoming uTP connection server (BEP 29)
// ============================================================

// connKey uniquely identifies an active uTP connection on a shared UDP socket.
// We key by (remoteAddr, ourRecvID) so that DATA packets (conn_id = dialer's send_id
// = our recv_id) map directly to the right Conn.
type connKey struct {
	addr   string
	connID uint16
}

// Listener accepts incoming uTP connections on a shared UDP port.
type Listener struct {
	conn   net.PacketConn
	mu     sync.Mutex
	conns  map[connKey]*Conn
	accept chan *Conn
	ctx    context.Context
	cancel context.CancelFunc
}

// Listen creates a uTP listener on the given address (e.g. ":6881").
func Listen(ctx context.Context, addr string) (*Listener, error) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("uTP listen %s: %w", addr, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	l := &Listener{
		conn:   conn,
		conns:  make(map[connKey]*Conn),
		accept: make(chan *Conn, 32),
		ctx:    ctx,
		cancel: cancel,
	}
	go l.readLoop()
	return l, nil
}

// Accept blocks until a new uTP connection is established or ctx is done.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-l.ctx.Done():
		return nil, l.ctx.Err()
	}
}

// Addr returns the listener's network address.
func (l *Listener) Addr() net.Addr { return l.conn.LocalAddr() }

// Close stops the listener and all active connections.
func (l *Listener) Close() error {
	l.cancel()
	return l.conn.Close()
}

// removeConn is called by Conn.Close() in acceptor mode.
func (l *Listener) removeConn(c *Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Find and delete this Conn from the map.
	for k, v := range l.conns {
		if v == c {
			delete(l.conns, k)
			break
		}
	}
}

// readLoop receives UDP datagrams and dispatches them to the right Conn,
// or creates a new Conn when a SYN arrives.
func (l *Listener) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := l.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-l.ctx.Done():
				return
			default:
				continue
			}
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		l.dispatch(addr, pkt)
	}
}

// dispatch routes a received UDP packet to an existing Conn or creates one on SYN.
func (l *Listener) dispatch(addr net.Addr, pkt []byte) {
	if len(pkt) < 20 {
		return
	}
	h, _, err := parseHeader(pkt)
	if err != nil {
		return
	}
	addrStr := addr.String()

	if h.Type == ST_SYN {
		// BEP 29 §Connection Setup:
		//   SYN.conn_id  = dialer.recv_id
		//   our send_id  = SYN.conn_id
		//   our recv_id  = SYN.conn_id + 1  (= dialer.send_id)
		//
		// We key the conn under our recv_id so that subsequent DATA packets
		// (which carry conn_id = dialer.send_id = our recv_id) match directly.
		key := connKey{addr: addrStr, connID: h.ConnectionID + 1}

		l.mu.Lock()
		if _, exists := l.conns[key]; exists {
			l.mu.Unlock()
			return // Duplicate SYN — ignore.
		}

		ctx, cancel := context.WithCancel(l.ctx)
		c := &Conn{
			conn:       l.conn,
			remoteAddr: addr,
			ctx:        ctx,
			cancel:     cancel,
			state:      NewState(),
			recvChan:   make(chan []byte, 64),
			listener:   l,
		}
		// Set conn IDs per BEP 29 acceptor convention.
		c.state.mu.Lock()
		c.state.connIDSend = h.ConnectionID     // our send_id = dialer's recv_id
		c.state.connIDRecv = h.ConnectionID + 1 // our recv_id = dialer's send_id
		c.state.ackNr = h.SeqNr
		now := uint32(time.Now().UnixNano() / 1000)
		c.state.replyMicro = now - h.Timestamp
		c.state.mu.Unlock()
		c.state.UpdateBaseDelay(now - h.Timestamp)

		l.conns[key] = c
		l.mu.Unlock()

		// Respond with ST_STATE (SYN-ACK).
		synAck := c.buildHeader(ST_STATE)
		if _, err := l.conn.WriteTo(synAck, addr); err != nil {
			l.mu.Lock()
			delete(l.conns, key)
			l.mu.Unlock()
			cancel()
			return
		}
		c.state.SetState(CS_CONNECTED)

		select {
		case l.accept <- c:
		default:
			// Accept backlog full — drop connection.
			l.mu.Lock()
			delete(l.conns, key)
			l.mu.Unlock()
			cancel()
		}
		return
	}

	// Non-SYN: find existing Conn by our recv_id (= packet's conn_id for DATA/FIN/RESET)
	// or our send_id (= packet's conn_id for ST_STATE ACKs back to us).
	l.mu.Lock()
	c, ok := l.conns[connKey{addr: addrStr, connID: h.ConnectionID}]
	l.mu.Unlock()
	if !ok {
		return
	}

	select {
	case c.recvChan <- pkt:
	default:
		// Receiver too slow — drop packet (congestion control will retransmit).
	}
}
