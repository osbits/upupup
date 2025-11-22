package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/osbits/upupup/worker/internal/checks"
	"github.com/osbits/upupup/worker/internal/config"
	"github.com/osbits/upupup/worker/internal/notifier"
	"github.com/osbits/upupup/worker/internal/render"
	"github.com/osbits/upupup/worker/internal/storage"
	"github.com/robfig/cron/v3"
)

// Runner coordinates periodic execution of checks and notifications.
type Runner struct {
	cfg       *config.Config
	defaults  config.ServiceDefault
	secrets   map[string]string
	renderer  *render.Engine
	notifiers *notifier.Registry
	policies  map[string]config.NotificationPolicy
	logger    *slog.Logger
	location  *time.Location
	store     *storage.Store

	stateMu sync.Mutex
	state   map[string]*checkState

	maintenance []maintenanceWindow
}

// New constructs a new runner.
func New(cfg *config.Config, secrets map[string]string, reg *notifier.Registry, renderer *render.Engine, logger *slog.Logger, location *time.Location, store *storage.Store) (*Runner, error) {
	if err := applyAssertionSets(cfg); err != nil {
		return nil, err
	}
	policies := make(map[string]config.NotificationPolicy, len(cfg.NotificationPolicies))
	for _, p := range cfg.NotificationPolicies {
		policies[p.ID] = p
	}
	maintenance, err := parseMaintenance(cfg.Service.Defaults.MaintenanceWindows, location, cfg.Service.Defaults.Interval.Duration)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		cfg:         cfg,
		defaults:    cfg.Service.Defaults,
		secrets:     secrets,
		renderer:    renderer,
		notifiers:   reg,
		policies:    policies,
		logger:      logger,
		location:    location,
		store:       store,
		state:       map[string]*checkState{},
		maintenance: maintenance,
	}, nil
}

// Start launches check goroutines.
func (r *Runner) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, checkCfg := range r.cfg.Checks {
		check := checkCfg
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.runCheckLoop(ctx, check)
		}()
	}
	<-ctx.Done()
	wg.Wait()
	return ctx.Err()
}

func (r *Runner) runCheckLoop(ctx context.Context, check config.CheckConfig) {
	interval := r.effectiveInterval(check)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	r.logger.Info("starting check loop", "check_id", check.ID, "interval", interval)
	r.executeCheck(ctx, check)

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("stopping check loop", "check_id", check.ID)
			return
		case <-ticker.C:
			r.executeCheck(ctx, check)
		}
	}
}

