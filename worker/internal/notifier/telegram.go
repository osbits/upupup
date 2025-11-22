package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TelegramConfig configures Telegram notifications.
type TelegramConfig struct {
	BotTokenRef string `mapstructure:"bot_token_ref"`
	ChatID      string `mapstructure:"chat_id"`
	ParseMode   string `mapstructure:"parse_mode"`
}

type telegramNotifier struct {
	id     string
	cfg    TelegramConfig
	token  string
	client *http.Client
}

// NewTelegramNotifier constructs a Telegram notifier.
func NewTelegramNotifier(id string, cfg TelegramConfig, secrets map[string]string) (Notifier, error) {
	token, ok := secrets[cfg.BotTokenRef]
	if cfg.BotTokenRef != "" && !ok {
		return nil, fmt.Errorf("missing secret %q", cfg.BotTokenRef)
	}
	return &telegramNotifier{
		id:    id,
		cfg:   cfg,
		token: token,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (t *telegramNotifier) ID() string {
	return t.id
}

func (t *telegramNotifier) Notify(ctx context.Context, event Event) error {
	text := fmt.Sprintf("*%s* %s\nStatus: %s\nSeverity: %s\nRun: `%s`",
		event.Check.Name,
		event.Summary,
		strings.ToUpper(event.Status),
		strings.ToUpper(event.Severity),
		event.RunID,
	)
	payload := map[string]interface{}{
		"chat_id": t.cfg.ChatID,
		"text":    text,
	}
	parseMode := t.cfg.ParseMode
	if parseMode == "" {
		parseMode = "Markdown"
	}
	payload["parse_mode"] = parseMode
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram response: %s", resp.Status)
	}
	return nil
}
