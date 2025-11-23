package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/osbits/upupup/server/internal/config"
)

type healthResponse struct {
	Status        string           `json:"status"`
	GeneratedAt   time.Time        `json:"generated_at"`
	Database      componentStatus  `json:"database"`
	Checks        []checkComponent `json:"checks"`
	Notifications componentStatus  `json:"notifications"`
	ActiveHooks   []hookComponent  `json:"hooks,omitempty"`
}

type componentStatus struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type checkComponent struct {
	CheckID          string          `json:"check_id"`
	Name             string          `json:"name"`
	Status           string          `json:"status"`
	Detail           string          `json:"detail,omitempty"`
	LastRun          *checkRunDetail `json:"last_run,omitempty"`
	RequiredRecent   int             `json:"required_recent"`
	RecentWithinSecs int64           `json:"recent_within_seconds"`
}

type checkRunDetail struct {
	Success    bool      `json:"success"`
	Summary    string    `json:"summary"`
	Error      string    `json:"error,omitempty"`
	LatencyMs  float64   `json:"latency_ms"`
	OccurredAt time.Time `json:"occurred_at"`
}

type hookComponent struct {
	HookID          string     `json:"hook_id"`
	Kind            string     `json:"kind"`
	Scope           string     `json:"scope"`
	TargetIDs       []string   `json:"target_ids"`
	RequestedBy     string     `json:"requested_by,omitempty"`
	RequestedFromIP string     `json:"requested_from_ip,omitempty"`
	RequestedAt     time.Time  `json:"requested_at"`
	ActiveUntil     *time.Time `json:"active_until,omitempty"`
	UntilSuccess    bool       `json:"until_first_success"`
}

const (
	statusOK       = "ok"
	statusWarn     = "warn"
	statusCritical = "critical"
)

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := a.healthSnapshot(ctx, time.Now().UTC())
	statusCode := http.StatusOK
	if resp.Status != statusOK {
		statusCode = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *App) healthSnapshot(ctx context.Context, now time.Time) healthResponse {
	dbComponent := componentStatus{Status: statusOK}
	if err := a.store.Ping(ctx); err != nil {
		dbComponent.Status = statusCritical
		dbComponent.Detail = err.Error()
	}

	checkStatuses := a.evaluateChecks(ctx, now)
	notificationStatus := a.evaluateNotifications(ctx)
	activeHooks := a.listActiveHooks(ctx, now)

	overall := deriveOverallStatus(dbComponent.Status, notificationStatus.Status, checkStatuses)

	return healthResponse{
		Status:        overall,
		GeneratedAt:   now,
		Database:      dbComponent,
		Checks:        checkStatuses,
		Notifications: notificationStatus,
		ActiveHooks:   activeHooks,
	}
}

func (a *App) evaluateChecks(ctx context.Context, now time.Time) []checkComponent {
	results := make([]checkComponent, 0, len(a.cfg.Checks))
	requiredRuns := a.healthCfg.RequiredRecentRuns
	multiplier := a.healthCfg.MaxIntervalMultiplier

	for _, check := range a.cfg.Checks {
		result := checkComponent{
			CheckID:        check.ID,
			Name:           check.Name,
			Status:         statusOK,
			RequiredRecent: requiredRuns,
		}
		interval := a.effectiveInterval(check)
		if interval <= 0 {
			interval = 60 * time.Second
		}
		window := interval * time.Duration(multiplier)
		if window <= 0 {
			window = interval
		}
		result.RecentWithinSecs = int64(window.Seconds())

		lastRun, err := a.store.LatestCheckRun(ctx, check.ID)
		if err != nil {
			result.Status = statusCritical
			result.Detail = err.Error()
			results = append(results, result)
			continue
		}
		if lastRun == nil {
			if a.healthCfg.SkipChecksWithNoHistory {
				result.Status = statusOK
				result.Detail = "no history yet - skipped"
			} else if a.healthCfg.FailOnMissingCheckState {
				result.Status = statusCritical
				result.Detail = "no check runs recorded"
			} else {
				result.Status = statusWarn
				result.Detail = "no check runs recorded"
			}
			results = append(results, result)
			continue
		}
		result.LastRun = &checkRunDetail{
			Success:    lastRun.Success,
			Summary:    lastRun.Summary,
			Error:      lastRun.Error,
			LatencyMs:  float64(lastRun.Latency.Milliseconds()),
			OccurredAt: lastRun.OccurredAt,
		}

		if window > 0 {
			since := now.Add(-window)
			count, err := a.store.CountRecentCheckRuns(ctx, check.ID, since)
			if err != nil {
				result.Status = statusCritical
				result.Detail = err.Error()
				results = append(results, result)
				continue
			}
			if count < requiredRuns {
				result.Status = statusWarn
				result.Detail = "insufficient recent check runs"
			}
		}

		if dt := now.Sub(lastRun.OccurredAt); dt > window {
			result.Status = statusWarn
			result.Detail = "last run exceeded expected interval"
		}
		if !lastRun.Success {
			if result.Status == statusOK {
				result.Status = statusWarn
			}
			result.Detail = appendDetail(result.Detail, "last run failed")
		}
		results = append(results, result)
	}
	return results
}

