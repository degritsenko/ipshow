package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go.etcd.io/bbolt"
	"html/template"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
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

	defaultPage     = 1
	defaultPageSize = 200
	maxPageSize     = 1000
	defaultSort     = "count_desc"
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

type statsItem struct {
	IP      string `json:"ip"`
	Country string `json:"country"`
	City    string `json:"city"`
	ASN     string `json:"asn"`
	ASName  string `json:"as_name"`
	Count   uint64 `json:"count"`
}

type statsAggItem struct {
	Name  string
	Count uint64
}

type statsHTMLPageData struct {
	TotalRequests uint64
	TotalIPs      int
	CLIRequests   uint64
	BrowserReqs   uint64
	StatsSince    string
	Page          int
	PageSize      int
	TotalPages    int
	Sort          string
	Items         []statsItem
	TopCountries  []statsAggItem
	TopCities     []statsAggItem
	HasPrev       bool
	HasNext       bool
	PrevURL       string
	NextURL       string
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
		_, err := tx.CreateBucketIfNotExists([]byte(reqStatsBucketName))
		return err
	}); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stats" {
			showStatsHTML(w, r)
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
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, htmlTemplate, ip, fallback(country, "unknown"), fallback(city, "unknown"))
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
		_, _, _, _, _ = lookupGeoCached(lines[i].ip)
		updated++
		time.Sleep(geoRefreshPause)
	}
	log.Printf("geo refresh: processed %d IPs", updated)
}

func showStatsHTML(w http.ResponseWriter, r *http.Request) {
	page, pageSize := parsePageParams(r)
	sortBy := parseSortParam(r)

	lines, total, err := loadStatsLines()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("db view error: %v", err)
		return
	}

	sortStatsLines(lines, sortBy)
	page, start, end, totalPages := pageWindow(len(lines), page, pageSize)

	countryAgg := make(map[string]uint64)
	cityAgg := make(map[string]uint64)
	items := make([]statsItem, 0, end-start)
	for idx, line := range lines {
		country, _, city, asn, asnName, ok := readGeoCache(line.ip)
		if !ok {
			country, city, asn, asnName = "", "", "", ""
		}
		if c := strings.TrimSpace(country); c != "" {
			countryAgg[c] += line.count
		}
		if c := strings.TrimSpace(city); c != "" {
			cityAgg[c] += line.count
		}

		if idx < start || idx >= end {
			continue
		}
		items = append(items, statsItem{
			IP:      line.ip,
			Country: fallback(country, "unknown"),
			City:    fallback(city, "unknown"),
			ASN:     fallback(asn, "--"),
			ASName:  fallback(asnName, "unknown"),
			Count:   line.count,
		})
	}

	prevPage := page - 1
	if prevPage < 1 {
		prevPage = 1
	}
	nextPage := page + 1
	if nextPage > totalPages {
		nextPage = totalPages
	}
	hasPrev := page > 1
	hasNext := page < totalPages

	cliReq, browserReq, err := readRequestSourceCounts()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("db req stats error: %v", err)
		return
	}
	statsSince, err := readStatsSince()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("db stats since error: %v", err)
		return
	}

	data := statsHTMLPageData{
		TotalRequests: total,
		TotalIPs:      len(lines),
		CLIRequests:   cliReq,
		BrowserReqs:   browserReq,
		StatsSince:    statsSince,
		Page:          page,
		PageSize:      pageSize,
		TotalPages:    totalPages,
		Sort:          sortBy,
		Items:         items,
		TopCountries:  topAggItems(countryAgg, 10),
		TopCities:     topAggItems(cityAgg, 10),
		HasPrev:       hasPrev,
		HasNext:       hasNext,
		PrevURL:       buildStatsURL("/stats", prevPage, pageSize, sortBy),
		NextURL:       buildStatsURL("/stats", nextPage, pageSize, sortBy),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statsHTMLTemplate.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		log.Printf("stats template error: %v", err)
	}
}

func buildStatsURL(path string, page, pageSize int, sortBy string) string {
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(pageSize))
	q.Set("sort", sortBy)
	return path + "?" + q.Encode()
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
		if err := b.Put([]byte(ip), itob(count+1)); err != nil {
			return err
		}

		reqBucket := tx.Bucket([]byte(reqStatsBucketName))
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

