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

	http.HandleFunc("/", handleRequest)
	startGeoRefreshWorker()

	log.Println("Running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/json" && r.URL.Path != "/ip" {
		http.NotFound(w, r)
		return
	}

	ip := clientIP(r)
	isCLI := r.URL.Path == "/ip" || isCLIUserAgent(r.Header.Get("User-Agent"))
	if err := incrementIPStat(ip, isCLI, r.URL.Path == "/json"); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		log.Printf("db update error: %v", err)
		return
	}

	if r.URL.Path == "/json" {
		serveJSON(w, ip)
		return
	}

	if isCLI {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s\n", ip)
	} else {
		country, _, city, _, _ := lookupGeoCached(ip)
		totalReq, _, cliReq, jsonReq, browserReq, statsSince, err := readBasicStats()
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
			jsonReq,
			browserReq,
			statsSince,
		)
	}
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

func incrementIPStat(ip string, isCLI, isJSON bool) error {
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
		if isJSON {
			key = []byte("json")
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

func readBasicStats() (uint64, int, uint64, uint64, uint64, string, error) {
	var cliReq uint64
	var jsonReq uint64
	var browserReq uint64
	var uniqueIPs uint64
	var since uint64

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(reqStatsBucketName))
		if v := b.Get([]byte("cli")); v != nil {
			cliReq = btoi(v)
		}
		if v := b.Get([]byte("json")); v != nil {
			jsonReq = btoi(v)
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
		return 0, 0, 0, 0, 0, "", err
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

	totalReq := cliReq + jsonReq + browserReq
	return totalReq, int(uniqueIPs), cliReq, jsonReq, browserReq, statsSince, nil
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

        .footer {
            margin-top: 18px;
            padding-top: 10px;
            border-top: 1px solid #d4d4d4;
            font-size: 1rem;
        }

        .usage {
            margin: 0 0 12px;
            font-size: 0.85rem;
            color: #444444;
        }

        .footer a {
            color: #111111;
            text-decoration: underline;
            text-underline-offset: 3px;
        }

        .footer a:hover {
            color: #555555;
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
        <div class="line">JSON requests: %d</div>
        <div class="line">Browser requests: %d</div>
        <div class="line">Stats since: %s</div>
        <div class="footer">
            <p class="usage">Plain text at <a href="/ip">/ip</a> &amp; JSON at <a href="/json">/json</a>. Use with curl, wget or HTTPie.</p>
            <a href="https://github.com/degritsenko/ipshow" target="_blank" rel="noopener noreferrer">GitHub repo</a>
        </div>
    </div>
</body>
</html>`
