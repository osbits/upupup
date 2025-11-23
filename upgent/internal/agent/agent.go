package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/osbits/upupup/upgent/internal/config"
)

// Agent periodically scrapes a source endpoint and forwards the payload to the ingest API.
type Agent struct {
	cfg    *config.Config
	logger *slog.Logger
	client *http.Client
}

// New constructs an Agent instance with reasonable defaults.
func New(cfg *config.Config, logger *slog.Logger) (*Agent, error) {
	if cfg == nil {
		return nil, errors.New("config is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if cfg.SkipTLSVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // intentional opt-in via config
	}

	return &Agent{
		cfg:    cfg,
		logger: logger,
		client: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: transport,
		},
	}, nil
}

// Run starts the scrape/forward loop and blocks until context cancellation.
func (a *Agent) Run(ctx context.Context) error {
	a.logger.Info("starting upgent", "node_id", a.cfg.NodeID, "interval", a.cfg.Interval)

	if err := a.execute(ctx); err != nil && !errors.Is(err, context.Canceled) {
		a.logger.Error("initial scrape failed", "error", err)
	}

	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("shutting down")
			return ctx.Err()
		case <-ticker.C:
			if err := a.execute(ctx); err != nil && !errors.Is(err, context.Canceled) {
				a.logger.Error("scrape cycle failed", "error", err)
			}
		}
	}
}

func (a *Agent) execute(ctx context.Context) error {
	start := time.Now()

	payload, err := a.scrape(ctx)
	if err != nil {
		return err
	}

	if err := a.forward(ctx, payload); err != nil {
		return err
	}

	a.logger.Info("forwarded metrics", "bytes", len(payload), "duration", time.Since(start))
	return nil
}

func (a *Agent) scrape(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.ScrapeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build scrape request: %w", err)
	}
	req.Header.Set("User-Agent", a.cfg.UserAgent)
	req.Header.Set("Accept", "text/plain; version=0.0.4")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", a.cfg.ScrapeURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("scrape %s: unexpected status %d: %s", a.cfg.ScrapeURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	limited := io.LimitReader(resp.Body, a.cfg.MaxMetricsBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read scrape response: %w", err)
	}
	if int64(len(data)) > a.cfg.MaxMetricsBytes {
		return nil, fmt.Errorf("scrape payload exceeds %d bytes", a.cfg.MaxMetricsBytes)
	}
	return data, nil
}

func (a *Agent) forward(ctx context.Context, payload []byte) error {
	var body bytes.Buffer
	content := payload
	contentEncoding := ""

	if a.cfg.EnableGzip {
		gz := gzip.NewWriter(&body)
		if _, err := gz.Write(payload); err != nil {
			return fmt.Errorf("gzip payload: %w", err)
		}
		if err := gz.Close(); err != nil {
			return fmt.Errorf("finalize gzip payload: %w", err)
		}
		content = body.Bytes()
		contentEncoding = "gzip"
	} else {
		if _, err := body.Write(payload); err != nil {
			return fmt.Errorf("buffer payload: %w", err)
		}
		content = body.Bytes()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.IngestURL, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("build ingest request: %w", err)
	}

	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	req.Header.Set("User-Agent", a.cfg.UserAgent)
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("forward metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("ingest %s: unexpected status %d: %s", a.cfg.IngestURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
