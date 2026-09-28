package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bk201/harvester-runner-gateway/internal/auth"
	"github.com/bk201/harvester-runner-gateway/internal/config"
	"github.com/bk201/harvester-runner-gateway/internal/gateway"
	"github.com/bk201/harvester-runner-gateway/internal/harvester"
)

func main() {
	path := flag.String("config", "", "path to gateway YAML configuration")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "--config is required")
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*path, logger); err != nil {
		logger.Error("gateway failed", "error", err)
		os.Exit(1)
	}
}

func run(path string, logger *slog.Logger) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	logger.Info("configuration loaded", "listen_address", cfg.ListenAddress, "repositories", len(cfg.Repositories))
	verifier, err := auth.NewLocalSmokeVerifier(auth.NewVerifier(cfg.OIDC.Issuer, cfg.OIDC.Audience), cfg)
	if err != nil {
		return err
	}
	backend, err := harvester.New(cfg, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	preflightCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = backend.Preflight(preflightCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("Harvester preflight: %w", err)
	}
	logger.Info("Harvester preflight completed")
	store, err := gateway.OpenSQLiteAllocationStore(ctx, cfg.Database.Path, cfg.IDPrefixes())
	if err != nil {
		return err
	}
	defer store.Close()
	api := gateway.NewServerWithAllocationStore(cfg, verifier, backend, logger, store)
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 45*time.Second)
	err = api.RecoverAllocations(recoveryCtx)
	recoveryCancel()
	if err != nil {
		return fmt.Errorf("recover allocations: %w", err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 45*time.Second)
	cleanupErr := api.CleanupExpired(cleanupCtx, time.Now())
	cleanupCancel()
	if cleanupErr != nil {
		logger.Warn("initial expiry cleanup failed", "error", cleanupErr)
	} else {
		logger.Info("initial expiry cleanup completed")
	}
	server := &http.Server{Addr: cfg.ListenAddress, Handler: api.Handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 16 * 1024, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile) }()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				if err := api.CleanupExpired(checkCtx, time.Now()); err != nil {
					logger.Error("expiry cleanup failed", "error", err)
				}
				cancel()
			}
		}
	}()
	logger.Info("gateway listening", "address", cfg.ListenAddress)
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		logger.Info("gateway stopped")
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
