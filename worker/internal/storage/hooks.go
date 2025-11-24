package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

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

// HookExecution mirrors server-side hook execution rows for local evaluation.
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

// ActiveHookExecutions returns currently active hook executions.
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
			return nil, fmt.Errorf("decode hook targets: %w", err)
		}
		if paramsJSON != "" {
			if err := json.Unmarshal([]byte(paramsJSON), &exec.Parameters); err != nil {
				return nil, fmt.Errorf("decode hook parameters: %w", err)
			}
		}
		exec.UntilFirstSuccess = untilFirst == 1
		result = append(result, exec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hook executions: %w", err)
	}
	return result, nil
}

// CompleteHookExecution marks an active hook as completed.
func (s *Store) CompleteHookExecution(ctx context.Context, id int64) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE hook_executions
		SET status = 'completed'
		WHERE id = ? AND status = 'active'
	`, id)
	if err != nil {
		return fmt.Errorf("complete hook execution: %w", err)
	}
	return nil
}
