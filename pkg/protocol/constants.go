// Package protocol defines constants for uTP (Micro Transport Protocol) per BEP 29
package protocol

import "time"

// Packet types per BEP 29
const (
	ST_DATA  = 0 // Regular data packet
	ST_FIN   = 1 // Finalize connection
	ST_STATE = 2 // State packet (ACK only)
	ST_RESET = 3 // Reset connection
	ST_SYN   = 4 // Connect SYN
)

// Extension types per BEP 29
const (
	EXT_NO_EXTENSION  = 0 // No extensions
	EXT_SELECTIVE_ACK = 1 // Selective ACK extension
)

// Connection states per BEP 29
const (
	CS_IDLE      = iota // Connection idle
	CS_SYN_SENT         // SYN sent, waiting for SYN-ACK
	CS_SYN_RECV         // SYN received, sent SYN-ACK
	CS_CONNECTED        // Connected
	CS_FIN_SENT         // FIN sent, waiting for FIN-ACK
	CS_CLOSED           // Connection closed
)

// Constants per BEP 29
const (
	// Congestion control targets
	CCONTROL_TARGET                   = 100 * time.Millisecond // Target delay
	MAX_CWND_INCREASE_PACKETS_PER_RTT = 3000                   // Max window increase per RTT

	// Packet sizes
	MIN_WINDOW_SIZE = 150  // Minimum packet size
	MAX_PACKET_SIZE = 1400 // Maximum data per packet

	// Timeouts
	INIT_TIMEOUT = 1000 * time.Millisecond // Initial timeout
	MIN_TIMEOUT  = 500 * time.Millisecond  // Minimum timeout

	// Protocol version
	UTP_VERSION = 1
)
