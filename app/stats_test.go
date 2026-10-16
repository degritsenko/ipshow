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
	stats, err := readBasicStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRequests != 34 || stats.CLIRequests != 11 || stats.JSONRequests != 2 || stats.BrowserRequests != 21 {
		t.Fatalf("unexpected counts: %+v", stats)
	}
	if stats.TodayRequests != 4 {
		t.Fatalf("today=%d, want 4 (historical requests must not be backfilled)", stats.TodayRequests)
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
		kind := browserRequest
		if request.cli {
			kind = cliRequest
		}
		if request.json {
			kind = jsonRequest
		}
		if err := recordRequest("127.0.0.1", kind, request.at); err != nil {
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
	for _, want := range []struct {
		at    time.Time
		count uint64
	}{{before, 2}, {after, 1}, {after.Add(24 * time.Hour), 0}} {
		stats, err := readBasicStatsAt(want.at)
		if err != nil {
			t.Fatal(err)
		}
		if stats.TotalRequests != 3 || stats.TodayRequests != want.count {
			t.Errorf("%s: unexpected stats %+v", want.at, stats)
		}
	}
}
