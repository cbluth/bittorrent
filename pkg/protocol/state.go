// Package protocol implements uTP connection state management per BEP 29
package protocol

import (
	"math/rand/v2"
	"sync"
	"time"
)

// State tracks connection state and all per-connection data
type State struct {
	mu sync.RWMutex

	// Connection IDs
	connIDSend uint16 // Our sending connection ID
	connIDRecv uint16 // Our receiving connection ID

	// Sequence numbers
	seqNr uint16 // Next seq_nr to send
	ackNr uint16 // Last seq_nr received

	// Connection state
	state int // CS_* constants

	// Window management (bytes)
	maxWindow uint32 // Maximum bytes in flight
	curWindow uint32 // Current bytes in flight
	wndSize   uint32 // Advertised receive window from peer

	// RTT tracking (microseconds)
	rtt    int64 // Round trip time
	rttVar int64 // RTT variance

	// Timeout management
	timeout       time.Duration // Current packet timeout
	timeoutCount  int           // Consecutive timeouts
	lastResetTime time.Time     // Last timeout reset

	// Delay-based congestion control (BEP 29)
	baseDelay    uint32    // Minimum delay observed (sliding 2min window)
	baseDelays   []uint32  // Last 120 Delay samples (2 min at 1 sample/sec)
	baseDelayIdx int       // Index for circular buffer
	replyMicro   uint32    // Last delay measurement from peer
	lastRecvTime time.Time // Last packet receive time

	// Packet tracking
	outstandingPackets map[uint16]*outstandingPacket
	receivedPackets    map[uint16][]byte // Out-of-order received packets buffer

	// Duplicate ACK detection (BEP 29: 3 duplicate acks trigger fast retransmit)
	lastAckNr     uint16 // Last ack_nr received
	duplicateAcks int    // Count of duplicate acks for same ack_nr

	// Selective ACK support
	selectiveAcks []byte // Bitmask of received packets beyond ack_nr
	eofPkt        uint16 // Expected final sequence number (from ST_FIN)
}

// outstandingPacket tracks a sent packet awaiting ACK
type outstandingPacket struct {
	seqNr       uint16
	sentAt      time.Time
	data        []byte
	resendCount int
}

// NewState creates a new uTP connection state
func NewState() *State {
	state := &State{
		connIDRecv:         uint16(rand.Uint32()),
		connIDSend:         uint16(rand.Uint32()) + 1,
		seqNr:              1,
		ackNr:              0,
		state:              CS_IDLE,
		maxWindow:          100000, // Start with 100KB window
		curWindow:          0,
		wndSize:            1048576, // 1MB receive buffer
		rtt:                0,
		rttVar:             0,
		timeout:            INIT_TIMEOUT,
		timeoutCount:       0,
		lastResetTime:      time.Now(),
		baseDelay:          0,
		baseDelays:         make([]uint32, 120),
		baseDelayIdx:       0,
		replyMicro:         0,
		lastRecvTime:       time.Now(),
		outstandingPackets: make(map[uint16]*outstandingPacket),
		receivedPackets:    make(map[uint16][]byte),
		lastAckNr:          0,
		duplicateAcks:      0,
		selectiveAcks:      make([]byte, 0),
		eofPkt:             0,
	}

	return state
}

// UpdateBaseDelay updates sliding minimum delay (BEP 29: base_delay)
func (s *State) UpdateBaseDelay(delay uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Add to circular buffer
	s.baseDelays[s.baseDelayIdx] = delay
	s.baseDelayIdx = (s.baseDelayIdx + 1) % len(s.baseDelays)

	// Find minimum in the buffer
	minDelay := delay
	for _, d := range s.baseDelays {
		if d > 0 && d < minDelay {
			minDelay = d
		}
	}
	s.baseDelay = minDelay
}

// UpdateRTT updates RTT and timeout per BEP 29 section "timeouts"
func (s *State) UpdateRTT(packetRTT int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rtt == 0 {
		// First measurement
		s.rtt = packetRTT
		s.rttVar = packetRTT / 2
	} else {
		// BEP 29 formula:
		// delta = rtt - packet_rtt
		// rtt_var += (abs(delta) - rtt_var) / 4
		// rtt += (packet_rtt - rtt) / 8
		delta := s.rtt - packetRTT
		if delta < 0 {
			delta = -delta
		}
		s.rttVar += (delta - s.rttVar) / 4
		s.rtt += (packetRTT - s.rtt) / 8
	}

	// Calculate timeout based on RTT measurements
	// timeout = max(rtt + rtt_var * 4, 500ms)
	timeoutMs := (s.rtt + s.rttVar*4) / 1000
	if timeoutMs < 500 {
		timeoutMs = 500
	}
	s.timeout = time.Duration(timeoutMs) * time.Millisecond
}

