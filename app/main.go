// Command inceptionlabs2api exposes chat.inceptionlabs.ai (Mercury 3) as an
// OpenAI-compatible Chat Completions API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"inceptionlabs2api/internal/config"
	"inceptionlabs2api/internal/pool"
	"inceptionlabs2api/internal/server"
	"inceptionlabs2api/internal/upstream"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run builds the gateway and serves until SIGINT/SIGTERM. It returns an error
// instead of exiting so the wiring is testable.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
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
		APIKey:             cfg.APIKey,
		Model:              cfg.Model,
		Models:             cfg.Models,
		ContextChars:       cfg.ContextChars,
		IdleTimeout:        cfg.IdleTimeout,
		SearchTimeout:      cfg.SearchTimeout,
		SearchProbeTimeout: cfg.SearchProbeTimeout,
		Logf:               logf,
	}, up, p)
	srv.SetSearcher(up)
	if cfg.SearchConfigCheck {
		// Background and bounded: a challenged config route must never delay
		// or fail boot.
		srv.StartSearchProbe()
	}

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
			cfg.Addr, cfg.UpstreamBase, config.RedactProxy(cfg.Proxy), cfg.Models)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	logf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
