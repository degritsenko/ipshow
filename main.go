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
	"sort"
	"strings"
	"sync"
	"time"
)

var db *bbolt.DB
var geo = newGeoService()

const (
	ipStatsBucketName  = "IPStats"
	geoCacheBucketName = "GeoCache"
	reqStatsBucketName = "RequestStats"

	geoSuccessCacheTTL = 14 * 24 * time.Hour
	geoFailureCacheTTL = 60 * time.Minute
	geoRefreshInterval = 2 * time.Hour
	geoRefreshTopN     = 500
	geoRefreshPause    = 75 * time.Millisecond

	defaultSort = "count_desc"
)

func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}
func btoi(v []byte) uint64 {
	return binary.BigEndian.Uint64(v)
}

type ipStatLine struct {
	ip    string
	count uint64
}

func main() {
	var err error
	db, err = bbolt.Open("data/stats.db", 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(ipStatsBucketName)); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists([]byte(geoCacheBucketName)); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists([]byte(reqStatsBucketName)); err != nil {
			return err
		}
		return nil
	}); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		ip := clientIP(r)
		isCLI := isCLIUserAgent(r.Header.Get("User-Agent"))
		if err := incrementIPStat(ip, isCLI); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			log.Printf("db update error: %v", err)
			return
		}

		if isCLI {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "%s\n", ip)
		} else {
			country, _, city, _, _ := lookupGeoCached(ip)
			totalReq, _, cliReq, browserReq, statsSince, err := readBasicStats()
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				log.Printf("read basic stats error: %v", err)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(
				w,
				htmlTemplate,
				ip,
				fallback(country, "unknown"),
				fallback(city, "unknown"),
				totalReq,
				cliReq,
				browserReq,
				statsSince,
			)
		}
	})

	startGeoRefreshWorker()

	log.Println("Running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func startGeoRefreshWorker() {
	go func() {
		// First pass shortly after startup to warm the cache.
		time.Sleep(30 * time.Second)
		refreshGeoCacheTopIPs()

		ticker := time.NewTicker(geoRefreshInterval)
		defer ticker.Stop()

		for range ticker.C {
			refreshGeoCacheTopIPs()
		}
	}()
}

func refreshGeoCacheTopIPs() {
	lines, _, err := loadStatsLines()
	if err != nil {
		log.Printf("geo refresh: load stats error: %v", err)
		return
	}
	if len(lines) == 0 {
		return
	}

	sortStatsLines(lines, defaultSort)
	limit := geoRefreshTopN
	if len(lines) < limit {
		limit = len(lines)
	}

	updated := 0
	for i := 0; i < limit; i++ {
		ip := lines[i].ip
		_, _, _, _, _ = lookupGeoCached(ip)
		updated++
		time.Sleep(geoRefreshPause)
	}
	log.Printf("geo refresh: processed %d IPs", updated)
}

func loadStatsLines() ([]ipStatLine, uint64, error) {
	lines := make([]ipStatLine, 0, 256)
	total := uint64(0)

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(ipStatsBucketName))
		return b.ForEach(func(k, v []byte) error {
			count := btoi(v)
			total += count
			lines = append(lines, ipStatLine{
				ip:    string(k),
				count: count,
			})
			return nil
		})
	})
	if err != nil {
		return nil, 0, err
	}
	return lines, total, nil
}

