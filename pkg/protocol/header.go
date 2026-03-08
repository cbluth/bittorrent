// Package protocol implements uTP (Micro Transport Protocol) per BEP 29
// This is a complete implementation of uTP with full congestion control,
// packet loss handling, selective ACKs, and retransmission logic.
package protocol

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Header represents uTP packet header per BEP 29 (20 bytes + extensions)
type Header struct {
	Type          uint8       // 4 bits: packet type
	Version       uint8       // 4 bits: protocol version (must be 1)
	Extension     uint8       // 8 bits: extension type
	ConnectionID  uint16      // 16 bits: connection identifier
	Timestamp     uint32      // 32 bits: timestamp in microseconds
	TimestampDiff uint32      // 32 bits: timestamp difference
	WndSize       uint32      // 32 bits: advertised window size
	SeqNr         uint16      // 16 bits: sequence number
	AckNr         uint16      // 16 bits: acknowledgment number
	Extensions    []Extension // Linked list of extensions
}

// Extension represents a uTP extension header
type Extension struct {
	Type  byte
	Bytes []byte
}

// SelectiveAck represents selective ACK extension
type SelectiveAck struct {
	Bitmask []byte // Bitmask of received packets
}

// NewHeader creates a new uTP header
func NewHeader(packetType uint8, connID uint16) *Header {
	return &Header{
		Type:          packetType,
		Version:       UTP_VERSION,
		Extension:     EXT_NO_EXTENSION,
		ConnectionID:  connID,
		Timestamp:     uint32(time.Now().UnixNano() / 1000),
		TimestampDiff: 0,
		WndSize:       0,
		SeqNr:         0,
		AckNr:         0,
		Extensions:    nil,
	}
}

// Marshal serializes header to bytes
func (h *Header) Marshal() ([]byte, error) {
	// Calculate total size: 20 bytes base + extensions
	totalSize := 20
	for _, ext := range h.Extensions {
		totalSize += 2 + len(ext.Bytes) // 2-byte ext header + payload
	}

	buf := make([]byte, totalSize)

	// Pack base header (20 bytes)
	buf[0] = (h.Type << 4) | h.Version
	buf[1] = h.Extension
	binary.BigEndian.PutUint16(buf[2:4], h.ConnectionID)
	binary.BigEndian.PutUint32(buf[4:8], h.Timestamp)
	binary.BigEndian.PutUint32(buf[8:12], h.TimestampDiff)
	binary.BigEndian.PutUint32(buf[12:16], h.WndSize)
	binary.BigEndian.PutUint16(buf[16:18], h.SeqNr)
	binary.BigEndian.PutUint16(buf[18:20], h.AckNr)

	// Pack extensions
	offset := 20
	for _, ext := range h.Extensions {
		if offset+2+len(ext.Bytes) > len(buf) {
			return nil, fmt.Errorf("extension overflow")
		}
		buf[offset] = ext.Type
		buf[offset+1] = byte(len(ext.Bytes))
		copy(buf[offset+2:], ext.Bytes)
		offset += 2 + len(ext.Bytes)

		// Set next extension pointer to 0 (terminate list)
		if offset < len(buf) {
			buf[offset] = 0
		}
	}

	return buf, nil
}

// Unmarshal deserializes header from bytes
func (h *Header) Unmarshal(buf []byte) error {
	if len(buf) < 20 {
		return fmt.Errorf("packet too small: %d bytes", len(buf))
	}

	// Parse base header
	h.Type = buf[0] >> 4
	h.Version = buf[0] & 0x0F
	h.Extension = buf[1]
	h.ConnectionID = binary.BigEndian.Uint16(buf[2:4])
	h.Timestamp = binary.BigEndian.Uint32(buf[4:8])
	h.TimestampDiff = binary.BigEndian.Uint32(buf[8:12])
	h.WndSize = binary.BigEndian.Uint32(buf[12:16])
	h.SeqNr = binary.BigEndian.Uint16(buf[16:18])
	h.AckNr = binary.BigEndian.Uint16(buf[18:20])

	// Validate version
	if h.Version != UTP_VERSION {
		return fmt.Errorf("unsupported uTP version: %d", h.Version)
	}

	// Parse extensions
	h.Extensions = nil
	if h.Extension != EXT_NO_EXTENSION {
		offset := 20
		for offset < len(buf) {
			if offset+2 > len(buf) {
				return fmt.Errorf("extension header truncated")
			}

			extType := buf[offset]
			extLen := int(buf[offset+1])
			if offset+2+extLen > len(buf) {
				return fmt.Errorf("extension payload truncated")
			}

			ext := Extension{
				Type:  extType,
				Bytes: make([]byte, extLen),
			}
			copy(ext.Bytes, buf[offset+2:offset+2+extLen])

			h.Extensions = append(h.Extensions, ext)
			offset += 2 + extLen

			// Terminate if next extension type is 0
			if extType == EXT_NO_EXTENSION {
				break
			}
		}
	}

	return nil
}

// AddSelectiveAck adds selective ACK extension to header
func (h *Header) AddSelectiveAck(ackBitmask []byte) {
	selAck := SelectiveAck{
		Bitmask: ackBitmask,
	}

	ext := Extension{
		Type:  EXT_SELECTIVE_ACK,
		Bytes: selAck.Bitmask,
	}

	h.Extensions = append(h.Extensions, ext)
}

// ParseSelectiveAck parses selective ACK from extensions
func (h *Header) ParseSelectiveAck() (*SelectiveAck, error) {
	for _, ext := range h.Extensions {
		if ext.Type == EXT_SELECTIVE_ACK {
			return &SelectiveAck{
				Bitmask: ext.Bytes,
			}, nil
		}
	}
	return nil, fmt.Errorf("selective ACK extension not found")
}

// String returns string representation of header for debugging
func (h *Header) String() string {
	typeStr := map[uint8]string{
		ST_DATA:  "DATA",
		ST_FIN:   "FIN",
		ST_STATE: "STATE",
		ST_RESET: "RESET",
		ST_SYN:   "SYN",
	}

	return fmt.Sprintf("Type=%s Ver=%d ConnID=%d Seq=%d Ack=%d Wnd=%d",
		typeStr[h.Type], h.Version, h.ConnectionID, h.SeqNr, h.AckNr, h.WndSize)
}