func (r *Runner) executeCheck(ctx context.Context, check config.CheckConfig) {
	now := time.Now().In(r.location)
	if r.inMaintenance(now) {
		r.logger.Info("skipping check due to maintenance window", "check_id", check.ID)
		return
	}

	retries := r.effectiveRetries(check)
	backoff := r.effectiveBackoff(check)

	env := checks.Environment{
		Defaults:       r.defaults,
		Secrets:        r.secrets,
		TemplateEngine: r.renderer,
		TimeLocation:   r.location,
	}

	var result checks.Result
	for attempt := 0; attempt <= retries; attempt++ {
		select {
		case <-ctx.Done():
			r.logger.Warn("context canceled", "check_id", check.ID)
			return
		default:
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		result = checks.Execute(attemptCtx, check, env)
		cancel()

		if result.Success {
			break
		}
		if attempt < retries {
			r.logger.Warn("check attempt failed, retrying", "check_id", check.ID, "attempt", attempt+1, "error", result.Error)
			time.Sleep(backoff)
		}
	}

	r.logRun(check, result)
	r.persistCheckState(check, result)
	r.handleResult(check, result)
}

func (r *Runner) handleResult(check config.CheckConfig, result checks.Result) {
	state := r.getState(check.ID)
	fail := !result.Success
	state.appendHistory(fail, r.windowSize(check))
	prevFailing := state.Failing
	nowFailing := r.thresholdBreached(check, state.history)

	state.LastResult = result
	state.LastUpdated = time.Now()
	if result.Error != nil {
		state.LastError = result.Error
	}

	if nowFailing {
		if !prevFailing {
			state.Failing = true
			state.FirstFailure = time.Now()
			state.StageNotified = map[int]bool{}
			state.InitialNotified = false
			r.logger.Error("check entered failing state", "check_id", check.ID, "summary", summarizeResult(result))
			r.sendInitialNotifications(check, state, result)
		}
		r.sendEscalations(check, state, result)
	} else {
		if prevFailing {
			state.Failing = false
			r.logger.Info("check recovered", "check_id", check.ID)
			r.sendResolveNotifications(check, state, result)
		}
	}
}

func (r *Runner) sendInitialNotifications(check config.CheckConfig, state *checkState, result checks.Result) {
	if check.Notifications.Overrides == nil || len(check.Notifications.Overrides.InitialNotifiers) == 0 {
		return
	}
	if state.InitialNotified {
		return
	}
	ids := check.Notifications.Overrides.InitialNotifiers
	event := r.buildEvent(check, state, result, "firing")
	r.dispatch(ids, event)
	state.InitialNotified = true
}

func (r *Runner) sendEscalations(check config.CheckConfig, state *checkState, result checks.Result) {
	policy, ok := r.policies[check.Notifications.Route]
	if !ok {
		r.logger.Error("missing notification policy", "route", check.Notifications.Route)
		return
	}
	event := r.buildEvent(check, state, result, "firing")
	now := time.Now()
	for idx, stage := range policy.Stages {
		if state.StageNotified[idx] {
			continue
		}
		if now.Sub(state.FirstFailure) >= stage.After.Duration {
			r.dispatch(stage.Notifiers, event)
			state.StageNotified[idx] = true
		}
	}
}

func (r *Runner) sendResolveNotifications(check config.CheckConfig, state *checkState, result checks.Result) {
	policy, ok := r.policies[check.Notifications.Route]
	if !ok {
		return
	}
	event := r.buildEvent(check, state, result, "resolved")
	r.dispatch(policy.ResolveNotifiers, event)
}

func (r *Runner) dispatch(ids []string, event notifier.Event) {
	for _, id := range ids {
		not, ok := r.notifiers.Get(id)
		if !ok {
			r.logger.Error("notifier not found", "notifier_id", id)
			continue
		}
		r.recordNotification(id, event)
		go func(n notifier.Notifier) {
			if err := n.Notify(context.Background(), event); err != nil {
				r.logger.Error("notifier error", "notifier_id", n.ID(), "error", err)
			}
		}(not)
	}
}

func (r *Runner) buildEvent(check config.CheckConfig, state *checkState, result checks.Result, status string) notifier.Event {
	severity := "critical"
	summary := summarizeResult(result)
	return notifier.Event{
		Check:          check,
		Result:         result,
		Status:         status,
		Severity:       severity,
		Summary:        summary,
		Details:        map[string]any{},
		Labels:         check.Labels,
		RunID:          fmt.Sprintf("%s-%d", check.ID, time.Now().UnixNano()),
		FirstFailureAt: state.FirstFailure,
		OccurredAt:     time.Now(),
	}
}

func (r *Runner) thresholdBreached(check config.CheckConfig, history []bool) bool {
	if len(history) == 0 {
		return false
	}
	if check.Thresholds.FailureRatio == nil {
		return history[len(history)-1]
	}
	th := check.Thresholds.FailureRatio
	window := th.Window
	if window <= 0 || window > len(history) {
		window = len(history)
	}
	failures := 0
	for _, failed := range history[len(history)-window:] {
		if failed {
			failures++
		}
	}
	return failures >= th.FailCount
}

func (r *Runner) windowSize(check config.CheckConfig) int {
	if check.Thresholds.FailureRatio != nil && check.Thresholds.FailureRatio.Window > 0 {
		return check.Thresholds.FailureRatio.Window
	}
	return 10
}

func (r *Runner) effectiveInterval(check config.CheckConfig) time.Duration {
	if check.Schedule != nil && check.Schedule.Interval != nil && check.Schedule.Interval.Set {
		return check.Schedule.Interval.Duration
	}
	return r.defaults.Interval.Duration
}

func (r *Runner) effectiveRetries(check config.CheckConfig) int {
	if check.Schedule != nil && check.Schedule.Retries != nil {
		return *check.Schedule.Retries
	}
	return r.defaults.Retries
}

func (r *Runner) effectiveBackoff(check config.CheckConfig) time.Duration {
	if check.Schedule != nil && check.Schedule.Backoff != nil && check.Schedule.Backoff.Set {
		return check.Schedule.Backoff.Duration
	}
	return r.defaults.Backoff.Duration
}

func (r *Runner) getState(checkID string) *checkState {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	st, ok := r.state[checkID]
	if !ok {
		st = &checkState{
			history:       []bool{},
			StageNotified: map[int]bool{},
		}
		r.state[checkID] = st
	}
	return st
}

func (r *Runner) inMaintenance(now time.Time) bool {
	for _, mw := range r.maintenance {
		if mw.contains(now) {
			return true
		}
	}
	return false
}

type checkState struct {
	history         []bool
	Failing         bool
	FirstFailure    time.Time
	StageNotified   map[int]bool
	InitialNotified bool
	LastResult      checks.Result
	LastUpdated     time.Time
	LastError       error
}

func (s *checkState) appendHistory(failed bool, max int) {
	s.history = append(s.history, failed)
	if max > 0 && len(s.history) > max {
		s.history = s.history[len(s.history)-max:]
	}
}

type maintenanceWindow struct {
	kind     config.MaintenanceKind
	start    time.Time
	end      time.Time
	schedule cron.Schedule
	duration time.Duration
}

func (m maintenanceWindow) contains(t time.Time) bool {
	switch m.kind {
	case config.MaintenanceKindRange:
		if m.start.IsZero() || m.end.IsZero() {
			return false
		}
		return (t.Equal(m.start) || t.After(m.start)) && t.Before(m.end)
	case config.MaintenanceKindCron:
		if m.schedule == nil {
			return false
		}
		prev := m.schedule.Next(t.Add(-m.duration))
		if prev.After(t) {
			return false
		}
		return t.Sub(prev) <= m.duration
	default:
		return false
	}
}

func parseMaintenance(specs []config.MaintenanceSpec, loc *time.Location, defaultDuration time.Duration) ([]maintenanceWindow, error) {
	result := make([]maintenanceWindow, 0, len(specs))
	for _, spec := range specs {
		switch spec.Kind {
		case config.MaintenanceKindRange:
			start, end, err := parseRange(spec.Expr, loc)
			if err != nil {
				return nil, err
			}
			result = append(result, maintenanceWindow{
				kind:  config.MaintenanceKindRange,
				start: start,
				end:   end,
			})
		case config.MaintenanceKindCron:
			schedule, err := cron.ParseStandard(spec.Expr)
			if err != nil {
				return nil, fmt.Errorf("parse cron %q: %w", spec.Expr, err)
			}
			duration := defaultDuration
			if duration == 0 {
				duration = time.Hour
			}
			result = append(result, maintenanceWindow{
				kind:     config.MaintenanceKindCron,
				schedule: schedule,
				duration: duration,
			})
		default:
			return nil, fmt.Errorf("unsupported maintenance kind %q", spec.Kind)
		}
	}
	return result, nil
}

func parseRange(expr string, loc *time.Location) (time.Time, time.Time, error) {
	parts := splitRange(expr)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid range %q", expr)
	}
	layout := "2006-01-02T15:04"
	start, err := time.ParseInLocation(layout, parts[0], loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse range start: %w", err)
	}
	end, err := time.ParseInLocation(layout, parts[1], loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse range end: %w", err)
	}
	return start, end, nil
}

