package app

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/osbits/upupup/server/internal/config"
)

type readinessResponse struct {
	Status        string                   `json:"status"`
	GeneratedAt   time.Time                `json:"generated_at"`
	Configuration readinessConfigComponent `json:"configuration"`
}

type readinessConfigComponent struct {
	Status        string     `json:"status"`
	Detail        string     `json:"detail,omitempty"`
	Path          string     `json:"path,omitempty"`
	LastGenerated *time.Time `json:"last_generated,omitempty"`
	Checks        int        `json:"checks"`
	Targets       []string   `json:"targets,omitempty"`
}

func (a *App) handleReadiness(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()

	configStatus := a.prometheusConfigStatus()
	ready := configStatus.Status == statusOK

	resp := readinessResponse{
		Status:        configStatus.Status,
		GeneratedAt:   now,
		Configuration: configStatus,
	}

	statusCode := http.StatusOK
	if !ready {
		statusCode = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *App) initialisePrometheusConfig() {
	path := strings.TrimSpace(a.metricsCfg.ConfigPath)
	if path == "" {
		a.setPrometheusConfigStatus(time.Time{}, nil, fmt.Errorf("server.prometheus.config_path is required"))
		if a.logger != nil {
			a.logger.Warn("prometheus config path not configured; readiness will remain unready")
		}
		return
	}

	targets, err := a.generatePrometheusConfig()
	if err != nil {
		a.setPrometheusConfigStatus(time.Time{}, nil, err)
		if a.logger != nil {
			a.logger.Error("failed to generate prometheus scrape config", "error", err, "path", path)
		}
		return
	}

	now := time.Now().UTC()
	a.setPrometheusConfigStatus(now, targets, nil)
	if a.logger != nil {
		a.logger.Info("generated prometheus scrape config", "path", path, "checks", len(a.checkConfigs), "targets", targets)
	}
}

func (a *App) setPrometheusConfigStatus(ts time.Time, targets []string, err error) {
	a.promConfigMu.Lock()
	defer a.promConfigMu.Unlock()
	a.promConfigPath = strings.TrimSpace(a.metricsCfg.ConfigPath)
	a.promConfigAt = ts
	if targets != nil {
		a.promConfigTargets = append([]string(nil), targets...)
	} else {
		a.promConfigTargets = nil
	}
	a.promConfigErr = err
}

func (a *App) prometheusConfigStatus() readinessConfigComponent {
	a.promConfigMu.RLock()
	defer a.promConfigMu.RUnlock()

	component := readinessConfigComponent{
		Status: statusOK,
		Path:   a.promConfigPath,
		Checks: len(a.checkConfigs),
	}
	if len(a.promConfigTargets) > 0 {
		component.Targets = append([]string(nil), a.promConfigTargets...)
	}

	if component.Path == "" {
		component.Status = statusWarn
		component.Detail = "server.prometheus.config_path not configured"
	}

	if a.promConfigErr != nil {
		component.Status = statusCritical
		component.Detail = a.promConfigErr.Error()
		return component
	}

	if a.promConfigAt.IsZero() {
		if component.Detail == "" {
			component.Detail = "prometheus scrape config not generated yet"
		}
		if component.Status == statusOK {
			component.Status = statusWarn
		}
		return component
	}

	ts := a.promConfigAt
	component.LastGenerated = &ts
	return component
}

func combineStatuses(statuses ...string) string {
	result := statusOK
	for _, st := range statuses {
		switch st {
		case statusCritical:
			return statusCritical
		case statusWarn:
			if result == statusOK {
				result = statusWarn
			}
		}
	}
	return result
}

func (a *App) generatePrometheusConfig() ([]string, error) {
	path := strings.TrimSpace(a.metricsCfg.ConfigPath)
	if path == "" {
		return nil, fmt.Errorf("prometheus config path is empty")
	}

	targets := dedupeTargets(a.metricsCfg.Targets)
	if len(targets) == 0 {
		if fallback := deriveTargetFromListen(a.cfg.Server.Listen); fallback != "" {
			targets = []string{fallback}
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no prometheus scrape targets configured")
	}

	checkIDs := make([]string, 0, len(a.checkConfigs))
	for id := range a.checkConfigs {
		checkIDs = append(checkIDs, id)
	}
	sort.Strings(checkIDs)

	staticConfigs := make([]promStaticConfig, 0, len(checkIDs)*len(targets))
	for _, checkID := range checkIDs {
		for _, target := range targets {
			staticConfigs = append(staticConfigs, promStaticConfig{
				Targets: []string{target},
				Labels: map[string]string{
					"check_id": checkID,
				},
			})
		}
	}

	promCfg := promConfigFile{
		Global: buildGlobalConfig(a.metricsCfg),
		ScrapeConfigs: []promScrapeConfig{
			{
				JobName:        a.metricsCfg.JobName,
				Scheme:         a.metricsCfg.Scheme,
				ScrapeInterval: durationString(a.metricsCfg.ScrapeInterval.Duration, 0),
				StaticConfigs:  staticConfigs,
				RelabelConfigs: []promRelabelConfig{
					{
						SourceLabels: []string{"check_id"},
						Regex:        "(.*)",
						TargetLabel:  "__metrics_path__",
						Replacement:  "/api/metrics/$1",
					},
				},
			},
		},
	}

	data, err := yaml.Marshal(&promCfg)
	if err != nil {
		return nil, fmt.Errorf("marshal prometheus config: %w", err)
	}

	content := []byte(fmt.Sprintf("# Generated by upupup at %s\n", time.Now().UTC().Format(time.RFC3339)))
	content = append(content, data...)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("prepare config directory %q: %w", dir, err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return nil, fmt.Errorf("write temporary config: %w", err)
	}
	if err := atomicReplace(tmp, path); err != nil {
		return nil, err
	}

	return targets, nil
}

func dedupeTargets(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	var result []string
	for _, entry := range raw {
		target := strings.TrimSpace(entry)
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		result = append(result, target)
	}
	sort.Strings(result)
	return result
}

func deriveTargetFromListen(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		// already in host:port format or unix socket etc.
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

func buildGlobalConfig(cfg config.MetricsConfig) *promGlobalConfig {
	scrape := durationString(cfg.GlobalScrapeInterval.Duration, 30*time.Second)
	eval := durationString(cfg.GlobalEvaluationInterval.Duration, 30*time.Second)
	if scrape == "" && eval == "" {
		return nil
	}
	return &promGlobalConfig{
		ScrapeInterval:     scrape,
		EvaluationInterval: eval,
	}
}

func durationString(value time.Duration, fallback time.Duration) string {
	if value > 0 {
		return value.String()
	}
	if fallback > 0 {
		return fallback.String()
	}
	return ""
}

func atomicReplace(tmpPath, finalPath string) error {
	if err := os.Rename(tmpPath, finalPath); err != nil {
		if removeErr := os.Remove(finalPath); removeErr != nil && !os.IsNotExist(removeErr) {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("replace config: %w", err)
		}
		if err := os.Rename(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("replace config: %w", err)
		}
	}
	return nil
}

type promConfigFile struct {
	Global        *promGlobalConfig  `yaml:"global,omitempty"`
	ScrapeConfigs []promScrapeConfig `yaml:"scrape_configs"`
}

type promGlobalConfig struct {
	ScrapeInterval     string `yaml:"scrape_interval,omitempty"`
	EvaluationInterval string `yaml:"evaluation_interval,omitempty"`
}

type promScrapeConfig struct {
	JobName        string              `yaml:"job_name"`
	Scheme         string              `yaml:"scheme,omitempty"`
	ScrapeInterval string              `yaml:"scrape_interval,omitempty"`
	StaticConfigs  []promStaticConfig  `yaml:"static_configs,omitempty"`
	RelabelConfigs []promRelabelConfig `yaml:"relabel_configs,omitempty"`
}

type promStaticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels,omitempty"`
}

type promRelabelConfig struct {
	SourceLabels []string `yaml:"source_labels,omitempty"`
	Regex        string   `yaml:"regex,omitempty"`
	TargetLabel  string   `yaml:"target_label,omitempty"`
	Replacement  string   `yaml:"replacement,omitempty"`
}
