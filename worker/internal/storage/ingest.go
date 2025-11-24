package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const nodeMetricsTableDDL = `
CREATE TABLE IF NOT EXISTS node_metrics (
	node_id TEXT PRIMARY KEY,
	payload TEXT NOT NULL,
	ingested_at TIMESTAMP NOT NULL,
	source_ip TEXT
);
`

// NodeMetricSnapshot represents the latest metrics payload for a node.
type NodeMetricSnapshot struct {
	NodeID     string
	Payload    string
	SourceIP   string
	IngestedAt time.Time
}

// EnsureNodeMetricsSchema guarantees that the metrics table exists.
func (s *Store) EnsureNodeMetricsSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialised")
	}
	if _, err := s.db.ExecContext(ctx, nodeMetricsTableDDL); err != nil {
		return fmt.Errorf("ensure node metrics schema: %w", err)
	}
	return nil
}

// UpsertNodeMetrics stores or updates the latest metrics payload for a node.
func (s *Store) UpsertNodeMetrics(ctx context.Context, snapshot NodeMetricSnapshot) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialised")
	}
	nodeID := strings.TrimSpace(snapshot.NodeID)
	if nodeID == "" {
		return errors.New("node id is required")
	}
	payload := snapshot.Payload
	if strings.TrimSpace(payload) == "" {
		return errors.New("payload is required")
	}
	if snapshot.IngestedAt.IsZero() {
		snapshot.IngestedAt = time.Now().UTC()
	} else {
		snapshot.IngestedAt = snapshot.IngestedAt.UTC()
	}
	var sourceIP any
	if strings.TrimSpace(snapshot.SourceIP) != "" {
		sourceIP = strings.TrimSpace(snapshot.SourceIP)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO node_metrics (node_id, payload, ingested_at, source_ip)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			payload = excluded.payload,
			ingested_at = excluded.ingested_at,
			source_ip = excluded.source_ip
	`, nodeID, payload, snapshot.IngestedAt, sourceIP)
	if err != nil {
		return fmt.Errorf("upsert node metrics: %w", err)
	}
	return nil
}

// LatestNodeMetrics returns the most recent metrics snapshot for the node.
func (s *Store) LatestNodeMetrics(ctx context.Context, nodeID string) (*NodeMetricSnapshot, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialised")
	}
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, errors.New("node id is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT node_id, payload, ingested_at, source_ip
		FROM node_metrics
		WHERE node_id = ?
	`, nodeID)

	var snapshot NodeMetricSnapshot
	var sourceIP sql.NullString
	if err := row.Scan(&snapshot.NodeID, &snapshot.Payload, &snapshot.IngestedAt, &sourceIP); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query node metrics: %w", err)
	}
	if sourceIP.Valid {
		snapshot.SourceIP = sourceIP.String
	}
	snapshot.IngestedAt = snapshot.IngestedAt.UTC()
	return &snapshot, nil
}
