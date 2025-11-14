package notifier

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/osbits/upupup/internal/render"
)

// WebhookConfig represents a generic webhook notifier.
type WebhookConfig struct {
	URL      string            `mapstructure:"url"`
	Method   string            `mapstructure:"method"`
	Headers  map[string]string `mapstructure:"headers"`
	Template string            `mapstructure:"template"`
}

type webhookNotifier struct {
	id       string
	cfg      WebhookConfig
	secrets  map[string]string
	renderer *render.Engine
	client   *http.Client
}

// NewWebhookNotifier creates a webhook notifier.
func NewWebhookNotifier(id string, cfg WebhookConfig, secrets map[string]string, engine *render.Engine) (Notifier, error) {
	return &webhookNotifier{
		id:       id,
		cfg:      cfg,
		secrets:  secrets,
		renderer: engine,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (w *webhookNotifier) ID() string {
	return w.id
}

func (w *webhookNotifier) Notify(ctx context.Context, event Event) error {
	method := w.cfg.Method
	if method == "" {
		method = http.MethodPost
	}
	data := map[string]interface{}{
		"check": map[string]interface{}{
			"id":     event.Check.ID,
			"name":   event.Check.Name,
			"target": event.Check.Target,
		},
		"status":      event.Status,
		"severity":    event.Severity,
		"summary":     event.Summary,
		"labels":      event.Labels,
		"run_id":      event.RunID,
		"occurred_at": event.OccurredAt.Format(time.RFC3339),
		"first_failure_at": func() interface{} {
			if event.FirstFailureAt.IsZero() {
				return nil
			}
			return event.FirstFailureAt.Format(time.RFC3339)
		}(),
		"ui": map[string]interface{}{
			"check_url": fmt.Sprintf("https://monitoring.local/checks/%s", event.Check.ID),
		},
	}
	ctxRender := render.TemplateContext{
		Secrets: w.secrets,
		Data:    data,
	}
	payload, err := w.renderer.RenderString(w.cfg.Template, ctxRender)
	if err != nil {
		return fmt.Errorf("render template: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.cfg.URL, bytes.NewBufferString(payload))
	if err != nil {
		return err
	}
	if len(w.cfg.Headers) > 0 {
		headers, err := render.RenderMap(w.cfg.Headers, ctxRender, w.renderer)
		if err != nil {
			return fmt.Errorf("render headers: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		if strings.HasPrefix(strings.TrimSpace(payload), "{") {
			req.Header.Set("Content-Type", "application/json")
		} else {
			req.Header.Set("Content-Type", "text/plain")
		}
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook response: %s", resp.Status)
	}
	return nil
}
