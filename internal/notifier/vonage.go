package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/osbits/upupup/internal/render"
)

// VonageSMSConfig configures Vonage SMS delivery.
type VonageSMSConfig struct {
	Provider      string   `mapstructure:"provider"`
	APIKey        string   `mapstructure:"api_key"`
	APIKeyRef     string   `mapstructure:"api_key_ref"`
	APISecret     string   `mapstructure:"api_secret"`
	APISecretRef  string   `mapstructure:"api_secret_ref"`
	From          string   `mapstructure:"from"`
	To            []string `mapstructure:"to"`
	MessagePrefix string   `mapstructure:"message_prefix"`
}

type vonageSMSNotifier struct {
	id        string
	cfg       VonageSMSConfig
	apiKey    string
	apiSecret string
	client    *http.Client
}

// NewVonageSMSNotifier constructs a Vonage (Nexmo) SMS notifier.
func NewVonageSMSNotifier(id string, cfg VonageSMSConfig, secrets map[string]string) (Notifier, error) {
	apiKey := cfg.APIKey
	if apiKey == "" && cfg.APIKeyRef != "" {
		if val, ok := secrets[cfg.APIKeyRef]; ok {
			apiKey = val
		} else {
			return nil, fmt.Errorf("vonage sms: missing secret %q", cfg.APIKeyRef)
		}
	}
	if apiKey == "" {
		return nil, fmt.Errorf("vonage sms: api_key or api_key_ref required")
	}
	apiSecret := cfg.APISecret
	if apiSecret == "" && cfg.APISecretRef != "" {
		if val, ok := secrets[cfg.APISecretRef]; ok {
			apiSecret = val
		} else {
			return nil, fmt.Errorf("vonage sms: missing secret %q", cfg.APISecretRef)
		}
	}
	if apiSecret == "" {
		return nil, fmt.Errorf("vonage sms: api_secret or api_secret_ref required")
	}
	return &vonageSMSNotifier{
		id:        id,
		cfg:       cfg,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (v *vonageSMSNotifier) ID() string {
	return v.id
}

func (v *vonageSMSNotifier) Notify(ctx context.Context, event Event) error {
	body := fmt.Sprintf("%s - %s (%s) status=%s severity=%s run=%s",
		event.Check.Name,
		event.Summary,
		event.Check.Target,
		event.Status,
		event.Severity,
		event.RunID,
	)
	if v.cfg.MessagePrefix != "" {
		body = fmt.Sprintf("%s %s", v.cfg.MessagePrefix, body)
	}
	for _, to := range v.cfg.To {
		if err := v.sendMessage(ctx, to, body); err != nil {
			return err
		}
	}
	return nil
}

func (v *vonageSMSNotifier) sendMessage(ctx context.Context, to, message string) error {
	endpoint := "https://rest.nexmo.com/sms/json"
	form := url.Values{}
	form.Set("api_key", v.apiKey)
	form.Set("api_secret", v.apiSecret)
	form.Set("to", to)
	form.Set("from", v.cfg.From)
	form.Set("text", message)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("vonage sms failed: %s", resp.Status)
	}
	return nil
}

// VonageVoiceConfig configures Vonage Voice calls.
type VonageVoiceConfig struct {
	Provider      string   `mapstructure:"provider"`
	JWT           string   `mapstructure:"jwt"`
	JWTRef        string   `mapstructure:"jwt_ref"`
	From          string   `mapstructure:"from"`
	To            []string `mapstructure:"to"`
	Message       string   `mapstructure:"message"`
	MessagePrefix string   `mapstructure:"message_prefix"`
}

type vonageVoiceNotifier struct {
	id       string
	cfg      VonageVoiceConfig
	jwt      string
	client   *http.Client
	renderer *render.Engine
	secrets  map[string]string
}

// NewVonageVoiceNotifier constructs a Vonage voice notifier.
func NewVonageVoiceNotifier(id string, cfg VonageVoiceConfig, secrets map[string]string, renderer *render.Engine) (Notifier, error) {
	jwt := cfg.JWT
	if jwt == "" && cfg.JWTRef != "" {
		if val, ok := secrets[cfg.JWTRef]; ok {
			jwt = val
		} else {
			return nil, fmt.Errorf("vonage voice: missing secret %q", cfg.JWTRef)
		}
	}
	if jwt == "" {
		return nil, fmt.Errorf("vonage voice: jwt or jwt_ref required")
	}
	return &vonageVoiceNotifier{
		id:       id,
		cfg:      cfg,
		jwt:      jwt,
		secrets:  secrets,
		renderer: renderer,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (v *vonageVoiceNotifier) ID() string {
	return v.id
}

func (v *vonageVoiceNotifier) Notify(ctx context.Context, event Event) error {
	message := v.composeMessage(event)
	for _, to := range v.cfg.To {
		if err := v.startCall(ctx, to, message); err != nil {
			return err
		}
	}
	return nil
}

func (v *vonageVoiceNotifier) composeMessage(event Event) string {
	if v.cfg.Message != "" && v.renderer != nil {
		ctx := render.TemplateContext{
			Secrets: v.secrets,
			Data: map[string]interface{}{
				"check":            event.Check,
				"status":           event.Status,
				"severity":         event.Severity,
				"summary":          event.Summary,
				"run_id":           event.RunID,
				"first_failure_at": event.FirstFailureAt,
				"labels":           event.Labels,
			},
		}
		if rendered, err := v.renderer.RenderString(v.cfg.Message, ctx); err == nil && rendered != "" {
			return rendered
		}
	}
	base := fmt.Sprintf("%s. Status %s. Severity %s. %s.",
		event.Check.Name,
		event.Status,
		event.Severity,
		event.Summary,
	)
	if v.cfg.MessagePrefix != "" {
		return fmt.Sprintf("%s %s", v.cfg.MessagePrefix, base)
	}
	return base
}

func (v *vonageVoiceNotifier) startCall(ctx context.Context, to, message string) error {
	payload := map[string]interface{}{
		"to": []map[string]string{
			{
				"type":   "phone",
				"number": to,
			},
		},
		"from": map[string]string{
			"type":   "phone",
			"number": v.cfg.From,
		},
		"ncco": []map[string]interface{}{
			{
				"action": "talk",
				"text":   message,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.nexmo.com/v1/calls", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.jwt)
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("vonage voice failed: %s", resp.Status)
	}
	return nil
}
