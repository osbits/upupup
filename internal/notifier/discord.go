package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DiscordConfig configures Discord webhook.
type DiscordConfig struct {
	WebhookURLRef string `mapstructure:"webhook_url_ref"`
	Username      string `mapstructure:"username"`
}

type discordNotifier struct {
	id     string
	cfg    DiscordConfig
	url    string
	client *http.Client
}

// NewDiscordNotifier constructs Discord notifier.
func NewDiscordNotifier(id string, cfg DiscordConfig, secrets map[string]string) (Notifier, error) {
	url, ok := secrets[cfg.WebhookURLRef]
	if cfg.WebhookURLRef != "" && !ok {
		return nil, fmt.Errorf("missing secret %q", cfg.WebhookURLRef)
	}
	return &discordNotifier{
		id:  id,
		cfg: cfg,
		url: url,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (d *discordNotifier) ID() string {
	return d.id
}

func (d *discordNotifier) Notify(ctx context.Context, event Event) error {
	payload := map[string]interface{}{
		"content": fmt.Sprintf("**%s** %s\nStatus: %s | Severity: %s | Run: %s",
			event.Check.Name,
			event.Summary,
			event.Status,
			event.Severity,
			event.RunID),
	}
	if d.cfg.Username != "" {
		payload["username"] = d.cfg.Username
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook: %s", resp.Status)
	}
	return nil
}
