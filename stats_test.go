package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestJSONRequestCounts(t *testing.T) {
	setupGeoTest(t)
	// An existing database has no JSON counter.
	if err := db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(reqStatsBucketName))
		if err := b.Put([]byte("cli"), itob(10)); err != nil {
			return err
		}
		return b.Put([]byte("browser"), itob(20))
	}); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct{ path, ua string }{
		{"/json", "curl/8"},
		{"/json", "Mozilla/5.0"},
		{"/", "curl/8"},
		{"/", "Mozilla/5.0"},
	} {
		r := httptest.NewRequest("GET", request.path, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("User-Agent", request.ua)
		w := httptest.NewRecorder()
		handleRequest(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d", request.path, w.Code)
		}
		if request.path == "/" && strings.HasPrefix(request.ua, "Mozilla") {
			if !strings.Contains(w.Body.String(), "CLI requests: 11</div>\n        <div class=\"line\">JSON requests: 2</div>") {
				t.Fatalf("JSON counter missing below CLI: %s", w.Body.String())
			}
		}
	}
	total, today, _, cli, jsonCount, browser, _, err := readBasicStats()
	if err != nil {
		t.Fatal(err)
	}
	if total != 34 || cli != 11 || jsonCount != 2 || browser != 21 {
		t.Fatalf("unexpected counts: total=%d cli=%d json=%d browser=%d", total, cli, jsonCount, browser)
	}
	if today != 4 {
		t.Fatalf("today=%d, want 4 (historical requests must not be backfilled)", today)
	}
}

func TestDailyRequestsAcrossUTCMidnightAndReopen(t *testing.T) {
	setupGeoTest(t)
	before := time.Date(2026, 10, 16, 23, 59, 59, 0, time.UTC)
	after := before.Add(time.Second)
	for _, request := range []struct {
		at        time.Time
		cli, json bool
	}{
		{before, true, false},
		{before, false, true},
		{after.In(time.FixedZone("MSK", 3*60*60)), false, false},
	} {
		if err := incrementIPStatAt("127.0.0.1", request.cli, request.json, request.at); err != nil {
			t.Fatal(err)
		}
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	db, err = bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(reqStatsBucketName))
		for _, want := range []struct {
			at    time.Time
			count uint64
		}{{before, 2}, {after, 1}} {
			v := b.Get(dailyRequestsKey(want.at))
			if len(v) != 8 || btoi(v) != want.count {
				t.Errorf("%s: got %v, want %d", want.at, v, want.count)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
