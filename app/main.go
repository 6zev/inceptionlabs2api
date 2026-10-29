// Command inceptionlabs2api exposes chat.inceptionlabs.ai (Mercury 3) as an
// OpenAI-compatible Chat Completions API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"inceptionlabs2api/internal/config"
	"inceptionlabs2api/internal/pool"
	"inceptionlabs2api/internal/selfupdate"
	"inceptionlabs2api/internal/server"
	"inceptionlabs2api/internal/upstream"
)

// Build metadata, injected at release time with -ldflags "-X main.version=...".
// They default to developer values so an ordinary `go build` still works.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run builds the gateway and serves until SIGINT/SIGTERM. It returns an error
// instead of exiting so the wiring is testable.
func run() error {
	logf := log.Printf

	// Self-update runs INSTEAD of serving: `inceptionlabs2api -update` fetches
	// the newest release, verifies its checksum, replaces this executable and
	// execs it. It happens before the app config is even parsed, so a broken
	// gateway setting (an unreachable proxy, a bad IL2API_* value — none of
	// which the updater uses) can never block an update, and a failed update
	// leaves nothing half-wired. On success the exec'd child serves normally.
	if updateMode() {
		if err := runSelfUpdate(logf); err != nil {
			return err
		}
		// Only reachable when this build is already the target release: the
		// update is a no-op and the update process exits instead of serving.
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Install the soft heap ceiling before anything allocates. Without one,
	// Go lets the heap grow to 2x the live set with no upper bound, so a
	// long-running gateway that saw a burst (or one oversized response) keeps
	// the pages it grew; the limit makes it collect instead. soft: an
	// over-limit allocation slows down, it does not fail.
	if limit := config.ApplyMemoryLimit(cfg.MemoryLimit); limit > 0 {
		logf("config: soft heap limit %d MiB (IL2API_MEMORY_LIMIT=0 removes it)", limit>>20)
	}

	up, fetch := upstream.NewClient(upstream.Config{
		BaseURL:           cfg.UpstreamBase,
		Proxy:             cfg.Proxy,
		SessionTimeout:    cfg.SessionTimeout,
		SessionPerRequest: cfg.SessionPerRequest,
		ClearanceRefresh:  cfg.ClearanceRefresh,
		ClearanceCooldown: cfg.ClearanceCooldown,
		ClearancePool:     cfg.ClearancePool,
		Logf:              logf,
	})

	// With a fresh session per request the pool's prewarm would only mint
	// tokens nobody draws from, so it stays empty (Max still bounds any
	// incidental use by other callers).
	poolMin := cfg.PoolMin
	if cfg.SessionPerRequest {
		poolMin = 0
	}
	p := pool.New(pool.Config{
		Min:      poolMin,
		Max:      cfg.PoolMax,
		TTL:      cfg.PoolTTL,
		Interval: 15 * time.Second,
		OnFetchError: func(err error) {
			logf("pool: refill failed (serving cached tokens if any): %v", err)
		},
	}, fetch)
	defer p.Close()
	up.BindPool(p)

	// Fill the clearance ring's first slot at boot (~0.1-0.4 s, so the first
	// chat request does not pay the PoW + round trip) and start the one
	// background loop that fills the rest, refills retired slots and refreshes
	// stale ones (SPEC §2.1.1, §2.1.2). A boot failure is logged, never fatal:
	// the service starts and the loop retries. The pool then fills behind it
	// (it needs clearance to fetch a token), and the search probe runs last
	// (it needs a pooled token).
	bootCtx, stopBoot := context.WithCancel(context.Background())
	defer stopBoot()
	up.StartClearancePrewarm(bootCtx)

	srv := server.New(server.Config{
		APIKey:             cfg.APIKey,
		Model:              cfg.Model,
		Models:             cfg.Models,
		IdleTimeout:        cfg.IdleTimeout,
		MaxMessages:        cfg.MaxMessages,
		EmptyRetries:       cfg.EmptyRetries,
		SearchTimeout:      cfg.SearchTimeout,
		SearchProbeTimeout: cfg.SearchProbeTimeout,
		AudioTimeout:       cfg.AudioTimeout,
		AudioMaxBytes:      cfg.AudioMaxBytes,
		DecisionTimeout:    cfg.DecisionTimeout,
		PreambleStyle:      cfg.PreambleStyle,
		NativeWebSearch:    cfg.NativeWebSearch,
		Logf:               logf,
	}, up, p)
	srv.SetSearcher(up)
	srv.SetSpeech(up)
	srv.SetDecider(up)
	srv.SetClearance(up.Clearance())
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
		proxyDesc := "none"
		if cfg.Proxy != "" {
			proxyDesc = fmt.Sprintf("%q via %s", config.RedactProxy(cfg.Proxy), cfg.ProxySource)
		}
		logf("inceptionlabs2api %s (commit %s, built %s) listening on %s (upstream=%s proxy=%s models=%v session_per_request=%v)",
			version, commit, date, cfg.Addr, cfg.UpstreamBase, proxyDesc, cfg.Models, cfg.SessionPerRequest)
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

// updateMode reports whether the process was started to self-update.
//
// The command line is parsed here, not in the config package: an update is an
// action, not a setting, and it must be decidable before config.Load (a broken
// gateway setting must never block an update).
//
// Accepted forms:
//
//	inceptionlabs2api update        (subcommand)
//	inceptionlabs2api --update      (long flag)
//	inceptionlabs2api -update       (accepted alias, kept so older cron lines work)
//	IL2API_UPDATE=1                 (env, for cron/systemd one-liners)
//
// "-serve" (what the exec'd child gets) always wins: it must SERVE even when
// IL2API_UPDATE=1 is still in the environment that started the update, or the
// new image would re-enter update mode and loop.
func updateMode() bool {
	args := os.Args[1:]
	for _, a := range args {
		if a == "-serve" || a == "--serve" {
			return false
		}
	}
	for _, a := range args {
		switch a {
		case "-update", "--update", "update":
			return true
		}
	}
	if v := strings.TrimSpace(strings.ToLower(os.Getenv("IL2API_UPDATE"))); v == "1" || v == "true" || v == "on" {
		return true
	}
	// A bare unknown word is a typo, not a request to serve something else:
	// `./inceptionlabs2api updte` must not silently start a gateway that looks
	// healthy while the operator believes it updated.
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return true
		}
	}
	return false
}

// serveArgv is the argument list handed to the exec'd image: it always carries
// -serve, so the new binary serves instead of trying to update itself again.
func serveArgv() []string { return []string{"-serve"} }

// runSelfUpdate performs the update. On success it execs the new binary, so the
// gateway comes back up in the same PID and serves normally; the function only
// returns when this build is already the newest release (a deliberate no-op).
func runSelfUpdate(logf func(format string, args ...any)) error {
	opts := selfupdate.Options{
		Repo:    strings.TrimSpace(os.Getenv("IL2API_UPDATE_REPO")),
		Tag:     strings.TrimSpace(os.Getenv("IL2API_UPDATE_TAG")),
		Logf:    logf,
		Current: version,
		// The exec'd image must not be in update mode again (even from a stale
		// IL2API_UPDATE), or it would fetch its own release forever.
		ARGV: serveArgv(),
	}
	res, err := selfupdate.Update(context.Background(), opts)
	if err != nil {
		return fmt.Errorf("self-update: %w", err)
	}
	logf("self-update: already on %s (%s); nothing to do", res.Version, res.Path)
	return nil
}
