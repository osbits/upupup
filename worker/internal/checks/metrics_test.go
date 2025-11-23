package checks

import (
	"context"
	"testing"
	"time"

	"github.com/osbits/upupup/worker/internal/config"
	"github.com/osbits/upupup/worker/internal/storage"
)

func TestRunMetricsSuccess(t *testing.T) {
	store, err := storage.Open(":memory:", storage.Options{})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	payload := `node_load1{instance="node-a"} 0.75
`
	err = store.UpsertNodeMetrics(context.Background(), storage.NodeMetricSnapshot{
		NodeID:     "node-a",
		Payload:    payload,
		IngestedAt: time.Now().Add(-1 * time.Minute),
		SourceIP:   "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}

	cfg := config.CheckConfig{
		ID:   "metrics-test",
		Name: "Metrics Check",
		Type: "metrics",
		Metrics: &config.MetricsCheck{
			NodeID: "node-a",
			Thresholds: []config.MetricThreshold{
				{
					Name:  "node_load1",
					Op:    "<",
					Value: 1.0,
					Labels: map[string]string{
						"instance": "node-a",
					},
				},
			},
		},
	}

	env := Environment{
		Defaults: config.ServiceDefault{},
		Store:    store,
	}

	result := Execute(context.Background(), cfg, env)
	if !result.Success {
		t.Fatalf("expected success, got failure: %+v", result)
	}
}

func TestRunMetricsThresholdFailure(t *testing.T) {
	store, err := storage.Open(":memory:", storage.Options{})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	payload := `node_load1{instance="node-a"} 2.5
`
	err = store.UpsertNodeMetrics(context.Background(), storage.NodeMetricSnapshot{
		NodeID:     "node-a",
		Payload:    payload,
		IngestedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}

	cfg := config.CheckConfig{
		ID:   "metrics-threshold",
		Name: "Metrics Threshold",
		Type: "metrics",
		Metrics: &config.MetricsCheck{
			NodeID: "node-a",
			Thresholds: []config.MetricThreshold{
				{
					Name:  "node_load1",
					Op:    "<",
					Value: 1.0,
					Labels: map[string]string{
						"instance": "node-a",
					},
				},
			},
		},
	}

	env := Environment{
		Defaults: config.ServiceDefault{},
		Store:    store,
	}

	result := Execute(context.Background(), cfg, env)
	if result.Success {
		t.Fatalf("expected failure, got success")
	}
	if len(result.AssertionResults) == 0 {
		t.Fatalf("expected assertion results")
	}
	if result.AssertionResults[0].Passed {
		t.Fatalf("expected assertion failure")
	}
}

func TestRunMetricsStaleData(t *testing.T) {
	store, err := storage.Open(":memory:", storage.Options{})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	payload := `node_load1{instance="node-a"} 0.5
`
	err = store.UpsertNodeMetrics(context.Background(), storage.NodeMetricSnapshot{
		NodeID:     "node-a",
		Payload:    payload,
		IngestedAt: time.Now().Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}

	cfg := config.CheckConfig{
		ID:   "metrics-stale",
		Name: "Metrics Stale",
		Type: "metrics",
		Metrics: &config.MetricsCheck{
			NodeID: "node-a",
			MaxAge: &config.NullableDuration{
				Duration: time.Minute,
				Set:      true,
			},
			Thresholds: []config.MetricThreshold{
				{
					Name:  "node_load1",
					Op:    "<",
					Value: 1.0,
					Labels: map[string]string{
						"instance": "node-a",
					},
				},
			},
		},
	}

	env := Environment{
		Defaults: config.ServiceDefault{},
		Store:    store,
	}

	result := Execute(context.Background(), cfg, env)
	if result.Success {
		t.Fatalf("expected failure due to stale data")
	}
	foundFreshness := false
	for _, assertion := range result.AssertionResults {
		if assertion.Kind == "freshness" {
			foundFreshness = true
		}
	}
	if !foundFreshness {
		t.Fatalf("expected freshness assertion failure")
	}
}

func TestRunMetricsComputedSuccess(t *testing.T) {
	store, err := storage.Open(":memory:", storage.Options{})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	payload := `node_filesystem_size_bytes{mountpoint="/"} 100
node_filesystem_avail_bytes{mountpoint="/"} 30
`
	err = store.UpsertNodeMetrics(context.Background(), storage.NodeMetricSnapshot{
		NodeID:     "node-a",
		Payload:    payload,
		IngestedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}

	cfg := config.CheckConfig{
		ID:   "metrics-computed-success",
		Name: "Metrics Computed Success",
		Type: "metrics",
		Metrics: &config.MetricsCheck{
			NodeID: "node-a",
			Computed: map[string]config.ComputedMetric{
				"disk_usage_percent": {
					Expression: "((size - avail) / size) * 100",
					Variables: map[string]config.MetricReference{
						"size": {
							Name: "node_filesystem_size_bytes",
							Labels: map[string]string{
								"mountpoint": "/",
							},
						},
						"avail": {
							Name: "node_filesystem_avail_bytes",
							Labels: map[string]string{
								"mountpoint": "/",
							},
						},
					},
				},
			},
			Thresholds: []config.MetricThreshold{
				{
					Name:  "disk_usage_percent",
					Op:    "<",
					Value: 80,
				},
			},
		},
	}

	env := Environment{
		Defaults: config.ServiceDefault{},
		Store:    store,
	}

	result := Execute(context.Background(), cfg, env)
	if !result.Success {
		t.Fatalf("expected success, got failure: %+v", result)
	}
}

func TestRunMetricsComputedFailure(t *testing.T) {
	store, err := storage.Open(":memory:", storage.Options{})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	payload := `node_filesystem_size_bytes{mountpoint="/"} 100
node_filesystem_avail_bytes{mountpoint="/"} 10
`
	err = store.UpsertNodeMetrics(context.Background(), storage.NodeMetricSnapshot{
		NodeID:     "node-a",
		Payload:    payload,
		IngestedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}

	cfg := config.CheckConfig{
		ID:   "metrics-computed-failure",
		Name: "Metrics Computed Failure",
		Type: "metrics",
		Metrics: &config.MetricsCheck{
			NodeID: "node-a",
			Computed: map[string]config.ComputedMetric{
				"disk_usage_percent": {
					Expression: "((size - avail) / size) * 100",
					Variables: map[string]config.MetricReference{
						"size": {
							Name: "node_filesystem_size_bytes",
							Labels: map[string]string{
								"mountpoint": "/",
							},
						},
						"avail": {
							Name: "node_filesystem_avail_bytes",
							Labels: map[string]string{
								"mountpoint": "/",
							},
						},
					},
				},
			},
			Thresholds: []config.MetricThreshold{
				{
					Name:  "disk_usage_percent",
					Op:    "<",
					Value: 80,
				},
			},
		},
	}

	env := Environment{
		Defaults: config.ServiceDefault{},
		Store:    store,
	}

	result := Execute(context.Background(), cfg, env)
	if result.Success {
		t.Fatalf("expected failure, got success")
	}
	if len(result.AssertionResults) == 0 {
		t.Fatalf("expected assertion results")
	}
	found := false
	for _, assertion := range result.AssertionResults {
		if assertion.Kind == "disk_usage_percent" && !assertion.Passed {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected computed metric assertion failure")
	}
}
