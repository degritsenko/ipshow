package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go.etcd.io/bbolt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

var db *bbolt.DB
var geo = newGeoService()

func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}
func btoi(v []byte) uint64 {
	return binary.BigEndian.Uint64(v)
}

func main() {
	var err error
	db, err = bbolt.Open("data/stats.db", 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("IPStats"))
		return err
	}); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stats" {
			showStats(w)
			return
		}

		ip := clientIP(r)
		country, code := geo.Lookup(ip)

		if err := db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket([]byte("IPStats"))
			count := uint64(0)
			if v := b.Get([]byte(ip)); v != nil {
				count = btoi(v)
			}
			return b.Put([]byte(ip), itob(count+1))
		}); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			log.Printf("db update error: %v", err)
			return
		}

		ua := strings.ToLower(r.Header.Get("User-Agent"))
		isCLI := strings.Contains(ua, "curl") || strings.Contains(ua, "wget") || strings.Contains(ua, "httpie")

		if isCLI {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "%s\n", ip)
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, htmlTemplate, ip, fallback(country, "unknown"), fallback(code, "--"))
		}
	})

	log.Println("Running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func showStats(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("IPStats"))
		return b.ForEach(func(k, v []byte) error {
			ip := string(k)
			country, code := geo.Lookup(ip)
			fmt.Fprintf(w, "IP: %s | Country: %s (%s) | Count: %d\n", ip, fallback(country, "unknown"), fallback(code, "--"), btoi(v))
			return nil
		})
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("db view error: %v", err)
	}
}

type geoService struct {
	client *http.Client
	mu     sync.RWMutex
	cache  map[string]geoCache
	ttl    time.Duration
}

type geoCache struct {
	country string
	code    string
	exp     time.Time
}

type ipWhoIsResponse struct {
	Success     bool   `json:"success"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
}

func newGeoService() *geoService {
	return &geoService{
		client: &http.Client{Timeout: 2 * time.Second},
		cache:  make(map[string]geoCache),
		ttl:    24 * time.Hour,
	}
}

func (g *geoService) Lookup(ip string) (string, string) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "", ""
	}

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", ""
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return "Local/Private", "LAN"
	}

	now := time.Now()
	g.mu.RLock()
	item, ok := g.cache[ip]
	g.mu.RUnlock()
	if ok && now.Before(item.exp) {
		return item.country, item.code
	}

	req, err := http.NewRequest(http.MethodGet, "https://ipwho.is/"+ip, nil)
	if err != nil {
		return "", ""
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}

	var parsed ipWhoIsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil || !parsed.Success {
		return "", ""
	}

	parsed.Country = strings.TrimSpace(parsed.Country)
	parsed.CountryCode = strings.TrimSpace(parsed.CountryCode)
	g.mu.Lock()
	g.cache[ip] = geoCache{
		country: parsed.Country,
		code:    parsed.CountryCode,
		exp:     now.Add(g.ttl),
	}
	g.mu.Unlock()
	return parsed.Country, parsed.CountryCode
}

func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			ip := stripPort(strings.TrimSpace(parts[0]))
			if ip != "" {
				return ip
			}
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		ip := stripPort(xri)
		if ip != "" {
			return ip
		}
	}
	return stripPort(strings.TrimSpace(r.RemoteAddr))
}

func stripPort(v string) string {
	if parsedIP, err := netip.ParseAddr(v); err == nil {
		return parsedIP.String()
	}
	if parsedAddrPort, err := netip.ParseAddrPort(v); err == nil {
		return parsedAddrPort.Addr().String()
	}
	host, _, err := net.SplitHostPort(v)
	if err == nil && host != "" {
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		return host
	}
	return v
}

func fallback(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

const htmlTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>IP Check</title>
    <style>
        body {font-family: -apple-system, system-ui, sans-serif; background: #fdfdfd; display: flex; justify-content: center; align-items: center; height: 100vh; margin: 0; color: #444;}
        .container {text-align: center; padding: 20px;}
        h1 {font-weight: normal; font-size: 1.1rem; color: #888; margin-bottom: 5px;}
        .ip {font-size: clamp(1.5rem, 8vw, 2.5rem); font-weight: bold; color: #222; margin-bottom: 10px;}
        .country {font-size: 1.1rem; color: #666;}
    </style>
</head>
<body>
    <div class="container">
        <h1>Your IP address</h1>
        <div class="ip">%s</div>
        <div class="country">%s (%s)</div>
    </div>
</body>
</html>`
