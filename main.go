package main

import (
	_ "embed"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

//go:embed web/index.html
var homePage string

func main() {
	addr := os.Getenv("CONTEXTSHARE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, homePage)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{\"status\":\"ok\",\"service\":\"contextshare\"}\n")
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("ContextShare listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}
