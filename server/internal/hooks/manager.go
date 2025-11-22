package hooks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/osbits/upupup/server/internal/config"
	"github.com/osbits/upupup/server/internal/storage"
)

// Manager coordinates hook invocation against persistent storage.
type Manager struct {
	store       *storage.Store
	hooksByID   map[string]config.HookConfig
	defaultOpts InvokeDefaults
}

// InvokeDefaults control behaviour when config is incomplete.
type InvokeDefaults struct {
	MaxDuration time.Duration
}

// NewManager initialises a new hook manager.
func NewManager(store *storage.Store, cfg []config.HookConfig) *Manager {
	hooksByID := make(map[string]config.HookConfig, len(cfg))
	for _, hook := range cfg {
		hooksByID[hook.ID] = hook
	}
	return &Manager{
		store:       store,
		hooksByID:   hooksByID,
		defaultOpts: InvokeDefaults{MaxDuration: 24 * time.Hour},
	}
}

// InvokeOptions holds runtime overrides for hook execution.
type InvokeOptions struct {
	DurationOverride   *time.Duration
	UntilFirstSuccess  *bool
	Note               string
	RequestedBy        string
	RequestedFromIP    string
	AdditionalMetadata map[string]string
}

// Invoke executes a hook identified by ID and persists the result.
func (m *Manager) Invoke(ctx context.Context, hookID string, opts InvokeOptions) (storage.HookExecution, error) {
	if m == nil {
		return storage.HookExecution{}, errors.New("manager is nil")
	}
	hookCfg, ok := m.hooksByID[hookID]
	if !ok {
		return storage.HookExecution{}, fmt.Errorf("unknown hook %q", hookID)
	}
	now := time.Now().UTC()

	duration := resolveDuration(&hookCfg, opts, m.defaultOpts.MaxDuration)
	var activeUntil sql.NullTime
	if duration > 0 {
		activeUntil = sql.NullTime{Time: now.Add(duration), Valid: true}
	}

	untilFirst := hookCfg.Action.UntilFirstSuccess
	if opts.UntilFirstSuccess != nil {
		if hookCfg.Action.UntilFirstSuccess && !*opts.UntilFirstSuccess {
			// Cannot relax config requirement.
		} else {
			untilFirst = *opts.UntilFirstSuccess
		}
	}

	parameters := make(map[string]string, len(hookCfg.Action.Parameters)+len(opts.AdditionalMetadata))
	for k, v := range hookCfg.Action.Parameters {
		parameters[k] = v
	}
	for k, v := range hookCfg.Metadata {
		if _, exists := parameters[k]; !exists {
			parameters[k] = v
		}
	}
	for k, v := range opts.AdditionalMetadata {
		parameters[k] = v
	}

	exec := storage.HookExecution{
		HookID:            hookCfg.ID,
		Kind:              hookCfg.Action.Kind,
		Scope:             hookCfg.Action.Scope,
		TargetIDs:         append([]string{}, hookCfg.Action.TargetIDs...),
		RequestedBy:       opts.RequestedBy,
		RequestedFromIP:   opts.RequestedFromIP,
		RequestedAt:       now,
		ActiveUntil:       activeUntil,
		UntilFirstSuccess: untilFirst,
		Parameters:        parameters,
		Note:              opts.Note,
		Status:            "active",
	}

	id, err := m.store.InsertHookExecution(ctx, exec)
	if err != nil {
		return storage.HookExecution{}, err
	}
	exec.ID = id
	return exec, nil
}

func resolveDuration(hookCfg *config.HookConfig, opts InvokeOptions, fallbackMax time.Duration) time.Duration {
	var duration time.Duration
	if hookCfg.Action.Duration != nil && hookCfg.Action.Duration.Set {
		duration = hookCfg.Action.Duration.Duration
	}
	if opts.DurationOverride != nil {
		duration = *opts.DurationOverride
	}
	var maxDuration time.Duration
	if hookCfg.Action.MaxDuration != nil && hookCfg.Action.MaxDuration.Set {
		maxDuration = hookCfg.Action.MaxDuration.Duration
	} else {
		maxDuration = fallbackMax
	}
	if maxDuration > 0 && duration > maxDuration {
		duration = maxDuration
	}
	if duration < 0 {
		duration = 0
	}
	return duration
}

// ListActive returns currently active hooks for observability purposes.
func (m *Manager) ListActive(ctx context.Context, now time.Time) ([]storage.HookExecution, error) {
	if m == nil {
		return nil, errors.New("manager is nil")
	}
	return m.store.ActiveHookExecutions(ctx, now)
}
