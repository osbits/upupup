package checks

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Knetic/govaluate"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/osbits/upupup/worker/internal/config"
)

func runMetrics(ctx context.Context, start time.Time, cfg config.CheckConfig, env Environment) Result {
	res := Result{
		CheckID:          cfg.ID,
		CheckName:        cfg.Name,
		StartedAt:        start,
		Metadata:         map[string]any{},
		AssertionResults: []AssertionResult{},
	}
	defer func() {
		res.CompletedAt = time.Now()
		res.Latency = res.CompletedAt.Sub(start)
	}()

	if env.Store == nil {
		res.Error = fmt.Errorf("metrics store not configured")
		return res
	}
	if cfg.Metrics == nil {
		res.Error = fmt.Errorf("metrics configuration missing")
		return res
	}
	if len(cfg.Metrics.Thresholds) == 0 {
		res.Error = fmt.Errorf("no metrics thresholds configured")
		return res
	}

	nodeID := strings.TrimSpace(cfg.Metrics.NodeID)
	if nodeID == "" {
		nodeID = strings.TrimSpace(cfg.Target)
	}
	if nodeID == "" {
		res.Error = fmt.Errorf("node id or target is required for metrics check")
		return res
	}
	res.Metadata["node_id"] = nodeID

	snapshot, err := env.Store.LatestNodeMetrics(ctx, nodeID)
	if err != nil {
		res.Error = fmt.Errorf("load node metrics: %w", err)
		return res
	}
	if snapshot == nil {
		res.Error = fmt.Errorf("no metrics available for node %q", nodeID)
		return res
	}
	res.Metadata["ingested_at"] = snapshot.IngestedAt

	if cfg.Metrics.MaxAge != nil && cfg.Metrics.MaxAge.Set {
		maxAge := cfg.Metrics.MaxAge.Duration
		if snapshot.IngestedAt.IsZero() || time.Since(snapshot.IngestedAt) > maxAge {
			res.AssertionResults = append(res.AssertionResults, AssertionResult{
				Kind:    "freshness",
				Op:      "max_age",
				Path:    "",
				Passed:  false,
				Message: fmt.Sprintf("metrics older than %s", maxAge),
			})
			res.Success = false
			return res
		}
	}

	families, err := parseMetricFamilies(snapshot.Payload)
	if err != nil {
		res.Error = fmt.Errorf("parse metrics payload: %w", err)
		return res
	}

	computedCache := make(map[string]computedMetricResult)
	for _, threshold := range cfg.Metrics.Thresholds {
		assertion := evaluateMetricThreshold(families, cfg.Metrics.Computed, computedCache, threshold)
		res.AssertionResults = append(res.AssertionResults, assertion)
	}
	res.Success = allPassed(res.AssertionResults)
	return res
}

func parseMetricFamilies(payload string) (map[string]*dto.MetricFamily, error) {
	var parser expfmt.TextParser
	reader := strings.NewReader(payload)
	families, err := parser.TextToMetricFamilies(reader)
	if err != nil {
		return nil, err
	}
	return families, nil
}

type computedMetricResult struct {
	value float64
	err   error
}

func evaluateMetricThreshold(
	families map[string]*dto.MetricFamily,
	computed map[string]config.ComputedMetric,
	cache map[string]computedMetricResult,
	threshold config.MetricThreshold,
) AssertionResult {
	labelPath := formatLabelSet(threshold.Labels)
	result := AssertionResult{
		Kind: threshold.Name,
		Op:   threshold.Op,
		Path: labelPath,
	}
	if strings.TrimSpace(threshold.Name) == "" {
		result.Passed = false
		result.Message = "metric name is required"
		return result
	}

	if spec, ok := computed[threshold.Name]; ok {
		if len(spec.Labels) > 0 && !labelsEqual(spec.Labels, threshold.Labels) {
			result.Passed = false
			result.Message = "threshold labels do not match computed metric labels"
			return result
		}
		compResult := resolveComputedMetric(threshold.Name, families, computed, cache)
		if compResult.err != nil {
			result.Passed = false
			result.Message = compResult.err.Error()
			return result
		}
		return evaluateNumericThreshold(result, compResult.value, threshold)
	}

	family, ok := families[threshold.Name]
	if !ok {
		result.Passed = false
		result.Message = "metric not found"
		return result
	}
	value, found, err := findMetricValue(family, threshold.Labels)
	if err != nil {
		result.Passed = false
		result.Message = err.Error()
		return result
	}
	if !found {
		result.Passed = false
		result.Message = "no series matched labels"
		return result
	}
	return evaluateNumericThreshold(result, value, threshold)
}

func evaluateNumericThreshold(result AssertionResult, value float64, threshold config.MetricThreshold) AssertionResult {
	if compareFloats(value, threshold.Value, threshold.Op) {
		result.Passed = true
		return result
	}
	result.Passed = false
	result.Message = fmt.Sprintf("value %.4f not %s %.4f", value, threshold.Op, threshold.Value)
	return result
}