func (a *App) evaluateNotifications(ctx context.Context) componentStatus {
	status := componentStatus{Status: statusOK}
	lookback := a.healthCfg.NotificationErrorLookback
	logs, err := a.store.RecentNotificationLogs(ctx, lookback)
	if err != nil {
		status.Status = statusCritical
		status.Detail = err.Error()
		return status
	}
	if len(logs) == 0 {
		if !a.healthCfg.AllowNoNotifications {
			status.Status = statusWarn
			status.Detail = "no notifications recorded"
		}
		return status
	}
	errorStatuses := make(map[string]struct{}, len(a.healthCfg.NotificationErrorStatuses))
	for _, s := range a.healthCfg.NotificationErrorStatuses {
		errorStatuses[strings.ToLower(strings.TrimSpace(s))] = struct{}{}
	}
	for _, entry := range logs {
		if _, ok := errorStatuses[strings.ToLower(entry.Status)]; ok {
			status.Status = statusWarn
			status.Detail = appendDetail(status.Detail, "recent notification recorded failure status")
			break
		}
	}
	return status
}

func (a *App) listActiveHooks(ctx context.Context, now time.Time) []hookComponent {
	execs, err := a.hookManager.ListActive(ctx, now)
	if err != nil || len(execs) == 0 {
		return nil
	}
	result := make([]hookComponent, 0, len(execs))
	for _, exec := range execs {
		component := hookComponent{
			HookID:          exec.HookID,
			Kind:            exec.Kind,
			Scope:           exec.Scope,
			TargetIDs:       append([]string{}, exec.TargetIDs...),
			RequestedBy:     exec.RequestedBy,
			RequestedFromIP: exec.RequestedFromIP,
			RequestedAt:     exec.RequestedAt,
			UntilSuccess:    exec.UntilFirstSuccess,
		}
		if exec.ActiveUntil.Valid {
			component.ActiveUntil = &exec.ActiveUntil.Time
		}
		result = append(result, component)
	}
	return result
}

func deriveOverallStatus(databaseStatus string, notificationStatus string, checks []checkComponent) string {
	status := statusOK
	if databaseStatus == statusCritical || notificationStatus == statusCritical {
		return statusCritical
	}
	if databaseStatus == statusWarn || notificationStatus == statusWarn {
		status = statusWarn
	}
	for _, check := range checks {
		if check.Status == statusCritical {
			return statusCritical
		}
		if check.Status == statusWarn {
			status = statusWarn
		}
	}
	return status
}

func (a *App) effectiveInterval(check config.CheckConfig) time.Duration {
	if check.Schedule != nil && check.Schedule.Interval != nil && check.Schedule.Interval.Set {
		return check.Schedule.Interval.Duration
	}
	if a.serviceDefaults.Interval.Duration > 0 {
		return a.serviceDefaults.Interval.Duration
	}
	return 60 * time.Second
}

func appendDetail(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}
