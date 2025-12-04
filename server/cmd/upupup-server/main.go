package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/osbits/upupup/server/internal/app"
	"github.com/osbits/upupup/server/internal/config"
	"github.com/osbits/upupup/server/internal/observability"
	"github.com/osbits/upupup/server/internal/storage"
)

func main() {
	var (
		configPath      string
		listenOverride  string
		shutdownTimeout time.Duration
	)
	flag.StringVar(&configPath, "config", "config.yml", "path to configuration file")
	flag.StringVar(&listenOverride, "listen", "", "override listen address")
	flag.DurationVar(&shutdownTimeout, "shutdown-timeout", 10*time.Second, "graceful shutdown timeout")
	flag.Parse()

	logOpts := &slog.HandlerOptions{Level: slog.LevelInfo}
	logger := slog.New(slog.NewTextHandler(os.Stdout, logOpts))
	slog.SetDefault(logger)

	observability.LoadDotEnv(logger)

	rollbarEnabled, rollbarCleanup := observability.SetupRollbar(logger)
	defer func() {
		if rollbarCleanup != nil {
			rollbarCleanup()
		}
	}()
	defer observability.CapturePanic(logger, rollbarEnabled)()

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if listenOverride != "" {
		cfg.Server.Listen = listenOverride
	}
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8080"
	}

	dbPath := cfg.Storage.Path
	if envPath := os.Getenv("MONITOR_DB_PATH"); envPath != "" {
		dbPath = envPath
	}
	store, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}
	defer func() {
		_ = store.Close()
	}()

	ctx := context.Background()
	application, err := app.New(ctx, cfg, store, logger)
	if err != nil {
		log.Fatalf("initialise app: %v", err)
	}

	server := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      application.Routes(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-shutdownCh
		logger.Info("shutdown signal received", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("server listening", "addr", cfg.Server.Listen, "db", dbPath)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
}