func resolveComputedMetric(
	name string,
	families map[string]*dto.MetricFamily,
	computed map[string]config.ComputedMetric,
	cache map[string]computedMetricResult,
) computedMetricResult {
	if cached, ok := cache[name]; ok {
		return cached
	}
	spec, ok := computed[name]
	if !ok {
		res := computedMetricResult{err: fmt.Errorf("computed metric %q not defined", name)}
		cache[name] = res
		return res
	}

	exprStr := strings.TrimSpace(spec.Expression)
	if exprStr == "" {
		res := computedMetricResult{err: fmt.Errorf("computed metric %q missing expression", name)}
		cache[name] = res
		return res
	}
	if len(spec.Variables) == 0 {
		res := computedMetricResult{err: fmt.Errorf("computed metric %q has no variables", name)}
		cache[name] = res
		return res
	}

	vars := make(map[string]interface{}, len(spec.Variables))
	for varName, ref := range spec.Variables {
		if strings.TrimSpace(varName) == "" {
			res := computedMetricResult{err: fmt.Errorf("computed metric %q has empty variable name", name)}
			cache[name] = res
			return res
		}
		val, err := resolveMetricReference(families, ref)
		if err != nil {
			res := computedMetricResult{err: fmt.Errorf("variable %q: %w", varName, err)}
			cache[name] = res
			return res
		}
		vars[varName] = val
	}

	expr, err := govaluate.NewEvaluableExpression(exprStr)
	if err != nil {
		res := computedMetricResult{err: fmt.Errorf("parse expression: %w", err)}
		cache[name] = res
		return res
	}
	value, err := expr.Evaluate(vars)
	if err != nil {
		res := computedMetricResult{err: fmt.Errorf("evaluate expression: %w", err)}
		cache[name] = res
		return res
	}
	floatVal, ok := toFloat64(value)
	if !ok || math.IsNaN(floatVal) || math.IsInf(floatVal, 0) {
		res := computedMetricResult{err: fmt.Errorf("expression result is not a finite number")}
		cache[name] = res
		return res
	}
	res := computedMetricResult{value: floatVal}
	cache[name] = res
	return res
}

func resolveMetricReference(families map[string]*dto.MetricFamily, ref config.MetricReference) (float64, error) {
	if strings.TrimSpace(ref.Name) == "" {
		return 0, fmt.Errorf("metric name is required")
	}
	family, ok := families[ref.Name]
	if !ok {
		if ref.Default != nil {
			return *ref.Default, nil
		}
		return 0, fmt.Errorf("metric %q not found", ref.Name)
	}
	value, found, err := findMetricValue(family, ref.Labels)
	if err != nil {
		return 0, err
	}
	if !found {
		if ref.Default != nil {
			return *ref.Default, nil
		}
		return 0, fmt.Errorf("no series matched labels %s", formatLabelSet(ref.Labels))
	}
	return value, nil
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, val := range a {
		if b[key] != val {
			return false
		}
	}
	return true
}

func toFloat64(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case int16:
		return float64(v), true
	case int8:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint64:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint8:
		return float64(v), true
	default:
		return 0, false
	}
}

func findMetricValue(family *dto.MetricFamily, labels map[string]string) (float64, bool, error) {
	if family == nil {
		return 0, false, nil
	}
	for _, metric := range family.Metric {
		if !labelsMatch(metric, labels) {
			continue
		}
		switch family.GetType() {
		case dto.MetricType_COUNTER:
			if metric.Counter == nil {
				return 0, false, fmt.Errorf("metric missing counter value")
			}
			return metric.Counter.GetValue(), true, nil
		case dto.MetricType_GAUGE:
			if metric.Gauge == nil {
				return 0, false, fmt.Errorf("metric missing gauge value")
			}
			return metric.Gauge.GetValue(), true, nil
		case dto.MetricType_UNTYPED:
			if metric.Untyped == nil {
				return 0, false, fmt.Errorf("metric missing untyped value")
			}
			return metric.Untyped.GetValue(), true, nil
		default:
			return 0, false, fmt.Errorf("unsupported metric type %s", family.GetType().String())
		}
	}
	return 0, false, nil
}

func labelsMatch(metric *dto.Metric, expected map[string]string) bool {
	if len(expected) == 0 {
		return true
	}
	for key, value := range expected {
		if !metricHasLabel(metric, key, value) {
			return false
		}
	}
	return true
}

func metricHasLabel(metric *dto.Metric, key, value string) bool {
	for _, labelPair := range metric.Label {
		if labelPair.GetName() == key && labelPair.GetValue() == value {
			return true
		}
	}
	return false
}

func formatLabelSet(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, key, labels[key]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
