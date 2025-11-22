package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/osbits/upupup/server/internal/access"
	"github.com/osbits/upupup/server/internal/config"
	"github.com/osbits/upupup/server/internal/hooks"
	"github.com/osbits/upupup/server/internal/storage"
)

// App wires configuration, storage and HTTP handlers together.
type App struct {
	cfg             *config.Config
	store           *storage.Store
	hookManager     *hooks.Manager
	allowlist       *access.Allowlist
	hookAllowlist   map[string]*access.Allowlist
	hookConfigs     map[string]config.HookConfig
	checkConfigs    map[string]config.CheckConfig
	trustedProxies  []*net.IPNet
	logger          *slog.Logger
	serviceDefaults config.ServiceDefault
	healthCfg       config.HealthConfig
	metricsCfg      config.MetricsConfig
	location        *time.Location
}

// New constructs an App instance ready to serve requests.
func New(ctx context.Context, cfg *config.Config, store *storage.Store, logger *slog.Logger) (*App, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if store == nil {
		return nil, fmt.Errorf("storage is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	if err := store.EnsureHookSchema(ctx); err != nil {
		return nil, err
	}
	if err := store.EnsureIngestSchema(ctx); err != nil {
		return nil, err
	}

	allowlist, err := access.NewAllowlist(cfg.Server.AllowedIPs)
	if err != nil {
		return nil, fmt.Errorf("build allowlist: %w", err)
	}
	hookAllow := make(map[string]*access.Allowlist, len(cfg.Hooks))
	hookConfigs := make(map[string]config.HookConfig, len(cfg.Hooks))
	for _, hook := range cfg.Hooks {
		hookConfigs[hook.ID] = hook
		if len(hook.AllowedIPs) == 0 {
			continue
		}
		al, err := access.NewAllowlist(hook.AllowedIPs)
		if err != nil {
			return nil, fmt.Errorf("hook %q allowlist: %w", hook.ID, err)
		}
		hookAllow[hook.ID] = al
	}

	checkConfigs := make(map[string]config.CheckConfig, len(cfg.Checks))
	for _, check := range cfg.Checks {
		checkConfigs[check.ID] = check
	}

	trustedProxies, err := access.ParseCIDRs(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("parse trusted proxies: %w", err)
	}

	location := time.UTC
	if tz := cfg.Service.Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			location = loc
		} else {
			logger.Warn("failed to load timezone, falling back to UTC", "timezone", tz, "error", err)
		}
	}

	app := &App{
		cfg:             cfg,
		store:           store,
		hookManager:     hooks.NewManager(store, cfg.Hooks),
		allowlist:       allowlist,
		hookAllowlist:   hookAllow,
		hookConfigs:     hookConfigs,
		checkConfigs:    checkConfigs,
		trustedProxies:  trustedProxies,
		logger:          logger,
		serviceDefaults: cfg.Service.Defaults,
		healthCfg:       applyHealthDefaults(cfg.Server.Health),
		metricsCfg:      applyMetricsDefaults(cfg.Server.Prometheus),
		location:        location,
	}
	return app, nil
}

// Routes returns the HTTP handler tree for the server.
func (a *App) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	if a.cfg.Server.LogRequests {
		r.Use(middleware.Logger)
	}
	r.Use(a.ipAllowMiddleware)
	r.Get("/healthcheck", a.handleHealth)
	r.Route("/api", func(r chi.Router) {
		r.Route("/hook", func(r chi.Router) {
			r.Post("/{hookID}", a.handleHook)
		})
		r.Route("/data", func(r chi.Router) {
			r.Get("/{checkID}", a.handleMetrics)
		})
		r.Route("/ingest", func(r chi.Router) {
			r.Post("/{nodeID}", a.handleIngestMetrics)
		})
	})
	return r
}

func applyHealthDefaults(cfg config.HealthConfig) config.HealthConfig {
	if cfg.MaxIntervalMultiplier <= 0 {
		cfg.MaxIntervalMultiplier = 3
	}
	if cfg.RequiredRecentRuns <= 0 {
		cfg.RequiredRecentRuns = 1
	}
	if cfg.NotificationErrorLookback <= 0 {
		cfg.NotificationErrorLookback = 20
	}
	if len(cfg.NotificationErrorStatuses) == 0 {
		cfg.NotificationErrorStatuses = []string{"error", "failed", "failure"}
	}
	return cfg
}

func applyMetricsDefaults(cfg config.MetricsConfig) config.MetricsConfig {
	if cfg.Namespace == "" {
		cfg.Namespace = "upupup"
	}
	return cfg
}

type clientIPKey struct{}

func (a *App) ipAllowMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, ipStr := access.ClientIPFromRequest(r, a.trustedProxies)
		if !a.allowlist.Allowed(ip) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), clientIPKey{}, ipStr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) clientIP(ctx context.Context) string {
	if val := ctx.Value(clientIPKey{}); val != nil {
		if ip, ok := val.(string); ok {
			return ip
		}
	}
	return ""
}
