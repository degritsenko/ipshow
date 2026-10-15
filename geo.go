package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

type geoData struct {
	Country  string `json:"country"`
	Code     string `json:"code"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Loc      string `json:"loc"`
	ASN      string `json:"asn"`
	ASNName  string `json:"asn_name"`
	Postal   string `json:"postal"`
	Timezone string `json:"timezone"`
}

func (d geoData) legacy() (string, string, string, string, string) {
	return d.Country, d.Code, d.City, d.ASN, d.ASNName
}

type geoCache struct {
	data geoData
	exp  time.Time
}

type geoService struct {
	client  *http.Client
	mu      sync.RWMutex
	cache   map[string]geoCache
	ttl     time.Duration
	failTTL time.Duration
}

// Keep the original fields so existing databases remain readable.
type persistedGeoEntry struct {
	Country string   `json:"country"`
	Code    string   `json:"code"`
	City    string   `json:"city"`
	ASN     string   `json:"asn"`
	ASNName string   `json:"asn_name"`
	Success bool     `json:"success"`
	SavedAt int64    `json:"saved_at"`
	Details *geoData `json:"details,omitempty"`
}

func newGeoService() *geoService {
	return &geoService{client: &http.Client{Timeout: 2 * time.Second}, cache: make(map[string]geoCache), ttl: 24 * time.Hour, failTTL: 15 * time.Minute}
}

func coordinates(lat, lon *float64) string {
	if lat == nil || lon == nil {
		return ""
	}
	return strconv.FormatFloat(*lat, 'f', -1, 64) + "," + strconv.FormatFloat(*lon, 'f', -1, 64)
}

func (g *geoService) fetch(url string, result any) bool {
	resp, err := g.client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(result) == nil
}

func (g *geoService) lookupIPWhoIs(ip string) geoData {
	var p struct {
		Success   bool     `json:"success"`
		Country   string   `json:"country"`
		Code      string   `json:"country_code"`
		City      string   `json:"city"`
		Region    string   `json:"region"`
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
		Postal    string   `json:"postal"`
		Timezone  struct {
			ID string `json:"id"`
		} `json:"timezone"`
		Connection struct {
			ASN int    `json:"asn"`
			Org string `json:"org"`
		} `json:"connection"`
	}
	if !g.fetch("https://ipwho.is/"+ip, &p) || !p.Success {
		return geoData{}
	}
	asn := ""
	if p.Connection.ASN > 0 {
		asn = fmt.Sprintf("AS%d", p.Connection.ASN)
	}
	return geoData{Country: p.Country, Code: p.Code, City: p.City, Region: p.Region, Loc: coordinates(p.Latitude, p.Longitude), ASN: asn, ASNName: p.Connection.Org, Postal: p.Postal, Timezone: p.Timezone.ID}
}

func (g *geoService) lookupIPAPI(ip string) geoData {
	var p struct {
		Country   string   `json:"country_name"`
		Code      string   `json:"country"`
		City      string   `json:"city"`
		Region    string   `json:"region"`
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
		ASN       string   `json:"asn"`
		Org       string   `json:"org"`
		Postal    string   `json:"postal"`
		Timezone  string   `json:"timezone"`
	}
	if !g.fetch("https://ipapi.co/"+ip+"/json/", &p) {
		return geoData{}
	}
	return geoData{Country: p.Country, Code: p.Code, City: p.City, Region: p.Region, Loc: coordinates(p.Latitude, p.Longitude), ASN: p.ASN, ASNName: p.Org, Postal: p.Postal, Timezone: p.Timezone}
}

func (g *geoService) lookupGeoJS(ip string) geoData {
	var p struct {
		Country   string `json:"country"`
		Code      string `json:"country_code"`
		City      string `json:"city"`
		Region    string `json:"region"`
		Latitude  string `json:"latitude"`
		Longitude string `json:"longitude"`
		ASN       int    `json:"asn"`
		Org       string `json:"organization_name"`
		Timezone  string `json:"timezone"`
	}
	if !g.fetch("https://get.geojs.io/v1/ip/geo/"+ip+".json", &p) {
		return geoData{}
	}
	loc, asn := "", ""
	if p.Latitude != "" && p.Longitude != "" {
		loc = p.Latitude + "," + p.Longitude
	}
	if p.ASN > 0 {
		asn = fmt.Sprintf("AS%d", p.ASN)
	}
	return geoData{Country: p.Country, Code: p.Code, City: p.City, Region: p.Region, Loc: loc, ASN: asn, ASNName: p.Org, Timezone: p.Timezone}
}

func (g *geoService) Lookup(ip string) geoData {
	ip = strings.TrimSpace(ip)
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return geoData{}
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return geoData{Country: "Local/Private", Code: "LAN", City: "Local/Private", ASN: "LAN", ASNName: "Local/Private"}
	}
	now := time.Now()
	g.mu.RLock()
	item, ok := g.cache[ip]
	g.mu.RUnlock()
	if ok && now.Before(item.exp) {
		return item.data
	}
	d := g.lookupIPWhoIs(ip)
	if strings.TrimSpace(d.Country) == "" {
		d = g.lookupIPAPI(ip)
	}
	if strings.TrimSpace(d.Country) == "" {
		d = g.lookupGeoJS(ip)
	}
	ttl := g.ttl
	if strings.TrimSpace(d.Country) == "" {
		d = geoData{}
		ttl = g.failTTL
	}
	g.mu.Lock()
	g.cache[ip] = geoCache{data: d, exp: now.Add(ttl)}
	g.mu.Unlock()
	return d
}

func lookupGeoCached(ip string) (string, string, string, string, string) {
	return lookupGeoDetailsCached(ip, false).legacy()
}

func lookupGeoDetailsCached(ip string, requireDetails bool) geoData {
	var e persistedGeoEntry
	found := false
	_ = db.View(func(tx *bbolt.Tx) error { e, found = readGeoCacheFromTx(tx, ip); return nil })
	if found {
		ttl := geoFailureCacheTTL
		if e.Success {
			ttl = geoSuccessCacheTTL
		}
		if time.Since(time.Unix(e.SavedAt, 0)) <= ttl {
			if !e.Success {
				return geoData{}
			}
			if e.Details != nil {
				return *e.Details
			}
			if !requireDetails {
				return geoData{Country: e.Country, Code: e.Code, City: e.City, ASN: e.ASN, ASNName: e.ASNName}
			}
		}
	}
	d := geo.Lookup(ip)
	e = persistedGeoEntry{Country: d.Country, Code: d.Code, City: d.City, ASN: d.ASN, ASNName: d.ASNName, Success: strings.TrimSpace(d.Country) != "", SavedAt: time.Now().Unix(), Details: &d}
	if raw, err := json.Marshal(e); err == nil {
		_ = db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte(geoCacheBucketName)).Put([]byte(ip), raw) })
	}
	return d
}

func serveJSON(w http.ResponseWriter, ip string) {
	d := lookupGeoDetailsCached(ip, true)
	result := struct {
		IP       string `json:"ip"`
		City     string `json:"city"`
		Region   string `json:"region"`
		Country  string `json:"country"`
		Loc      string `json:"loc"`
		Org      string `json:"org"`
		Postal   string `json:"postal"`
		Timezone string `json:"timezone"`
	}{ip, d.City, d.Region, d.Code, d.Loc, strings.TrimSpace(d.ASN + " " + d.ASNName), d.Postal, d.Timezone}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result)
}
