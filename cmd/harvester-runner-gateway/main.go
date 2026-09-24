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

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
	"github.com/bk201-org/harvester-runner-gateway/internal/harvester"
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
		logger.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(path string, logger *slog.Logger) error {
	cfg, err := config.Load(path)
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
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 45*time.Second)
	if err := backend.CleanupExpired(cleanupCtx, time.Now()); err != nil {
		logger.Warn("initial expiry cleanup failed", "error", err)
	}
	cleanupCancel()
	api := gateway.NewServer(cfg, auth.NewVerifier(cfg.OIDC.Issuer, cfg.OIDC.Audience), backend)
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
