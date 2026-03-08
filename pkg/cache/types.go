package cache

import "time"

// Package cache persists BitTorrent runtime state across process restarts.
//
// Each section of the cache maps to one or more BEP-defined behaviours:
//
//   DHT nodes  — BEP 5  (routing table, node state machine)
//                BEP 32 (IPv6 nodes; same structure, different address family)
//                BEP 42 (node ID derived from IP; SelfID must satisfy constraints)
//
//   Trackers   — BEP 3  (HTTP tracker announce/scrape)
//                BEP 15 (UDP tracker protocol)
//                BEP 48 (tracker scrape — failure/success counts map here)
//
//   Infohashes — BEP 5  (peers discovered via get_peers)
//                BEP 33 (DHT scrape — seed/leech counts per infohash)
//                BEP 51 (sample_infohashes — bulk discovery; source="dht_sample")
//                BEP 11 (ut_pex — peer exchange; source="pex")
//
//   Items      — BEP 44 (mutable/immutable DHT items; key→value with optional seq+sig)
//
//   Feeds      — BEP 46 (single updating torrent: pubkey → current infohash + seq)
//                BEP 49 (distributed torrent feed: pubkey → list of infohashes + seq)

// CurrentCacheVersion is the cache format version.
// Increment when making breaking changes to the structure.
const CurrentCacheVersion = 3

// NodeState models the BEP 5 node state machine.
// A node is "good" if it has responded in the last 15 minutes, OR has ever
// responded and sent us a query in the last 15 minutes.
// Questionable nodes are pinged before eviction. Bad nodes are removed.
// When restoring from cache across restarts, nodes start as "questionable"
// and must be re-verified (ping) before being promoted to "good".
type NodeState string

const (
	NodeStateGood         NodeState = "good"         // BEP 5: responded within 15 min
	NodeStateQuestionable NodeState = "questionable" // BEP 5: unverified; needs ping
	NodeStateBad          NodeState = "bad"          // BEP 5: failed multiple queries; evict
)

// Node represents a cached DHT routing table entry (BEP 5 / BEP 32).
// Only "good" nodes are written to the cache. On restore they are loaded
// as "questionable" because liveness is unknown after a restart.
type Node struct {
	ID       string    `json:"id"`        // Hex-encoded 20-byte node ID (BEP 5 / BEP 42)
	IP       string    `json:"ip"`        // IPv4 or IPv6 address (BEP 32 for IPv6)
	Port     int       `json:"port"`      // UDP port
	LastSeen time.Time `json:"last_seen"` // BEP 5: used to determine good/questionable/bad
	State    NodeState `json:"state"`     // BEP 5 state machine; see NodeState consts
}

// Tracker represents a cached tracker with BEP 3/15/48 success/failure history.
// Failure count maps to BEP 48 scrape semantics: trackers that fail consistently
// are deprioritised and eventually evicted by CleanupOldTrackers.
type Tracker struct {
	URL         string    `json:"url"`          // Announce URL (http://, udp://, ws://)
	LastSuccess time.Time `json:"last_success"` // BEP 3/15: last successful announce
	Failures    int       `json:"failures"`     // Consecutive failures since last success
}

// Infohash represents a torrent we have observed via DHT, PEX, or trackers.
//
// Seeds/Leeches come from BEP 33 (DHT scrape extension to get_peers).
// A responding node may include "BFsd" and "BFpe" Bloom filters encoding
// seed and peer counts; Seeds/Leeches store the decoded estimates.
//
// Source values:
//
//	"dht_getpeers" — BEP 5 get_peers response
//	"dht_sample"   — BEP 51 sample_infohashes response
//	"pex"          — BEP 11 ut_pex peer exchange
//	"tracker"      — BEP 3/15 tracker announce response
//	"dht_scrape"   — BEP 33 DHT scrape Bloom filter
type Infohash struct {
	Infohash  string    `json:"infohash"`          // Hex-encoded 20-byte info hash
	FirstSeen time.Time `json:"first_seen"`        // When we first observed this hash
	LastSeen  time.Time `json:"last_seen"`         // Most recent observation
	SeenCount int       `json:"seen_count"`        // Total observations
	Source    string    `json:"source"`            // How it was discovered (see above)
	Seeds     int       `json:"seeds,omitempty"`   // BEP 33: estimated seed count
	Leeches   int       `json:"leeches,omitempty"` // BEP 33: estimated leech count
}

