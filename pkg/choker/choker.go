// Package choker implements the BitTorrent choking algorithm (BEP 3).
//
// # Purpose
//
// The choking algorithm controls which peers we upload data to.  Uploading to
// everyone simultaneously would saturate our upload bandwidth without improving
// our download rate.  By being selective, we implement tit-for-tat: we prefer
// peers who upload to us, which incentivises mutual cooperation.
//
// # Algorithm overview
//
// Every [RechokePeriod] (10 s) the choker selects [Slots]−1 peers to receive
// regular unchoke slots and one additional peer for the optimistic unchoke slot.
//
// Regular slot selection:
//   - Leecher mode: rank interested peers by bytes downloaded FROM them in the
//     last measurement window (descending).  Top [Slots]−1 win regular slots.
//   - Seeder mode:  rank interested peers by bytes uploaded TO them (descending).
//   - Snubbed peers (no data received for [SnubTimeout] seconds) are excluded
//     from regular slots regardless of mode.
//
// Optimistic unchoke (rotated every [OptimisticPeriod] = 30 s):
//   - One slot is always reserved for a randomly selected choked-but-interested
//     peer, giving them a chance to demonstrate they are a good upload partner.
//   - Newly connected peers (< [NewPeerWindow]) are given 3× the selection
//     probability to accelerate swarm discovery.
//
// # Integration
//
// The caller provides two callbacks in [Config]:
//   - GetPeers returns the current snapshot of all peer stats.  Called every tick.
//   - SetChoked applies a choke or unchoke wire message to a specific peer.
//     This is called outside the choker's internal mutex.
//
// Call [New] to start the background ticker goroutine, and [Stop] when the
// torrent session ends.  [Trigger] forces an immediate rechoke cycle (useful
// when a peer connects or disconnects).
package choker

