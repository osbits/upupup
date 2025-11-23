package app

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checkID := chi.URLParam(r, "checkID")
	if checkID == "" {
		http.Error(w, "check id is required", http.StatusBadRequest)
		return
	}
	check, ok := a.checkConfigs[checkID]
	if !ok {
		http.NotFound(w, r)
		return
	}

	lastRun, err := a.store.LatestCheckRun(ctx, checkID)
	if err != nil {
		http.Error(w, "failed to load check data: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if lastRun == nil {
		http.Error(w, "no check data available", http.StatusNotFound)
		return
	}

	now := time.Now().UTC()
	interval := a.effectiveInterval(check)
	if interval <= 0 {
		interval = 60 * time.Second
	}
	window := interval * time.Duration(a.healthCfg.MaxIntervalMultiplier)
	if window <= 0 {
		window = interval
	}
	since := now.Add(-window)

	total, failed, err := a.store.RecentOutcomeCounts(ctx, checkID, since)
	if err != nil {
		http.Error(w, "failed to aggregate check data: "+err.Error(), http.StatusInternalServerError)
		return
	}

	namespace := a.metricsCfg.Namespace
	if namespace == "" {
		namespace = "upupup"
	}

	builder := &strings.Builder{}
	labelPairs := []string{
		fmt.Sprintf(`check_id="%s"`, promLabelValue(checkID)),
		fmt.Sprintf(`check_name="%s"`, promLabelValue(check.Name)),
	}
	if len(check.Labels) > 0 {
		keys := make([]string, 0, len(check.Labels))
		for k := range check.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := check.Labels[key]
			labelPairs = append(labelPairs, fmt.Sprintf(`%s="%s"`, promLabelKey(key), promLabelValue(value)))
		}
	}
	labels := strings.Join(labelPairs, ",")
	fmt.Fprintf(builder, "# HELP %s_check_status Last check status (1=success)\n", namespace)
	fmt.Fprintf(builder, "# TYPE %s_check_status gauge\n", namespace)
	fmt.Fprintf(builder, "%s_check_status{%s} %.0f\n\n", namespace, labels, boolToFloat(lastRun.Success))

	fmt.Fprintf(builder, "# HELP %s_check_last_run_timestamp_seconds Unix time of last check run\n", namespace)
	fmt.Fprintf(builder, "# TYPE %s_check_last_run_timestamp_seconds gauge\n", namespace)
	fmt.Fprintf(builder, "%s_check_last_run_timestamp_seconds{%s} %.0f\n\n", namespace, labels, float64(lastRun.OccurredAt.Unix()))

	fmt.Fprintf(builder, "# HELP %s_check_latency_seconds Last check latency in seconds\n", namespace)
	fmt.Fprintf(builder, "# TYPE %s_check_latency_seconds gauge\n", namespace)
	fmt.Fprintf(builder, "%s_check_latency_seconds{%s} %.6f\n\n", namespace, labels, float64(lastRun.Latency.Seconds()))

	fmt.Fprintf(builder, "# HELP %s_check_recent_window_seconds Observation window for recent counts\n", namespace)
	fmt.Fprintf(builder, "# TYPE %s_check_recent_window_seconds gauge\n", namespace)
	fmt.Fprintf(builder, "%s_check_recent_window_seconds{%s} %.0f\n\n", namespace, labels, window.Seconds())

	fmt.Fprintf(builder, "# HELP %s_check_recent_total Total runs within the observation window\n", namespace)
	fmt.Fprintf(builder, "# TYPE %s_check_recent_total gauge\n", namespace)
	fmt.Fprintf(builder, "%s_check_recent_total{%s} %d\n\n", namespace, labels, total)

	fmt.Fprintf(builder, "# HELP %s_check_recent_failures Failed runs within the observation window\n", namespace)
	fmt.Fprintf(builder, "# TYPE %s_check_recent_failures gauge\n", namespace)
	fmt.Fprintf(builder, "%s_check_recent_failures{%s} %d\n", namespace, labels, failed)

	if check.Metrics != nil {
		nodeID := strings.TrimSpace(check.Metrics.NodeID)
		if nodeID == "" {
			nodeID = strings.TrimSpace(check.Target)
		}
		if nodeID != "" {
			snapshot, err := a.store.LatestNodeMetrics(ctx, nodeID)
			if err != nil {
				http.Error(w, "failed to load node metrics: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if snapshot != nil && snapshot.Payload != "" {
				decoratedPayload := ensureCheckIDLabel(snapshot.Payload, nodeID)
				builder.WriteString("\n")
				ingestedAt := snapshot.IngestedAt.UTC()
				timestamp := "unknown"
				if !ingestedAt.IsZero() {
					timestamp = ingestedAt.Format(time.RFC3339)
				}
				fmt.Fprintf(builder, "# Raw metrics from node %s (ingested_at=%s)\n", promLabelValue(nodeID), timestamp)
				builder.WriteString(decoratedPayload)
				if !strings.HasSuffix(decoratedPayload, "\n") {
					builder.WriteString("\n")
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(builder.String()))
}

func promLabelValue(input string) string {
	replacer := strings.NewReplacer("\\", `\\`, "\n", `\n`, "\"", `\"`)
	return replacer.Replace(input)
}

func promLabelKey(input string) string {
	if input == "" {
		return "_"
	}
	var builder strings.Builder
	for i, r := range input {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
		if i == 0 && (r >= '0' && r <= '9') {
			valid = false
		}
		if valid {
			builder.WriteRune(r)
		} else {
			builder.WriteRune('_')
		}
	}
	return builder.String()
}

func boolToFloat(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

func ensureCheckIDLabel(payload string, checkID string) string {
	checkID = strings.TrimSpace(checkID)
	if payload == "" || checkID == "" {
		return payload
	}
	checkLabel := fmt.Sprintf(`check_id="%s"`, promLabelValue(checkID))
	lines := strings.Split(payload, "\n")
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		leading := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		rest := strings.TrimLeft(line, " \t")

		whitespacePos := strings.IndexAny(rest, " \t")
		if whitespacePos == -1 {
			continue
		}

		metricPart := rest[:whitespacePos]
		valuePart := rest[whitespacePos:]

		if strings.Contains(metricPart, "check_id=") {
			lines[idx] = line
			continue
		}

		if bracePos := strings.Index(metricPart, "{"); bracePos != -1 {
			closePos := strings.LastIndex(metricPart, "}")
			if closePos == -1 || closePos < bracePos {
				lines[idx] = line
				continue
			}
			prefix := metricPart[:closePos]
			suffix := metricPart[closePos:]
			if strings.HasSuffix(prefix, "{") {
				metricPart = prefix + checkLabel + suffix
			} else {
				metricPart = prefix + "," + checkLabel + suffix
			}
		} else {
			metricPart = metricPart + "{" + checkLabel + "}"
		}

		lines[idx] = leading + metricPart + valuePart
	}
	return strings.Join(lines, "\n")
}
