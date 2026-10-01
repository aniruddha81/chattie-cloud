// Command chattie runs one of two processes from the same binary:
//
//	chattie serve      the chat instance: HTTP, WebSocket and web client (default)
//	chattie publisher  the outbox worker that forwards events to Redis
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aniruddha81/chattie-cloud/internal/api"
	"github.com/aniruddha81/chattie-cloud/internal/bus"
	"github.com/aniruddha81/chattie-cloud/internal/config"
	"github.com/aniruddha81/chattie-cloud/internal/outbox"
	"github.com/aniruddha81/chattie-cloud/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("instance", cfg.InstanceID, "mode", mode))

	// SIGTERM is what a VM or container gets when it is asked to stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	events, err := bus.Open(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("redis url: %w", err)
	}
	defer events.Close()

	switch mode {
	case "serve":
		return api.New(cfg, store, events).Run(ctx)
	case "publisher":
		return outbox.Run(ctx, store.Pool(), events.Publish)
	default:
		return fmt.Errorf("unknown mode %q: use serve or publisher", mode)
	}
}
