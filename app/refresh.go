package main

import (
	"log"
	"sort"
	"time"
)

func startGeoRefreshWorker() {
	go func() {
		// First pass shortly after startup to warm the cache.
		time.Sleep(30 * time.Second)
		refreshGeoCacheTopIPs()

		ticker := time.NewTicker(geoRefreshInterval)
		defer ticker.Stop()

		for range ticker.C {
			refreshGeoCacheTopIPs()
		}
	}()
}

func refreshGeoCacheTopIPs() {
	lines, err := loadStatsLines()
	if err != nil {
		log.Printf("geo refresh: load stats error: %v", err)
		return
	}
	if len(lines) == 0 {
		return
	}

	sort.Slice(lines, func(i, j int) bool {
		if lines[i].count == lines[j].count {
			return lines[i].ip < lines[j].ip
		}
		return lines[i].count > lines[j].count
	})
	limit := geoRefreshTopN
	if len(lines) < limit {
		limit = len(lines)
	}

	updated := 0
	for i := 0; i < limit; i++ {
		ip := lines[i].ip
		_ = lookupGeoDetailsCached(ip, false)
		updated++
		time.Sleep(geoRefreshPause)
	}
	log.Printf("geo refresh: processed %d IPs", updated)
}
