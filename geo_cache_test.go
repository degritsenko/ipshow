package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestExpiredGeoCacheRefreshesProvider(t *testing.T) {
	setupGeoTest(t)
	calls := 0
	geo.client.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		calls++
		city := "Before"
		if calls > 1 {
			city = "After"
		}
		body := `{"success":true,"country":"United States","country_code":"US","city":"` + city + `"}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if got := lookupGeoDetailsCached("8.8.8.8", true); got.City != "Before" {
		t.Fatal(got)
	}
	if _, err := saveGeoLookup("8.8.8.8", geoData{Country: "United States", Code: "US", City: "Before"}, time.Now().Add(-geoSuccessCacheTTL-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := lookupGeoDetailsCached("8.8.8.8", true); got.City != "After" {
		t.Fatalf("expired persistent entry hidden by another cache: %+v", got)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestFailedGeoLookupCannotOverwriteConcurrentSuccess(t *testing.T) {
	setupGeoTest(t)
	now := time.Now()
	success := geoData{Country: "United States", Code: "US", City: "New York", Timezone: "America/New_York"}
	if _, err := saveGeoLookup("8.8.8.8", success, now); err != nil {
		t.Fatal(err)
	}
	// A lookup that started before this success finishes with an error.
	if _, err := saveGeoLookup("8.8.8.8", geoData{}, now); err != nil {
		t.Fatal(err)
	}
	var entry persistedGeoEntry
	if err := db.View(func(tx *bbolt.Tx) error { entry, _ = readGeoCacheFromTx(tx, "8.8.8.8"); return nil }); err != nil {
		t.Fatal(err)
	}
	if !entry.Success || entry.data() != success || entry.SavedAt != now.Unix() {
		t.Fatalf("successful data lost: %+v", entry)
	}
	if entry.RetryAfter != now.Add(geoFailureCacheTTL).Unix() {
		t.Fatal("retry time not saved")
	}
}

func TestGeoCacheLifetimes(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		entry   persistedGeoEntry
		details bool
		want    bool
	}{
		{"fresh success", persistedGeoEntry{Success: true, SavedAt: now.Add(-13 * 24 * time.Hour).Unix(), Details: &geoData{}}, true, true},
		{"expired success", persistedGeoEntry{Success: true, SavedAt: now.Add(-15 * 24 * time.Hour).Unix(), Details: &geoData{}}, true, false},
		{"recent failure", persistedGeoEntry{SavedAt: now.Add(-59 * time.Minute).Unix()}, true, true},
		{"retry failure", persistedGeoEntry{SavedAt: now.Add(-61 * time.Minute).Unix()}, true, false},
		{"legacy home", persistedGeoEntry{Success: true, SavedAt: now.Unix()}, false, true},
		{"legacy JSON", persistedGeoEntry{Success: true, SavedAt: now.Unix()}, true, false},
		{"stale retry delayed", persistedGeoEntry{Success: true, SavedAt: now.Add(-15 * 24 * time.Hour).Unix(), RetryAfter: now.Add(time.Minute).Unix()}, true, true},
		{"stale retry ready", persistedGeoEntry{Success: true, SavedAt: now.Add(-15 * 24 * time.Hour).Unix(), RetryAfter: now.Add(-time.Second).Unix()}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.usable(now, tc.details); got != tc.want {
				t.Fatalf("usable=%t want %t", got, tc.want)
			}
		})
	}
}
