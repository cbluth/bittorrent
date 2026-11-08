package ws

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestPeerRegistrations(t *testing.T) {
	tracker := &Tracker{}
	swarm := &Swarm{Peers: make(map[string]*Peer)}
	connection := &Peer{}
	first := binaryString(bytes.Repeat([]byte{'a'}, 20))
	second := binaryString(bytes.Repeat([]byte{'b'}, 20))
	firstID := hex.EncodeToString(first)
	secondID := hex.EncodeToString(second)

	tracker.addPeer(swarm, connection, &AnnounceRequest{PeerID: first, Left: 1}, false)
	tracker.addPeer(swarm, connection, &AnnounceRequest{PeerID: second, Left: 1}, false)
	if len(swarm.Peers) != 2 || swarm.Leechers != 2 {
		t.Fatalf("expected two independent registrations, got %d peers and %d leechers", len(swarm.Peers), swarm.Leechers)
	}

	other := &Peer{}
	tracker.addPeer(swarm, other, &AnnounceRequest{PeerID: first, Left: 1}, true)
	if swarm.Peers[firstID].owner != connection {
		t.Fatal("answer moved peer registration to a different connection")
	}

	tracker.addPeer(swarm, other, &AnnounceRequest{PeerID: first, Left: 1}, false)
	tracker.removePeer(swarm, connection, firstID)
	if swarm.Peers[firstID].owner != other {
		t.Fatal("closing an old connection removed a transferred peer")
	}

	tracker.removePeer(swarm, connection, secondID)
	if len(swarm.Peers) != 1 || swarm.Leechers != 1 {
		t.Fatalf("incorrect peer cleanup: %d peers and %d leechers", len(swarm.Peers), swarm.Leechers)
	}
}
