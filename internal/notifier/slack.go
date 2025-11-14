package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SlackConfig defines Slack webhook integration.
type SlackConfig struct {
	WebhookURLRef string `mapstructure:"webhook_url_ref"`
	Channel       string `mapstructure:"channel"`
	Username      string `mapstructure:"username"`
}

type slackNotifier struct {
	id     string
	cfg    SlackConfig
	url    string
	client *http.Client
}

// NewSlackNotifier builds Slack notifier.
func NewSlackNotifier(id string, cfg SlackConfig, secrets map[string]string) (Notifier, error) {
	url, ok := secrets[cfg.WebhookURLRef]
	if cfg.WebhookURLRef != "" && !ok {
		return nil, fmt.Errorf("missing secret %q", cfg.WebhookURLRef)
	}
	return &slackNotifier{
		id:  id,
		cfg: cfg,
		url: url,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (s *slackNotifier) ID() string {
	return s.id
}

func (s *slackNotifier) Notify(ctx context.Context, event Event) error {
	payload := map[string]interface{}{
		"text": fmt.Sprintf("*%s* %s\nStatus: %s | Severity: %s | Run: %s",
			event.Check.Name,
			event.Summary,
			event.Status,
			event.Severity,
			event.RunID),
	}
	if s.cfg.Channel != "" {
		payload["channel"] = s.cfg.Channel
	}
	if s.cfg.Username != "" {
		payload["username"] = s.cfg.Username
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook: %s", resp.Status)
	}
	return nil
}