import (
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// ── Tunables ──────────────────────────────────────────────────────────────────

const (
	// DefaultSlots is the default number of unchoke slots (regular + optimistic).
	DefaultSlots = 4

	// RechokePeriod is how often the regular unchoke set is recomputed.
	RechokePeriod = 10 * time.Second

	// OptimisticPeriod is how often the optimistic unchoke slot is rotated.
	OptimisticPeriod = 30 * time.Second

	// SnubTimeout is how long a peer can be silent (no piece data received)
	// before the choker marks it snubbed and excludes it from regular slots.
	SnubTimeout = 60 * time.Second

	// NewPeerWindow is the age threshold for 3× optimistic-unchoke weighting.
	// Peers connected more recently than this are preferred for discovery.
	NewPeerWindow = 3 * time.Minute
)

// ── Types ─────────────────────────────────────────────────────────────────────

// PeerID uniquely identifies a peer connection.
// In practice this is the remote address string ("1.2.3.4:6881") or peer_id.
type PeerID string

// PeerStats is a snapshot of the metrics the choker needs for one peer.
// The peer connection layer is responsible for updating these values before
// each GetPeers call.
type PeerStats struct {
	ID PeerID

	// Interested is true if the peer has sent us an Interested message and not
	// yet a NotInterested message.
	Interested bool

	// BytesDownloadedFrom is the bytes received from this peer in the current
	// measurement window (~20 s recommended).  Used in leecher mode ranking.
	BytesDownloadedFrom int64

	// BytesUploadedTo is the bytes sent to this peer in the current measurement
	// window.  Used in seeder mode ranking.
	BytesUploadedTo int64

	// LastReceived is the wall time of the most recent piece block received from
	// this peer.  Zero if no data has ever been received.  Used for snub detection.
	LastReceived time.Time

	// ConnectedAt is when the TCP connection to this peer was established.
	// Used to give newly connected peers a 3× optimistic-unchoke weight.
	ConnectedAt time.Time
}

// isSnubbed returns true if the peer has not sent us any piece data for longer
// than SnubTimeout.  Peers that have never sent data are not considered snubbed
// (they may not have had a chance yet).
func (p *PeerStats) isSnubbed() bool {
	return !p.LastReceived.IsZero() && time.Since(p.LastReceived) > SnubTimeout
}

// isNew returns true if the peer connected recently enough to qualify for the
// 3× optimistic-unchoke weighting.
func (p *PeerStats) isNew() bool {
	return !p.ConnectedAt.IsZero() && time.Since(p.ConnectedAt) < NewPeerWindow
}

// ── Choker ────────────────────────────────────────────────────────────────────

// Choker implements the BEP 3 choking algorithm.
type Choker struct {
	mu       sync.Mutex
	slots    int // total unchoke slots (regular + 1 optimistic)
	isSeeder bool

	// regular holds the IDs of peers currently holding a regular unchoke slot.
	// The optimistic peer is tracked separately in optPeer.
	regular map[PeerID]struct{}
	optPeer PeerID // "" means no optimistic peer

	rechokeTicker    *time.Ticker
	optimisticTicker *time.Ticker
	trigger          chan struct{} // unblocks run() for an immediate rechoke
	done             chan struct{}

	getPeers  func() []PeerStats
	setChoked func(id PeerID, choked bool)
}

// Config holds constructor parameters for [New].
type Config struct {
	// Slots is the total number of unchoke slots (regular + 1 optimistic).
	// 0 falls back to [DefaultSlots].
	Slots int

	// IsSeeder indicates we have 100% of the torrent's pieces.  Switches the
	// ranking metric from download rate to upload rate.
	IsSeeder bool

	// GetPeers is called each rechoke cycle to obtain the current peer stats.
	// It must be non-nil.
	GetPeers func() []PeerStats

	// SetChoked is called to apply a choke (true) or unchoke (false) decision
	// to a peer.  Called outside the choker's internal mutex.
	// It must be non-nil.
	SetChoked func(id PeerID, choked bool)
}

// New creates a Choker and starts the background rechoke goroutine.
// Call [Choker.Stop] when the torrent session ends to release resources.
func New(cfg Config) *Choker {
	if cfg.GetPeers == nil {
		panic("choker.New: GetPeers callback is required")
	}
	if cfg.SetChoked == nil {
		panic("choker.New: SetChoked callback is required")
	}
	slots := cfg.Slots
	if slots <= 0 {
		slots = DefaultSlots
	}
	c := &Choker{
		slots:            slots,
		isSeeder:         cfg.IsSeeder,
		regular:          make(map[PeerID]struct{}),
		trigger:          make(chan struct{}, 1),
		done:             make(chan struct{}),
		rechokeTicker:    time.NewTicker(RechokePeriod),
		optimisticTicker: time.NewTicker(OptimisticPeriod),
		getPeers:         cfg.GetPeers,
		setChoked:        cfg.SetChoked,
	}
	go c.run()
	return c
}

// Stop shuts down the background goroutine.  Safe to call multiple times.
func (c *Choker) Stop() {
	c.rechokeTicker.Stop()
	c.optimisticTicker.Stop()
	select {
	case <-c.done: // already stopped
	default:
		close(c.done)
	}
}

// SetSeeder switches between leecher ranking (by download rate) and seeder
// ranking (by upload rate).  Triggers an immediate rechoke.
func (c *Choker) SetSeeder(seeder bool) {
	c.mu.Lock()
	c.isSeeder = seeder
	c.mu.Unlock()
	c.Trigger()
}

// Trigger forces an immediate rechoke cycle without waiting for the next tick.
// Call this when a peer connects, disconnects, or changes interest state.
func (c *Choker) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default: // already pending
	}
}

// Unchoked returns a copy of the current set of unchoked peer IDs
// (regular + optimistic).  Safe to call from any goroutine.
func (c *Choker) Unchoked() []PeerID {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]PeerID, 0, len(c.regular)+1)
	for id := range c.regular {
		out = append(out, id)
	}
	if c.optPeer != "" {
		if _, inRegular := c.regular[c.optPeer]; !inRegular {
			out = append(out, c.optPeer)
		}
	}
	return out
}

// ── Background loop ───────────────────────────────────────────────────────────

func (c *Choker) run() {
	for {
		select {
		case <-c.rechokeTicker.C:
			c.rechoke()
		case <-c.trigger:
			c.rechoke()
		case <-c.optimisticTicker.C:
			c.rotateOptimistic()
		case <-c.done:
			return
		}
	}
}

// ── Rechoke ───────────────────────────────────────────────────────────────────

// rechoke recomputes the regular unchoke set and applies changes.
// It never touches the optimistic slot — that is rotateOptimistic's job.
func (c *Choker) rechoke() {
	decisions := c.computeRechoke()
	for id, choked := range decisions {
		c.setChoked(id, choked)
	}
}

