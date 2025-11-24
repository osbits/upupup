package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/osbits/upupup/upgent/internal/agent"
	"github.com/osbits/upupup/upgent/internal/config"
)

func main() {
	level := parseLogLevel(os.Getenv("UPGENT_LOG_LEVEL"))
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level, AddSource: false}))
	slog.SetDefault(logger)

	cfg, err := config.LoadFromEnv()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ag, err := agent.New(cfg, logger)
	if err != nil {
		logger.Error("failed to construct agent", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := ag.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("agent exited with error", "error", err)
		os.Exit(1)
	}
}

func parseLogLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
