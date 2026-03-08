package cache

import (
	"time"
)

// CleanupOldTrackers removes trackers that have failed too many times or whose
// last successful announce is older than maxAge (BEP 3 / BEP 15 / BEP 48).
// Trackers that have never succeeded (LastSuccess.IsZero) are kept until they
// exceed maxFailures, giving new entries a chance to work.
func (m *Manager) CleanupOldTrackers(maxFailures int, maxAge time.Duration) error {
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

	var kept []Tracker
	now := time.Now()
	for _, t := range m.cache.Trackers {
		if t.Failures < maxFailures && (t.LastSuccess.IsZero() || now.Sub(t.LastSuccess) < maxAge) {
			kept = append(kept, t)
		}
	}

	m.cache.Trackers = kept
	return m.saveUnlocked(m.cache)
}

// CleanupOldDHTNodes removes cached routing table entries not seen within maxAge.
//
// BEP 5 defines "good" as responded within the last 15 minutes (in-memory).
// For the on-disk cache we use a much longer window (e.g. 24h) because we
// don't know how long the process was stopped. Restored nodes are treated as
// "questionable" and must be re-verified with a ping before being used.
func (m *Manager) CleanupOldDHTNodes(maxAge time.Duration) error {
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

	var kept []Node
	now := time.Now()
	for _, n := range m.cache.DHT.Nodes {
		if now.Sub(n.LastSeen) < maxAge {
			kept = append(kept, n)
		}
	}

	m.cache.DHT.Nodes = kept
	return m.saveUnlocked(m.cache)
}

// CleanupOldInfohashes removes infohashes not seen within maxAge.
// Sources: BEP 5 get_peers, BEP 51 sample_infohashes, BEP 11 ut_pex.
func (m *Manager) CleanupOldInfohashes(maxAge time.Duration) error {
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

	var kept []Infohash
	now := time.Now()
	for _, ih := range m.cache.Infohashes {
		if now.Sub(ih.LastSeen) < maxAge {
			kept = append(kept, ih)
		}
	}

	m.cache.Infohashes = kept
	return m.saveUnlocked(m.cache)
}

// CleanupOldItems removes BEP 44 DHT items not fetched within maxAge.
// Immutable items are content-addressed and never change, so a longer maxAge
// is appropriate. Mutable items should be re-fetched periodically anyway.
func (m *Manager) CleanupOldItems(maxAge time.Duration) error {
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

	var kept []Item
	now := time.Now()
	for _, item := range m.cache.Items {
		if now.Sub(item.Fetched) < maxAge {
			kept = append(kept, item)
		}
	}

	m.cache.Items = kept
	return m.saveUnlocked(m.cache)
}

// CleanupOldFeeds removes BEP 46/49 feed subscriptions not fetched within maxAge.
func (m *Manager) CleanupOldFeeds(maxAge time.Duration) error {
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

	var kept []Feed
	now := time.Now()
	for _, f := range m.cache.Feeds {
		if now.Sub(f.LastFetched) < maxAge {
			kept = append(kept, f)
		}
	}

	m.cache.Feeds = kept
	return m.saveUnlocked(m.cache)
}

// CleanupAll runs all cleanup operations with sensible defaults.
func (m *Manager) CleanupAll() error {
	// BEP 3/15: drop trackers with 10+ failures or no success in 7 days
	if err := m.CleanupOldTrackers(10, 7*24*time.Hour); err != nil {
		return err
	}
	// BEP 5: drop nodes not seen in 24h (in-memory good-node window is 15min)
	if err := m.CleanupOldDHTNodes(24 * time.Hour); err != nil {
		return err
	}
	// BEP 5/51: drop infohashes not seen in 30 days
	if err := m.CleanupOldInfohashes(30 * 24 * time.Hour); err != nil {
		return err
	}
	// BEP 44: drop items not fetched in 7 days
	if err := m.CleanupOldItems(7 * 24 * time.Hour); err != nil {
		return err
	}
	// BEP 46/49: drop feeds not fetched in 7 days
	if err := m.CleanupOldFeeds(7 * 24 * time.Hour); err != nil {
		return err
	}
	return nil
}

// CacheStats summarises cache contents.
type CacheStats struct {
	Version         int       `json:"version"`
	SavedAt         time.Time `json:"saved_at"`
	TotalTrackers   int       `json:"total_trackers"`
	WorkingTrackers int       `json:"working_trackers"`
	TotalDHTNodes   int       `json:"total_dht_nodes"`
	GoodDHTNodes    int       `json:"good_dht_nodes"`
	TotalInfohashes int       `json:"total_infohashes"`
	TotalItems      int       `json:"total_items"`
	TotalFeeds      int       `json:"total_feeds"`
}

// Stats returns statistics about the current cache contents.
func (m *Manager) Stats() (CacheStats, error) {
	cache, err := m.Load()
	if err != nil {
		return CacheStats{}, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := CacheStats{
		Version:         cache.Version,
		SavedAt:         cache.SavedAt,
		TotalTrackers:   len(cache.Trackers),
		TotalDHTNodes:   len(cache.DHT.Nodes),
		TotalInfohashes: len(cache.Infohashes),
		TotalItems:      len(cache.Items),
		TotalFeeds:      len(cache.Feeds),
	}
	for _, t := range cache.Trackers {
		if t.IsWorking() {
			stats.WorkingTrackers++
		}
	}
	for _, n := range cache.DHT.Nodes {
		if n.IsGoodNode() {
			stats.GoodDHTNodes++
		}
	}
	return stats, nil
}
