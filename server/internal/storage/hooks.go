package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// HookExecution captures a hook invocation persisted for coordination.
type HookExecution struct {
	ID                int64
	HookID            string
	Kind              string
	Scope             string
	TargetIDs         []string
	RequestedBy       string
	RequestedFromIP   string
	RequestedAt       time.Time
	ActiveUntil       sql.NullTime
	UntilFirstSuccess bool
	Parameters        map[string]string
	Note              string
	Status            string
}

const hookTableDDL = `
CREATE TABLE IF NOT EXISTS hook_executions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	hook_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	scope TEXT NOT NULL,
	target_ids_json TEXT NOT NULL,
	requested_by TEXT,
	requested_from_ip TEXT,
	parameters_json TEXT,
	note TEXT,
	until_first_success INTEGER NOT NULL DEFAULT 0,
	active_until TIMESTAMP,
	requested_at TIMESTAMP NOT NULL,
	status TEXT NOT NULL DEFAULT 'active'
);
CREATE INDEX IF NOT EXISTS idx_hook_executions_status ON hook_executions (status);
CREATE INDEX IF NOT EXISTS idx_hook_executions_active_until ON hook_executions (active_until);
`

// EnsureHookSchema creates the auxiliary tables required for hook handling.
func (s *Store) EnsureHookSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("store not initialised")
	}
	if _, err := s.db.ExecContext(ctx, hookTableDDL); err != nil {
		return fmt.Errorf("ensure hook schema: %w", err)
	}
	return nil
}

// InsertHookExecution persists a new hook invocation.
func (s *Store) InsertHookExecution(ctx context.Context, exec HookExecution) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store not initialised")
	}
	targetsJSON, err := json.Marshal(exec.TargetIDs)
	if err != nil {
		return 0, fmt.Errorf("encode target ids: %w", err)
	}
	paramsJSON, err := json.Marshal(exec.Parameters)
	if err != nil {
		return 0, fmt.Errorf("encode parameters: %w", err)
	}
	if exec.RequestedAt.IsZero() {
		exec.RequestedAt = time.Now().UTC()
	}
	if exec.Status == "" {
		exec.Status = "active"
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO hook_executions (
			hook_id, kind, scope, target_ids_json, requested_by, requested_from_ip,
			parameters_json, note, until_first_success, active_until, requested_at, status
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		exec.HookID,
		exec.Kind,
		exec.Scope,
		string(targetsJSON),
		exec.RequestedBy,
		exec.RequestedFromIP,
		string(paramsJSON),
		exec.Note,
		boolToInt(exec.UntilFirstSuccess),
		nullTimePointer(exec.ActiveUntil),
		exec.RequestedAt,
		exec.Status,
	)
	if err != nil {
		return 0, fmt.Errorf("insert hook execution: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("retrieve hook execution id: %w", err)
	}
	return id, nil
}

func nullTimePointer(val sql.NullTime) any {
	if !val.Valid {
		return nil
	}
	return val.Time
}

// ActiveHookExecutions returns hooks that are still active at given moment.
func (s *Store) ActiveHookExecutions(ctx context.Context, now time.Time) ([]HookExecution, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not initialised")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, hook_id, kind, scope, target_ids_json, requested_by, requested_from_ip,
		       parameters_json, note, until_first_success, active_until, requested_at, status
		FROM hook_executions
		WHERE status = 'active' AND (active_until IS NULL OR active_until >= ?)
	`, now)
	if err != nil {
		return nil, fmt.Errorf("query active hooks: %w", err)
	}
	defer rows.Close()

	var result []HookExecution
	for rows.Next() {
		var exec HookExecution
		var targetJSON string
		var paramsJSON string
		var untilFirst int
		if err := rows.Scan(
			&exec.ID,
			&exec.HookID,
			&exec.Kind,
			&exec.Scope,
			&targetJSON,
			&exec.RequestedBy,
			&exec.RequestedFromIP,
			&paramsJSON,
			&exec.Note,
			&untilFirst,
			&exec.ActiveUntil,
			&exec.RequestedAt,
			&exec.Status,
		); err != nil {
			return nil, fmt.Errorf("scan hook execution: %w", err)
		}
		if err := json.Unmarshal([]byte(targetJSON), &exec.TargetIDs); err != nil {
			return nil, fmt.Errorf("decode target ids: %w", err)
		}
		if paramsJSON != "" {
			if err := json.Unmarshal([]byte(paramsJSON), &exec.Parameters); err != nil {
				return nil, fmt.Errorf("decode parameters: %w", err)
			}
		}
		exec.UntilFirstSuccess = untilFirst == 1
		result = append(result, exec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active hooks: %w", err)
	}
	return result, nil
}

// boolToInt converts boolean to sqlite friendly integer.
func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
