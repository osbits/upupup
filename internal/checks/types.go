package checks

import (
	"context"
	"time"

	"github.com/osbits/upupup/internal/config"
)

// Result captures the outcome of a single check execution.
type Result struct {
	CheckID          string
	CheckName        string
	Success          bool
	StartedAt        time.Time
	CompletedAt      time.Time
	Latency          time.Duration
	AssertionResults []AssertionResult
	Error            error
	Metadata         map[string]any
}

// AssertionResult captures the outcome of a single assertion.
type AssertionResult struct {
	Kind    string
	Op      string
	Path    string
	Passed  bool
	Message string
}

// Executor executes a configured check.
type Executor interface {
	Run(ctx context.Context) Result
}

// Factory creates executors based on check type.
type Factory interface {
	NewExecutor(cfg config.CheckConfig) (Executor, error)
}
