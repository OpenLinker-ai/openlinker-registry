// Command openlinker-registry serves the OpenLinker Skill and MCP registry.
//
// Usage:
//
//	openlinker-registry [flags]          serve (default)
//	openlinker-registry migrate [flags]  apply database migrations and exit
//	openlinker-registry version          print the version and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/api"
	"github.com/OpenLinker-ai/openlinker-registry/internal/auth"
	"github.com/OpenLinker-ai/openlinker-registry/internal/blobstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/config"
	"github.com/OpenLinker-ai/openlinker-registry/internal/github"
	"github.com/OpenLinker-ai/openlinker-registry/internal/pgstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/ratelimit"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
	"github.com/OpenLinker-ai/openlinker-registry/internal/safehttp"
)

// version and commit are set by release builds:
//
//	-ldflags "-X main.version=v1.2.3 -X main.commit=<full commit>"
//
// A release version is the v* tag the release was cut from (see RELEASE.md);
// an ordinary local build says "dev".
var (
	version = "dev"
	commit  = ""
)

// buildCommit is the commit a release build was stamped with, else the one
// the Go toolchain recorded when it built from a checkout, else "".
func buildCommit() string {
	if commit != "" {
		return commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}

func versionLine() string {
	if c := buildCommit(); c != "" {
		return "openlinker-registry " + version + " (" + c + ")"
	}
	return "openlinker-registry " + version
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(os.Args[1:], log, os.Stdout); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run(args []string, log *slog.Logger, stdout io.Writer) error {
	// Answered before the configuration is read: it needs no database.
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version" || args[0] == "-version") {
		_, err := fmt.Fprintln(stdout, versionLine())
		return err
	}
	migrateOnly := len(args) > 0 && args[0] == "migrate"
	if migrateOnly || (len(args) > 0 && args[0] == "serve") {
		args = args[1:]
	}
	cfg, err := config.Load(args, os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := pgstore.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if migrateOnly || cfg.AutoMigrate {
		if err := st.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		log.Info("migrations applied")
	}
	if migrateOnly {
		return nil
	}
	blobs, err := blobstore.NewLocal(cfg.StorageDir)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	gh, err := github.New(github.Config{APIBase: cfg.GitHubAPIURL, RawBase: cfg.GitHubRawURL, WebHost: cfg.GitHubWebHost,
		Token: cfg.GitHubToken, Timeout: cfg.GitHubTimeout, Limits: cfg.Limits})
	if err != nil {
		return err
	}
	svc := registry.New(st, blobs, gh, registry.Config{Mode: cfg.Mode, AllowPublic: cfg.AllowPublic, Limits: cfg.Limits,
		PublicURL: cfg.PublicURL, VerifyInterval: cfg.VerifyInterval, UpstreamToken: cfg.UpstreamToken,
		UpstreamPolicy: safehttp.Policy{AllowHTTP: cfg.UpstreamAllowHTTP, AllowCIDRs: cfg.UpstreamAllowCIDRs, Timeout: cfg.UpstreamTimeout},
		Review:         cfg.Review, ReportThreshold: cfg.ReportThreshold})
	svc.Log = log
	srv := &api.Server{Svc: svc, Auth: auth.NewAuthenticator(st, cfg.AdminToken), BasePath: cfg.BasePath,
		AllowAnonRead: cfg.AllowAnonRead, MaxUploadBytes: cfg.MaxUploadBytes, Log: log,
		ReadLimit: ratelimit.New(cfg.RateLimitRead), WriteLimit: ratelimit.New(cfg.RateLimitWrite),
		ForwardLimit: ratelimit.New(cfg.RateLimitForward), AuthFailLimit: ratelimit.New(cfg.RateLimitAuthFailures),
		Clients:           ratelimit.ClientAddr{Trusted: cfg.TrustedProxies},
		AssertionAudience: cfg.AssertionAudience,
		UI:                cfg.UI, SecureCookies: strings.HasPrefix(cfg.PublicURL, "https://"), Background: ctx}

	if cfg.BackgroundSync {
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				svc.SyncDue(ctx)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if n, err := blobs.Evict(ctx, cfg.CacheMaxAge, cfg.CacheMaxBytes); err != nil {
				log.Warn("cache eviction failed", "err", err.Error())
			} else if n > 0 {
				log.Info("cache evicted", "entries", n)
			}
			if n, err := svc.GCArtifacts(ctx, time.Now().Add(-cfg.ArtifactGCGrace)); err != nil {
				log.Warn("artifact gc failed", "err", err.Error())
			} else if n > 0 {
				log.Info("unreferenced artifacts removed", "entries", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "base_path", cfg.BasePath, "mode", cfg.Mode, "ui", cfg.UI, "review", cfg.Review,
			"version", version, "commit", buildCommit())
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}
