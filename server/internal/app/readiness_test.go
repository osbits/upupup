package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/osbits/upupup/server/internal/config"
)

func TestGeneratePrometheusConfigWritesExpectedScrapeConfig(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	outputPath := filepath.Join(dir, "prometheus.yml")

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: "0.0.0.0:9090",
			Prometheus: config.MetricsConfig{
				Namespace:                "testns",
				ConfigPath:               outputPath,
				JobName:                  "custom_job",
				Scheme:                   "https",
				Targets:                  []string{"server:8080", "server:8080", "other:9090"},
				GlobalScrapeInterval:     config.Duration{Duration: 15 * time.Second},
				GlobalEvaluationInterval: config.Duration{Duration: 20 * time.Second},
				ScrapeInterval:           config.Duration{Duration: 10 * time.Second},
			},
		},
		Checks: []config.CheckConfig{
			{ID: "check-b", Name: "Check B"},
			{ID: "check-a", Name: "Check A"},
		},
	}

	testLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := &App{
		cfg:    cfg,
		logger: testLogger,
		checkConfigs: map[string]config.CheckConfig{
			"check-a": cfg.Checks[1],
			"check-b": cfg.Checks[0],
		},
		metricsCfg: applyMetricsDefaults(cfg.Server.Prometheus),
	}

	targets, err := app.generatePrometheusConfig()
	if err != nil {
		t.Fatalf("generatePrometheusConfig returned error: %v", err)
	}

	expectedTargets := []string{"other:9090", "server:8080"}
	if !reflect.DeepEqual(targets, expectedTargets) {
		t.Fatalf("unexpected targets: got %v want %v", targets, expectedTargets)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read generated config: %v", err)
	}

	var prom promConfigFile
	if err := yaml.Unmarshal(data, &prom); err != nil {
		t.Fatalf("failed to unmarshal generated config: %v", err)
	}

	if len(prom.ScrapeConfigs) != 1 {
		t.Fatalf("expected one scrape config but got %d", len(prom.ScrapeConfigs))
	}

	job := prom.ScrapeConfigs[0]
	if job.JobName != "custom_job" {
		t.Fatalf("unexpected job name: %s", job.JobName)
	}
	if job.Scheme != "https" {
		t.Fatalf("unexpected scheme: %s", job.Scheme)
	}
	if job.ScrapeInterval != "10s" {
		t.Fatalf("unexpected scrape interval: %s", job.ScrapeInterval)
	}
	if len(job.StaticConfigs) != len(cfg.Checks)*len(expectedTargets) {
		t.Fatalf("unexpected static config count: got %d want %d", len(job.StaticConfigs), len(cfg.Checks)*len(expectedTargets))
	}
	first := job.StaticConfigs[0]
	if !reflect.DeepEqual(first.Targets, []string{"other:9090"}) {
		t.Fatalf("unexpected targets in first static config: %v", first.Targets)
	}
	if first.Labels["check_id"] != "check-a" {
		t.Fatalf("unexpected check_id label: %s", first.Labels["check_id"])
	}
	if len(job.RelabelConfigs) != 1 {
		t.Fatalf("expected one relabel config but got %d", len(job.RelabelConfigs))
	}
	rel := job.RelabelConfigs[0]
	if rel.Replacement != "/api/metrics/$1" {
		t.Fatalf("unexpected relabel replacement: %s", rel.Replacement)
	}
}

func TestGeneratePrometheusConfigUsesListenFallback(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	outputPath := filepath.Join(dir, "prometheus.yml")

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: ":8081",
			Prometheus: config.MetricsConfig{
				Namespace:  "ns",
				ConfigPath: outputPath,
			},
		},
		Checks: []config.CheckConfig{
			{ID: "check-1"},
		},
	}

	testLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := &App{
		cfg:    cfg,
		logger: testLogger,
		checkConfigs: map[string]config.CheckConfig{
			"check-1": cfg.Checks[0],
		},
		metricsCfg: applyMetricsDefaults(cfg.Server.Prometheus),
	}

	targets, err := app.generatePrometheusConfig()
	if err != nil {
		t.Fatalf("generatePrometheusConfig returned error: %v", err)
	}

	if len(targets) != 1 || targets[0] != "localhost:8081" {
		t.Fatalf("expected fallback target localhost:8081, got %v", targets)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read generated config: %v", err)
	}
	if !strings.Contains(string(data), "localhost:8081") {
		t.Fatalf("generated config does not include fallback target: %s", string(data))
	}

	now := time.Now().UTC()
	app.setPrometheusConfigStatus(now, targets, nil)
	status := app.prometheusConfigStatus()
	if status.Status != statusOK {
		t.Fatalf("expected readiness config status to be ok, got %s", status.Status)
	}
	if status.LastGenerated == nil || status.LastGenerated.IsZero() {
		t.Fatal("expected last generated timestamp to be set")
	}
	if len(status.Targets) != 1 || status.Targets[0] != "localhost:8081" {
		t.Fatalf("unexpected readiness targets: %v", status.Targets)
	}
}
