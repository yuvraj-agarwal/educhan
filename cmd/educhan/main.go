// Command educhan runs the educhan HTTP service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/yuvraj-agarwal/educhan/internal/config"
	"github.com/yuvraj-agarwal/educhan/internal/server"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet when configuration fails, so report to
		// stderr directly and exit non-zero for the supervisor.
		fmt.Fprintf(os.Stderr, "educhan: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	})).With("service", "educhan", "version", server.Version)
	slog.SetDefault(log)

	// SIGTERM is what container runtimes send before killing the container;
	// cancelling ctx starts the graceful drain.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return server.New(cfg, log).Run(ctx)
}
