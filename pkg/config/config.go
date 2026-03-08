package config

import (
	"bufio"
	"fmt"
	"github.com/cbluth/bittorrent/pkg/log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GetBTDir returns the .bt directory path (typically ~/.bt)
func GetBTDir() string {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		log.Warn("could not get home directory, using current directory", "sub", "bittorrent")
		return ".bt"
	}
	return filepath.Join(homeDir, ".bt")
}

// GetCachePath returns the full path to cache.json
func GetCachePath() string {
	return filepath.Join(GetBTDir(), "cache.json")
}

// GetConfigPath returns the full path to config.json
func GetConfigPath() string {
	return filepath.Join(GetBTDir(), "config.json")
}

// Trackers returns the list of BitTorrent trackers from all sources.
// Sources in order: trackers.list (user config), cache (previous sessions), trackers.feed (fetched).
func Trackers() []string {
	ops := NewOperations()
	cfg, err := ops.load()
	if err != nil {
		return nil
	}
	trackersCfg, _ := cfg["trackers"].(map[string]interface{})

	var result []string

	// 1. trackers.list — explicitly configured by the user
	result = append(result, stringsFromArray(trackersCfg["list"])...)

	// 2. cache.json — working trackers from previous sessions (BEP 3/15/48)
	cacheOps := NewCacheOperations()
	if cachedTrackers, err := cacheOps.GetTrackers(); err == nil {
		for _, t := range cachedTrackers {
			if t.URL != "" {
				result = append(result, t.URL)
			}
		}
	}

	// 3. trackers.feed — fetched from configured feed URLs
	result = append(result, loadTrackersFromFeeds(stringsFromArray(trackersCfg["feed"]))...)

	return result
}

// DHTBootstrapNodes returns the list of DHT bootstrap nodes from all sources.
// Sources in order: dht.nodes (user config), cache (previous sessions, BEP 5), dht.feed (fetched).
func DHTBootstrapNodes() []string {
	ops := NewOperations()
	cfg, err := ops.load()
	if err != nil {
		return nil
	}
	dhtCfg, _ := cfg["dht"].(map[string]any)

	var result []string

	// 1. dht.nodes — explicitly configured bootstrap nodes
	result = append(result, stringsFromArray(dhtCfg["nodes"])...)

	// 2. cache.json — good nodes from previous sessions (BEP 5 routing table persistence)
	cacheOps := NewCacheOperations()
	if cachedNodes, err := cacheOps.GetDHTNodes(); err == nil {
		for _, n := range cachedNodes {
			if n.IP != "" && n.Port > 0 {
				result = append(result, fmt.Sprintf("%s:%d", n.IP, n.Port))
			}
		}
	}

	// 3. dht.feed — fetched from configured feed URLs
	result = append(result, loadDHTNodesFromFeeds(stringsFromArray(dhtCfg["feed"]))...)

	return result
}

// STUNServers returns the list of STUN servers from all sources.
// Sources in order: stun.servers (user config), stun.feed (fetched).
// Falls back to hardcoded defaults if no servers are configured.
func STUNServers() []string {
	ops := NewOperations()
	cfg, err := ops.load()
	if err != nil {
		return defaultSTUNServers()
	}
	stunCfg, _ := cfg["stun"].(map[string]interface{})

	var result []string

	// 1. stun.servers — explicitly configured by the user
	result = append(result, stringsFromArray(stunCfg["servers"])...)

	// 2. stun.feed — fetched from configured feed URLs
	result = append(result, loadLinesFromFeed(stringsFromArray(stunCfg["feed"]), "stun")...)

	if len(result) == 0 {
		return defaultSTUNServers()
	}
	return result
}

func defaultSTUNServers() []string {
	return []string{
		"stun.l.google.com:19302",
		"stun1.l.google.com:19302",
		"stun2.l.google.com:19302",
		"stun3.l.google.com:19302",
		"stun4.l.google.com:19302",
		"global.stun.twilio.com:3478",
	}
}

// stringsFromArray converts a []interface{} config value to []string, ignoring non-strings.
func stringsFromArray(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Dir returns the default cache directory for BitTorrent data
func Dir() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ".bt"
	}
	return filepath.Join(homeDir, ".bt")
}

// loadLinesFromFeed fetches line-delimited text from feed URLs (http/https or local file).
// topic is used for log messages (e.g. "tracker", "dht", "stun").
func loadLinesFromFeed(feedURLs []string, topic string) []string {
	if len(feedURLs) == 0 {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var all []string
	for _, url := range feedURLs {
		lines := loadLinesFromURL(url, topic, client)
		if len(lines) > 0 {
			all = append(all, lines...)
			log.Info("loaded from feed", "sub", topic, "url", url, "count", len(lines))
		}
	}
	return all
}

// loadLinesFromURL loads non-empty, non-comment lines from a URL or file path.
func loadLinesFromURL(url, topic string, client *http.Client) []string {
	var data []byte
	var err error

	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		resp, err := client.Get(url)
		if err != nil {
			log.Warn("failed to fetch feed", "sub", topic, "url", url, "err", err)
			return nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Warn("feed returned error", "sub", topic, "url", url, "status", resp.StatusCode)
			return nil
		}

		scanner := bufio.NewScanner(resp.Body)
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		data = []byte(strings.Join(lines, "\n"))
		if err := scanner.Err(); err != nil {
			log.Warn("error reading feed", "sub", topic, "url", url, "err", err)
			return nil
		}
	} else {
		data, err = os.ReadFile(url)
		if err != nil {
			log.Warn("failed to read feed file", "sub", topic, "url", url, "err", err)
			return nil
		}
	}

	raw := strings.Split(string(data), "\n")
	result := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			result = append(result, line)
		}
	}
	return result
}

// loadTrackersFromFeeds fetches tracker lists from the given feed URLs.
func loadTrackersFromFeeds(feedURLs []string) []string {
	return loadLinesFromFeed(feedURLs, "tracker")
}

// loadDHTNodesFromFeeds fetches DHT node lists from the given feed URLs.
func loadDHTNodesFromFeeds(feedURLs []string) []string {
	return loadLinesFromFeed(feedURLs, "dht")
}
