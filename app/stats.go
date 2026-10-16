package main

import (
	"encoding/binary"
	"time"

	"go.etcd.io/bbolt"
)

type requestKind string

const (
	cliRequest     requestKind = "cli"
	jsonRequest    requestKind = "json"
	browserRequest requestKind = "browser"
)

type basicStats struct {
	TotalRequests   uint64
	TodayRequests   uint64
	CLIRequests     uint64
	JSONRequests    uint64
	BrowserRequests uint64
	StatsSince      string
}

type ipStatLine struct {
	ip    string
	count uint64
}

func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func btoi(v []byte) uint64 {
	return binary.BigEndian.Uint64(v)
}

func counterValue(bucket *bbolt.Bucket, key []byte) uint64 {
	if value := bucket.Get(key); value != nil {
		return btoi(value)
	}
	return 0
}

func incrementCounter(bucket *bbolt.Bucket, key []byte) error {
	return bucket.Put(key, itob(counterValue(bucket, key)+1))
}

func dailyRequestsKey(now time.Time) []byte {
	return []byte("daily:" + now.UTC().Format("2006-01-02"))
}

func recordRequest(ip string, kind requestKind, now time.Time) error {
	return db.Update(func(tx *bbolt.Tx) error {
		ips := tx.Bucket([]byte(ipStatsBucketName))
		requests := tx.Bucket([]byte(reqStatsBucketName))
		ipKey := []byte(ip)
		if ips.Get(ipKey) == nil {
			if err := incrementCounter(requests, []byte("unique_ips")); err != nil {
				return err
			}
		}
		if err := incrementCounter(ips, ipKey); err != nil {
			return err
		}
		if err := incrementCounter(requests, []byte(kind)); err != nil {
			return err
		}
		if err := incrementCounter(requests, dailyRequestsKey(now)); err != nil {
			return err
		}
		if requests.Get([]byte("stats_since_unix")) == nil {
			return requests.Put([]byte("stats_since_unix"), itob(uint64(now.Unix())))
		}
		return nil
	})
}

func readBasicStats() (basicStats, error) {
	return readBasicStatsAt(time.Now())
}

func readBasicStatsAt(now time.Time) (basicStats, error) {
	stats := basicStats{StatsSince: "n/a"}
	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(reqStatsBucketName))
		stats.CLIRequests = counterValue(b, []byte(cliRequest))
		stats.JSONRequests = counterValue(b, []byte(jsonRequest))
		stats.BrowserRequests = counterValue(b, []byte(browserRequest))
		stats.TodayRequests = counterValue(b, dailyRequestsKey(now))
		stats.TotalRequests = stats.CLIRequests + stats.JSONRequests + stats.BrowserRequests
		if since := counterValue(b, []byte("stats_since_unix")); since > 0 {
			stats.StatsSince = time.Unix(int64(since), 0).UTC().Format("2006-01-02 15:04 UTC")
		}
		return nil
	})
	return stats, err
}

func loadStatsLines() ([]ipStatLine, error) {
	lines := make([]ipStatLine, 0, 256)
	err := db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(ipStatsBucketName)).ForEach(func(k, v []byte) error {
			lines = append(lines, ipStatLine{ip: string(k), count: btoi(v)})
			return nil
		})
	})
	return lines, err
}
