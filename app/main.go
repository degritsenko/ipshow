package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.etcd.io/bbolt"
)

var db *bbolt.DB
var geo = newGeoService()

const (
	geoCacheBucketName = "GeoCache"
	reqStatsBucketName = "RequestStats"

	geoSuccessCacheTTL = 14 * 24 * time.Hour
	geoFailureCacheTTL = 60 * time.Minute
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func openDatabase(path string) (*bbolt.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	storage, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	if err := storage.Update(func(tx *bbolt.Tx) error {
		for _, name := range []string{geoCacheBucketName, reqStatsBucketName} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		// Remove obsolete detailed statistics without resetting the active counters.
		for _, name := range []string{"IPStats", "CountryAgg", "CityAgg", "IPAggAccounted"} {
			if tx.Bucket([]byte(name)) != nil {
				if err := tx.DeleteBucket([]byte(name)); err != nil {
					return err
				}
			}
		}
		return tx.Bucket([]byte(reqStatsBucketName)).Delete([]byte("unique_ips"))
	}); err != nil {
		storage.Close()
		return nil, err
	}
	return storage, nil
}

func run(ctx context.Context) error {
	storage, err := openDatabase("data/stats.db")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	db = storage
	defer storage.Close()

	server := newHTTPServer()
	serverDone := make(chan error, 1)
	log.Println("Running on :8080")
	go func() { serverDone <- server.ListenAndServe() }()
	select {
	case err := <-serverDone:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			server.Close()
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		return nil
	}
}

func newHTTPServer() *http.Server {
	return &http.Server{
		Addr:              ":8080",
		Handler:           http.HandlerFunc(handleRequest),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}
