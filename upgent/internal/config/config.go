package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultScrapeURL       = "http://node-exporter:9100/metrics"
	defaultInterval        = 15 * time.Second
	defaultTimeout         = 10 * time.Second
	defaultMaxMetricsBytes = 2 * 1024 * 1024 // 2 MiB
	defaultUserAgent       = "upgent/0.1"
)

// Config represents runtime configuration for the agent.
type Config struct {
	NodeID          string
	ScrapeURL       string
	ServerBaseURL   string
	Interval        time.Duration
	Timeout         time.Duration
	MaxMetricsBytes int64
	EnableGzip      bool
	SkipTLSVerify   bool
	UserAgent       string
	IngestURL       string
}

// LoadFromEnv builds a Config from environment variables.
func LoadFromEnv() (*Config, error) {
	nodeID := strings.TrimSpace(os.Getenv("UPGENT_NODE_ID"))
	if nodeID == "" {
		return nil, errors.New("UPGENT_NODE_ID is required")
	}

	serverBase := strings.TrimSpace(os.Getenv("UPGENT_SERVER_URL"))
	if serverBase == "" {
		return nil, errors.New("UPGENT_SERVER_URL is required")
	}
	if _, err := url.ParseRequestURI(serverBase); err != nil {
		return nil, fmt.Errorf("invalid UPGENT_SERVER_URL: %w", err)
	}

	scrapeURL := strings.TrimSpace(os.Getenv("UPGENT_SCRAPE_URL"))
	if scrapeURL == "" {
		scrapeURL = defaultScrapeURL
	}
	if _, err := url.ParseRequestURI(scrapeURL); err != nil {
		return nil, fmt.Errorf("invalid UPGENT_SCRAPE_URL: %w", err)
	}

	interval, err := parseDurationEnv("UPGENT_INTERVAL", defaultInterval)
	if err != nil {
		return nil, err
	}
	timeout, err := parseDurationEnv("UPGENT_TIMEOUT", defaultTimeout)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, errors.New("UPGENT_TIMEOUT must be positive")
	}
	if interval <= 0 {
		return nil, errors.New("UPGENT_INTERVAL must be positive")
	}

	maxBytes, err := parseSizeEnv("UPGENT_MAX_METRICS_BYTES", defaultMaxMetricsBytes)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, errors.New("UPGENT_MAX_METRICS_BYTES must be positive")
	}

	enableGzip, err := parseBoolEnv("UPGENT_ENABLE_GZIP", true)
	if err != nil {
		return nil, err
	}
	skipTLS, err := parseBoolEnv("UPGENT_SKIP_TLS_VERIFY", false)
	if err != nil {
		return nil, err
	}

	userAgent := strings.TrimSpace(os.Getenv("UPGENT_USER_AGENT"))
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	ingestURL := buildIngestURL(serverBase, nodeID)

	cfg := &Config{
		NodeID:          nodeID,
		ScrapeURL:       scrapeURL,
		ServerBaseURL:   serverBase,
		Interval:        interval,
		Timeout:         timeout,
		MaxMetricsBytes: maxBytes,
		EnableGzip:      enableGzip,
		SkipTLSVerify:   skipTLS,
		UserAgent:       userAgent,
		IngestURL:       ingestURL,
	}
	return cfg, nil
}

func parseDurationEnv(name string, def time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return def, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	return d, nil
}

func parseSizeEnv(name string, def int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return def, nil
	}
	num, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	return num, nil
}

func parseBoolEnv(name string, def bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return def, nil
	}
	switch strings.ToLower(value) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid %s: expected boolean, got %q", name, value)
	}
}

func buildIngestURL(base, nodeID string) string {
	base = strings.TrimRight(base, "/")
	return fmt.Sprintf("%s/api/ingest/%s", base, url.PathEscape(nodeID))
}
