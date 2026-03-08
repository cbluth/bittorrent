package bt

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cbluth/bittorrent/pkg/config"
	"github.com/cbluth/bittorrent/pkg/log"
)

// LoadTrackersOptions contains options for loading trackers
type LoadTrackersOptions struct {
	TrackersFile string
	CacheOps     *config.CacheOperations
}

// LoadTrackers loads trackers from config, file, or cache
func LoadTrackers(opts *LoadTrackersOptions) []string {
	var trackers []string
	var cachedTrackers []string

	// Try to load cached working trackers from CacheOperations
	if opts.CacheOps != nil {
		cache, err := opts.CacheOps.Load()
		if err == nil && cache != nil {
			cacheAge := time.Since(cache.SavedAt)
			if cacheAge <= 7*24*time.Hour {
				for _, pt := range cache.Trackers {
					if time.Since(pt.LastSuccess) <= 24*time.Hour && pt.Failures == 0 {
						cachedTrackers = append(cachedTrackers, pt.URL)
					}
				}
				if len(cachedTrackers) > 0 {
					log.Info("loaded working trackers from cache", "sub", "tracker", "count", len(cachedTrackers), "age", cacheAge)
				}
			}
		}
	}

	// Check for configured trackers first
	configuredTrackers := config.Trackers()
	if len(configuredTrackers) > 0 {
		trackers = configuredTrackers
		log.Debug("using configured trackers", "sub", "tracker", "count", len(trackers))
	} else if opts.TrackersFile != "" {
		var err error
		trackers, err = LoadTrackersFromFile(opts.TrackersFile)
		if err != nil {
			log.Error("failed to load trackers file", "sub", "tracker", "err", err)
			os.Exit(1)
		}
		log.Debug("loaded trackers from file", "sub", "tracker", "count", len(trackers))
	} else {
		log.Warn("no trackers configured", "sub", "tracker")
		trackers = []string{}
	}

	// Prepend cached working trackers
	if len(cachedTrackers) > 0 {
		trackers = append(cachedTrackers, trackers...)
		log.Debug("prioritizing recently working trackers", "sub", "tracker", "count", len(cachedTrackers))
	}

	return trackers
}

// LoadTrackersFromFile loads trackers from a file
func LoadTrackersFromFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(data), "\n")
	trackers := make([]string, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			trackers = append(trackers, line)
		}
	}

	return trackers, nil
}

// FetchTrackersFromURL fetches a tracker list from a user-configured URL
func FetchTrackersFromURL(url string) ([]string, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	trackers := make([]string, 0)
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			trackers = append(trackers, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return trackers, nil
}

// FallbackTrackers returns trackers from config (no hardcoded values)
func FallbackTrackers() []string {
	return config.Trackers()
}
