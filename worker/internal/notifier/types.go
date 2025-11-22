package notifier

import (
	"context"
	"time"

	"github.com/osbits/upupup/worker/internal/checks"
	"github.com/osbits/upupup/worker/internal/config"
	"github.com/osbits/upupup/worker/internal/render"
)

// Event represents a notification event.
type Event struct {
	Check          config.CheckConfig
	Result         checks.Result
	Status         string // firing, resolved
	Severity       string
	Summary        string
	Details        map[string]any
	Labels         map[string]string
	RunID          string
	FirstFailureAt time.Time
	OccurredAt     time.Time
}

// Notifier represents a delivery mechanism.
type Notifier interface {
	ID() string
	Notify(ctx context.Context, event Event) error
}

// Factory builds notifiers based on config.
type Factory struct {
	Secrets map[string]string
	Render  *render.Engine
}
