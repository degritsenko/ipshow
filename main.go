package main

import (
	"log"
	"net/http"
	"time"

	"go.etcd.io/bbolt"
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
)

func main() {
	var err error
	db, err = bbolt.Open("data/stats.db", 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range []string{ipStatsBucketName, geoCacheBucketName, reqStatsBucketName} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", handleRequest)
	startGeoRefreshWorker()

	log.Println("Running on :8080")
	log.Fatal(newHTTPServer().ListenAndServe())
}

func newHTTPServer() *http.Server {
	return &http.Server{
		Addr:              ":8080",
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}
