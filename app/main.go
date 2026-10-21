// Command inceptionlabs2api exposes chat.inceptionlabs.ai (Mercury 3) as an
// OpenAI-compatible Chat Completions API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"inceptionlabs2api/internal/config"
	"inceptionlabs2api/internal/pool"
	"inceptionlabs2api/internal/server"
	"inceptionlabs2api/internal/upstream"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	logf := log.Printf

	up, fetch := upstream.NewClient(upstream.Config{
		BaseURL:        cfg.UpstreamBase,
		Proxy:          cfg.Proxy,
		SessionTimeout: cfg.SessionTimeout,
		Logf:           logf,
	})

	p := pool.New(pool.Config{
		Min:      cfg.PoolMin,
		Max:      cfg.PoolMax,
		TTL:      cfg.PoolTTL,
		Interval: 15 * time.Second,
		OnFetchError: func(err error) {
			logf("pool: refill failed (serving cached tokens if any): %v", err)
		},
	}, fetch)
	defer p.Close()
	up.BindPool(p)

	srv := server.New(server.Config{
		APIKey:       cfg.APIKey,
		Model:        cfg.Model,
		Models:       cfg.Models,
		ContextChars: cfg.ContextChars,
		IdleTimeout:  cfg.IdleTimeout,
		Logf:         logf,
	}, up, p)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logf("inceptionlabs2api listening on %s (upstream=%s proxy=%q models=%v)",
			cfg.Addr, cfg.UpstreamBase, cfg.Proxy, cfg.Models)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	logf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logf("shutdown: %v", err)
		os.Exit(1)
	}
}
