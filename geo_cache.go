package main

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// Keep the original fields so existing databases remain readable.
type persistedGeoEntry struct {
	Country    string   `json:"country"`
	Code       string   `json:"code"`
	City       string   `json:"city"`
	ASN        string   `json:"asn"`
	ASNName    string   `json:"asn_name"`
	Success    bool     `json:"success"`
	SavedAt    int64    `json:"saved_at"`
	RetryAfter int64    `json:"retry_after,omitempty"`
	Details    *geoData `json:"details,omitempty"`
}

func (e persistedGeoEntry) data() geoData {
	if e.Details != nil {
		return *e.Details
	}
	return geoData{Country: e.Country, Code: e.Code, City: e.City, ASN: e.ASN, ASNName: e.ASNName}
}

func lookupGeoDetailsCached(ip string, requireDetails bool) geoData {
	var e persistedGeoEntry
	found := false
	if err := db.View(func(tx *bbolt.Tx) error {
		e, found = readGeoCacheFromTx(tx, ip)
		return nil
	}); err != nil {
		log.Printf("geo cache read error: %v", err)
	}
	if found && e.usable(time.Now(), requireDetails) {
		return e.data()
	}
	d := geo.Lookup(ip)
	updated, err := saveGeoLookup(ip, d, time.Now())
	if err != nil {
		log.Printf("geo cache write error: %v", err)
		if d.Country == "" && found && e.Success {
			return e.data()
		}
		return d
	}
	return updated.data()
}

func (e persistedGeoEntry) usable(now time.Time, requireDetails bool) bool {
	if now.Before(time.Unix(e.RetryAfter, 0)) {
		return true
	}
	ttl := geoFailureCacheTTL
	if e.Success {
		ttl = geoSuccessCacheTTL
		if requireDetails && e.Details == nil {
			return false
		}
	}
	return now.Sub(time.Unix(e.SavedAt, 0)) <= ttl
}

func saveGeoLookup(ip string, d geoData, now time.Time) (persistedGeoEntry, error) {
	e := persistedGeoEntry{Country: d.Country, Code: d.Code, City: d.City, ASN: d.ASN, ASNName: d.ASNName, Success: strings.TrimSpace(d.Country) != "", SavedAt: now.Unix(), Details: &d}
	err := db.Update(func(tx *bbolt.Tx) error {
		// Re-read inside the write transaction: a concurrent success must survive a failed lookup.
		if previous, found := readGeoCacheFromTx(tx, ip); !e.Success && found && previous.Success {
			e = previous
			e.RetryAfter = now.Add(geoFailureCacheTTL).Unix()
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(geoCacheBucketName)).Put([]byte(ip), raw)
	})
	return e, err
}

func readGeoCacheFromTx(tx *bbolt.Tx, ip string) (persistedGeoEntry, bool) {
	b := tx.Bucket([]byte(geoCacheBucketName))
	if b == nil {
		return persistedGeoEntry{}, false
	}
	raw := b.Get([]byte(ip))
	if len(raw) == 0 {
		return persistedGeoEntry{}, false
	}
	var entry persistedGeoEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return persistedGeoEntry{}, false
	}
	return entry, true
}
