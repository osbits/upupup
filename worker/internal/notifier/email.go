package notifier

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/smtp"
	"strings"

	"github.com/jordan-wright/email"
)

// EmailConfig contains SMTP configuration.
type EmailConfig struct {
	SMTPHost    string   `mapstructure:"smtp_host"`
	SMTPPort    int      `mapstructure:"smtp_port"`
	Username    string   `mapstructure:"username"`
	PasswordRef string   `mapstructure:"password_ref"`
	From        string   `mapstructure:"from"`
	To          []string `mapstructure:"to"`
}

type emailNotifier struct {
	id       string
	cfg      EmailConfig
	password string
}

// NewEmailNotifier creates an email notifier.
func NewEmailNotifier(id string, cfg EmailConfig, secrets map[string]string) (Notifier, error) {
	pass, ok := secrets[cfg.PasswordRef]
	if cfg.PasswordRef != "" && !ok {
		return nil, fmt.Errorf("missing secret %q", cfg.PasswordRef)
	}
	return &emailNotifier{
		id:       id,
		cfg:      cfg,
		password: pass,
	}, nil
}

func (e *emailNotifier) ID() string {
	return e.id
}

func (e *emailNotifier) Notify(ctx context.Context, event Event) error {
	subject := fmt.Sprintf("[%s] %s", strings.ToUpper(event.Status), event.Check.Name)
	body := fmt.Sprintf("%s\n\nCheck: %s (%s)\nStatus: %s\nSeverity: %s\nSummary: %s\nRunID: %s\n",
		subject,
		event.Check.Name,
		event.Check.Target,
		event.Status,
		event.Severity,
		event.Summary,
		event.RunID,
	)
	em := email.NewEmail()
	em.From = e.cfg.From
	em.To = append([]string{}, e.cfg.To...)
	em.Subject = subject
	em.Text = []byte(body)

	addr := fmt.Sprintf("%s:%d", e.cfg.SMTPHost, e.cfg.SMTPPort)
	var auth smtp.Auth
	if e.cfg.Username != "" {
		auth = smtp.PlainAuth("", e.cfg.Username, e.password, e.cfg.SMTPHost)
	}
	tlsConfig := &tls.Config{
		ServerName: e.cfg.SMTPHost,
	}
	return em.SendWithTLS(addr, auth, tlsConfig)
}
