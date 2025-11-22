package app

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/osbits/upupup/server/internal/storage"
)

const maxIngestPayloadBytes = 2 * 1024 * 1024 // 2 MiB

func (a *App) handleIngestMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodeID := strings.TrimSpace(chi.URLParam(r, "nodeID"))
	if nodeID == "" {
		http.Error(w, "node id is required", http.StatusBadRequest)
		return
	}

	reader := http.MaxBytesReader(w, r.Body, maxIngestPayloadBytes)
	defer reader.Close()

	raw, err := io.ReadAll(reader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, fmt.Sprintf("payload exceeds %d bytes", maxIngestPayloadBytes), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	payload, err := decodeMetricsPayload(raw, r.Header.Get("Content-Encoding"))
	if err != nil {
		http.Error(w, "invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	metrics := strings.TrimSpace(string(payload))
	if metrics == "" {
		http.Error(w, "payload is empty", http.StatusBadRequest)
		return
	}

	ingestedAt := time.Now().UTC()
	snapshot := storage.NodeMetricSnapshot{
		NodeID:     nodeID,
		Payload:    metrics,
		SourceIP:   a.clientIP(ctx),
		IngestedAt: ingestedAt,
	}
	if err := a.store.UpsertNodeMetrics(ctx, snapshot); err != nil {
		http.Error(w, "failed to persist metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := struct {
		Status     string    `json:"status"`
		NodeID     string    `json:"node_id"`
		IngestedAt time.Time `json:"ingested_at"`
	}{
		Status:     "stored",
		NodeID:     nodeID,
		IngestedAt: ingestedAt,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		a.logger.Error("failed to encode ingest response", "error", err)
	}
}

func decodeMetricsPayload(data []byte, contentEncoding string) ([]byte, error) {
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	switch encoding {
	case "", "identity":
		return data, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("decompress gzip: %w", err)
		}
		defer reader.Close()
		decompressed, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("read gzip payload: %w", err)
		}
		return decompressed, nil
	default:
		return nil, fmt.Errorf("unsupported content encoding %q", contentEncoding)
	}
}
