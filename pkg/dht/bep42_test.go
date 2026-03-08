package dht

import (
	"net"
	"testing"
)

// Test vectors from BEP 42 specification.
//
// IP           rand  expected node ID prefix
// 124.31.75.21   1   5fbfbf
// 21.75.31.124  86   5a3ce9
// 65.23.51.170  22   a5d432
// 84.124.73.14  65   1b0321
// 43.213.53.83  90   e56f6c

func TestComputeBEP42CRC(t *testing.T) {
	tests := []struct {
		ip     string
		rand   byte // full rand byte (id[19])
		prefix [3]byte
	}{
		{"124.31.75.21", 1, [3]byte{0x5f, 0xbf, 0xbf}},
		{"21.75.31.124", 86, [3]byte{0x5a, 0x3c, 0xe9}},
		{"65.23.51.170", 22, [3]byte{0xa5, 0xd4, 0x32}},
		{"84.124.73.14", 65, [3]byte{0x1b, 0x03, 0x21}},
		{"43.213.53.83", 90, [3]byte{0xe5, 0x6f, 0x6c}},
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		r := tt.rand & 0x07
		crc := computeBEP42CRC(ip, r)

		got0 := byte(crc >> 24)
		got1 := byte(crc >> 16)
		got2 := byte(crc >> 8)

		if got0 != tt.prefix[0] || got1 != tt.prefix[1] || (got2&0xf8) != (tt.prefix[2]&0xf8) {
			t.Errorf("IP=%s rand=%d: got prefix %02x%02x%02x, want %02x%02x%02x (21-bit match)",
				tt.ip, tt.rand, got0, got1, got2, tt.prefix[0], tt.prefix[1], tt.prefix[2])
		}
	}
}

func TestGenerateSecureNodeID(t *testing.T) {
	ip := net.ParseIP("124.31.75.21")

	for i := 0; i < 100; i++ {
		id := GenerateSecureNodeID(ip)
		if !ValidateNodeID(id, ip) {
			t.Fatalf("generated ID %x does not validate for IP %s", id, ip)
		}
	}
}

func TestValidateNodeID(t *testing.T) {
	// Construct a known-valid ID from test vector: IP=124.31.75.21, rand=1
	// prefix=5fbfbf, last byte=01
	ip := net.ParseIP("124.31.75.21")
	var id Key
	id[0] = 0x5f
	id[1] = 0xbf
	id[2] = 0xbf // top 5 bits: 0xb8, plus 0x07 from low bits = 0xbf
	id[19] = 0x01

	if !ValidateNodeID(id, ip) {
		t.Errorf("expected valid for known test vector")
	}

	// Corrupt the first byte
	id[0] = 0x00
	if ValidateNodeID(id, ip) {
		t.Errorf("expected invalid after corrupting first byte")
	}
}

func TestValidateNodeID_LocalExempt(t *testing.T) {
	// Local IPs are always exempt
	local := net.ParseIP("192.168.1.1")
	var id Key // all zeros — would fail for any public IP
	if !ValidateNodeID(id, local) {
		t.Errorf("local IP should be exempt from BEP 42 validation")
	}
}
