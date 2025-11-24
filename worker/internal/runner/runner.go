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

	hookCacheMu     sync.Mutex
	hookCache       []storage.HookExecution
	hookCacheExpiry time.Time

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
		Store:          r.store,
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
			state.StageState = map[int]stageNotificationState{}
			state.InitialNotified = false
			r.logger.Error("check entered failing state", "check_id", check.ID, "summary", summarizeResult(result))
		}
		r.sendInitialNotifications(check, state, result)
		r.sendEscalations(check, state, result)
	} else {
		if prevFailing {
			state.Failing = false
			r.completePauseHooks(check)
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
	if hooks := r.applicablePauseHooks(time.Now().UTC(), check); len(hooks) > 0 {
		r.logger.Info("skipping initial notifications due to active pause hook", "check_id", check.ID, "hooks", hookIDs(hooks))
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
	if state.StageState == nil {
		state.StageState = map[int]stageNotificationState{}
	}
	now := time.Now()
	if hooks := r.applicablePauseHooks(now.UTC(), check); len(hooks) > 0 {
		r.logger.Info("skipping escalation notifications due to active pause hook", "check_id", check.ID, "hooks", hookIDs(hooks))
		return
	}
	event := r.buildEvent(check, state, result, "firing")
	for idx, stage := range policy.Stages {
		elapsed := now.Sub(state.FirstFailure)
		if elapsed < stage.After.Duration {
			continue
		}

		stageState := state.StageState[idx]
		var every time.Duration
		if stage.Every != nil {
			every = stage.Every.Duration
		}

		switch {
		case every > 0:
			if stageState.LastSent.IsZero() || now.Sub(stageState.LastSent) >= every {
				r.dispatch(stage.Notifiers, event)
				stageState.Sent = true
				stageState.LastSent = now
				state.StageState[idx] = stageState
			}
		default:
			if stage.Every != nil && stageState.LastSent.IsZero() {
				r.logger.Warn("ignoring non-positive escalation frequency", "route", policy.ID, "stage_index", idx, "every", every)
			}
			if stageState.Sent {
				continue
			}
			r.dispatch(stage.Notifiers, event)
			stageState.Sent = true
			stageState.LastSent = now
			state.StageState[idx] = stageState
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
			history:    []bool{},
			StageState: map[int]stageNotificationState{},
		}
		r.state[checkID] = st
	}
	return st
}

func (r *Runner) applicablePauseHooks(now time.Time, check config.CheckConfig) []storage.HookExecution {
	hooks := r.activePauseHooks(now)
	if len(hooks) == 0 {
		return nil
	}
	result := make([]storage.HookExecution, 0, len(hooks))
	for _, hook := range hooks {
		if !strings.EqualFold(strings.TrimSpace(hook.Kind), "pause_notifications") {
			continue
		}
		if hookMatchesCheck(hook, check) {
			result = append(result, hook)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (r *Runner) activePauseHooks(now time.Time) []storage.HookExecution {
	hooks := r.fetchActiveHooks(now)
	if len(hooks) == 0 {
		return nil
	}

	var pauseHooks []storage.HookExecution
	var resumeHooks []storage.HookExecution

	for _, hook := range hooks {
		switch strings.ToLower(strings.TrimSpace(hook.Kind)) {
		case "pause_notifications":
			pauseHooks = append(pauseHooks, hook)
		case "resume_notifications":
			resumeHooks = append(resumeHooks, hook)
		}
	}

	if r.applyResumeHooks(resumeHooks, pauseHooks) {
		return r.activePauseHooks(now)
	}

	return pauseHooks
}

func (r *Runner) fetchActiveHooks(now time.Time) []storage.HookExecution {
	if r.store == nil {
		return nil
	}
	r.hookCacheMu.Lock()
	defer r.hookCacheMu.Unlock()

	if !r.hookCacheExpiry.IsZero() && time.Now().Before(r.hookCacheExpiry) {
		return append([]storage.HookExecution(nil), r.hookCache...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hooks, err := r.store.ActiveHookExecutions(ctx, now)
	if err != nil {
		r.logger.Error("failed to load active hooks", "error", err)
		r.hookCache = nil
		r.hookCacheExpiry = time.Now().Add(5 * time.Second)
		return nil
	}
	r.hookCache = hooks
	r.hookCacheExpiry = time.Now().Add(5 * time.Second)
	return append([]storage.HookExecution(nil), hooks...)
}

func (r *Runner) invalidateHookCache() {
	r.hookCacheMu.Lock()
	r.hookCache = nil
	r.hookCacheExpiry = time.Time{}
	r.hookCacheMu.Unlock()
}

func (r *Runner) applyResumeHooks(resumeHooks, pauseHooks []storage.HookExecution) bool {
	if len(resumeHooks) == 0 || r.store == nil {
		return false
	}

	completedPause := make(map[int64]bool, len(pauseHooks))
	var changed bool

	for _, resumeHook := range resumeHooks {
		matched := false
		for _, pauseHook := range pauseHooks {
			if completedPause[pauseHook.ID] {
				continue
			}
			if !hooksOverlap(resumeHook, pauseHook) {
				continue
			}
			matched = true
			if err := r.completeHookNow(pauseHook.ID); err != nil {
				r.logger.Error("failed to resume notifications", "resume_hook_id", resumeHook.HookID, "pause_hook_id", pauseHook.HookID, "error", err)
				continue
			}
			completedPause[pauseHook.ID] = true
			changed = true
			r.logger.Info("resumed notifications via hook", "resume_hook_id", resumeHook.HookID, "pause_hook_id", pauseHook.HookID)
		}
		if err := r.completeHookNow(resumeHook.ID); err != nil {
			r.logger.Error("failed to mark resume hook completed", "hook_id", resumeHook.HookID, "error", err)
		} else {
			changed = true
			if !matched {
				r.logger.Info("resume hook completed with no matching pause", "hook_id", resumeHook.HookID)
			}
		}
	}

	if changed {
		r.invalidateHookCache()
	}
	return changed
}

func hooksOverlap(resume, pause storage.HookExecution) bool {
	if targetMatches(resume.TargetIDs, "*") || targetMatches(pause.TargetIDs, "*") {
		return true
	}
	if len(resume.TargetIDs) == 0 {
		if strings.EqualFold(strings.TrimSpace(resume.Scope), "global") {
			return true
		}
	}
	for _, target := range resume.TargetIDs {
		id := strings.TrimSpace(target)
		if id == "" {
			continue
		}
		if targetMatches(pause.TargetIDs, id) {
			return true
		}
	}
	resumeScope := strings.ToLower(strings.TrimSpace(resume.Scope))
	pauseScope := strings.ToLower(strings.TrimSpace(pause.Scope))
	return resumeScope == "global" || pauseScope == "global" || resumeScope == pauseScope
}

func (r *Runner) completeHookNow(id int64) error {
	if r.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return r.store.CompleteHookExecution(ctx, id)
}

func (r *Runner) completePauseHooks(check config.CheckConfig) {
	hooks := r.applicablePauseHooks(time.Now().UTC(), check)
	if len(hooks) == 0 || r.store == nil {
		return
	}
	var anyCompleted bool
	for _, hook := range hooks {
		if !hook.UntilFirstSuccess {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(hook.Scope), "check") {
			continue
		}
		if err := r.store.CompleteHookExecution(context.Background(), hook.ID); err != nil {
			r.logger.Error("failed to complete pause hook", "hook_id", hook.HookID, "error", err)
			continue
		}
		anyCompleted = true
		r.logger.Info("completed pause hook after check recovery", "hook_id", hook.HookID, "check_id", check.ID)
	}
	if anyCompleted {
		r.invalidateHookCache()
	}
}

func hookMatchesCheck(hook storage.HookExecution, check config.CheckConfig) bool {
	scope := strings.ToLower(strings.TrimSpace(hook.Scope))
	switch scope {
	case "global":
		if len(hook.TargetIDs) == 0 {
			return true
		}
		return targetMatches(hook.TargetIDs, "*") || targetMatches(hook.TargetIDs, check.ID)
	case "check":
		return targetMatches(hook.TargetIDs, check.ID)
	case "route":
		return targetMatches(hook.TargetIDs, check.Notifications.Route)
	default:
		return targetMatches(hook.TargetIDs, check.ID)
	}
}

func targetMatches(targets []string, candidate string) bool {
	if len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		t := strings.TrimSpace(target)
		if t == "" {
			continue
		}
		if t == "*" {
			return true
		}
		if candidate != "" && t == candidate {
			return true
		}
	}
	return false
}

func hookIDs(hooks []storage.HookExecution) []string {
	if len(hooks) == 0 {
		return nil
	}
	ids := make([]string, 0, len(hooks))
	for _, hook := range hooks {
		ids = append(ids, hook.HookID)
	}
	return ids
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
	StageState      map[int]stageNotificationState
	InitialNotified bool
	LastResult      checks.Result
	LastUpdated     time.Time
	LastError       error
}

type stageNotificationState struct {
	Sent     bool
	LastSent time.Time
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
