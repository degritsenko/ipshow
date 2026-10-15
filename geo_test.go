package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func setupGeoTest(t *testing.T) {
	t.Helper()
	oldDB, oldGeo := db, geo
	var err error
	db, err = bbolt.Open(filepath.Join(t.TempDir(), "stats.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); db, geo = oldDB, oldGeo })
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range []string{ipStatsBucketName, geoCacheBucketName, reqStatsBucketName} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	geo = newGeoService()
}

func TestJSONProvidersAndPersistentCache(t *testing.T) {
	for _, tc := range []struct{ name, host, body, org, postal string }{
		{"primary", "ipwho.is", `{"success":true,"country":"Russia","country_code":"RU","city":"Dubna","region":"Moscow Oblast","latitude":56.7405,"longitude":37.1865,"postal":"141980","timezone":{"id":"Europe/Moscow"},"connection":{"asn":12714,"org":"PJSC MegaFon"}}`, "AS12714 PJSC MegaFon", "141980"},
		{"ipapi", "ipapi.co", `{"country_name":"Russia","country":"RU","city":"Dubna","region":"Moscow Oblast","latitude":56.7405,"longitude":37.1865,"postal":"141980","timezone":"Europe/Moscow","asn":"AS12714","org":"PJSC MegaFon"}`, "AS12714 PJSC MegaFon", "141980"},
		{"geojs", "get.geojs.io", `{"country":"Russia","country_code":"RU","city":"Dubna","region":"Moscow Oblast","latitude":"56.7405","longitude":"37.1865","timezone":"Europe/Moscow","asn":12714,"organization_name":"PJSC MegaFon"}`, "AS12714 PJSC MegaFon", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupGeoTest(t)
			calls := 0
			geo.client.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `{}`
				if r.URL.Host == tc.host {
					body = tc.body
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			// A valid old cache entry must be enriched when /json is first used.
			old, _ := json.Marshal(persistedGeoEntry{Country: "Russia", Code: "RU", City: "Dubna", Success: true, SavedAt: time.Now().Unix()})
			if err := db.Update(func(tx *bbolt.Tx) error {
				return tx.Bucket([]byte(geoCacheBucketName)).Put([]byte("176.195.79.127"), old)
			}); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "http://example.com/json", nil)
			r.RemoteAddr = "176.195.79.127:1234"
			r.Header.Set("User-Agent", "curl/8")
			w := httptest.NewRecorder()
			handleRequest(w, r)
			if w.Code != 200 || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("unexpected response: %d %s", w.Code, w.Body)
			}
			var got map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"ip": "176.195.79.127", "city": "Dubna", "region": "Moscow Oblast", "country": "RU", "loc": "56.7405,37.1865", "org": tc.org, "postal": tc.postal, "timezone": "Europe/Moscow"}
			if len(got) != len(want) {
				t.Fatalf("unexpected fields: %v", got)
			}
			for key, value := range want {
				if got[key] != value {
					t.Errorf("%s: got %q want %q", key, got[key], value)
				}
			}
			if calls == 0 {
				t.Fatal("old cache was not enriched")
			}
			geo = newGeoService()
			geo.client.Transport = testTransport(func(*http.Request) (*http.Response, error) { t.Fatal("persistent cache missed"); return nil, nil })
			w = httptest.NewRecorder()
			handleRequest(w, r)
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
		})
	}
}

func TestPlainCLIAndUnknownPath(t *testing.T) {
	setupGeoTest(t)
	for _, path := range []string{"/", "/missing"} {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "176.195.79.127:1234"
		r.Header.Set("User-Agent", "curl/8")
		w := httptest.NewRecorder()
		handleRequest(w, r)
		if path == "/" && w.Body.String() != "176.195.79.127\n" {
			t.Fatal(w.Body.String())
		}
		if path == "/missing" && w.Code != 404 {
			t.Fatal(w.Code)
		}
	}
}

func TestJSONProviderFailureIsCached(t *testing.T) {
	setupGeoTest(t)
	calls := 0
	geo.client.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		serveJSON(w, "8.8.8.8")
		var got map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["ip"] != "8.8.8.8" || got["loc"] != "" || got["country"] != "" {
			t.Fatal(got)
		}
	}
	if calls != 3 {
		t.Fatalf("failure cache missed: %d upstream calls", calls)
	}
}
