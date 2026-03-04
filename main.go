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
	geoSuccessCacheTTL = 30 * 24 * time.Hour
	geoFailureCacheTTL = 10 * time.Minute
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
	IP          string `json:"ip"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	City        string `json:"city"`
	Count       uint64 `json:"count"`
}

type statsHTMLPageData struct {
	TotalRequests uint64
	TotalIPs      int
	Page          int
	PageSize      int
	TotalPages    int
	Sort          string
	Items         []statsItem
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
		_, err := tx.CreateBucketIfNotExists([]byte(geoCacheBucketName))
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
		ua := strings.ToLower(r.Header.Get("User-Agent"))
		isCLI := strings.Contains(ua, "curl") || strings.Contains(ua, "wget") || strings.Contains(ua, "httpie")

		if err := db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket([]byte(ipStatsBucketName))
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

		if isCLI {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "%s\n", ip)
		} else {
			country, code, city := lookupGeoCached(ip)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, htmlTemplate, ip, fallback(country, "unknown"), fallback(code, "--"), fallback(city, "unknown"))
		}
	})

	log.Println("Running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
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

	items := make([]statsItem, 0, end-start)
	for _, line := range lines[start:end] {
		country, code, city, ok := readGeoCache(line.ip)
		if !ok {
			country, code, city = "", "", ""
		}
		items = append(items, statsItem{
			IP:          line.ip,
			Country:     fallback(country, "unknown"),
			CountryCode: fallback(code, "--"),
			City:        fallback(city, "unknown"),
			Count:       line.count,
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

	data := statsHTMLPageData{
		TotalRequests: total,
		TotalIPs:      len(lines),
		Page:          page,
		PageSize:      pageSize,
		TotalPages:    totalPages,
		Sort:          sortBy,
		Items:         items,
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

func parsePageParams(r *http.Request) (int, int) {
	page := 1
	pageSize := 200

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
	if pageSize > 1000 {
		pageSize = 1000
	}
	return page, pageSize
}

func parseSortParam(r *http.Request) string {
	sortBy := strings.TrimSpace(r.URL.Query().Get("sort"))
	switch sortBy {
	case "count_asc", "count_desc", "ip_asc", "ip_desc":
		return sortBy
	default:
		return "count_desc"
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
		return 1, 0, 0, 1
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
	exp     time.Time
}

type ipWhoIsResponse struct {
	Success     bool   `json:"success"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	City        string `json:"city"`
}

type ipAPIResponse struct {
	CountryName string `json:"country_name"`
	CountryCode string `json:"country"`
	City        string `json:"city"`
}

type geoJSResponse struct {
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	City        string `json:"city"`
}

type persistedGeoEntry struct {
	Country string `json:"country"`
	Code    string `json:"code"`
	City    string `json:"city"`
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

func (g *geoService) Lookup(ip string) (string, string, string) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "", "", ""
	}

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", "", ""
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return "Local/Private", "LAN", "Local/Private"
	}

	now := time.Now()
	g.mu.RLock()
	item, ok := g.cache[ip]
	g.mu.RUnlock()
	if ok && now.Before(item.exp) {
		return item.country, item.code, item.city
	}

	country, code, city := g.lookupIPWhoIs(ip)
	if strings.TrimSpace(country) == "" {
		country, code, city = g.lookupIPAPI(ip)
	}
	if strings.TrimSpace(country) == "" {
		country, code, city = g.lookupGeoJS(ip)
	}

	country = strings.TrimSpace(country)
	code = strings.TrimSpace(code)
	city = strings.TrimSpace(city)
	if country == "" {
		g.mu.Lock()
		g.cache[ip] = geoCache{
			country: "",
			code:    "",
			city:    "",
			exp:     now.Add(g.failTTL),
		}
		g.mu.Unlock()
		return "", "", ""
	}

	g.mu.Lock()
	g.cache[ip] = geoCache{
		country: country,
		code:    code,
		city:    city,
		exp:     now.Add(g.ttl),
	}
	g.mu.Unlock()
	return country, code, city
}

func (g *geoService) lookupIPWhoIs(ip string) (string, string, string) {
	req, err := http.NewRequest(http.MethodGet, "https://ipwho.is/"+ip, nil)
	if err != nil {
		return "", "", ""
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", ""
	}

	var parsed ipWhoIsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil || !parsed.Success {
		return "", "", ""
	}
	return parsed.Country, parsed.CountryCode, parsed.City
}

func (g *geoService) lookupIPAPI(ip string) (string, string, string) {
	req, err := http.NewRequest(http.MethodGet, "https://ipapi.co/"+ip+"/json/", nil)
	if err != nil {
		return "", "", ""
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", ""
	}

	var parsed ipAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", "", ""
	}
	return parsed.CountryName, parsed.CountryCode, parsed.City
}

func (g *geoService) lookupGeoJS(ip string) (string, string, string) {
	req, err := http.NewRequest(http.MethodGet, "https://get.geojs.io/v1/ip/geo/"+ip+".json", nil)
	if err != nil {
		return "", "", ""
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", ""
	}

	var parsed geoJSResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", "", ""
	}
	return parsed.Country, parsed.CountryCode, parsed.City
}

func lookupGeoCached(ip string) (string, string, string) {
	if country, code, city, ok := readGeoCache(ip); ok {
		return country, code, city
	}

	country, code, city := geo.Lookup(ip)
	writeGeoCache(ip, country, code, city)
	return country, code, city
}

func readGeoCache(ip string) (string, string, string, bool) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "", "", "", false
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
		return "", "", "", false
	}
	if !found {
		return "", "", "", false
	}

	age := time.Since(time.Unix(entry.SavedAt, 0))
	if entry.Success {
		if age > geoSuccessCacheTTL {
			return "", "", "", false
		}
		return entry.Country, entry.Code, entry.City, true
	}
	if age > geoFailureCacheTTL {
		return "", "", "", false
	}
	return "", "", "", true
}

func writeGeoCache(ip, country, code, city string) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return
	}

	entry := persistedGeoEntry{
		Country: strings.TrimSpace(country),
		Code:    strings.TrimSpace(code),
		City:    strings.TrimSpace(city),
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
        <div class="badge">Page: {{.Page}} / {{.TotalPages}}</div>
        <div class="badge">Page size: {{.PageSize}}</div>
        <div class="badge">Sort: {{.Sort}}</div>
    </div>
    <div class="actions">
        <a href="/">Back to IP page</a>
    </div>
    <table>
        <thead>
            <tr>
                <th>IP</th>
                <th>Country</th>
                <th>Code</th>
                <th>City</th>
                <th>Count</th>
            </tr>
        </thead>
        <tbody>
            {{range .Items}}
            <tr>
                <td>{{.IP}}</td>
                <td>{{.Country}}</td>
                <td>{{.CountryCode}}</td>
                <td>{{.City}}</td>
                <td>{{.Count}}</td>
            </tr>
            {{end}}
        </tbody>
    </table>
    <div class="pager">
        <a href="{{.PrevURL}}">Prev</a>
        <a href="{{.NextURL}}">Next</a>
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
        .country {font-size: 1.1rem; color: #666;}
        .city {font-size: 1rem; color: #777; margin-top: 4px;}
    </style>
</head>
<body>
    <div class="container">
        <h1>Your IP address</h1>
        <div class="ip">%s</div>
        <div class="country">%s (%s)</div>
        <div class="city">City: %s</div>
    </div>
</body>
</html>`