func splitRange(expr string) []string {
	chunks := splitN(expr, "-", 6)
	if len(chunks) < 6 {
		return nil
	}
	start := strings.Join(chunks[:3], "-")
	end := strings.Join(chunks[3:], "-")
	return []string{start, end}
}

func splitN(s, sep string, n int) []string {
	result := make([]string, 0, n)
	for len(result) < n-1 {
		idx := strings.Index(s, sep)
		if idx < 0 {
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
	}
	result = append(result, s)
	return result
}

func summarizeResult(result checks.Result) string {
	if result.Success {
		return "Check succeeded"
	}
	if result.Error != nil {
		return result.Error.Error()
	}
	var failed []string
	for _, assertion := range result.AssertionResults {
		if !assertion.Passed {
			if assertion.Message != "" {
				failed = append(failed, assertion.Message)
			} else {
				failed = append(failed, fmt.Sprintf("%s %s failed", assertion.Kind, assertion.Op))
			}
		}
	}
	if len(failed) == 0 {
		return "check failed"
	}
	return strings.Join(failed, "; ")
}

func (r *Runner) shouldLogRuns(check config.CheckConfig) bool {
	if check.LogRuns != nil {
		return *check.LogRuns
	}
	return r.defaults.LogRuns
}

func (r *Runner) logRun(check config.CheckConfig, result checks.Result) {
	if !r.shouldLogRuns(check) {
		return
	}
	attrs := []any{
		"check_id", check.ID,
		"success", result.Success,
		"latency", result.Latency,
	}
	if result.Error != nil {
		attrs = append(attrs, "error", result.Error.Error())
	}
	failures := 0
	for _, assertion := range result.AssertionResults {
		if !assertion.Passed {
			failures++
		}
	}
	if failures > 0 {
		attrs = append(attrs, "failed_assertions", failures)
	}
	r.logger.Info("check run", attrs...)
}

func (r *Runner) persistCheckState(check config.CheckConfig, result checks.Result) {
	if r.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	latency := result.Latency
	if latency == 0 && !result.CompletedAt.IsZero() && !result.StartedAt.IsZero() {
		latency = result.CompletedAt.Sub(result.StartedAt)
	}
	occurredAt := result.CompletedAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	errText := ""
	if result.Error != nil {
		errText = result.Error.Error()
	}

	run := storage.CheckRun{
		CheckID:    check.ID,
		CheckName:  check.Name,
		Success:    result.Success,
		Summary:    summarizeResult(result),
		Error:      errText,
		Latency:    latency,
		OccurredAt: occurredAt,
	}
	if err := r.store.RecordCheckRun(ctx, run); err != nil {
		r.logger.Error("failed to record check state", "check_id", check.ID, "error", err)
	}
}

func (r *Runner) recordNotification(notifierID string, event notifier.Event) {
	if r.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}

	logEntry := storage.NotificationLog{
		NotifierID: notifierID,
		CheckID:    event.Check.ID,
		CheckName:  event.Check.Name,
		RunID:      event.RunID,
		Status:     event.Status,
		Severity:   event.Severity,
		Summary:    event.Summary,
		Labels:     event.Labels,
		OccurredAt: occurredAt,
	}

	if err := r.store.RecordNotification(ctx, logEntry); err != nil {
		r.logger.Error("failed to record notification", "notifier_id", notifierID, "check_id", event.Check.ID, "error", err)
	}
}

func applyAssertionSets(cfg *config.Config) error {
	if len(cfg.CheckAssertionSets) == 0 {
		return nil
	}
	for i := range cfg.Checks {
		check := &cfg.Checks[i]
		if len(check.AssertionSets) == 0 {
			continue
		}
		var combined []config.Assertion
		for _, setName := range check.AssertionSets {
			set, ok := cfg.CheckAssertionSets[setName]
			if !ok {
				return fmt.Errorf("check %q references unknown assertion_set %q", check.ID, setName)
			}
			combined = append(combined, cloneAssertions(set)...)
		}
		combined = append(combined, check.Assertions...)
		check.Assertions = combined
	}
	return nil
}

func cloneAssertions(src []config.Assertion) []config.Assertion {
	if len(src) == 0 {
		return nil
	}
	cloned := make([]config.Assertion, len(src))
	copy(cloned, src)
	return cloned
}
