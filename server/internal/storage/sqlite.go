package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps read/write access to the sqlite database.
type Store struct {
	db *sql.DB
}

// Open initialises a sqlite connection with sane defaults.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if err := configure(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close terminates the underlying database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping verifies the database connection.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialised")
	}
	return s.db.PingContext(ctx)
}

func configure(db *sql.DB) error {
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

// CheckRun represents the last known state of a check execution.
type CheckRun struct {
	CheckID    string
	CheckName  string
	Success    bool
	Summary    string
	Error      string
	Latency    time.Duration
	OccurredAt time.Time
}

// LatestCheckRun returns the most recent check execution for a given check.
func (s *Store) LatestCheckRun(ctx context.Context, checkID string) (*CheckRun, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialised")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT check_id, check_name, success, summary, error, latency_ms, occurred_at
		FROM check_states
		WHERE check_id = ?
		ORDER BY occurred_at DESC
		LIMIT 1
	`, checkID)

	var run CheckRun
	var success int
	var latencyMs sql.NullInt64
	if err := row.Scan(&run.CheckID, &run.CheckName, &success, &run.Summary, &run.Error, &latencyMs, &run.OccurredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // no history yet
		}
		return nil, fmt.Errorf("query latest check run: %w", err)
	}
	run.Success = success == 1
	if latencyMs.Valid {
		run.Latency = time.Duration(latencyMs.Int64) * time.Millisecond
	}
	return &run, nil
}

// CountRecentCheckRuns returns number of check executions within timeframe.
func (s *Store) CountRecentCheckRuns(ctx context.Context, checkID string, since time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store not initialised")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM check_states
		WHERE check_id = ? AND occurred_at >= ?
	`, checkID, since)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count recent check runs: %w", err)
	}
	return count, nil
}

// RecentOutcomeCounts returns total and failed runs within timeframe.
func (s *Store) RecentOutcomeCounts(ctx context.Context, checkID string, since time.Time) (total int, failed int, err error) {
	if s == nil || s.db == nil {
		return 0, 0, errors.New("store not initialised")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0)
		FROM check_states
		WHERE check_id = ? AND occurred_at >= ?
	`, checkID, since)
	if err := row.Scan(&total, &failed); err != nil {
		return 0, 0, fmt.Errorf("count recent outcomes: %w", err)
	}
	return total, failed, nil
}

// NotificationLog represents a row from notification_logs.
type NotificationLog struct {
	NotifierID string
	CheckID    string
	Status     string
	Summary    string
	OccurredAt time.Time
}

// RecentNotificationLogs returns latest notification entries up to limit.
func (s *Store) RecentNotificationLogs(ctx context.Context, limit int) ([]NotificationLog, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialised")
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT notifier_id, check_id, status, summary, occurred_at
		FROM notification_logs
		ORDER BY occurred_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("query notification logs: %w", err)
	}
	defer rows.Close()

	var logs []NotificationLog
	for rows.Next() {
		var entry NotificationLog
		if err := rows.Scan(&entry.NotifierID, &entry.CheckID, &entry.Status, &entry.Summary, &entry.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan notification log: %w", err)
		}
		logs = append(logs, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notification logs: %w", err)
	}
	return logs, nil
}

// DB exposes the underlying sql.DB for advanced consumers.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}
