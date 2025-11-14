package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration to allow YAML unmarshalling from strings.
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		raw := strings.TrimSpace(value.Value)
		if raw == "" {
			d.Duration = 0
			return nil
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", raw, err)
		}
		d.Duration = parsed
		return nil
	default:
		return fmt.Errorf("duration must be a string, got %s", value.ShortTag())
	}
}

// NullableDuration allows distinguishing between zero and unset durations.
type NullableDuration struct {
	Duration time.Duration
	Set      bool
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *NullableDuration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode && strings.TrimSpace(value.Value) == "" {
		d.Set = false
		return nil
	}
	var tmp Duration
	if err := value.Decode(&tmp); err != nil {
		return err
	}
	d.Duration = tmp.Duration
	d.Set = true
	return nil
}

// Config is the root configuration.
type Config struct {
	Version              int                    `yaml:"version"`
	Service              ServiceConfig          `yaml:"service"`
	Secrets              map[string]SecretSpec  `yaml:"secrets"`
	Notifiers            []NotifierConfig       `yaml:"notifiers"`
	NotificationPolicies []NotificationPolicy   `yaml:"notification_policies"`
	CheckAssertionSets   map[string][]Assertion `yaml:"assertion_sets"`
	Checks               []CheckConfig          `yaml:"checks"`
	Templates            map[string]interface{} `yaml:"templates"`
}

// ServiceConfig contains global settings.
type ServiceConfig struct {
	Name     string         `yaml:"name"`
	Timezone string         `yaml:"timezone"`
	Defaults ServiceDefault `yaml:"defaults"`
}

// ServiceDefault defines default runtime values.
type ServiceDefault struct {
	Interval           Duration          `yaml:"interval"`
	Timeout            Duration          `yaml:"timeout"`
	Retries            int               `yaml:"retries"`
	Backoff            Duration          `yaml:"backoff"`
	MaintenanceWindows []MaintenanceSpec `yaml:"maintenance_windows"`
	LogRuns            bool              `yaml:"log_runs"`
}

// MaintenanceSpec includes cron or range expressions.
type MaintenanceSpec struct {
	Expr string
	Kind MaintenanceKind
}

// MaintenanceKind indicates the maintenance window type.
type MaintenanceKind string

const (
	MaintenanceKindCron  MaintenanceKind = "cron"
	MaintenanceKindRange MaintenanceKind = "range"
)

// UnmarshalYAML allows parsing "cron: ..." or "range: ...".
func (m *MaintenanceSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("maintenance spec must be scalar, got %s", value.ShortTag())
	}
	raw := strings.TrimSpace(value.Value)
	switch {
	case strings.HasPrefix(raw, "cron:"):
		m.Kind = MaintenanceKindCron
		m.Expr = strings.TrimSpace(strings.TrimPrefix(raw, "cron:"))
	case strings.HasPrefix(raw, "range:"):
		m.Kind = MaintenanceKindRange
		m.Expr = strings.TrimSpace(strings.TrimPrefix(raw, "range:"))
	default:
		return fmt.Errorf("unsupported maintenance spec %q", raw)
	}
	return nil
}

// SecretSpec defines how to resolve a secret.
type SecretSpec struct {
	Source string
	Value  string
}

// UnmarshalYAML parses secret definitions like "env:SMTP_PASSWORD".
func (s *SecretSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("secret must be scalar, got %s", value.ShortTag())
	}
	raw := strings.TrimSpace(value.Value)
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid secret spec %q", raw)
	}
	s.Source = strings.TrimSpace(parts[0])
	s.Value = strings.TrimSpace(parts[1])
	return nil
}

// ResolveSecrets resolves secrets into a map.
func (c *Config) ResolveSecrets() (map[string]string, error) {
	resolved := make(map[string]string, len(c.Secrets))
	for key, spec := range c.Secrets {
		switch spec.Source {
		case "env":
			val, ok := os.LookupEnv(spec.Value)
			if !ok {
				return nil, fmt.Errorf("missing env var %q for secret %q", spec.Value, key)
			}
			resolved[key] = val
		default:
			return nil, fmt.Errorf("unsupported secret source %q for secret %q", spec.Source, key)
		}
	}
	return resolved, nil
}

// NotifierConfig describes a notification endpoint.
type NotifierConfig struct {
	ID     string                 `yaml:"id"`
	Type   string                 `yaml:"type"`
	Config map[string]interface{} `yaml:"config"`
}

// NotificationPolicy describes an escalation chain.
type NotificationPolicy struct {
	ID               string            `yaml:"id"`
	Match            map[string]string `yaml:"match"`
	Stages           []PolicyStage     `yaml:"stages"`
	ResolveNotifiers []string          `yaml:"resolve_notifiers"`
}

// PolicyStage describes a notification stage.
type PolicyStage struct {
	After     Duration `yaml:"after"`
	Notifiers []string `yaml:"notifiers"`
}

// CheckConfig represents a check.
type CheckConfig struct {
	ID            string            `yaml:"id"`
	Name          string            `yaml:"name"`
	Type          string            `yaml:"type"`
	Target        string            `yaml:"target"`
	AssertionSets []string          `yaml:"assertion_sets"`
	Schedule      *CheckSchedule    `yaml:"schedule"`
	Request       *HTTPRequest      `yaml:"request"`
	PreAuth       *PreAuthConfig    `yaml:"preauth"`
	Assertions    []Assertion       `yaml:"assertions"`
	Thresholds    Thresholds        `yaml:"thresholds"`
	Labels        map[string]string `yaml:"labels"`
	Notifications CheckNotification `yaml:"notifications"`
	Resolver      string            `yaml:"resolver"`
	RecordType    string            `yaml:"record_type"`
	SNI           string            `yaml:"sni"`
	LogRuns       *bool             `yaml:"log_runs"`
}

// CheckSchedule customizing schedule per check.
type CheckSchedule struct {
	Interval *NullableDuration `yaml:"interval"`
	Timeout  *NullableDuration `yaml:"timeout"`
	Retries  *int              `yaml:"retries"`
	Backoff  *NullableDuration `yaml:"backoff"`
}

// HTTPRequest describes an HTTP request template.
type HTTPRequest struct {
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	Timeout *NullableDuration `yaml:"timeout"`
}

// PreAuthConfig defines an authentication flow prior to running the check.
type PreAuthConfig struct {
	Flow    string      `yaml:"flow"`
	Request HTTPRequest `yaml:"request"`
	Capture CaptureSpec `yaml:"capture"`
}

// CaptureSpec defines how to capture data from preauth responses.
type CaptureSpec struct {
	From string `yaml:"from"`
	Path string `yaml:"path"`
	As   string `yaml:"as"`
}

// Assertion expresses an expectation.
type Assertion struct {
	Kind  string      `yaml:"kind"`
	Op    string      `yaml:"op"`
	Path  string      `yaml:"path"`
	Value interface{} `yaml:"value"`
}

// Thresholds describes alerting thresholds.
type Thresholds struct {
	FailureRatio *FailureRatioThreshold `yaml:"failure_ratio"`
}

// FailureRatioThreshold triggers when failure ratio exceeds.
type FailureRatioThreshold struct {
	Window    int `yaml:"window"`
	FailCount int `yaml:"fail_count"`
}

// CheckNotification describes check-specific notification config.
type CheckNotification struct {
	Route     string                `yaml:"route"`
	Overrides *NotificationOverride `yaml:"overrides"`
}

// NotificationOverride overrides the policy for a check.
type NotificationOverride struct {
	InitialNotifiers []string `yaml:"initial_notifiers"`
}
