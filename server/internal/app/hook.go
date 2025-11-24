package app

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/osbits/upupup/server/internal/hooks"
)

type hookRequestPayload struct {
	Duration          string            `json:"duration"`
	DurationSeconds   *int64            `json:"duration_seconds"`
	UntilFirstSuccess *bool             `json:"until_first_success"`
	Note              string            `json:"note"`
	RequestedBy       string            `json:"requested_by"`
	Metadata          map[string]string `json:"metadata"`
}

type hookResponsePayload struct {
	Status            string            `json:"status"`
	HookID            string            `json:"hook_id"`
	ExecutionID       int64             `json:"execution_id"`
	Kind              string            `json:"kind"`
	Scope             string            `json:"scope"`
	TargetIDs         []string          `json:"target_ids"`
	RequestedAt       time.Time         `json:"requested_at"`
	ActiveUntil       *time.Time        `json:"active_until,omitempty"`
	DurationSeconds   *int64            `json:"duration_seconds,omitempty"`
	UntilFirstSuccess bool              `json:"until_first_success"`
	RequestedBy       string            `json:"requested_by,omitempty"`
	RequestedFromIP   string            `json:"requested_from_ip,omitempty"`
	Note              string            `json:"note,omitempty"`
	Parameters        map[string]string `json:"parameters,omitempty"`
	Message           string            `json:"message,omitempty"`
}

func (a *App) handleHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hookID := chi.URLParam(r, "hookID")
	if hookID == "" {
		http.Error(w, "hook id is required", http.StatusBadRequest)
		return
	}
	cfg, ok := a.hookConfigs[hookID]
	if !ok {
		http.Error(w, "unknown hook", http.StatusNotFound)
		return
	}

	clientIPStr := a.clientIP(ctx)
	if allow, ok := a.hookAllowlist[hookID]; ok {
		ip := net.ParseIP(clientIPStr)
		if !allow.Allowed(ip) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	var payload hookRequestPayload
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && err != io.EOF {
			http.Error(w, "invalid json payload: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	var durationOverride *time.Duration
	if payload.Duration != "" {
		d, err := time.ParseDuration(payload.Duration)
		if err != nil {
			http.Error(w, "invalid duration: "+err.Error(), http.StatusBadRequest)
			return
		}
		durationOverride = &d
	}
	if durationOverride == nil && payload.DurationSeconds != nil {
		d := time.Duration(*payload.DurationSeconds) * time.Second
		durationOverride = &d
	}

	requestedBy := payload.RequestedBy
	if requestedBy == "" {
		if owner, ok := cfg.Metadata["owner"]; ok {
			requestedBy = owner
		}
	}
	if requestedBy == "" {
		requestedBy = clientIPStr
	}

	opts := hooks.InvokeOptions{
		DurationOverride:   durationOverride,
		UntilFirstSuccess:  payload.UntilFirstSuccess,
		Note:               payload.Note,
		RequestedBy:        requestedBy,
		RequestedFromIP:    clientIPStr,
		AdditionalMetadata: payload.Metadata,
	}

	exec, err := a.hookManager.Invoke(ctx, hookID, opts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := hookResponsePayload{
		Status:            "accepted",
		HookID:            hookID,
		ExecutionID:       exec.ID,
		Kind:              exec.Kind,
		Scope:             exec.Scope,
		TargetIDs:         exec.TargetIDs,
		RequestedAt:       exec.RequestedAt,
		UntilFirstSuccess: exec.UntilFirstSuccess,
		RequestedBy:       exec.RequestedBy,
		RequestedFromIP:   exec.RequestedFromIP,
		Note:              exec.Note,
		Parameters:        exec.Parameters,
		Message:           cfg.Description,
	}
	if exec.ActiveUntil.Valid {
		resp.ActiveUntil = &exec.ActiveUntil.Time
		duration := exec.ActiveUntil.Time.Sub(exec.RequestedAt)
		secs := int64(duration / time.Second)
		resp.DurationSeconds = &secs
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}
