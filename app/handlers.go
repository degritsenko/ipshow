package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/json" && r.URL.Path != "/ip" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ip := clientIP(r)
	kind := browserRequest
	if r.URL.Path == "/ip" || isCLIUserAgent(r.UserAgent()) {
		kind = cliRequest
	}
	if r.URL.Path == "/json" {
		kind = jsonRequest
	}
	if err := recordRequest(kind, time.Now()); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("db update error: %v", err)
		return
	}
	if kind == jsonRequest {
		serveJSON(r.Context(), w, ip)
		return
	}
	if kind == cliRequest {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, ip)
		return
	}
	location := lookupGeoDetailsCached(r.Context(), ip)
	stats, err := readBasicStats()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("read basic stats error: %v", err)
		return
	}
	if err := renderHome(w, homePage{IP: ip, Domain: r.Host, Country: fallback(location.Country, "unknown"), City: fallback(location.City, "unknown"), Location: location, Stats: stats}); err != nil {
		log.Printf("render home error: %v", err)
	}
}

func isCLIUserAgent(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	return strings.Contains(ua, "curl") || strings.Contains(ua, "wget") || strings.Contains(ua, "httpie")
}

func fallback(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

type ipInfo struct {
	IP       string `json:"ip"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Country  string `json:"country"`
	Loc      string `json:"loc"`
	Org      string `json:"org"`
	Postal   string `json:"postal"`
	Timezone string `json:"timezone"`
}

func makeIPInfo(ip string, d geoData) ipInfo {
	return ipInfo{
		IP: ip, City: d.City, Region: d.Region, Country: d.Code,
		Loc: d.Loc, Org: strings.TrimSpace(d.ASN + " " + d.ASNName),
		Postal: d.Postal, Timezone: d.Timezone,
	}
}

func serveJSON(ctx context.Context, w http.ResponseWriter, ip string) {
	result := makeIPInfo(ip, lookupGeoDetailsCached(ctx, ip))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result)
}
