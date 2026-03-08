package dht

// BEP 5: DHT Protocol - Routing Table Persistence
// https://www.bittorrent.org/beps/bep_0005.html
//
// This file implements routing table persistence to speed up bootstrap:
// - Save good/questionable nodes to disk on shutdown
// - Load cached nodes on startup for faster bootstrap
// - Validate cache age and node freshness

import (
	"encoding/json"
	"net"
	"os"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
)

// PersistedNode represents a DHT node saved to disk
type PersistedNode struct {
	ID       string    `json:"id"`        // Hex-encoded node ID
	IP       string    `json:"ip"`        // IP address (IPv4 or IPv6)
	Port     int       `json:"port"`      // UDP port
	LastSeen time.Time `json:"last_seen"` // When we last heard from this node
	State    string    `json:"state"`     // "good", "questionable", "bad"
}

// PersistedDHT represents the routing table saved to disk
type PersistedDHT struct {
	Version int             `json:"version"`  // Format version for future changes
	SelfID  string          `json:"self_id"`  // Our node ID (should stay consistent)
	Nodes   []PersistedNode `json:"nodes"`    // Known nodes
	SavedAt time.Time       `json:"saved_at"` // When this was saved
}

// SaveToFile saves the routing table to a JSON file
// Only saves good and questionable nodes (bad nodes are discarded)
func (d *DHT) SaveToFile(path string) error {
	d.mu.RLock()
	defer d.mu.RUnlock()

	persisted := &PersistedDHT{
		Version: 1,
		SelfID:  d.selfNode.id.Hex(),
		SavedAt: time.Now(),
		Nodes:   make([]PersistedNode, 0, len(d.nodeMap)),
	}

	// Only save good and questionable nodes (not bad ones)
	for _, node := range d.nodeMap {
		state := node.State()
		if state == NodeBad {
			continue
		}

		addr := node.Address()
		if addr == nil || addr.IP == nil {
			continue
		}

		persisted.Nodes = append(persisted.Nodes, PersistedNode{
			ID:       node.ID().Hex(),
			IP:       addr.IP.String(),
			Port:     addr.Port,
			LastSeen: node.LastSeen(),
			State:    state.String(),
		})
	}

	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// DEAD CODE: LoadFromFile is currently unused but preserved for future use.
// DHT bootstrap now uses pkg/cache.Manager via config.CacheOperations instead.
//
// SaveToFile() was previously used for temp file conversion in bt/cache.go,
// but that has been eliminated - SaveDHTNodesToCache() now calls DHT.GetGoodNodes() directly.
//
// Current status: ENTIRE FILE IS DEAD CODE
// - LoadFromFile: Zero call sites
// - SaveToFile: Zero call sites (temp file conversion removed)
// - PersistedNode: Replaced by pkg/cache.Node
// - PersistedDHT: Not used
//
// CANDIDATE FOR DELETION: This entire file can be deleted after confirming no external dependencies.

// LoadFromFile loads the routing table from a JSON file
// Returns the number of nodes successfully loaded
// Validates cache age (rejects if > 24 hours old)
// Filters out stale nodes (not seen in > 1 hour)
func (d *DHT) LoadFromFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Debug("no cache file found (first run)", "sub", "dht")
			return 0, nil // No cache file, not an error
		}
		return 0, err
	}

	var persisted PersistedDHT
	if err := json.Unmarshal(data, &persisted); err != nil {
		return 0, err
	}

	// Validate cache age (don't use if > 24 hours old)
	cacheAge := time.Since(persisted.SavedAt)
	if cacheAge > 24*time.Hour {
		log.Debug("cache too old, ignoring", "sub", "dht", "age", cacheAge)
		return 0, nil
	}

	loaded := 0
	for _, pnode := range persisted.Nodes {
		// Skip nodes we haven't seen in over 1 hour
		if time.Since(pnode.LastSeen) > time.Hour {
			continue
		}

		// Parse node ID
		nodeID, err := KeyFromHex(pnode.ID)
		if err != nil {
			continue
		}

		// Parse IP address
		ip := net.ParseIP(pnode.IP)
		if ip == nil {
			continue
		}

		addr := &net.UDPAddr{
			IP:   ip,
			Port: pnode.Port,
		}

		// Create and add node
		node := NewRemoteNode(addr)
		node.id = nodeID
		d.AddNode(node)
		loaded++
	}

	log.Info("loaded nodes from cache", "sub", "dht", "count", loaded, "age", cacheAge)

	return loaded, nil
}
