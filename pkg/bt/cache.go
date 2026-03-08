package bt

import (
	"github.com/cbluth/bittorrent/pkg/cache"
	"github.com/cbluth/bittorrent/pkg/config"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
)

// LoadDHTNodesFromCache loads DHT nodes from the new cache system
// Returns count of nodes loaded
func LoadDHTNodesFromCache(cacheOps *config.CacheOperations, dhtNode *dht.Node) (int, error) {
	if dhtNode == nil {
		return 0, nil
	}

	// Load nodes from new cache
	cachedNodes, err := cacheOps.GetDHTNodes()
	if err != nil {
		return 0, err
	}

	// Add nodes to DHT routing table
	addedCount := 0
	for _, node := range cachedNodes {
		// Parse node ID
		nodeID, err := dht.KeyFromHex(node.ID)
		if err != nil {
			log.Warn("invalid cached node ID", "sub", "dht", "id", node.ID, "err", err)
			continue
		}

		// Create remote node with ID and add to routing table
		remoteNode, err := dht.NewNodeWithID(0, []string{}, nodeID)
		if err != nil {
			log.Warn("failed to create node with ID", "sub", "dht", "err", err)
			continue
		}

		// Add to routing table
		dhtNode.DHT().AddNode(remoteNode)
		addedCount++
	}

	if addedCount > 0 {
		log.Info("loaded DHT nodes from cache", "sub", "dht", "count", addedCount)
	}

	return addedCount, nil
}

// SaveDHTNodesToCache saves DHT nodes from a DHT instance to cache.
// Only saves "good" nodes to keep cache clean.
func SaveDHTNodesToCache(cacheOps *config.CacheOperations, dhtNode *dht.Node) error {
	if dhtNode == nil || cacheOps == nil {
		return nil
	}

	// Get all good nodes directly from DHT
	dhtInstance := dhtNode.DHT()
	goodNodes := dhtInstance.GetGoodNodes()

	if len(goodNodes) == 0 {
		return nil
	}

	// Convert DHT nodes to cache format
	cacheNodes := make([]config.DHTNodeCache, 0, len(goodNodes))
	for _, node := range goodNodes {
		addr := node.Address()
		if addr == nil || addr.IP == nil {
			continue
		}

		cacheNodes = append(cacheNodes, config.DHTNodeCache{
			ID:       node.ID().Hex(),
			IP:       addr.IP.String(),
			Port:     addr.Port,
			LastSeen: node.LastSeen(),
			State:    cache.NodeStateGood, // Only good nodes are cached
		})
	}

	if len(cacheNodes) > 0 {
		log.Debug("saving DHT nodes to cache", "sub", "dht", "count", len(cacheNodes))
		return cacheOps.AddOrUpdateDHTNodes(cacheNodes)
	}

	return nil
}
