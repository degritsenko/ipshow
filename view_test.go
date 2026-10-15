package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHomeEscapesExternalData(t *testing.T) {
	w := httptest.NewRecorder()
	page := homePage{
		IP:      "176.195.79.127",
		Country: `<script>alert(1)</script>`,
		City:    `<img src=x onerror=alert(1)>`,
		Stats:   basicStats{TotalRequests: 42, TodayRequests: 7, CLIRequests: 20, JSONRequests: 10, BrowserRequests: 12, StatsSince: "2026-10-16 00:00 UTC"},
	}
	if err := renderHome(w, page); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	for _, unsafe := range []string{page.Country, page.City} {
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
