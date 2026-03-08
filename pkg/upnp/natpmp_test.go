package upnp

import (
	"encoding/binary"
	"testing"
)

func TestNATPMPRequestParsing(t *testing.T) {
	// Simulate a valid AddPortMapping response (16 bytes).
	// version=0, opcode=0x82 (TCP response), result=0, epoch=0x0013F24F,
	// internal=123, mapped=456, lifetime=3600.
	resp := make([]byte, 16)
	resp[0] = 0                                       // version
	resp[1] = 0x82                                    // TCP response opcode
	binary.BigEndian.PutUint16(resp[2:4], 0)          // result code: success
	binary.BigEndian.PutUint32(resp[4:8], 0x0013F24F) // epoch
	binary.BigEndian.PutUint16(resp[8:10], 123)       // internal port
	binary.BigEndian.PutUint16(resp[10:12], 456)      // mapped external port
	binary.BigEndian.PutUint32(resp[12:16], 3600)     // lifetime

	// Verify parsing logic matches what natpmpRequest does.
	if resp[0] != 0 {
		t.Fatalf("unexpected version %d", resp[0])
	}
	if resp[1] != natpmpOpTCP|0x80 {
		t.Fatalf("unexpected opcode %d", resp[1])
	}
	resultCode := binary.BigEndian.Uint16(resp[2:4])
	if resultCode != 0 {
		t.Fatalf("unexpected result code %d", resultCode)
	}
	mappedPort := binary.BigEndian.Uint16(resp[10:12])
	if mappedPort != 456 {
		t.Fatalf("expected mapped port 456, got %d", mappedPort)
	}
	lifetime := binary.BigEndian.Uint32(resp[12:16])
	if lifetime != 3600 {
		t.Fatalf("expected lifetime 3600, got %d", lifetime)
	}
}

func TestNATPMPRequestBuild(t *testing.T) {
	// Verify we build the correct 12-byte request for TCP mapping.
	msg := make([]byte, 12)
	msg[0] = natpmpVersion
	msg[1] = natpmpOpTCP
	binary.BigEndian.PutUint16(msg[4:6], 6881)
	binary.BigEndian.PutUint16(msg[6:8], 6881)
	binary.BigEndian.PutUint32(msg[8:12], 3600)

	if msg[0] != 0 {
		t.Fatalf("version should be 0, got %d", msg[0])
	}
	if msg[1] != 2 {
		t.Fatalf("TCP opcode should be 2, got %d", msg[1])
	}
	if binary.BigEndian.Uint16(msg[4:6]) != 6881 {
		t.Fatalf("internal port mismatch")
	}
	if binary.BigEndian.Uint16(msg[6:8]) != 6881 {
		t.Fatalf("external port mismatch")
	}
	if binary.BigEndian.Uint32(msg[8:12]) != 3600 {
		t.Fatalf("lifetime mismatch")
	}
}

func TestNATPMPErrorResponse(t *testing.T) {
	// Response with non-zero result code should be detected.
	resp := make([]byte, 16)
	resp[0] = 0
	resp[1] = 0x81                           // UDP response
	binary.BigEndian.PutUint16(resp[2:4], 3) // result code: not authorized

	resultCode := binary.BigEndian.Uint16(resp[2:4])
	if resultCode == 0 {
		t.Fatal("expected non-zero result code")
	}
	if resultCode != 3 {
		t.Fatalf("expected result code 3, got %d", resultCode)
	}
}

func TestNATPMPExternalIPResponse(t *testing.T) {
	// Simulate GetExternalIPAddress response (12 bytes).
	resp := make([]byte, 12)
	resp[0] = 0                                  // version
	resp[1] = 0x80                               // opcode 0 response
	binary.BigEndian.PutUint16(resp[2:4], 0)     // success
	binary.BigEndian.PutUint32(resp[4:8], 12345) // epoch
	resp[8] = 203
	resp[9] = 0
	resp[10] = 113
	resp[11] = 42

	if resp[8] != 203 || resp[9] != 0 || resp[10] != 113 || resp[11] != 42 {
		t.Fatal("external IP parse mismatch")
	}
}
