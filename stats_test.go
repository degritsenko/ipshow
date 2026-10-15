package main

import (
	"net/http/httptest"
	"strings"
	"testing"

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
	total, _, cli, jsonCount, browser, _, err := readBasicStats()
	if err != nil {
		t.Fatal(err)
	}
	if total != 34 || cli != 11 || jsonCount != 2 || browser != 21 {
		t.Fatalf("unexpected counts: total=%d cli=%d json=%d browser=%d", total, cli, jsonCount, browser)
	}
}
