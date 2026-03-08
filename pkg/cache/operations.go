package cache

import (
	"time"
)

// ===== Tracker Operations (BEP 3 / BEP 15 / BEP 48) =====

// AddOrUpdateTracker adds or updates a tracker in the cache.
// Thread-safe with proper locking.
func (m *Manager) AddOrUpdateTracker(url string, success bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load cache if not already loaded
	if !m.loaded {
		m.mu.Unlock()
		if _, err := m.Load(); err != nil {
			m.mu.Lock()
			return err
		}
		m.mu.Lock()
	}

	// Find existing tracker
	found := false
	for i := range m.cache.Trackers {
		if m.cache.Trackers[i].URL == url {
			if success {
				m.cache.Trackers[i].LastSuccess = time.Now()
				m.cache.Trackers[i].Failures = 0
			} else {
				m.cache.Trackers[i].Failures++
			}
			found = true
			break
		}
	}

	// Add new tracker if not found
	if !found {
		tracker := Tracker{
			URL:      url,
			Failures: 0,
		}
		if success {
			tracker.LastSuccess = time.Now()
		} else {
			tracker.Failures = 1
		}
		m.cache.Trackers = append(m.cache.Trackers, tracker)
	}

	return m.saveUnlocked(m.cache)
}

// GetTrackers returns all trackers from the cache.
// Thread-safe read operation.
func (m *Manager) GetTrackers() ([]Tracker, error) {
	cache, err := m.Load()
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Return a copy to prevent external modification
	trackers := make([]Tracker, len(cache.Trackers))
	copy(trackers, cache.Trackers)
	return trackers, nil
}

// ===== DHT Node Operations (BEP 5 / BEP 32 / BEP 42) =====

// AddOrUpdateDHTNode adds or updates a single DHT node in the cache.
// Thread-safe with proper locking.
func (m *Manager) AddOrUpdateDHTNode(id, ip string, port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load cache if not already loaded
	if !m.loaded {
		m.mu.Unlock()
		if _, err := m.Load(); err != nil {
			m.mu.Lock()
			return err
		}
		m.mu.Lock()
	}

	// Find existing node
	found := false
	for i := range m.cache.DHT.Nodes {
		if m.cache.DHT.Nodes[i].ID == id {
			m.cache.DHT.Nodes[i].IP = ip
			m.cache.DHT.Nodes[i].Port = port
			m.cache.DHT.Nodes[i].LastSeen = time.Now()
			m.cache.DHT.Nodes[i].State = NodeStateGood // Cached nodes are always "good"
			found = true
			break
		}
	}

	// Add new node if not found
	if !found {
		node := Node{
			ID:       id,
			IP:       ip,
			Port:     port,
			LastSeen: time.Now(),
			State:    NodeStateGood, // Only cache "good" nodes
		}
		m.cache.DHT.Nodes = append(m.cache.DHT.Nodes, node)
	}

	return m.saveUnlocked(m.cache)
}

// AddOrUpdateDHTNodes adds or updates multiple DHT nodes in a single transaction.
// This is much more efficient than calling AddOrUpdateDHTNode in a loop.
// Thread-safe with proper locking.
func (m *Manager) AddOrUpdateDHTNodes(nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Load cache if not already loaded
	if !m.loaded {
		m.mu.Unlock()
		if _, err := m.Load(); err != nil {
			m.mu.Lock()
			return err
		}
		m.mu.Lock()
	}

	// Create a map for efficient lookup
	existingNodes := make(map[string]Node)
	for _, node := range m.cache.DHT.Nodes {
		existingNodes[node.ID] = node
	}

	// Add or update nodes
	now := time.Now()
	for _, node := range nodes {
		// Ensure cached nodes are marked as "good"
		node.State = NodeStateGood
		node.LastSeen = now
		existingNodes[node.ID] = node
	}

	// Rebuild node list
	m.cache.DHT.Nodes = make([]Node, 0, len(existingNodes))
	for _, node := range existingNodes {
		m.cache.DHT.Nodes = append(m.cache.DHT.Nodes, node)
	}

	return m.saveUnlocked(m.cache)
}

// GetDHTNodes returns all DHT nodes from the cache.
// Thread-safe read operation.
func (m *Manager) GetDHTNodes() ([]Node, error) {
	cache, err := m.Load()
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Return a copy to prevent external modification
	nodes := make([]Node, len(cache.DHT.Nodes))
	copy(nodes, cache.DHT.Nodes)
	return nodes, nil
}

// ===== Infohash Operations (BEP 5 / BEP 33 / BEP 51) =====

