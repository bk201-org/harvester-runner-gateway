package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/bk201/harvester-runner-gateway/internal/clientcli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := clientcli.Run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
