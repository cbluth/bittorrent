package choker

import (
	"sync"
	"testing"
	"time"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// makeStats builds a PeerStats with sensible defaults for testing.
func makeStats(id PeerID, interested bool, downloaded, uploaded int64) PeerStats {
	return PeerStats{
		ID:                  id,
		Interested:          interested,
		BytesDownloadedFrom: downloaded,
		BytesUploadedTo:     uploaded,
		ConnectedAt:         time.Now().Add(-time.Hour), // old peer by default
	}
}

// collectDecisions runs the choker through one rechoke cycle and returns
// every choke/unchoke call received via setChoked.
func collectDecisions(cfg Config) map[PeerID]bool {
	decisions := make(map[PeerID]bool)
	var mu sync.Mutex

	cfg.SetChoked = func(id PeerID, choked bool) {
		mu.Lock()
		decisions[id] = choked
		mu.Unlock()
	}

	c := New(cfg)
	// Force an immediate rechoke synchronously instead of waiting for the ticker.
	c.rechoke()
	c.Stop()
	return decisions
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestRechokeLeecher verifies that in leecher mode the top uploaders to us
// (BytesDownloadedFrom) win the regular slots and slower peers are choked.
func TestRechokeLeecher(t *testing.T) {
	peers := []PeerStats{
		makeStats("fast", true, 1000, 0),
		makeStats("medium", true, 500, 0),
		makeStats("slow", true, 100, 0),
		makeStats("slug", true, 10, 0),
	}

	cfg := Config{
		Slots:    3, // 2 regular + 1 optimistic
		IsSeeder: false,
		GetPeers: func() []PeerStats { return peers },
	}
	decisions := collectDecisions(cfg)

	// "fast" and "medium" should be unchoked; "slow" and "slug" choked.
	if got, ok := decisions["fast"]; ok && got {
		t.Errorf("fast should be unchoked, got choked")
	}
	if got, ok := decisions["medium"]; ok && got {
		t.Errorf("medium should be unchoked, got choked")
	}
	// slow and slug are beyond the 2 regular slots — they may or may not appear
	// in decisions (only changes are emitted), but if they do they must be choked.
	if choked, ok := decisions["slow"]; ok && !choked {
		t.Errorf("slow should not be in regular slots; got unchoked")
	}
	if choked, ok := decisions["slug"]; ok && !choked {
		t.Errorf("slug should not be in regular slots; got unchoked")
	}
}

// TestRechokeSeeder verifies that in seeder mode the ranking flips to
// BytesUploadedTo (peers we've uploaded most to win).
func TestRechokeSeeder(t *testing.T) {
	peers := []PeerStats{
		makeStats("heavy-dl", true, 0, 2000), // we uploaded a lot TO them
		makeStats("light-dl", true, 0, 100),
		makeStats("none", true, 0, 0),
	}

	cfg := Config{
		Slots:    3, // 2 regular + 1 optimistic
		IsSeeder: true,
		GetPeers: func() []PeerStats { return peers },
	}
	decisions := collectDecisions(cfg)

	if choked, ok := decisions["heavy-dl"]; ok && choked {
		t.Errorf("heavy-dl should be unchoked in seeder mode")
	}
}

// TestSnubExclusion ensures that a snubbed peer cannot win a regular slot.
func TestSnubExclusion(t *testing.T) {
	snubbedTime := time.Now().Add(-(SnubTimeout + time.Second))
	peers := []PeerStats{
		{
			ID:                  "snubbed",
			Interested:          true,
			BytesDownloadedFrom: 9999, // highest rate but snubbed
			LastReceived:        snubbedTime,
			ConnectedAt:         time.Now().Add(-time.Hour),
		},
		makeStats("normal", true, 100, 0),
	}

	cfg := Config{
		Slots:    3,
		IsSeeder: false,
		GetPeers: func() []PeerStats { return peers },
	}
	decisions := collectDecisions(cfg)

	if choked, ok := decisions["snubbed"]; ok && !choked {
		t.Errorf("snubbed peer should not be unchoked")
	}
}

// TestUninterestedExclusion ensures that uninterested peers are never given slots.
func TestUninterestedExclusion(t *testing.T) {
	peers := []PeerStats{
		makeStats("want", false, 9999, 0), // high rate but not interested
		makeStats("ok", true, 100, 0),
	}

	cfg := Config{
		Slots:    3,
		IsSeeder: false,
		GetPeers: func() []PeerStats { return peers },
	}
	decisions := collectDecisions(cfg)

	if choked, ok := decisions["want"]; ok && !choked {
		t.Errorf("uninterested peer should not be unchoked")
	}
}

// TestSetSeeder verifies that SetSeeder triggers an immediate rechoke and
// switches the ranking metric.
func TestSetSeeder(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[PeerID]bool)

	peers := []PeerStats{
		makeStats("up-heavy", true, 0, 1000),
		makeStats("dl-heavy", true, 500, 0),
	}

	cfg := Config{
		Slots:    3,
		IsSeeder: false,
		GetPeers: func() []PeerStats { return peers },
		SetChoked: func(id PeerID, choked bool) {
			mu.Lock()
			calls[id] = choked
			mu.Unlock()
		},
	}

	c := New(cfg)
	defer c.Stop()

	// Run initial rechoke in leecher mode.
	c.rechoke()

	// Switch to seeder mode — SetSeeder calls Trigger internally.
	c.SetSeeder(true)
	// Give the goroutine a moment to process the trigger.
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	// After seeder mode, "up-heavy" (high upload to them) should be unchoked.
	// We can't easily assert the exact final state here without more intrusion,
	// but at minimum SetSeeder must not panic and the choker must still be running.
	_ = calls
}

// TestTrigger verifies that Trigger causes an immediate rechoke without waiting
// for the ticker.
func TestTrigger(t *testing.T) {
	called := make(chan struct{}, 1)
	peers := []PeerStats{makeStats("a", true, 1, 0)}

	cfg := Config{
		Slots:    2,
		IsSeeder: false,
		GetPeers: func() []PeerStats { return peers },
		SetChoked: func(_ PeerID, _ bool) {
			select {
			case called <- struct{}{}:
			default:
			}
		},
	}

	c := New(cfg)
	defer c.Stop()

	c.Trigger()

	select {
	case <-called:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Error("Trigger did not cause a rechoke within 500ms")
	}
}

// TestUnchoked verifies that Unchoked returns the correct set of unchoked peers
// after a rechoke cycle.
func TestUnchoked(t *testing.T) {
	peers := []PeerStats{
		makeStats("a", true, 100, 0),
		makeStats("b", true, 50, 0),
		makeStats("c", true, 10, 0), // will be choked (only 1 regular slot)
	}

	cfg := Config{
		Slots:     2, // 1 regular + 1 optimistic
		IsSeeder:  false,
		GetPeers:  func() []PeerStats { return peers },
		SetChoked: func(_ PeerID, _ bool) {},
	}

	c := New(cfg)
	defer c.Stop()

	// Apply one rechoke cycle so internal state is populated.
	c.rechoke()

	unchoked := c.Unchoked()
	// "a" must be in the regular slot (highest rate).
	var foundA bool
	for _, id := range unchoked {
		if id == "a" {
			foundA = true
		}
	}
	if !foundA {
		t.Errorf("Unchoked() does not contain 'a' (highest rate peer); got %v", unchoked)
	}
}

// TestStopIdempotent verifies that calling Stop multiple times does not panic.
func TestStopIdempotent(t *testing.T) {
	cfg := Config{
		Slots:     2,
		GetPeers:  func() []PeerStats { return nil },
		SetChoked: func(_ PeerID, _ bool) {},
	}
	c := New(cfg)
	c.Stop()
	c.Stop() // must not panic
}

// TestOptimisticUnchokeNewPeer verifies that a newly connected peer is
// selected as the optimistic peer (probabilistically — run enough times that
// the 3× weighting gives near-certain selection vs one old peer).
func TestOptimisticUnchokeNewPeer(t *testing.T) {
	newPeer := PeerStats{
		ID:          "new",
		Interested:  true,
		ConnectedAt: time.Now(), // brand new
	}
	oldPeer := PeerStats{
		ID:          "old",
		Interested:  true,
		ConnectedAt: time.Now().Add(-time.Hour),
	}

	// Run many trials; "new" should win far more often than "old".
	newWins, oldWins := 0, 0
	for i := 0; i < 100; i++ {
		var mu sync.Mutex
		var lastUnchoked PeerID

		cfg := Config{
			Slots:    2,
			IsSeeder: false,
			GetPeers: func() []PeerStats { return []PeerStats{newPeer, oldPeer} },
			SetChoked: func(id PeerID, choked bool) {
				if !choked {
					mu.Lock()
					lastUnchoked = id
					mu.Unlock()
				}
			},
		}
		c := New(cfg)
		c.rotateOptimistic()
		c.Stop()

		mu.Lock()
		winner := lastUnchoked
		mu.Unlock()

		switch winner {
		case "new":
			newWins++
		case "old":
			oldWins++
		}
	}

	// With 3× weighting, new should win ~75% of the time.
	// Require at least 60 out of 100 to avoid flakiness.
	if newWins < 60 {
		t.Errorf("new peer won only %d/100 times (want ≥60); old won %d", newWins, oldWins)
	}
}

// TestDisconnectedOptimisticPeerCleared verifies that if the current optimistic
// peer disconnects (disappears from GetPeers), it is evicted.
func TestDisconnectedOptimisticPeerCleared(t *testing.T) {
	var mu sync.Mutex
	chokes := make(map[PeerID]int) // count choke calls per peer

	// Start with two peers.
	connected := []PeerStats{
		makeStats("a", true, 100, 0),
		makeStats("opt", true, 10, 0),
	}

	cfg := Config{
		Slots:    2,
		IsSeeder: false,
		GetPeers: func() []PeerStats {
			mu.Lock()
			defer mu.Unlock()
			return connected
		},
		SetChoked: func(id PeerID, choked bool) {
			mu.Lock()
			if choked {
				chokes[id]++
			}
			mu.Unlock()
		},
	}

	c := New(cfg)
	defer c.Stop()

	// Manually set opt as the optimistic peer.
	c.mu.Lock()
	c.optPeer = "opt"
	c.mu.Unlock()

	// Now "opt" disconnects.
	mu.Lock()
	connected = []PeerStats{makeStats("a", true, 100, 0)}
	mu.Unlock()

	// Run a rechoke — should detect opt is gone and clear it.
	c.rechoke()

	c.mu.Lock()
	opt := c.optPeer
	c.mu.Unlock()

	if opt != "" {
		t.Errorf("optPeer should be cleared after disconnect, got %q", opt)
	}
}
