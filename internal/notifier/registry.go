package notifier

import (
	"fmt"
	"strings"

	"github.com/mitchellh/mapstructure"
	"github.com/osbits/upupup/internal/config"
)

// Registry stores notifiers by ID.
type Registry struct {
	items map[string]Notifier
}

// NewRegistry creates a registry.
func NewRegistry() *Registry {
	return &Registry{
		items: map[string]Notifier{},
	}
}

// Add stores a notifier.
func (r *Registry) Add(n Notifier) error {
	if _, exists := r.items[n.ID()]; exists {
		return fmt.Errorf("duplicate notifier %q", n.ID())
	}
	r.items[n.ID()] = n
	return nil
}

// Get returns notifier by id.
func (r *Registry) Get(id string) (Notifier, bool) {
	n, ok := r.items[id]
	return n, ok
}

// Items returns map copy.
func (r *Registry) Items() map[string]Notifier {
	out := make(map[string]Notifier, len(r.items))
	for k, v := range r.items {
		out[k] = v
	}
	return out
}

// Build constructs notifiers from config.
func Build(factory Factory, configs []config.NotifierConfig) (*Registry, error) {
	reg := NewRegistry()
	for _, cfg := range configs {
		n, err := buildNotifier(factory, cfg)
		if err != nil {
			return nil, fmt.Errorf("notifier %q: %w", cfg.ID, err)
		}
		if err := reg.Add(n); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func buildNotifier(factory Factory, cfg config.NotifierConfig) (Notifier, error) {
	switch cfg.Type {
	case "email":
		var nc EmailConfig
		if err := decode(cfg.Config, &nc); err != nil {
			return nil, err
		}
		return NewEmailNotifier(cfg.ID, nc, factory.Secrets)
	case "sms":
		var nc TwilioSMSConfig
		if err := decode(cfg.Config, &nc); err != nil {
			return nil, err
		}
		switch strings.ToLower(nc.Provider) {
		case "twilio", "":
			return NewTwilioSMSNotifier(cfg.ID, nc, factory.Secrets)
		case "vonage":
			var vc VonageSMSConfig
			if err := decode(cfg.Config, &vc); err != nil {
				return nil, err
			}
			return NewVonageSMSNotifier(cfg.ID, vc, factory.Secrets)
		default:
			return nil, fmt.Errorf("unsupported sms provider %q", nc.Provider)
		}
	case "voice":
		var nc TwilioVoiceConfig
		if err := decode(cfg.Config, &nc); err != nil {
			return nil, err
		}
		switch strings.ToLower(nc.Provider) {
		case "twilio", "":
			return NewTwilioVoiceNotifier(cfg.ID, nc, factory.Secrets)
		case "vonage":
			var vc VonageVoiceConfig
			if err := decode(cfg.Config, &vc); err != nil {
				return nil, err
			}
			return NewVonageVoiceNotifier(cfg.ID, vc, factory.Secrets, factory.Render)
		default:
			return nil, fmt.Errorf("unsupported voice provider %q", nc.Provider)
		}
	case "webhook":
		var nc WebhookConfig
		if err := decode(cfg.Config, &nc); err != nil {
			return nil, err
		}
		return NewWebhookNotifier(cfg.ID, nc, factory.Secrets, factory.Render)
	case "slack":
		var nc SlackConfig
		if err := decode(cfg.Config, &nc); err != nil {
			return nil, err
		}
		return NewSlackNotifier(cfg.ID, nc, factory.Secrets)
	case "telegram":
		var nc TelegramConfig
		if err := decode(cfg.Config, &nc); err != nil {
			return nil, err
		}
		return NewTelegramNotifier(cfg.ID, nc, factory.Secrets)
	case "discord":
		var nc DiscordConfig
		if err := decode(cfg.Config, &nc); err != nil {
			return nil, err
		}
		return NewDiscordNotifier(cfg.ID, nc, factory.Secrets)
	default:
		return nil, fmt.Errorf("unsupported notifier type %q", cfg.Type)
	}
}

func decode(input map[string]interface{}, target interface{}) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		WeaklyTypedInput: true,
		Result:           target,
	})
	if err != nil {
		return err
	}
	return decoder.Decode(input)
}
