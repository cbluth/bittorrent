package pex

import (
	"net"
	"testing"
	"time"
)

func mustTCPAddr(s string) *net.TCPAddr {
	a, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		panic(err)
	}
	return a
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
	peers := []PeerInfo{
		{Addr: mustTCPAddr("1.2.3.4:6881"), Flags: FlagEncryption | FlagUTP},
		{Addr: mustTCPAddr("5.6.7.8:51413"), Flags: FlagSeed},
		{Addr: mustTCPAddr("[2001:db8::1]:6881"), Flags: FlagUTP},
	}
	dropped := []PeerInfo{
		{Addr: mustTCPAddr("9.10.11.12:1234"), Flags: 0},
	}

	msg := &Message{Added: peers, Dropped: dropped}

	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(got.Added) != len(msg.Added) {
		t.Errorf("Added: got %d peers, want %d", len(got.Added), len(msg.Added))
	}
	if len(got.Dropped) != len(msg.Dropped) {
		t.Errorf("Dropped: got %d peers, want %d", len(got.Dropped), len(msg.Dropped))
	}

	// Spot-check first IPv4 peer.
	if len(got.Added) > 0 {
		p := got.Added[0]
		if p.Addr.String() != "1.2.3.4:6881" {
			t.Errorf("Added[0].Addr = %s, want 1.2.3.4:6881", p.Addr)
		}
		if p.Flags != FlagEncryption|FlagUTP {
			t.Errorf("Added[0].Flags = %02x, want %02x", p.Flags, FlagEncryption|FlagUTP)
		}
	}

	// Spot-check IPv6 peer (third in input, position may vary after split).
	var foundV6 bool
	for _, p := range got.Added {
		if p.Addr.IP.To4() == nil {
			foundV6 = true
			if p.Addr.Port != 6881 {
				t.Errorf("IPv6 peer port = %d, want 6881", p.Addr.Port)
			}
			if p.Flags != FlagUTP {
				t.Errorf("IPv6 peer flags = %02x, want %02x", p.Flags, FlagUTP)
			}
		}
	}
	if !foundV6 {
		t.Error("no IPv6 peer found in decoded Added list")
	}
}

func TestEncodeEmpty(t *testing.T) {
	data, err := Encode(&Message{})
	if err != nil {
		t.Fatalf("Encode empty: %v", err)
	}
	// Should produce a valid empty bencoded dict.
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode empty: %v", err)
	}
	if len(got.Added) != 0 || len(got.Dropped) != 0 {
		t.Errorf("empty message decoded non-empty: %+v", got)
	}
}

func TestStateSnapshot(t *testing.T) {
	s := NewState()
	peers := []PeerInfo{
		{Addr: mustTCPAddr("1.1.1.1:6881"), Flags: FlagEncryption},
		{Addr: mustTCPAddr("2.2.2.2:6881"), Flags: 0},
	}

	msg, err := s.BuildMessage(peers)
	if err != nil {
		t.Fatalf("first BuildMessage: %v", err)
	}
	if msg == nil {
		t.Fatal("first BuildMessage returned nil, want snapshot")
	}
	if len(msg.Added) != 2 {
		t.Errorf("snapshot Added count = %d, want 2", len(msg.Added))
	}
	if len(msg.Dropped) != 0 {
		t.Errorf("snapshot Dropped count = %d, want 0", len(msg.Dropped))
	}
}

func TestStateRateLimit(t *testing.T) {
	s := NewState()
	peers := []PeerInfo{{Addr: mustTCPAddr("1.1.1.1:6881")}}

	_, _ = s.BuildMessage(peers) // first call — snapshot, advances lastSent

	// Second call immediately should be rate-limited.
	msg, err := s.BuildMessage(peers)
	if err != nil {
		t.Fatalf("second BuildMessage: %v", err)
	}
	if msg != nil {
		t.Errorf("expected nil (rate-limited), got %+v", msg)
	}
}

func TestStateDiff(t *testing.T) {
	s := NewState()
	initial := []PeerInfo{
		{Addr: mustTCPAddr("1.1.1.1:6881")},
		{Addr: mustTCPAddr("2.2.2.2:6881")},
	}

	_, _ = s.BuildMessage(initial)

	// Advance the clock by manipulating lastSent directly.
	s.mu.Lock()
	s.lastSent = time.Now().Add(-MinInterval - time.Second)
	s.mu.Unlock()

	// Second call: one peer dropped, one added.
	updated := []PeerInfo{
		{Addr: mustTCPAddr("2.2.2.2:6881")}, // still here
		{Addr: mustTCPAddr("3.3.3.3:6881")}, // new
	}
	msg, err := s.BuildMessage(updated)
	if err != nil {
		t.Fatalf("diff BuildMessage: %v", err)
	}
	if msg == nil {
		t.Fatal("diff BuildMessage returned nil")
	}

	// Expect 1 added (3.3.3.3) and 1 dropped (1.1.1.1).
	if len(msg.Added) != 1 {
		t.Errorf("diff Added = %d, want 1", len(msg.Added))
	}
	if len(msg.Dropped) != 1 {
		t.Errorf("diff Dropped = %d, want 1", len(msg.Dropped))
	}
	if len(msg.Added) > 0 && msg.Added[0].Addr.String() != "3.3.3.3:6881" {
		t.Errorf("diff Added[0] = %s, want 3.3.3.3:6881", msg.Added[0].Addr)
	}
	if len(msg.Dropped) > 0 && msg.Dropped[0].Addr.String() != "1.1.1.1:6881" {
		t.Errorf("diff Dropped[0] = %s, want 1.1.1.1:6881", msg.Dropped[0].Addr)
	}
}
