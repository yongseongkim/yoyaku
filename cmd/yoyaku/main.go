package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"yoyaku/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.Run(ctx, os.Args[1:], os.Stdout); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
