package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestOpenDatabasePreservesStatistics(t *testing.T) {
	previous := db
	t.Cleanup(func() { db = previous })
	path := filepath.Join(t.TempDir(), "nested", "stats.db")
	storage, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	db = storage
	now := time.Now()
	if err := recordRequest(jsonRequest, now); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	db = storage
	stats, err := readBasicStatsAt(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRequests != 1 || stats.TodayRequests != 1 || stats.JSONRequests != 1 {
		t.Fatal(stats)
	}
}

func TestOpenDatabaseLockHasTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	storage, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	second, err := openDatabase(path)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, bbolt.ErrTimeout) {
		t.Fatalf("expected lock timeout, got %v", err)
	}
}

func TestOpenDatabaseRemovesOnlyObsoleteStatistics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	storage, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	since := time.Date(2026, 3, 4, 21, 46, 0, 0, time.UTC)
	cache := []byte(`{"country":"United States","code":"US","city":"Mountain View","success":true,"saved_at":1792411200}`)
	if err := storage.Update(func(tx *bbolt.Tx) error {
		for _, name := range []string{"IPStats", "CountryAgg", "CityAgg", "IPAggAccounted", reqStatsBucketName, geoCacheBucketName} {
			if _, err := tx.CreateBucket([]byte(name)); err != nil {
				return err
			}
		}
		if err := tx.Bucket([]byte("IPStats")).Put([]byte("8.8.8.8"), itob(35)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(geoCacheBucketName)).Put([]byte("8.8.8.8"), cache); err != nil {
			return err
		}
		r := tx.Bucket([]byte(reqStatsBucketName))
		for key, count := range map[string]uint64{"cli": 10, "json": 5, "browser": 20, "unique_ips": 1, "stats_since_unix": uint64(since.Unix()), string(dailyRequestsKey(now)): 8} {
			if err := r.Put([]byte(key), itob(count)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	previous := db
	db = storage
	defer func() { db = previous }()
	if err := recordRequest(cliRequest, now); err != nil {
		t.Fatal(err)
	}
	stats, err := readBasicStatsAt(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRequests != 36 || stats.CLIRequests != 11 || stats.JSONRequests != 5 || stats.BrowserRequests != 20 || stats.TodayRequests != 9 || stats.StatsSince != "2026-03-04 21:46 UTC" {
		t.Fatalf("active statistics changed during cleanup: %+v", stats)
	}
	if err := storage.View(func(tx *bbolt.Tx) error {
		for _, name := range []string{"IPStats", "CountryAgg", "CityAgg", "IPAggAccounted"} {
			if tx.Bucket([]byte(name)) != nil {
				t.Errorf("obsolete bucket %s still present", name)
			}
		}
		if tx.Bucket([]byte(reqStatsBucketName)).Get([]byte("unique_ips")) != nil {
			t.Error("obsolete unique counter still present")
		}
		if !bytes.Equal(tx.Bucket([]byte(geoCacheBucketName)).Get([]byte("8.8.8.8")), cache) {
			t.Error("geocache changed during cleanup")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
