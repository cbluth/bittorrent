package config

import (
	"github.com/cbluth/bittorrent/pkg/cache"
	"time"
)

// CacheOperations provides cache file CRUD operations.
// This is now a thin wrapper around cache.Manager for backward compatibility.
// New code should use cache.Manager directly.
type CacheOperations struct {
	manager *cache.Manager
}

// NewCacheOperations creates a new cache operations instance.
// Uses the new cache.Manager with proper concurrency control.
func NewCacheOperations() *CacheOperations {
	return &CacheOperations{
		manager: cache.NewManager(GetCachePath()),
	}
}

// Type aliases for backward compatibility.
// These allow existing code to use config.Cache, config.TrackerCache, etc.

// Cache is an alias to cache.Cache
type Cache = cache.Cache

// TrackerCache is an alias to cache.Tracker
type TrackerCache = cache.Tracker

// DHTCache is an alias to cache.DHT
type DHTCache = cache.DHT

// DHTNodeCache is an alias to cache.Node
// NOTE: The new cache.Node type includes State field which was previously missing!
type DHTNodeCache = cache.Node

// InfohashCache is an alias to cache.Infohash
type InfohashCache = cache.Infohash

// Load loads the cache file, creating default if it doesn't exist.
// Delegates to cache.Manager with proper concurrency control.
func (c *CacheOperations) Load() (*Cache, error) {
	return c.manager.Load()
}

// Save saves the cache file.
// Delegates to cache.Manager with proper concurrency control.
func (c *CacheOperations) Save(cache *Cache) error {
	return c.manager.Save(cache)
}

// Path returns the cache file path.
func (c *CacheOperations) Path() string {
	return c.manager.Path()
}

// AddOrUpdateTracker adds or updates a tracker in cache.
// Delegates to cache.Manager with proper concurrency control.
func (c *CacheOperations) AddOrUpdateTracker(url string, success bool) error {
	return c.manager.AddOrUpdateTracker(url, success)
}

// AddOrUpdateDHTNode adds or updates a "good" DHT node in cache.
// Delegates to cache.Manager with proper concurrency control.
func (c *CacheOperations) AddOrUpdateDHTNode(id, ip string, port int) error {
	return c.manager.AddOrUpdateDHTNode(id, ip, port)
}

// AddOrUpdateInfohash adds or updates an infohash in cache.
// seeds and leeches are BEP 33 scrape estimates; pass 0 when unknown.
// Delegates to cache.Manager with proper concurrency control.
func (c *CacheOperations) AddOrUpdateInfohash(infohash, source string, seeds, leeches int) error {
	return c.manager.AddOrUpdateInfohash(infohash, source, seeds, leeches)
}

// GetTrackers returns all trackers from cache.
// Delegates to cache.Manager with proper concurrency control.
func (c *CacheOperations) GetTrackers() ([]TrackerCache, error) {
	return c.manager.GetTrackers()
}

// GetDHTNodes returns all DHT nodes from cache.
// Delegates to cache.Manager with proper concurrency control.
func (c *CacheOperations) GetDHTNodes() ([]DHTNodeCache, error) {
	return c.manager.GetDHTNodes()
}

// AddOrUpdateDHTNodes adds or updates multiple DHT nodes in cache.
// Delegates to cache.Manager with proper concurrency control.
func (c *CacheOperations) AddOrUpdateDHTNodes(nodes []DHTNodeCache) error {
	return c.manager.AddOrUpdateDHTNodes(nodes)
}

// GetInfohashes returns all infohashes from cache.
// Delegates to cache.Manager with proper concurrency control.
func (c *CacheOperations) GetInfohashes() ([]InfohashCache, error) {
	return c.manager.GetInfohashes()
}

// CleanupOldTrackers removes trackers with too many failures or old last success.
// Now properly integrated with cache.Manager with concurrency control.
// Delegates to cache.Manager.
func (c *CacheOperations) CleanupOldTrackers(maxFailures int, maxAge time.Duration) error {
	return c.manager.CleanupOldTrackers(maxFailures, maxAge)
}

// CleanupOldDHTNodes removes DHT nodes that haven't been seen recently.
// Now properly integrated with cache.Manager with concurrency control.
// Delegates to cache.Manager.
func (c *CacheOperations) CleanupOldDHTNodes(maxAge time.Duration) error {
	return c.manager.CleanupOldDHTNodes(maxAge)
}

// CleanupOldInfohashes removes infohashes that haven't been seen recently.
// Now properly integrated with cache.Manager with concurrency control.
// Delegates to cache.Manager.
func (c *CacheOperations) CleanupOldInfohashes(maxAge time.Duration) error {
	return c.manager.CleanupOldInfohashes(maxAge)
}

// GetDHTSelfID returns the persisted DHT node ID, or "" if not set.
func (c *CacheOperations) GetDHTSelfID() (string, error) {
	return c.manager.GetDHTSelfID()
}

// SetDHTSelfID persists the DHT node ID (hex string).
func (c *CacheOperations) SetDHTSelfID(id string) error {
	return c.manager.SetDHTSelfID(id)
}