func incrementIPStat(ip string, isCLI bool) error {
	return db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(ipStatsBucketName))
		count := uint64(0)
		if v := b.Get([]byte(ip)); v != nil {
			count = btoi(v)
		}
		newCount := count + 1
		if err := b.Put([]byte(ip), itob(newCount)); err != nil {
			return err
		}

		reqBucket := tx.Bucket([]byte(reqStatsBucketName))
		if count == 0 {
			uniqueIPs := uint64(0)
			if v := reqBucket.Get([]byte("unique_ips")); v != nil {
				uniqueIPs = btoi(v)
			}
			if err := reqBucket.Put([]byte("unique_ips"), itob(uniqueIPs+1)); err != nil {
				return err
			}
		}

		key := []byte("browser")
		if isCLI {
			key = []byte("cli")
		}
		reqCount := uint64(0)
		if v := reqBucket.Get(key); v != nil {
			reqCount = btoi(v)
		}
		if err := reqBucket.Put(key, itob(reqCount+1)); err != nil {
			return err
		}

		if v := reqBucket.Get([]byte("stats_since_unix")); v == nil {
			if err := reqBucket.Put([]byte("stats_since_unix"), itob(uint64(time.Now().Unix()))); err != nil {
				return err
			}
		}
		return nil
	})
}

func readRequestSourceCounts() (uint64, uint64, error) {
	var cliReq uint64
	var browserReq uint64
	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(reqStatsBucketName))
		if v := b.Get([]byte("cli")); v != nil {
			cliReq = btoi(v)
		}
		if v := b.Get([]byte("browser")); v != nil {
			browserReq = btoi(v)
		}
		return nil
	})
	return cliReq, browserReq, err
}

func readStatsSince() (string, error) {
	var since uint64
	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(reqStatsBucketName))
		if v := b.Get([]byte("stats_since_unix")); v != nil {
			since = btoi(v)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if since == 0 {
		return "n/a", nil
	}
	return time.Unix(int64(since), 0).UTC().Format("2006-01-02 15:04 UTC"), nil
}

func readBasicStats() (uint64, int, uint64, uint64, string, error) {
	var cliReq uint64
	var browserReq uint64
	var uniqueIPs uint64
	var since uint64

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(reqStatsBucketName))
		if v := b.Get([]byte("cli")); v != nil {
			cliReq = btoi(v)
		}
		if v := b.Get([]byte("browser")); v != nil {
			browserReq = btoi(v)
		}
		if v := b.Get([]byte("unique_ips")); v != nil {
			uniqueIPs = btoi(v)
		}
		if v := b.Get([]byte("stats_since_unix")); v != nil {
			since = btoi(v)
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, 0, "", err
	}

	// Backward compatibility for old DBs before unique counter existed.
	if uniqueIPs == 0 {
		lines, _, err := loadStatsLines()
		if err == nil {
			uniqueIPs = uint64(len(lines))
		}
	}

	statsSince := "n/a"
	if since > 0 {
		statsSince = time.Unix(int64(since), 0).UTC().Format("2006-01-02 15:04 UTC")
	}

	totalReq := cliReq + browserReq
	return totalReq, int(uniqueIPs), cliReq, browserReq, statsSince, nil
}

func isCLIUserAgent(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	return strings.Contains(ua, "curl") || strings.Contains(ua, "wget") || strings.Contains(ua, "httpie")
}

func sortStatsLines(lines []ipStatLine, sortBy string) {
	switch sortBy {
	case "count_asc":
		sort.Slice(lines, func(i, j int) bool {
			if lines[i].count == lines[j].count {
				return lines[i].ip < lines[j].ip
			}
			return lines[i].count < lines[j].count
		})
	case "ip_asc":
		sort.Slice(lines, func(i, j int) bool { return lines[i].ip < lines[j].ip })
	case "ip_desc":
		sort.Slice(lines, func(i, j int) bool { return lines[i].ip > lines[j].ip })
	default:
		sort.Slice(lines, func(i, j int) bool {
			if lines[i].count == lines[j].count {
				return lines[i].ip < lines[j].ip
			}
			return lines[i].count > lines[j].count
		})
	}
}

type geoService struct {
	client  *http.Client
	mu      sync.RWMutex
	cache   map[string]geoCache
	ttl     time.Duration
	failTTL time.Duration
}

type geoCache struct {
	country string
	code    string
	city    string
	asn     string
	asnName string
	exp     time.Time
}