// computeRechoke holds the mutex only long enough to compute decisions and
// update internal state, then returns decisions for the caller to apply.
func (c *Choker) computeRechoke() map[PeerID]bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	allPeers := c.getPeers()

	// Build a quick lookup of all current peer IDs so we can detect disconnected peers.
	currentIDs := make(map[PeerID]struct{}, len(allPeers))
	for _, p := range allPeers {
		currentIDs[p.ID] = struct{}{}
	}

	// ── Select candidates for regular slots ───────────────────────────────────
	//
	// Eligible = interested AND not snubbed.
	candidates := make([]PeerStats, 0, len(allPeers))
	for _, p := range allPeers {
		if p.Interested && !p.isSnubbed() {
			candidates = append(candidates, p)
		}
	}

	// Sort by rate metric (descending).
	isSeeder := c.isSeeder
	sort.Slice(candidates, func(i, j int) bool {
		if isSeeder {
			return candidates[i].BytesUploadedTo > candidates[j].BytesUploadedTo
		}
		return candidates[i].BytesDownloadedFrom > candidates[j].BytesDownloadedFrom
	})

	// Regular slots = total slots − 1 (the 1 reserved for the optimistic peer).
	regularSlots := c.slots - 1
	if regularSlots < 1 {
		regularSlots = 1
	}

	newRegular := make(map[PeerID]struct{}, regularSlots)
	for i, p := range candidates {
		if i >= regularSlots {
			break
		}
		newRegular[p.ID] = struct{}{}
	}

	// ── Compute choke/unchoke decisions ───────────────────────────────────────
	//
	// The optimistic peer stays unchoked regardless (rotateOptimistic manages it),
	// UNLESS it has disconnected or is no longer interested.
	decisions := make(map[PeerID]bool)

	// Peers that were unchoked (regular or optimistic) but should now be choked.
	wasUnchoked := func(id PeerID) bool {
		_, inRegular := c.regular[id]
		return inRegular || id == c.optPeer
	}
	shouldBeUnchoked := func(id PeerID) bool {
		_, inNew := newRegular[id]
		if inNew {
			return true
		}
		// Optimistic peer stays unchoked if it's still connected and interested.
		if id == c.optPeer {
			for _, p := range allPeers {
				if p.ID == id && p.Interested {
					return true
				}
			}
		}
		return false
	}

	for _, p := range allPeers {
		was := wasUnchoked(p.ID)
		should := shouldBeUnchoked(p.ID)
		if was && !should {
			decisions[p.ID] = true // choke
		} else if !was && should {
			decisions[p.ID] = false // unchoke
		}
	}

	// If the optimistic peer disconnected, clear it.
	if c.optPeer != "" {
		if _, alive := currentIDs[c.optPeer]; !alive {
			c.optPeer = ""
		}
	}

	// Update internal state.
	c.regular = newRegular

	return decisions
}

// ── Optimistic unchoke ────────────────────────────────────────────────────────

// rotateOptimistic picks a new optimistic unchoke peer.
func (c *Choker) rotateOptimistic() {
	prev, next := c.computeOptimistic()

	// Apply decisions outside the mutex.
	if prev != "" {
		c.setChoked(prev, true)
	}
	if next != "" {
		c.setChoked(next, false)
	}
}

// computeOptimistic selects the next optimistic peer and returns the previous
// peer to choke (if it should be choked) and the new peer to unchoke.
func (c *Choker) computeOptimistic() (prevToChoke, nextToUnchoke PeerID) {
	c.mu.Lock()
	defer c.mu.Unlock()

	allPeers := c.getPeers()

	// ── Build weighted candidate pool ─────────────────────────────────────────
	//
	// Eligible = interested AND NOT already in regular unchoke set.
	// New peers (ConnectedAt < NewPeerWindow) get 3 entries in the pool for 3×
	// selection probability, giving the swarm a chance to discover fast peers.
	type candidate struct {
		id PeerID
	}
	pool := make([]candidate, 0, len(allPeers)*3)
	for _, p := range allPeers {
		if !p.Interested {
			continue
		}
		if _, inRegular := c.regular[p.ID]; inRegular {
			continue
		}
		weight := 1
		if p.isNew() {
			weight = 3
		}
		for i := 0; i < weight; i++ {
			pool = append(pool, candidate{id: p.ID})
		}
	}

	if len(pool) == 0 {
		// No candidates: clear optimistic slot, choke old peer if necessary.
		if c.optPeer != "" {
			if _, inRegular := c.regular[c.optPeer]; !inRegular {
				prevToChoke = c.optPeer
			}
			c.optPeer = ""
		}
		return
	}

	// Pick uniformly from the weighted pool.
	chosen := pool[rand.IntN(len(pool))].id

	// If the chosen peer is the same as the current optimistic peer, keep it
	// unchoked but don't re-send an unchoke message.
	if chosen == c.optPeer {
		return
	}

	// Choke the old optimistic peer if it's not in the regular set.
	if c.optPeer != "" {
		if _, inRegular := c.regular[c.optPeer]; !inRegular {
			prevToChoke = c.optPeer
		}
	}

	c.optPeer = chosen
	nextToUnchoke = chosen
	return
}