// AddOrUpdateInfohash records an observed infohash (BEP 5 / BEP 51 / BEP 11).
// seeds and leeches are BEP 33 DHT scrape estimates; pass 0 when unknown.
func (m *Manager) AddOrUpdateInfohash(infohash, source string, seeds, leeches int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load cache if not already loaded
	if !m.loaded {
		m.mu.Unlock()
		if _, err := m.Load(); err != nil {
			m.mu.Lock()
			return err
		}
		m.mu.Lock()
	}

	// Find existing infohash
	found := false
	now := time.Now()
	for i := range m.cache.Infohashes {
		if m.cache.Infohashes[i].Infohash == infohash {
			m.cache.Infohashes[i].LastSeen = now
			m.cache.Infohashes[i].SeenCount++
			// Update BEP 33 scrape data when provided
			if seeds > 0 {
				m.cache.Infohashes[i].Seeds = seeds
			}
			if leeches > 0 {
				m.cache.Infohashes[i].Leeches = leeches
			}
			found = true
			break
		}
	}

	// Add new infohash if not found
	if !found {
		ih := Infohash{
			Infohash:  infohash,
			FirstSeen: now,
			LastSeen:  now,
			SeenCount: 1,
			Source:    source,
			Seeds:     seeds,
			Leeches:   leeches,
		}
		m.cache.Infohashes = append(m.cache.Infohashes, ih)
	}

	return m.saveUnlocked(m.cache)
}

// GetDHTSelfID returns the persisted DHT node ID (hex string), or "" if not set.
func (m *Manager) GetDHTSelfID() (string, error) {
	cache, err := m.Load()
	if err != nil {
		return "", err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cache.DHT.SelfID, nil
}

// SetDHTSelfID persists the DHT node ID (hex string).
func (m *Manager) SetDHTSelfID(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.loaded {
		m.mu.Unlock()
		if _, err := m.Load(); err != nil {
			m.mu.Lock()
			return err
		}
		m.mu.Lock()
	}
	m.cache.DHT.SelfID = id
	return m.saveUnlocked(m.cache)
}

// GetInfohashes returns all infohashes from the cache.
// Thread-safe read operation.
func (m *Manager) GetInfohashes() ([]Infohash, error) {
	cache, err := m.Load()
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	infohashes := make([]Infohash, len(cache.Infohashes))
	copy(infohashes, cache.Infohashes)
	return infohashes, nil
}

// ===== DHT Item Operations (BEP 44) =====

// PutItem stores a mutable or immutable DHT item.
// For immutable items, Key = hex(SHA1(Value)); the caller is responsible for
// computing the key. For mutable items, Key = hex(ed25519 public key) and
// Seq must be incremented on each update to satisfy BEP 44 CAS semantics.
func (m *Manager) PutItem(item Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.loaded {
		m.mu.Unlock()
		if _, err := m.Load(); err != nil {
			m.mu.Lock()
			return err
		}
		m.mu.Lock()
	}

	now := time.Now()
	for i := range m.cache.Items {
		if m.cache.Items[i].Key == item.Key && m.cache.Items[i].Salt == item.Salt {
			// For mutable items, only overwrite if seq is newer (BEP 44 CAS).
			if item.Mutable && item.Seq <= m.cache.Items[i].Seq {
				return nil
			}
			item.Fetched = now
			m.cache.Items[i] = item
			return m.saveUnlocked(m.cache)
		}
	}

	item.Fetched = now
	m.cache.Items = append(m.cache.Items, item)
	return m.saveUnlocked(m.cache)
}

// GetItem retrieves a DHT item by key and optional salt.
// Returns nil if not found.
func (m *Manager) GetItem(key, salt string) (*Item, error) {
	cache, err := m.Load()
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, item := range cache.Items {
		if item.Key == key && item.Salt == salt {
			cp := item
			return &cp, nil
		}
	}
	return nil, nil
}

// ===== Feed Operations (BEP 46 / BEP 49) =====

// PutFeed records or updates a feed subscription.
// Only updates if seq is strictly greater than the stored value (BEP 44 CAS).
func (m *Manager) PutFeed(feed Feed) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.loaded {
		m.mu.Unlock()
		if _, err := m.Load(); err != nil {
			m.mu.Lock()
			return err
		}
		m.mu.Lock()
	}

	for i := range m.cache.Feeds {
		if m.cache.Feeds[i].PublicKey == feed.PublicKey {
			if feed.Seq <= m.cache.Feeds[i].Seq {
				return nil // stale update; ignore
			}
			m.cache.Feeds[i] = feed
			return m.saveUnlocked(m.cache)
		}
	}

	m.cache.Feeds = append(m.cache.Feeds, feed)
	return m.saveUnlocked(m.cache)
}

// GetFeed returns the cached feed for a public key, or nil if not found.
func (m *Manager) GetFeed(publicKey string) (*Feed, error) {
	cache, err := m.Load()
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, f := range cache.Feeds {
		if f.PublicKey == publicKey {
			cp := f
			return &cp, nil
		}
	}
	return nil, nil
}

// GetFeeds returns all cached feed subscriptions.
func (m *Manager) GetFeeds() ([]Feed, error) {
	cache, err := m.Load()
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	feeds := make([]Feed, len(cache.Feeds))
	copy(feeds, cache.Feeds)
	return feeds, nil
}