// Item represents a stored DHT item (BEP 44).
//
// Immutable items: Value is arbitrary bencoded data; Key = hex(SHA1(Value)).
// The value is content-addressed and cannot change.
//
// Mutable items: Key = hex(ed25519 public key, 32 bytes); Value is signed
// bencoded data. Seq is a monotonically increasing sequence number used for
// CAS (compare-and-swap) to prevent replay. Salt is an optional per-item
// namespace under the same public key.
//
// Both types are stored in the DHT with a token lifetime (~2h); this cache
// persists them locally so we don't need to re-fetch after restarts.
type Item struct {
	Key     string    `json:"key"`            // Hex-encoded key (see above)
	Value   []byte    `json:"value"`          // Raw bencoded value
	Seq     int64     `json:"seq,omitempty"`  // BEP 44: mutable sequence number
	Salt    string    `json:"salt,omitempty"` // BEP 44: optional mutable item salt
	Mutable bool      `json:"mutable"`        // true = mutable (has Seq/sig), false = immutable
	Fetched time.Time `json:"fetched"`        // When this item was last fetched from DHT
}

// Feed represents a subscribed DHT feed.
//
// BEP 46 — Updating Torrents via DHT:
//
//	A single mutable item (BEP 44) keyed by ed25519 public key encodes the
//	current infohash. The publisher increments Seq on each update; subscribers
//	use the seq for CAS. Infohashes holds exactly one entry (the current hash).
//
// BEP 49 — Distributed Torrent Feeds:
//
//	Same DHT mechanism but the mutable item value is a bencoded list of
//	infohashes rather than a single hash. Infohashes holds all entries from
//	the latest resolved item.
//
// Distinguishing BEP 46 from BEP 49 is done by inspecting the decoded Value
// (single infohash dict vs list). Both share the same Feed type here.
type Feed struct {
	PublicKey   string    `json:"public_key"`   // Hex-encoded ed25519 public key (32 bytes)
	Infohashes  []string  `json:"infohashes"`   // Resolved infohashes from latest item (hex)
	Seq         int64     `json:"seq"`          // BEP 44: latest sequence number (for CAS)
	LastFetched time.Time `json:"last_fetched"` // When we last resolved this feed from DHT
}

// DHT groups DHT-specific cache fields.
type DHT struct {
	SelfID string `json:"self_id,omitempty"` // Our own node ID (hex, 40 chars); BEP 5 / BEP 42
	Nodes  []Node `json:"nodes"`             // Routing table snapshot (BEP 5 good nodes only)
}

// Cache is the complete on-disk cache structure (~/.bt/cache.json).
type Cache struct {
	Version    int        `json:"version"`         // Format version; see CurrentCacheVersion
	SavedAt    time.Time  `json:"saved_at"`        // Timestamp of last save
	DHT        DHT        `json:"dht"`             // BEP 5/32/42 routing table + self ID
	Trackers   []Tracker  `json:"trackers"`        // BEP 3/15/48 tracker history
	Infohashes []Infohash `json:"infohashes"`      // BEP 5/33/51 observed infohashes
	Items      []Item     `json:"items,omitempty"` // BEP 44 mutable/immutable DHT items
	Feeds      []Feed     `json:"feeds,omitempty"` // BEP 46/49 DHT feed subscriptions
}

// NewCache returns an empty cache initialised to the current format version.
func NewCache() *Cache {
	return &Cache{
		Version:    CurrentCacheVersion,
		SavedAt:    time.Now(),
		DHT:        DHT{Nodes: []Node{}},
		Trackers:   []Tracker{},
		Infohashes: []Infohash{},
		Items:      []Item{},
		Feeds:      []Feed{},
	}
}

// IsGoodNode reports whether the node is in BEP 5 "good" state.
func (n *Node) IsGoodNode() bool { return n.State == NodeStateGood }

// IsStale reports whether the node hasn't been seen within maxAge.
// BEP 5 uses 15 minutes for in-memory state; the cache uses a longer window
// (e.g. 24h) since we don't know how long the process was stopped.
func (n *Node) IsStale(maxAge time.Duration) bool { return time.Since(n.LastSeen) > maxAge }

// IsWorking reports whether the tracker has zero consecutive failures.
func (t *Tracker) IsWorking() bool { return t.Failures == 0 }

// IsStale reports whether the tracker hasn't had a successful announce within maxAge.
func (t *Tracker) IsStale(maxAge time.Duration) bool {
	if t.LastSuccess.IsZero() {
		return true
	}
	return time.Since(t.LastSuccess) > maxAge
}

// IsStale reports whether the infohash hasn't been observed within maxAge.
func (i *Infohash) IsStale(maxAge time.Duration) bool { return time.Since(i.LastSeen) > maxAge }
