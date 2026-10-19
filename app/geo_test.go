package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
	oldProxy := caddyProxy
	caddyProxy = newProxyTrust()
	caddyProxy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	t.Cleanup(func() { caddyProxy = oldProxy })
	var err error
	db, err = openDatabase(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); db, geo = oldDB, oldGeo })
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

func TestIPEndpoint(t *testing.T) {
	setupGeoTest(t)
	for _, ua := range []string{"curl/8", "Mozilla/5.0", ""} {
		for _, ip := range []string{"176.195.79.127", "2001:db8::1"} {
			r := httptest.NewRequest("GET", "/ip", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("X-Forwarded-For", ip)
			r.Header.Set("User-Agent", ua)
			w := httptest.NewRecorder()
			handleRequest(w, r)
			if w.Code != 200 || w.Body.String() != ip+"\n" || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("ua=%q ip=%q: %d %s", ua, ip, w.Code, w.Body)
			}
		}
	}
	stats, err := readBasicStats()
	if err != nil || stats.TotalRequests != 6 || stats.CLIRequests != 6 || stats.JSONRequests != 0 || stats.BrowserRequests != 0 {
		t.Fatalf("unexpected counts: %+v err=%v", stats, err)
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
		serveJSON(context.Background(), w, "8.8.8.8")
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

func TestGeoFailurePreservesLastSuccess(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			setupGeoTest(t)
			ip := "8.8.8.8"
			savedAt := time.Now().Add(-geoSuccessCacheTTL - time.Hour).Unix()
			entry := persistedGeoEntry{Country: "United States", Code: "US", City: "Mountain View", Success: true, SavedAt: savedAt}
			if legacy {
				// Even a fresh legacy record needs enrichment for /json.
				entry.SavedAt = time.Now().Unix()
			} else {
				entry.Details = &geoData{Country: entry.Country, Code: entry.Code, City: entry.City, Region: "California", Loc: "37.386,-122.0838", Timezone: "America/Los_Angeles"}
			}
			put := func(e persistedGeoEntry) {
				t.Helper()
				raw, err := json.Marshal(e)
				if err != nil {
					t.Fatal(err)
				}
				if err = db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte(geoCacheBucketName)).Put([]byte(ip), raw) }); err != nil {
					t.Fatal(err)
				}
			}
			put(entry)
			calls := 0
			transport := testTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			geo.client.Transport = transport
			got := lookupGeoDetailsCached(context.Background(), ip)
			if got.Country != entry.Country || got.City != entry.City {
				t.Fatalf("lost last success: %+v", got)
			}
			if !legacy && got.Loc != entry.Details.Loc {
				t.Fatal("lost extended fields")
			}
			var stored persistedGeoEntry
			if err := db.View(func(tx *bbolt.Tx) error { stored, _ = readGeoCacheFromTx(tx, ip); return nil }); err != nil {
				t.Fatal(err)
			}
			if !stored.Success || stored.SavedAt != entry.SavedAt {
				t.Fatalf("last success overwritten: %+v", stored)
			}
			if calls != 3 {
				t.Fatalf("want fallback chain, calls=%d", calls)
			}
			// Retry suppression must survive an application restart.
			geo = newGeoService()
			geo.client.Transport = transport
			if got := lookupGeoDetailsCached(context.Background(), ip); got.City != entry.City {
				t.Fatal(got)
			}
			if calls != 3 {
				t.Fatal("repeated failed lookup after restart")
			}
			stored.RetryAfter = time.Now().Add(-time.Second).Unix()
			put(stored)
			geo.client.Transport = testTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"country":"United States","country_code":"US","city":"New York"}`))}, nil
			})
			if got := lookupGeoDetailsCached(context.Background(), ip); got.City != "New York" {
				t.Fatalf("retry failed: %+v", got)
			}
		})
	}
}

func TestCanceledGeoLookupDoesNotChangePersistentCache(t *testing.T) {
	setupGeoTest(t)
	ip := "8.8.8.8"
	now := time.Now().Add(-geoSuccessCacheTTL - time.Hour)
	previous, err := saveGeoLookup(ip, geoData{Country: "United States", Code: "US", City: "Mountain View"}, now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	geo.client.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		cancel()
		if !errors.Is(r.Context().Err(), context.Canceled) {
			t.Fatal("provider request did not inherit caller context")
		}
		return nil, context.Canceled
	})
	if got := lookupGeoDetailsCached(ctx, ip); got.City != previous.City {
		t.Fatalf("stale data lost: %+v", got)
	}
	if err := db.View(func(tx *bbolt.Tx) error {
		stored, _ := readGeoCacheFromTx(tx, ip)
		if stored.RetryAfter != previous.RetryAfter || stored.SavedAt != previous.SavedAt || !stored.Success {
			t.Fatalf("cancellation stored as lookup failure: %+v", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
