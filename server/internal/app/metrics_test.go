package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/osbits/upupup/server/internal/config"
	"github.com/osbits/upupup/server/internal/storage"
)

func TestPromLabelKeySanitisesDisallowedCharacters(t *testing.T) {
	tests := map[string]string{
		"env":        "env",
		"team-name":  "team_name",
		"9invalid":   "_invalid",
		"":           "_",
		"service.ok": "service_ok",
	}
	for input, expected := range tests {
		if got := promLabelKey(input); got != expected {
			t.Fatalf("promLabelKey(%q) = %q, expected %q", input, got, expected)
		}
	}
}

func TestPromLabelValueEscapesSpecialCharacters(t *testing.T) {
	input := "foo\"bar\nbaz\\"
	got := promLabelValue(input)
	expected := "foo\\\"bar\\nbaz\\\\"
	if got != expected {
		t.Fatalf("unexpected escaped value: %q (expected %q)", got, expected)
	}
}

func TestHandleMetricsAppendsNodeMetricsPayload(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "state.db")

	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	if err := store.EnsureIngestSchema(ctx); err != nil {
		t.Fatalf("ensure ingest schema: %v", err)
	}

	_, err = store.DB().Exec(`
		CREATE TABLE IF NOT EXISTS check_states (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			check_id TEXT NOT NULL,
			check_name TEXT NOT NULL,
			success INTEGER NOT NULL,
			summary TEXT,
			error TEXT,
			latency_ms INTEGER,
			occurred_at TIMESTAMP NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("create check_states: %v", err)
	}

	occurredAt := time.Now().UTC()
	_, err = store.DB().Exec(`
		INSERT INTO check_states (check_id, check_name, success, summary, error, latency_ms, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "metrics-check", "Metrics Check", 1, "", "", 250, occurredAt)
	if err != nil {
		t.Fatalf("insert check_state: %v", err)
	}

	ingestedAt := time.Now().UTC().Truncate(time.Second)
	err = store.UpsertNodeMetrics(ctx, storage.NodeMetricSnapshot{
		NodeID:     "node-1",
		Payload:    "node_load1 0.5\n",
		IngestedAt: ingestedAt,
	})
	if err != nil {
		t.Fatalf("upsert node metrics: %v", err)
	}

	app := &App{
		store: store,
		checkConfigs: map[string]config.CheckConfig{
			"metrics-check": {
				ID:   "metrics-check",
				Name: "Metrics Check",
				Type: "metrics",
				Metrics: &config.MetricsCheck{
					NodeID: "node-1",
				},
			},
		},
		metricsCfg: config.MetricsConfig{Namespace: "upupup"},
		healthCfg:  config.HealthConfig{MaxIntervalMultiplier: 3},
		serviceDefaults: config.ServiceDefault{
			Interval: config.Duration{Duration: time.Minute},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/metrics/metrics-check", nil)
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("checkID", "metrics-check")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	rec := httptest.NewRecorder()
	app.handleMetrics(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", res.StatusCode)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `node_load1{check_id="node-1"} 0.5`) {
		t.Fatalf("expected node metrics payload with check_id label in response:\n%s", body)
	}

	expectedComment := fmt.Sprintf("# Raw metrics from node %s (ingested_at=%s)", promLabelValue("node-1"), ingestedAt.Format(time.RFC3339))
	if !strings.Contains(body, expectedComment) {
		t.Fatalf("expected comment %q in response:\n%s", expectedComment, body)
	}
}

func TestEnsureCheckIDLabelAddsLabelWhenMissing(t *testing.T) {
	input := "metric_without_labels 1\nmetric_with_labels{env=\"prod\"} 2\n# HELP comment\n"
	expected := "metric_without_labels{check_id=\"abc\"} 1\nmetric_with_labels{env=\"prod\",check_id=\"abc\"} 2\n# HELP comment\n"

	result := ensureCheckIDLabel(input, "abc")
	if result != expected {
		t.Fatalf("unexpected result:\n%s\nexpected:\n%s", result, expected)
	}
}

func TestEnsureCheckIDLabelSkipsExistingLabel(t *testing.T) {
	input := "metric_with_check{check_id=\"abc\",env=\"prod\"} 1"
	result := ensureCheckIDLabel(input, "abc")
	if result != input {
		t.Fatalf("expected unchanged result when check_id already present:\n%s", result)
	}
}

func TestEnsureCheckIDLabelHandlesEmptyCheckID(t *testing.T) {
	input := "metric 1"
	result := ensureCheckIDLabel(input, "")
	if result != input {
		t.Fatalf("expected unchanged result when check ID empty")
	}
}
