// Command openlinker-registry serves the OpenLinker Skill registry.
//
// Usage:
//
//	openlinker-registry [flags]          serve (default)
//	openlinker-registry migrate [flags]  apply database migrations and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/api"
	"github.com/OpenLinker-ai/openlinker-registry/internal/auth"
	"github.com/OpenLinker-ai/openlinker-registry/internal/blobstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/config"
	"github.com/OpenLinker-ai/openlinker-registry/internal/github"
	"github.com/OpenLinker-ai/openlinker-registry/internal/pgstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(os.Args[1:], log); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run(args []string, log *slog.Logger) error {
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
	svc := registry.New(st, blobs, gh, registry.Config{Mode: cfg.Mode, AllowPublic: cfg.AllowPublic, Limits: cfg.Limits, PublicURL: cfg.PublicURL})
	srv := &api.Server{Svc: svc, Auth: auth.NewAuthenticator(st, cfg.AdminToken), BasePath: cfg.BasePath,
		AllowAnonRead: cfg.AllowAnonRead, MaxUploadBytes: cfg.MaxUploadBytes, Log: log}

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if n, err := blobs.Evict(ctx, cfg.CacheMaxAge, cfg.CacheMaxBytes); err != nil {
				log.Warn("cache eviction failed", "err", err.Error())
			} else if n > 0 {
				log.Info("cache evicted", "entries", n)
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
		log.Info("listening", "addr", cfg.Listen, "base_path", cfg.BasePath, "mode", cfg.Mode)
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
