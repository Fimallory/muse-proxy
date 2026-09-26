// Package main implements muse-proxy: a byte-transparent forward proxy
// for the OpenAI Responses API in front of OpenCode Zen.
//
// Design rules:
//  1. Only the Responses protocol is served (POST /v1/responses),
//     plus a models list (GET /v1/models) and a health check.
//  2. Request bodies are forwarded byte-for-byte. Nothing is re-encoded,
//     so images, reasoning knobs and tool definitions always survive.
//  3. Session/project continuity is derived from a hash of the first
//     10k characters of the request text. Only hashes are persisted,
//     never content. Entries expire after 3 days.
//  4. No ports are published; the container lives on the axonhub network.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfgPath := flag.String("config", "config.json", "path to JSON config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	store, err := openStore(cfg.HashStorePath, cfg.HashTTL, cfg.HashMaxEntries)
	if err != nil {
		log.Fatalf("open hash store: %v", err)
	}

	cat := openCatalog(cfg.CatalogPath, cfg.CatalogURL, cfg.CatalogRefresh)

	srv, err := newServer(cfg, store, cat)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.handleHealth)
	mux.HandleFunc("GET /v1/models", srv.withAuth(srv.handleModels))
	mux.HandleFunc("POST /v1/responses", srv.withAuth(srv.handleResponses))
	mux.HandleFunc("POST /v1/chat/completions", srv.withAuth(srv.handleChat))
	mux.HandleFunc("POST /v1/messages", srv.withAuth(srv.handleMessages))

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()
	cat.start(appCtx)

	go func() {
		log.Printf("muse-proxy %s listening on %s (upstream %s, %d proxie(s))",
			version, cfg.Listen, cfg.Upstream, len(cfg.Proxies))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down")
	srv.close()
	store.close()
	cat.close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
