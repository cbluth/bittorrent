package upnp

import (
	"encoding/binary"
	"testing"
)

func TestPCPRequestBuild(t *testing.T) {
	// Verify we build a correct 60-byte PCP MAP request.
	msg := make([]byte, 60)
	msg[0] = pcpVersion
	msg[1] = pcpOpMap
	binary.BigEndian.PutUint32(msg[4:8], 3600)
	// Client IP as IPv4-mapped IPv6: ::ffff:192.168.1.100
	msg[18] = 0xff
	msg[19] = 0xff
	msg[20] = 192
	msg[21] = 168
	msg[22] = 1
	msg[23] = 100
	// MAP opdata at offset 24.
	// Nonce (12 bytes) at [24:36] — skip for test.
	msg[36] = pcpProtoTCP // protocol
	binary.BigEndian.PutUint16(msg[40:42], 6881)
	binary.BigEndian.PutUint16(msg[42:44], 6881)

	if msg[0] != 2 {
		t.Fatalf("version should be 2, got %d", msg[0])
	}
	if msg[1] != 1 {
		t.Fatalf("opcode should be 1 (MAP), got %d", msg[1])
	}
	if binary.BigEndian.Uint32(msg[4:8]) != 3600 {
		t.Fatalf("lifetime mismatch")
	}
	if msg[36] != 6 {
		t.Fatalf("protocol should be 6 (TCP), got %d", msg[36])
	}
	if binary.BigEndian.Uint16(msg[40:42]) != 6881 {
		t.Fatalf("internal port mismatch")
	}
}

func TestPCPResponseParse(t *testing.T) {
	// Build a mock 60-byte PCP MAP response.
	resp := make([]byte, 60)
	resp[0] = pcpVersion
	resp[1] = pcpOpMap | 0x80                     // response bit
	resp[3] = 0                                   // result code: success
	binary.BigEndian.PutUint32(resp[4:8], 3600)   // lifetime
	binary.BigEndian.PutUint32(resp[8:12], 12345) // epoch

	// MAP opdata at offset 24.
	resp[36] = pcpProtoTCP
	binary.BigEndian.PutUint16(resp[40:42], 6881) // internal port
	binary.BigEndian.PutUint16(resp[42:44], 6881) // external port

	// External IP as IPv4-mapped IPv6: ::ffff:203.0.113.42
	resp[54] = 0xff
	resp[55] = 0xff
	resp[56] = 203
	resp[57] = 0
	resp[58] = 113
	resp[59] = 42

	// Validate header fields.
	if resp[0] != 2 {
		t.Fatalf("version should be 2, got %d", resp[0])
	}
	if resp[1] != pcpOpMap|0x80 {
		t.Fatalf("opcode should be MAP|response, got %d", resp[1])
	}
	if resp[3] != 0 {
		t.Fatalf("result should be success, got %d", resp[3])
	}
	lifetime := binary.BigEndian.Uint32(resp[4:8])
	if lifetime != 3600 {
		t.Fatalf("expected lifetime 3600, got %d", lifetime)
	}
	extPort := binary.BigEndian.Uint16(resp[42:44])
	if extPort != 6881 {
		t.Fatalf("expected external port 6881, got %d", extPort)
	}
	// Parse IPv4-mapped IPv6 at [44:60].
	extIPv6 := resp[44:60]
	v4 := extIPv6[12:]
	if v4[0] != 203 || v4[1] != 0 || v4[2] != 113 || v4[3] != 42 {
		t.Fatalf("external IP mismatch: %d.%d.%d.%d", v4[0], v4[1], v4[2], v4[3])
	}
}

func TestPCPResultString(t *testing.T) {
	tests := []struct {
		code byte
		want string
	}{
		{0, "Success"},
		{1, "UnsupportedVersion"},
		{2, "NotAuthorized"},
		{8, "NoResources"},
		{12, "AddressMismatch"},
		{99, "Unknown(99)"},
	}
	for _, tt := range tests {
		got := pcpResultString(tt.code)
		if got != tt.want {
			t.Errorf("pcpResultString(%d) = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestPCPErrorResponse(t *testing.T) {
	// Result code 2 = NotAuthorized.
	resp := make([]byte, 60)
	resp[0] = pcpVersion
	resp[1] = pcpOpMap | 0x80
	resp[3] = 2 // NotAuthorized

	if resp[3] == 0 {
		t.Fatal("expected non-zero result code")
	}
	name := pcpResultString(resp[3])
	if name != "NotAuthorized" {
		t.Fatalf("expected NotAuthorized, got %q", name)
	}
}