func topAggItems(m map[string]uint64, limit int) []statsAggItem {
	if len(m) == 0 || limit <= 0 {
		return nil
	}
	out := make([]statsAggItem, 0, len(m))
	for k, v := range m {
		out = append(out, statsAggItem{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Name < out[j].Name
		}
		return out[i].Count > out[j].Count
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func isCLIUserAgent(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	return strings.Contains(ua, "curl") || strings.Contains(ua, "wget") || strings.Contains(ua, "httpie")
}

func parsePageParams(r *http.Request) (int, int) {
	page := defaultPage
	pageSize := defaultPageSize

	if raw := strings.TrimSpace(r.URL.Query().Get("page")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			page = v
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("page_size")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			pageSize = v
		}
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	return page, pageSize
}

func parseSortParam(r *http.Request) string {
	sortBy := strings.TrimSpace(r.URL.Query().Get("sort"))
	switch sortBy {
	case "count_asc", "count_desc", "ip_asc", "ip_desc":
		return sortBy
	default:
		return defaultSort
	}
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

func pageWindow(totalItems, page, pageSize int) (int, int, int, int) {
	if totalItems == 0 {
		return defaultPage, 0, 0, 1
	}

	totalPages := int(math.Ceil(float64(totalItems) / float64(pageSize)))
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * pageSize
	end := start + pageSize
	if end > totalItems {
		end = totalItems
	}
	return page, start, end, totalPages
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
		b := tx.Bucket([]byte(geoCacheBucketName))
		raw := b.Get([]byte(ip))
		if len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil
		}
		found = true
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

var statsHTMLTemplate = template.Must(template.New("stats").Parse(`
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Stats</title>
    <style>
        body {font-family: -apple-system, system-ui, sans-serif; margin: 24px; color: #222; background: #fafafa;}
        .top {display: flex; gap: 16px; flex-wrap: wrap; align-items: center; margin-bottom: 14px;}
        .badge {padding: 8px 10px; border: 1px solid #ddd; background: #fff; border-radius: 8px;}
        .actions a {margin-right: 12px;}
        table {width: 100%; border-collapse: collapse; background: #fff;}
        th, td {padding: 8px 10px; border-bottom: 1px solid #eee; text-align: left; font-size: 14px;}
        th {background: #f3f3f3;}
        .pager {margin-top: 12px;}
        .pager a {margin-right: 12px;}
    </style>
</head>
<body>
    <h1>IP Stats</h1>
    <div class="top">
        <div class="badge">Total requests: {{.TotalRequests}}</div>
        <div class="badge">Total IPs: {{.TotalIPs}}</div>
        <div class="badge">CLI requests: {{.CLIRequests}}</div>
        <div class="badge">Browser requests: {{.BrowserReqs}}</div>
        <div class="badge">Stats since: {{.StatsSince}}</div>
        <div class="badge">Page: {{.Page}} / {{.TotalPages}}</div>
        <div class="badge">Page size: {{.PageSize}}</div>
    </div>
    <div class="actions">
        <a href="/">Back to IP page</a>
    </div>
    <table>
        <thead>
            <tr>
                <th>IP</th>
                <th>Country</th>
                <th>City</th>
                <th>ASN</th>
                <th>AS Name</th>
                <th>Count</th>
            </tr>
        </thead>
        <tbody>
            {{range .Items}}
            <tr>
                <td>{{.IP}}</td>
                <td>{{.Country}}</td>
                <td>{{.City}}</td>
                <td>{{.ASN}}</td>
                <td>{{.ASName}}</td>
                <td>{{.Count}}</td>
            </tr>
            {{end}}
        </tbody>
    </table>
    <div style="display:flex; gap:24px; margin-top:18px; flex-wrap:wrap;">
        <div>
            <h3>Top Countries</h3>
            <table style="width:320px;">
                <thead><tr><th>Country</th><th>Requests</th></tr></thead>
                <tbody>
                    {{range .TopCountries}}
                    <tr><td>{{.Name}}</td><td>{{.Count}}</td></tr>
                    {{end}}
                </tbody>
            </table>
        </div>
        <div>
            <h3>Top Cities</h3>
            <table style="width:320px;">
                <thead><tr><th>City</th><th>Requests</th></tr></thead>
                <tbody>
                    {{range .TopCities}}
                    <tr><td>{{.Name}}</td><td>{{.Count}}</td></tr>
                    {{end}}
                </tbody>
            </table>
        </div>
    </div>
    <div class="pager">
        {{if .HasPrev}}<a href="{{.PrevURL}}">Prev</a>{{end}}
        {{if .HasNext}}<a href="{{.NextURL}}">Next</a>{{end}}
    </div>
</body>
</html>
`))

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
        .location {font-size: 1.1rem; color: #666;}
    </style>
</head>
<body>
    <div class="container">
        <h1>Your IP address</h1>
        <div class="ip">%s</div>
        <div class="location">%s, %s</div>
    </div>
</body>
</html>`
