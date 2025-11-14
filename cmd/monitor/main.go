package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/osbits/upupup/internal/config"
	"github.com/osbits/upupup/internal/notifier"
	"github.com/osbits/upupup/internal/render"
	"github.com/osbits/upupup/internal/runner"
)

func main() {
	var configPath string
	defaultConfig := os.Getenv("MONITOR_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "config.yml"
	}
	flag.StringVar(&configPath, "config", defaultConfig, "path to configuration file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	secrets, err := cfg.ResolveSecrets()
	if err != nil {
		logger.Error("failed to resolve secrets", "error", err)
		os.Exit(1)
	}

	engine := render.New()
	notifierFactory := notifier.Factory{
		Secrets: secrets,
		Render:  engine,
	}

	registry, err := notifier.Build(notifierFactory, cfg.Notifiers)
	if err != nil {
		logger.Error("failed to build notifiers", "error", err)
		os.Exit(1)
	}

	location, err := time.LoadLocation(cfg.Service.Timezone)
	if err != nil {
		logger.Warn("failed to load timezone, defaulting to UTC", "timezone", cfg.Service.Timezone, "error", err)
		location = time.UTC
	}

	run, err := runner.New(cfg, secrets, registry, engine, logger, location)
	if err != nil {
		logger.Error("failed to initialize runner", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signalContext()
	defer cancel()

	if err := run.Start(ctx); err != nil && err != context.Canceled {
		logger.Error("runner stopped", "error", err)
		os.Exit(1)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-signals:
			log.Println("shutdown signal received")
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
