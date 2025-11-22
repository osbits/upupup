package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osbits/upupup/server/internal/config"
	"github.com/osbits/upupup/server/internal/storage"
)

func TestHandleIngestMetricsStoresSnapshot(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	cfg := &config.Config{
		Storage: config.StorageConfig{Path: ":memory:"},
		Server:  config.ServerConfig{},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	app, err := New(context.Background(), cfg, store, logger)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	router := app.Routes()

	body := `# HELP node_cpu_seconds_total Seconds the CPUs spent in each mode.
node_cpu_seconds_total{cpu="0",mode="system"} 42.5`
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/node-a", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d (body=%s)", http.StatusAccepted, rec.Code, rec.Body.String())
	}

	snapshot, err := app.store.LatestNodeMetrics(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if snapshot == nil {
		t.Fatalf("snapshot not stored")
	}
	if snapshot.Payload != strings.TrimSpace(body) {
		t.Fatalf("unexpected payload: %q", snapshot.Payload)
	}
	if snapshot.SourceIP == "" {
		t.Fatalf("expected source ip to be recorded")
	}
	if snapshot.IngestedAt.IsZero() {
		t.Fatalf("expected ingested timestamp to be set")
	}
}

func TestDecodeMetricsPayloadSupportsGzip(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write([]byte("metric 123"))
	if err != nil {
		t.Fatalf("write gzip: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	out, err := decodeMetricsPayload(buf.Bytes(), "gzip")
	if err != nil {
		t.Fatalf("decode gzip payload: %v", err)
	}
	if string(out) != "metric 123" {
		t.Fatalf("unexpected payload: %q", string(out))
	}
}

func TestDecodeMetricsPayloadRejectsUnsupportedEncoding(t *testing.T) {
	if _, err := decodeMetricsPayload([]byte("foo"), "br"); err == nil {
		t.Fatalf("expected error for unsupported encoding")
	}
}