// AdjustWindow performs delay-based congestion control per BEP 29
func (s *State) AdjustWindow(ourDelay uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.baseDelay == 0 {
		return // Need base delay first
	}

	// Calculate off_target (BEP 29: CCONTROL_TARGET - our_delay)
	targetMicros := uint32(CCONTROL_TARGET.Microseconds())
	var offTarget int64
	if ourDelay > targetMicros {
		offTarget = -int64(ourDelay - targetMicros)
	} else {
		offTarget = int64(targetMicros - ourDelay)
	}

	// Calculate window adjustment (BEP 29 congestion control section)
	// delay_factor = off_target / CCONTROL_TARGET
	// window_factor = outstanding_packets / max_window
	// scaled_gain = MAX_CWND_INCREASE_PACKETS_PER_RTT * delay_factor * window_factor
	delayFactor := float64(offTarget) / float64(targetMicros)
	windowFactor := float64(s.curWindow) / float64(s.maxWindow)
	scaledGain := float64(MAX_CWND_INCREASE_PACKETS_PER_RTT) * delayFactor * windowFactor

	// Update max_window
	newWindow := int64(s.maxWindow) + int64(scaledGain)
	if newWindow < 0 {
		newWindow = 0
	}
	s.maxWindow = uint32(newWindow)

	// Ensure minimum window size
	if s.maxWindow < MIN_WINDOW_SIZE {
		s.maxWindow = MIN_WINDOW_SIZE
	}
}

// OnPacketLoss handles packet loss per BEP 29: multiply max_window by 0.5
func (s *State) OnPacketLoss() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.maxWindow = s.maxWindow / 2
	if s.maxWindow < MIN_WINDOW_SIZE {
		s.maxWindow = MIN_WINDOW_SIZE
	}
}

// CanSend checks if we can send a packet given window limits
func (s *State) CanSend(packetSize uint32) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Can send if: cur_window + packet_size <= min(max_window, wnd_size)
	maxAllowed := s.maxWindow
	if s.wndSize < maxAllowed {
		maxAllowed = s.wndSize
	}

	return s.curWindow+packetSize <= maxAllowed
}

// IncrementSeqNr increments and returns next sequence number
func (s *State) IncrementSeqNr() uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()

	seq := s.seqNr
	s.seqNr++
	return seq
}

// GetState returns current connection state
func (s *State) GetState() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// SetState updates connection state
func (s *State) SetState(state int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

// AddOutstandingPacket tracks a sent packet
func (s *State) AddOutstandingPacket(seqNr uint16, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.outstandingPackets[seqNr] = &outstandingPacket{
		seqNr:       seqNr,
		sentAt:      time.Now(),
		data:        data,
		resendCount: 0,
	}
}

// GetOutstandingPacket retrieves a packet for retransmission
func (s *State) GetOutstandingPacket(seqNr uint16) *outstandingPacket {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.outstandingPackets[seqNr]
}

// RemoveOutstandingPacket removes an ACKed packet
func (s *State) RemoveOutstandingPacket(seqNr uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pktSize := uint32(20) // Header size
	if pkt, exists := s.outstandingPackets[seqNr]; exists {
		pktSize += uint32(len(pkt.data))
	}

	// Decrease cur_window (bytes now ACKed, no longer in flight)
	if s.curWindow >= pktSize {
		s.curWindow -= pktSize
	} else {
		s.curWindow = 0
	}

	delete(s.outstandingPackets, seqNr)
}

// MarkPacketResent updates the sent time for a retransmitted packet
func (s *State) MarkPacketResent(seqNr uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if pkt, exists := s.outstandingPackets[seqNr]; exists {
		pkt.sentAt = time.Now()
		pkt.resendCount++
	}
}

// ProcessAck processes received ACK and detects duplicate ACKs (BEP 29: packet loss section)
// Returns list of packets to retransmit if packet loss detected
func (s *State) ProcessAck(ackNr uint16) []uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()

	toRetransmit := make([]uint16, 0)

	// Check for duplicate ACK (BEP 29: 3 duplicate acks trigger fast retransmit)
	if ackNr == s.lastAckNr {
		s.duplicateAcks++

		// Fast retransmit on 3rd duplicate ACK (BEP 29: lines 363-375)
		if s.duplicateAcks >= 3 {
			// Packet ack_nr + 1 is assumed lost
			lostSeqNr := ackNr + 1
			if _, exists := s.outstandingPackets[lostSeqNr]; exists {
				toRetransmit = append(toRetransmit, lostSeqNr)
				// Reduce window on packet loss (BEP 29)
				s.OnPacketLoss()
			}
			// Reset duplicate count
			s.duplicateAcks = 0
		}
	} else if ackNr > s.lastAckNr {
		// New ACK received, reset duplicate counter
		s.duplicateAcks = 0
		s.lastAckNr = ackNr

		// Remove ACKed packets from outstanding and update cur_window
		for seqNr := s.lastAckNr; seqNr <= ackNr; seqNr++ {
			if pkt, exists := s.outstandingPackets[seqNr]; exists {
				pktSize := uint32(20 + len(pkt.data))
				if s.curWindow >= pktSize {
					s.curWindow -= pktSize
				} else {
					s.curWindow = 0
				}
				// Remove from outstanding
				delete(s.outstandingPackets, seqNr)
			}
		}
	}

	return toRetransmit
}

