package notifier

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TwilioSMSConfig configures Twilio SMS delivery.
type TwilioSMSConfig struct {
	Provider     string   `mapstructure:"provider"`
	AccountSID   string   `mapstructure:"account_sid"`
	AuthTokenRef string   `mapstructure:"auth_token_ref"`
	From         string   `mapstructure:"from"`
	To           []string `mapstructure:"to"`
}

type twilioSMSNotifier struct {
	id        string
	cfg       TwilioSMSConfig
	authToken string
	client    *http.Client
}

// NewTwilioSMSNotifier constructs a Twilio SMS notifier.
func NewTwilioSMSNotifier(id string, cfg TwilioSMSConfig, secrets map[string]string) (Notifier, error) {
	if cfg.Provider == "" {
		cfg.Provider = "twilio"
	}
	if strings.ToLower(cfg.Provider) != "twilio" {
		return nil, fmt.Errorf("unsupported sms provider %q", cfg.Provider)
	}
	token, ok := secrets[cfg.AuthTokenRef]
	if cfg.AuthTokenRef != "" && !ok {
		return nil, fmt.Errorf("missing secret %q", cfg.AuthTokenRef)
	}
	return &twilioSMSNotifier{
		id:        id,
		cfg:       cfg,
		authToken: token,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (t *twilioSMSNotifier) ID() string {
	return t.id
}

func (t *twilioSMSNotifier) Notify(ctx context.Context, event Event) error {
	body := fmt.Sprintf("%s - %s (%s) status=%s severity=%s run=%s",
		event.Check.Name,
		event.Summary,
		event.Check.Target,
		event.Status,
		event.Severity,
		event.RunID,
	)
	for _, to := range t.cfg.To {
		if err := t.sendMessage(ctx, to, body); err != nil {
			return err
		}
	}
	return nil
}

func (t *twilioSMSNotifier) sendMessage(ctx context.Context, to, body string) error {
	endpoint := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", t.cfg.AccountSID)
	form := url.Values{}
	form.Set("From", t.cfg.From)
	form.Set("To", to)
	form.Set("Body", body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(t.cfg.AccountSID, t.authToken)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("twilio sms failed: %s", resp.Status)
	}
	return nil
}

// TwilioVoiceConfig configures voice call notifier.
type TwilioVoiceConfig struct {
	Provider     string   `mapstructure:"provider"`
	AccountSID   string   `mapstructure:"account_sid"`
	AuthTokenRef string   `mapstructure:"auth_token_ref"`
	From         string   `mapstructure:"from"`
	To           []string `mapstructure:"to"`
	VoiceMessage string   `mapstructure:"voice_message"`
}

type twilioVoiceNotifier struct {
	id        string
	cfg       TwilioVoiceConfig
	authToken string
	client    *http.Client
}

// NewTwilioVoiceNotifier constructs a Twilio voice notifier.
func NewTwilioVoiceNotifier(id string, cfg TwilioVoiceConfig, secrets map[string]string) (Notifier, error) {
	if cfg.Provider == "" {
		cfg.Provider = "twilio"
	}
	if strings.ToLower(cfg.Provider) != "twilio" {
		return nil, fmt.Errorf("unsupported voice provider %q", cfg.Provider)
	}
	token, ok := secrets[cfg.AuthTokenRef]
	if cfg.AuthTokenRef != "" && !ok {
		return nil, fmt.Errorf("missing secret %q", cfg.AuthTokenRef)
	}
	return &twilioVoiceNotifier{
		id:        id,
		cfg:       cfg,
		authToken: token,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (t *twilioVoiceNotifier) ID() string {
	return t.id
}

func (t *twilioVoiceNotifier) Notify(ctx context.Context, event Event) error {
	body := fmt.Sprintf("%s. Status %s. Severity %s. %s.",
		event.Check.Name,
		event.Status,
		event.Severity,
		event.Summary,
	)
	if t.cfg.VoiceMessage != "" {
		body = t.cfg.VoiceMessage + " (Status " + event.Status + ")"
	}
	for _, to := range t.cfg.To {
		if err := t.startCall(ctx, to, body); err != nil {
			return err
		}
	}
	return nil
}

func (t *twilioVoiceNotifier) startCall(ctx context.Context, to, message string) error {
	endpoint := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Calls.json", t.cfg.AccountSID)
	form := url.Values{}
	form.Set("From", t.cfg.From)
	form.Set("To", to)
	form.Set("Twiml", fmt.Sprintf("<Response><Say>%s</Say></Response>", message))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(t.cfg.AccountSID, t.authToken)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("twilio voice failed: %s", resp.Status)
	}
	return nil
}
