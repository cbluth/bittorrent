package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Manager provides thread-safe cache operations with proper concurrency control.
// This is the single source of truth for cache management, replacing:
// - config.CacheOperations (no locking, race conditions)
// - bt.BTCache wrapper (unused DHT field, inefficient temp files)
// - Direct file operations scattered across packages
type Manager struct {
	mu        sync.RWMutex // Protects all cache operations
	cachePath string       // Path to cache file (~/.bt/cache.json)
	cache     *Cache       // In-memory cache, lazily loaded
	loaded    bool         // Whether cache has been loaded
}

// NewManager creates a new cache manager with the given cache file path.
// The cache is loaded lazily on first access.
func NewManager(cachePath string) *Manager {
	return &Manager{
		cachePath: cachePath,
	}
}

// Load loads the cache from disk, creating a default cache if it doesn't exist.
// This method is thread-safe and can be called concurrently.
// The cache is cached in memory after first load.
func (m *Manager) Load() (*Cache, error) {
	m.mu.RLock()
	if m.loaded {
		// Return cached copy
		cache := m.cache
		m.mu.RUnlock()
		return cache, nil
	}
	m.mu.RUnlock()

	// Need to load from disk
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if m.loaded {
		return m.cache, nil
	}

	// Check if cache file exists
	if _, err := os.Stat(m.cachePath); os.IsNotExist(err) {
		// Create default cache
		m.cache = NewCache()
		m.loaded = true

		// Save default cache to disk
		if err := m.saveUnlocked(m.cache); err != nil {
			return nil, fmt.Errorf("failed to create default cache: %w", err)
		}
		return m.cache, nil
	}

	// Read cache file
	data, err := os.ReadFile(m.cachePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cache: %w", err)
	}

	var cache Cache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("failed to parse cache: %w", err)
	}

	m.cache = &cache
	m.loaded = true

	return m.cache, nil
}

// Save saves the cache to disk with proper locking.
// Updates the SavedAt timestamp automatically.
func (m *Manager) Save(cache *Cache) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveUnlocked(cache)
}

// saveUnlocked saves the cache without acquiring the lock.
// Internal method - caller must hold write lock.
func (m *Manager) saveUnlocked(cache *Cache) error {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(m.cachePath), 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Update timestamp
	cache.SavedAt = time.Now()

	// Marshal to JSON
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cache: %w", err)
	}

	// Write to disk
	if err := os.WriteFile(m.cachePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write cache: %w", err)
	}

	// Update in-memory cache
	m.cache = cache
	m.loaded = true

	return nil
}

// Path returns the cache file path.
func (m *Manager) Path() string {
	return m.cachePath
}

// Reload forces a reload of the cache from disk, discarding in-memory changes.
// This is useful for testing or recovering from corruption.
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.loaded = false
	m.cache = nil

	// Load will happen on next access
	return nil
}
