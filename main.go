package main

import (
        "encoding/binary"
        "fmt"
        "go.etcd.io/bbolt"
        "log"
        "net/http"
        "strings"
)

var db *bbolt.DB

func itob(v uint64) []byte {
        b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b
}
func btoi(v []byte) uint64 {
        return binary.BigEndian.Uint64(v)
}

func main() {
        var err error
        db, err = bbolt.Open("data/stats.db", 0600, nil)
        if err != nil { log.Fatal(err) }
        defer db.Close()

        db.Update(func(tx *bbolt.Tx) error {
                _, err := tx.CreateBucketIfNotExists([]byte("IPStats"))
                return err
        })

        http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
                if r.URL.Path == "/stats" {
                        showStats(w)
                        return
                }

                // 1. Получаем IP
                ip := r.Header.Get("X-Forwarded-For")
                if ip == "" { ip = r.RemoteAddr }
                // Очищаем IP от порта, если он есть
                if idx := strings.LastIndex(ip, ":"); idx != -1 { ip = ip[:idx] }

                // 2. Пишем в статистику
                db.Update(func(tx *bbolt.Tx) error {
                        b := tx.Bucket([]byte("IPStats"))
                        count := uint64(0)
                        if v := b.Get([]byte(ip)); v != nil { count = btoi(v) }
                        return b.Put([]byte(ip), itob(count+1))
                })

                // 3. Определяем формат ответа (CLI или Браузер)
                ua := strings.ToLower(r.Header.Get("User-Agent"))
                isCLI := strings.Contains(ua, "curl") || strings.Contains(ua, "wget") || strings.Contains(ua, "httpie")

                if isCLI {
                        w.Header().Set("Content-Type", "text/plain")
                        fmt.Fprint(w, ip)
                } else {
                        w.Header().Set("Content-Type", "text/html")
                        fmt.Fprintf(w, htmlTemplate, ip)
                }
        })

        log.Println("Running on :8080")
        log.Fatal(http.ListenAndServe(":8080", nil))
}

func showStats(w http.ResponseWriter) {
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        db.View(func(tx *bbolt.Tx) error {
                b := tx.Bucket([]byte("IPStats"))
                return b.ForEach(func(k, v []byte) error {
                        fmt.Fprintf(w, "IP: %s | Count: %d\n", k, btoi(v))
                        return nil
                })
        })
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
        .ip {font-size: clamp(1.5rem, 8vw, 2.5rem); font-weight: bold; color: #222;}
    </style>
</head>
<body>
    <div class="container">
        <h1>Your IP address</h1>
        <div class="ip">%s</div>
    </div>
</body>
</html>`