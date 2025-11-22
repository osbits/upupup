package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Options configures storage behaviour.
type Options struct {
	CheckStateRetention   int
	NotificationRetention int
}

// Store wraps sqlite persistence for check runs and notifications.
type Store struct {
	db                *sql.DB
	checkStateLimit   int
	notificationLimit int
}

// CheckRun represents a persisted check execution result.
type CheckRun struct {
	CheckID    string
	CheckName  string
	Success    bool
	Summary    string
	Error      string
	Latency    time.Duration
	OccurredAt time.Time
}

// NotificationLog captures a notifier dispatch attempt.
type NotificationLog struct {
	NotifierID string
	CheckID    string
	CheckName  string
	RunID      string
	Status     string
	Severity   string
	Summary    string
	Labels     map[string]string
	OccurredAt time.Time
}

// Open initialises a sqlite store with WAL enabled and required schema.
func Open(path string, opts Options) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("storage path is required")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create storage directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	if err := configureSQLite(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	checkLimit := opts.CheckStateRetention
	if checkLimit <= 0 {
		checkLimit = 30
	}
	notificationLimit := opts.NotificationRetention
	if notificationLimit <= 0 {
		notificationLimit = 100
	}

	store := &Store{
		db:                db,
		checkStateLimit:   checkLimit,
		notificationLimit: notificationLimit,
	}

	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func configureSQLite(db *sql.DB) error {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA busy_timeout = 5000;",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			return fmt.Errorf("apply pragma %q: %w", pragma, err)
		}
	}
	return nil
}

func (s *Store) initSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS check_states (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			check_id TEXT NOT NULL,
			check_name TEXT NOT NULL,
			success INTEGER NOT NULL,
			summary TEXT,
			error TEXT,
			latency_ms INTEGER,
			occurred_at TIMESTAMP NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_check_states_check ON check_states (check_id, occurred_at DESC);`,
		`CREATE TABLE IF NOT EXISTS notification_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			notifier_id TEXT NOT NULL,
			check_id TEXT NOT NULL,
			check_name TEXT NOT NULL,
			run_id TEXT,
			status TEXT,
			severity TEXT,
			summary TEXT,
			labels_json TEXT,
			occurred_at TIMESTAMP NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_notification_logs_occurred ON notification_logs (occurred_at DESC);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
	}
	return nil
}

// RecordCheckRun persists the outcome of a check execution and enforces retention.
func (s *Store) RecordCheckRun(ctx context.Context, run CheckRun) error {
	if s == nil || s.db == nil {
		return nil
	}
	if run.OccurredAt.IsZero() {
		run.OccurredAt = time.Now()
	}
	latency := int64(run.Latency / time.Millisecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO check_states (check_id, check_name, success, summary, error, latency_ms, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, run.CheckID, run.CheckName, boolToInt(run.Success), run.Summary, run.Error, latency, run.OccurredAt.UTC())
	if err != nil {
		return fmt.Errorf("insert check_state: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM check_states
		WHERE check_id = ? AND id NOT IN (
			SELECT id FROM check_states
			WHERE check_id = ?
			ORDER BY id DESC
			LIMIT ?
		)
	`, run.CheckID, run.CheckID, s.checkStateLimit)
	if err != nil {
		return fmt.Errorf("prune check_states: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit check_state: %w", err)
	}
	return nil
}

// RecordNotification stores a notification dispatch entry and enforces retention.
func (s *Store) RecordNotification(ctx context.Context, log NotificationLog) error {
	if s == nil || s.db == nil {
		return nil
	}
	if log.OccurredAt.IsZero() {
		log.OccurredAt = time.Now()
	}
	labels := ""
	if len(log.Labels) > 0 {
		data, err := json.Marshal(log.Labels)
		if err != nil {
			return fmt.Errorf("encode labels: %w", err)
		}
		labels = string(data)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO notification_logs (notifier_id, check_id, check_name, run_id, status, severity, summary, labels_json, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, log.NotifierID, log.CheckID, log.CheckName, log.RunID, log.Status, log.Severity, log.Summary, labels, log.OccurredAt.UTC())
	if err != nil {
		return fmt.Errorf("insert notification_log: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM notification_logs
		WHERE id NOT IN (
			SELECT id FROM notification_logs
			ORDER BY id DESC
			LIMIT ?
		)
	`, s.notificationLimit)
	if err != nil {
		return fmt.Errorf("prune notification_logs: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit notification_log: %w", err)
	}
	return nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
