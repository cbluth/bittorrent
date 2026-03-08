package punch

import (
	"net"
	"testing"
)

// ── Round-trip tests ──────────────────────────────────────────────────────────

func TestEncodeDecodeRendezvousIPv4(t *testing.T) {
	msg := &Message{
		Type: MsgRendezvous,
		Addr: &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 6881},
	}

	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if got.Type != MsgRendezvous {
		t.Errorf("Type: got %d, want %d", got.Type, MsgRendezvous)
	}
	if !got.Addr.IP.Equal(net.IPv4(1, 2, 3, 4)) {
		t.Errorf("IP: got %v, want 1.2.3.4", got.Addr.IP)
	}
	if got.Addr.Port != 6881 {
		t.Errorf("Port: got %d, want 6881", got.Addr.Port)
	}
}

func TestEncodeDecodeConnectIPv6(t *testing.T) {
	ip6 := net.ParseIP("2001:db8::1")
	msg := &Message{
		Type: MsgConnect,
		Addr: &net.UDPAddr{IP: ip6, Port: 1234},
	}

	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if got.Type != MsgConnect {
		t.Errorf("Type: got %d, want %d", got.Type, MsgConnect)
	}
	if !got.Addr.IP.Equal(ip6) {
		t.Errorf("IP: got %v, want %v", got.Addr.IP, ip6)
	}
	if got.Addr.Port != 1234 {
		t.Errorf("Port: got %d, want 1234", got.Addr.Port)
	}
}

func TestEncodeDecodeError(t *testing.T) {
	msg := &Message{
		Type:    MsgError,
		Addr:    &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 6881},
		ErrCode: ErrNotConnected,
	}

	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if got.Type != MsgError {
		t.Errorf("Type: got %d, want %d", got.Type, MsgError)
	}
	if got.ErrCode != ErrNotConnected {
		t.Errorf("ErrCode: got %d, want %d", got.ErrCode, ErrNotConnected)
	}
}

func TestAllErrorCodes(t *testing.T) {
	codes := []uint8{ErrNotConnected, ErrNoSupport, ErrNoSelf, ErrInvalidAddr}
	for _, code := range codes {
		msg := &Message{
			Type:    MsgError,
			Addr:    &net.UDPAddr{IP: net.IPv4(192, 168, 1, 1), Port: 6881},
			ErrCode: code,
		}
		data, err := Encode(msg)
		if err != nil {
			t.Fatalf("code %d: Encode: %v", code, err)
		}
		got, err := Decode(data)
		if err != nil {
			t.Fatalf("code %d: Decode: %v", code, err)
		}
		if got.ErrCode != code {
			t.Errorf("code %d: round-trip got %d", code, got.ErrCode)
		}
	}
}

// ── Error cases ───────────────────────────────────────────────────────────────

func TestEncodeNilMessage(t *testing.T) {
	_, err := Encode(nil)
	if err == nil {
		t.Error("Encode(nil) should return an error")
	}
}

func TestEncodeNilAddr(t *testing.T) {
	msg := &Message{Type: MsgConnect, Addr: nil}
	_, err := Encode(msg)
	if err == nil {
		t.Error("Encode with nil Addr should return an error")
	}
}

func TestDecodeInvalidAddrLength(t *testing.T) {
	// Bencoded dict with a 3-byte addr (neither 6 nor 18 bytes).
	data := []byte("d4:addr3:abc8:msg_typei1ee")
	_, err := Decode(data)
	if err == nil {
		t.Error("Decode should fail for addr with invalid length (not 6 or 18)")
	}
}

// ── Port preservation ─────────────────────────────────────────────────────────

func TestPortPreservation(t *testing.T) {
	ports := []int{0, 1, 443, 6881, 65535}
	for _, port := range ports {
		msg := &Message{
			Type: MsgConnect,
			Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
		}
		data, err := Encode(msg)
		if err != nil {
			t.Fatalf("port %d: Encode: %v", port, err)
		}
		got, err := Decode(data)
		if err != nil {
			t.Fatalf("port %d: Decode: %v", port, err)
		}
		if got.Addr.Port != port {
			t.Errorf("port %d: after round-trip got %d", port, got.Addr.Port)
		}
	}
}

// ── Constants ─────────────────────────────────────────────────────────────────

func TestMessageTypeConstants(t *testing.T) {
	if MsgRendezvous != 0 {
		t.Errorf("MsgRendezvous: got %d, want 0", MsgRendezvous)
	}
	if MsgConnect != 1 {
		t.Errorf("MsgConnect: got %d, want 1", MsgConnect)
	}
	if MsgError != 2 {
		t.Errorf("MsgError: got %d, want 2", MsgError)
	}
}

func TestErrorCodeConstants(t *testing.T) {
	cases := []struct {
		code uint8
		want uint8
	}{
		{ErrNotConnected, 1},
		{ErrNoSupport, 2},
		{ErrNoSelf, 3},
		{ErrInvalidAddr, 4},
	}
	for _, tc := range cases {
		if tc.code != tc.want {
			t.Errorf("error code: got %d, want %d", tc.code, tc.want)
		}
	}
}
