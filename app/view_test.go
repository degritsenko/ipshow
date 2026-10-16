package main

import (
	"encoding/json"
	"html"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHomeEscapesExternalData(t *testing.T) {
	w := httptest.NewRecorder()
	page := homePage{
		IP:      "176.195.79.127",
		Domain:  `<b>host</b>`,
		Country: `<script>alert(1)</script>`,
		City:    `<img src=x onerror=alert(1)>`,
		Stats:   basicStats{TotalRequests: 42, TodayRequests: 7, CLIRequests: 20, JSONRequests: 10, BrowserRequests: 12, StatsSince: "2026-10-16 00:00 UTC"},
	}
	if err := renderHome(w, page); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	for _, unsafe := range []string{page.Country, page.City, page.Domain} {
		if strings.Contains(body, unsafe) {
			t.Fatalf("unescaped external HTML: %s", unsafe)
		}
	}
	for _, want := range []string{"&lt;script&gt;", "&lt;img", "Total requests: 42", "Requests today (UTC): 7", "CLI requests: 20", "JSON requests: 10", "Browser requests: 12", "width: min(760px, 100%);", `href="/ip"`, `href="/json"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal(w.Header())
	}
}

func TestHomeCLIExamplesMatchJSON(t *testing.T) {
	setupGeoTest(t)
	ip := "8.8.8.8"
	location := geoData{Country: "United States", Code: "US", City: "Mountain View", Region: "California", Loc: "37.386,-122.0838", ASN: "AS15169", ASNName: "Google LLC", Postal: "94035", Timezone: "America/Los_Angeles"}
	if _, err := saveGeoLookup(ip, location, time.Now()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://example.test/", nil)
	r.RemoteAddr = ip + ":1234"
	r.Header.Set("User-Agent", "Mozilla/5.0")
	w := httptest.NewRecorder()
	handleRequest(w, r)
	body := w.Body.String()
	for _, want := range []string{"Command Line Interface", `<a href="/">example.test</a>`, `curl example.test<a href="/ip">/ip</a>`, `curl example.test<a href="/json">/json</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing command guide element %q", want)
		}
	}
	_, preview, ok := strings.Cut(body, "<pre>")
	if !ok {
		t.Fatal("JSON preview missing")
	}
	preview, _, ok = strings.Cut(preview, "</pre>")
	if !ok {
		t.Fatal("JSON preview not closed")
	}
	var shown, actual map[string]string
	if err := json.Unmarshal([]byte(html.UnescapeString(preview)), &shown); err != nil {
		t.Fatal(err)
	}
	stats, err := readBasicStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRequests != 1 || stats.BrowserRequests != 1 || stats.JSONRequests != 0 {
		t.Fatalf("preview inflated statistics: %+v", stats)
	}
	r = httptest.NewRequest("GET", "https://example.test/json", nil)
	r.RemoteAddr = ip + ":1234"
	w = httptest.NewRecorder()
	handleRequest(w, r)
	if err := json.Unmarshal(w.Body.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shown, actual) {
		t.Fatalf("preview %+v differs from JSON endpoint %+v", shown, actual)
	}
}
