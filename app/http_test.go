package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPRoutesAndCounters(t *testing.T) {
	setupGeoTest(t)
	ip := "8.8.8.8"
	if _, err := saveGeoLookup(ip, geoData{Country: "United States", Code: "US", City: "Mountain View"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	server.Config = newHTTPServer()
	server.Start()
	defer server.Close()
	for _, tc := range []struct {
		path, ua, contentType, body string
		status                      int
	}{
		{"/", "curl/8", "text/plain; charset=utf-8", ip + "\n", 200},
		{"/ip", "Mozilla/5.0", "text/plain; charset=utf-8", ip + "\n", 200},
		{"/json", "curl/8", "application/json; charset=utf-8", `"ip": "8.8.8.8"`, 200},
		{"/", "Mozilla/5.0", "text/html; charset=utf-8", "Command Line Interface", 200},
		{"/missing", "curl/8", "text/plain; charset=utf-8", "404 page not found", 404},
	} {
		r, err := http.NewRequest(http.MethodGet, server.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("User-Agent", tc.ua)
		r.Header.Set("X-Forwarded-For", ip)
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.status || resp.Header.Get("Content-Type") != tc.contentType || !strings.Contains(string(body), tc.body) {
			t.Fatalf("%s ua=%q: status=%d type=%q body=%s", tc.path, tc.ua, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
		if tc.status == 200 && resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("IP response can be cached")
		}
	}
	stats, err := readBasicStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRequests != 4 || stats.CLIRequests != 2 || stats.JSONRequests != 1 || stats.BrowserRequests != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}
