package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bk201-org/harvester-runner-gateway/client"
	"github.com/bk201-org/harvester-runner-gateway/internal/smoke"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := smoke.Run(ctx, os.Getenv, logger); err != nil {
		logger.Error("gateway smoke test failed", "error", err)
		var smokeConfigErr *smoke.ConfigError
		var clientConfigErr *client.ConfigError
		if errors.As(err, &smokeConfigErr) || errors.As(err, &clientConfigErr) {
			os.Exit(2)
		}
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "Gateway smoke test passed")
}