// DetectPacketLossViaSeqGap detects packet loss via sequence number gaps (BEP 29)
// If 3+ packets are ACKed past the oldest unacked packet, it's assumed lost
func (s *State) DetectPacketLossViaSeqGap(ackNr uint16, selectiveAcks []byte) []uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()

	toRetransmit := make([]uint16, 0)

	// Find the oldest outstanding packet
	var oldestSeqNr uint16 = 0xFFFF
	for seqNr := range s.outstandingPackets {
		if oldestSeqNr == 0xFFFF || seqNr < oldestSeqNr {
			oldestSeqNr = seqNr
		}
	}

	if oldestSeqNr == 0xFFFF {
		return toRetransmit // No outstanding packets
	}

	// Count how many packets after oldestSeqNr have been ACKed
	ackedAfter := 0

	// Check selective acks
	for i, ackByte := range selectiveAcks {
		for bit := 0; bit < 8; bit++ {
			if ackByte&(1<<bit) != 0 {
				seqOffset := uint16(i*8 + bit + 2) // +2 because ACK+1 is assumed dropped
				if seqOffset > oldestSeqNr {
					ackedAfter++
				}
			}
		}
	}

	// BEP 29: If 3 or more packets have been ACKed past it, packet is assumed lost
	if ackedAfter >= 3 {
		if _, exists := s.outstandingPackets[oldestSeqNr]; exists {
			toRetransmit = append(toRetransmit, oldestSeqNr)
			// Reduce window on packet loss (BEP 29)
			s.OnPacketLoss()
		}
	}

	return toRetransmit
}

// CheckTimeout checks if any outstanding packets have timed out (BEP 29)
// Returns list of packets to retransmit
func (s *State) CheckTimeout() []uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()

	toRetransmit := make([]uint16, 0)
	now := time.Now()

	for seqNr, pkt := range s.outstandingPackets {
		if now.Sub(pkt.sentAt) > s.timeout {
			toRetransmit = append(toRetransmit, seqNr)

			// Double timeout for next packet (BEP 29: exponential backoff)
			s.timeout = s.timeout * 2
			s.timeoutCount++

			// Reduce window on timeout (BEP 29)
			s.OnPacketLoss()
		}
	}

	return toRetransmit
}

// ResetTimeout resets timeout counter (BEP 29)
func (s *State) ResetTimeout() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastResetTime = time.Now()
	s.timeoutCount = 0
}

// UpdateDelayTracking updates delay measurements for congestion control
func (s *State) UpdateDelayTracking(timestamp uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := uint32(time.Now().UnixNano() / 1000)
	delay := now - timestamp

	// Update reply_micro (BEP 29: for timestamp_diff in next packet)
	s.replyMicro = delay

	// Update base_delay for congestion control (BEP 29)
	s.UpdateBaseDelay(delay)

	// Calculate our_delay and adjust window (BEP 29 congestion control)
	ourDelay := delay - s.baseDelay
	s.AdjustWindow(ourDelay)

	s.lastRecvTime = time.Now()
}

// ProcessSelectiveAck processes selective ACK extension
func (s *State) ProcessSelectiveAck(selectiveAcks []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.selectiveAcks = selectiveAcks
}

// GetConnectionIDs returns the connection IDs for this connection
func (s *State) GetConnectionIDs() (recvID, sendID uint16) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connIDRecv, s.connIDSend
}

// SetConnectionIDs sets the connection IDs during handshake
func (s *State) SetConnectionIDs(recvID, sendID uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connIDRecv = recvID
	s.connIDSend = sendID
}

// GetWindowInfo returns current window information for debugging
func (s *State) GetWindowInfo() (maxWindow, curWindow, wndSize uint32) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxWindow, s.curWindow, s.wndSize
}

// GetRTTInfo returns RTT information for debugging
func (s *State) GetRTTInfo() (rtt, rttVar int64, timeout time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rtt, s.rttVar, s.timeout
}