type ipWhoIsResponse struct {
	Success     bool   `json:"success"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	City        string `json:"city"`
	Connection  struct {
		ASN int    `json:"asn"`
		Org string `json:"org"`
	} `json:"connection"`
}

type ipAPIResponse struct {
	CountryName string `json:"country_name"`
	CountryCode string `json:"country"`
	City        string `json:"city"`
	ASN         string `json:"asn"`
	Org         string `json:"org"`
}

type geoJSResponse struct {
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	City        string `json:"city"`
	Org         string `json:"organization_name"`
}

type persistedGeoEntry struct {
	Country string `json:"country"`
	Code    string `json:"code"`
	City    string `json:"city"`
	ASN     string `json:"asn"`
	ASNName string `json:"asn_name"`
	Success bool   `json:"success"`
	SavedAt int64  `json:"saved_at"`
}

func newGeoService() *geoService {
	return &geoService{
		client:  &http.Client{Timeout: 2 * time.Second},
		cache:   make(map[string]geoCache),
		ttl:     24 * time.Hour,
		failTTL: 15 * time.Minute,
	}
}

func (g *geoService) Lookup(ip string) (string, string, string, string, string) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "", "", "", "", ""
	}

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", "", "", "", ""
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return "Local/Private", "LAN", "Local/Private", "LAN", "Local/Private"
	}

	now := time.Now()
	g.mu.RLock()
	item, ok := g.cache[ip]
	g.mu.RUnlock()
	if ok && now.Before(item.exp) {
		return item.country, item.code, item.city, item.asn, item.asnName
	}

	country, code, city, asn, asnName := g.lookupIPWhoIs(ip)
	if strings.TrimSpace(country) == "" {
		country, code, city, asn, asnName = g.lookupIPAPI(ip)
	}
	if strings.TrimSpace(country) == "" {
		country, code, city, asn, asnName = g.lookupGeoJS(ip)
	}

	country = strings.TrimSpace(country)
	code = strings.TrimSpace(code)
	city = strings.TrimSpace(city)
	asn = strings.TrimSpace(asn)
	asnName = strings.TrimSpace(asnName)
	if country == "" {
		g.mu.Lock()
		g.cache[ip] = geoCache{
			country: "",
			code:    "",
			city:    "",
			asn:     "",
			asnName: "",
			exp:     now.Add(g.failTTL),
		}
		g.mu.Unlock()
		return "", "", "", "", ""
	}

	g.mu.Lock()
	g.cache[ip] = geoCache{
		country: country,
		code:    code,
		city:    city,
		asn:     asn,
		asnName: asnName,
		exp:     now.Add(g.ttl),
	}
	g.mu.Unlock()
	return country, code, city, asn, asnName
}

func (g *geoService) lookupIPWhoIs(ip string) (string, string, string, string, string) {
	req, err := http.NewRequest(http.MethodGet, "https://ipwho.is/"+ip, nil)
	if err != nil {
		return "", "", "", "", ""
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", "", "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", "", ""
	}

	var parsed ipWhoIsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil || !parsed.Success {
		return "", "", "", "", ""
	}
	asn := ""
	if parsed.Connection.ASN > 0 {
		asn = fmt.Sprintf("AS%d", parsed.Connection.ASN)
	}
	return parsed.Country, parsed.CountryCode, parsed.City, asn, parsed.Connection.Org
}

func (g *geoService) lookupIPAPI(ip string) (string, string, string, string, string) {
	req, err := http.NewRequest(http.MethodGet, "https://ipapi.co/"+ip+"/json/", nil)
	if err != nil {
		return "", "", "", "", ""
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", "", "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", "", ""
	}

	var parsed ipAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", "", "", "", ""
	}
	return parsed.CountryName, parsed.CountryCode, parsed.City, parsed.ASN, parsed.Org
}

func (g *geoService) lookupGeoJS(ip string) (string, string, string, string, string) {
	req, err := http.NewRequest(http.MethodGet, "https://get.geojs.io/v1/ip/geo/"+ip+".json", nil)
	if err != nil {
		return "", "", "", "", ""
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", "", "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", "", ""
	}

	var parsed geoJSResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", "", "", "", ""
	}
	return parsed.Country, parsed.CountryCode, parsed.City, "", parsed.Org
}

func lookupGeoCached(ip string) (string, string, string, string, string) {
	if country, code, city, asn, asnName, ok := readGeoCache(ip); ok {
		return country, code, city, asn, asnName
	}

	country, code, city, asn, asnName := geo.Lookup(ip)
	writeGeoCache(ip, country, code, city, asn, asnName)
	return country, code, city, asn, asnName
}

func readGeoCache(ip string) (string, string, string, string, string, bool) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "", "", "", "", "", false
	}

	var entry persistedGeoEntry
	found := false
	if err := db.View(func(tx *bbolt.Tx) error {
		if e, ok := readGeoCacheFromTx(tx, ip); ok {
			entry = e
			found = true
		}
		return nil
	}); err != nil {
		return "", "", "", "", "", false
	}
	if !found {
		return "", "", "", "", "", false
	}

	age := time.Since(time.Unix(entry.SavedAt, 0))
	if entry.Success {
		if age > geoSuccessCacheTTL {
			return "", "", "", "", "", false
		}
		return entry.Country, entry.Code, entry.City, entry.ASN, entry.ASNName, true
	}
	if age > geoFailureCacheTTL {
		return "", "", "", "", "", false
	}
	return "", "", "", "", "", true
}

func writeGeoCache(ip, country, code, city, asn, asnName string) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return
	}

	entry := persistedGeoEntry{
		Country: strings.TrimSpace(country),
		Code:    strings.TrimSpace(code),
		City:    strings.TrimSpace(city),
		ASN:     strings.TrimSpace(asn),
		ASNName: strings.TrimSpace(asnName),
		Success: strings.TrimSpace(country) != "",
		SavedAt: time.Now().Unix(),
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}

	_ = db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(geoCacheBucketName))
		return b.Put([]byte(ip), raw)
	})
}

func readGeoCacheFromTx(tx *bbolt.Tx, ip string) (persistedGeoEntry, bool) {
	b := tx.Bucket([]byte(geoCacheBucketName))
	if b == nil {
		return persistedGeoEntry{}, false
	}
	raw := b.Get([]byte(ip))
	if len(raw) == 0 {
		return persistedGeoEntry{}, false
	}
	var entry persistedGeoEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return persistedGeoEntry{}, false
	}
	return entry, true
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
        body {
            margin: 0;
            background: #ffffff;
            color: #111111;
            font-family: "SF Mono", "Menlo", "Consolas", "Liberation Mono", monospace;
            padding: 28px 20px;
            line-height: 1.45;
        }

        .container {
            width: min(760px, 100%%);
            margin: 0 auto;
        }

        .label {
            font-size: 0.78rem;
            text-transform: uppercase;
            letter-spacing: 0.06em;
            color: #444444;
            margin-bottom: 8px;
        }

        .ip {
            margin: 0;
            font-size: clamp(2rem, 5.2vw, 3.1rem);
            font-weight: 700;
            letter-spacing: 0.01em;
            word-break: break-word;
        }

        .location {
            margin-top: 6px;
            font-size: 1rem;
            color: #222222;
        }

        .sep {
            margin: 18px 0 14px;
            color: #666666;
            font-size: 0.95rem;
        }

        .line {
            margin: 2px 0;
            font-size: 1rem;
            color: #111111;
        }

        @media (max-width: 640px) {
            body { padding: 18px 14px; }
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="label">Your IP Address</div>
        <h1 class="ip">%s</h1>
        <div class="location">%s, %s</div>
        <div class="sep">---</div>
        <div class="line">Total requests: %d</div>
        <div class="line">CLI requests: %d</div>
        <div class="line">Browser requests: %d</div>
        <div class="line">Stats since: %s</div>
    </div>
</body>
</html>`
