package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bk201-org/harvester-runner-gateway/internal/clusteraction"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) != 2 || (os.Args[1] != "create" && os.Args[1] != "cleanup") {
		fmt.Fprintln(os.Stderr, "usage: hvst-runner-gw-cluster create|cleanup")
		os.Exit(2)
	}
	if err := clusteraction.Run(ctx, os.Args[1], os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
