package checks

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-ping/ping"
	dnsclient "github.com/miekg/dns"
	"github.com/oliveagle/jsonpath"
	"github.com/osbits/upupup/internal/config"
	"github.com/osbits/upupup/internal/render"
	"golang.org/x/net/publicsuffix"
)

// Environment contains shared dependencies for running checks.
type Environment struct {
	Defaults       config.ServiceDefault
	Secrets        map[string]string
	TemplateEngine *render.Engine
	HttpClient     *http.Client
	TimeLocation   *time.Location
}

// Execute runs a check once.
func Execute(ctx context.Context, cfg config.CheckConfig, env Environment) Result {
	start := time.Now()
	timeout := effectiveTimeout(cfg, env.Defaults)
	// Align context with timeout.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	switch strings.ToLower(cfg.Type) {
	case "http", "https":
		return runHTTP(ctx, start, cfg, env)
	case "tcp":
		return runTCP(ctx, start, cfg, env)
	case "icmp":
		return runICMP(ctx, start, cfg, env)
	case "dns":
		return runDNS(ctx, start, cfg, env)
	case "tls":
		return runTLS(ctx, start, cfg, env)
	case "whois":
		return runWHOIS(ctx, start, cfg, env)
	default:
		return Result{
			CheckID:     cfg.ID,
			CheckName:   cfg.Name,
			StartedAt:   start,
			CompletedAt: time.Now(),
			Error:       fmt.Errorf("unsupported check type %q", cfg.Type),
			Success:     false,
		}
	}
}

func effectiveTimeout(cfg config.CheckConfig, defaults config.ServiceDefault) time.Duration {
	if cfg.Request != nil && cfg.Request.Timeout != nil && cfg.Request.Timeout.Set {
		return cfg.Request.Timeout.Duration
	}
	if cfg.Schedule != nil && cfg.Schedule.Timeout != nil && cfg.Schedule.Timeout.Set {
		return cfg.Schedule.Timeout.Duration
	}
	return defaults.Timeout.Duration
}

func effectiveRequestURL(cfg config.CheckConfig) string {
	if cfg.Request != nil && cfg.Request.URL != "" {
		return cfg.Request.URL
	}
	return cfg.Target
}

func runHTTP(ctx context.Context, start time.Time, cfg config.CheckConfig, env Environment) Result {
	res := Result{
		CheckID:   cfg.ID,
		CheckName: cfg.Name,
		StartedAt: start,
	}
	client := env.HttpClient
	if client == nil {
		client = &http.Client{Timeout: effectiveTimeout(cfg, env.Defaults)}
	}

	vars := map[string]string{}
	// Pre-authentication
	if cfg.PreAuth != nil {
		if err := executePreAuth(ctx, cfg, env, vars, client); err != nil {
			res.CompletedAt = time.Now()
			res.Error = fmt.Errorf("preauth failed: %w", err)
			return res
		}
	}

	target := effectiveRequestURL(cfg)
	reqMethod := "GET"
	if cfg.Request != nil && cfg.Request.Method != "" {
		reqMethod = cfg.Request.Method
	}

	templateData := map[string]interface{}{
		"vars":   vars,
		"labels": cfg.Labels,
		"check": map[string]interface{}{
			"id":     cfg.ID,
			"name":   cfg.Name,
			"target": cfg.Target,
		},
	}
	renderCtx := render.TemplateContext{
		Secrets: env.Secrets,
		Vars:    vars,
		Data:    templateData,
	}

	targetRendered, err := env.TemplateEngine.RenderString(target, renderCtx)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = fmt.Errorf("render target: %w", err)
		return res
	}
	req, err := http.NewRequestWithContext(ctx, reqMethod, targetRendered, nil)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = fmt.Errorf("build request: %w", err)
		return res
	}

	if cfg.Request != nil {
		if len(cfg.Request.Headers) > 0 {
			headers, err := render.RenderMap(cfg.Request.Headers, renderCtx, env.TemplateEngine)
			if err != nil {
				res.CompletedAt = time.Now()
				res.Error = fmt.Errorf("render headers: %w", err)
				return res
			}
			for k, v := range headers {
				req.Header.Set(k, v)
			}
		}
		if cfg.Request.Body != "" {
			bodyRendered, err := env.TemplateEngine.RenderString(cfg.Request.Body, renderCtx)
			if err != nil {
				res.CompletedAt = time.Now()
				res.Error = fmt.Errorf("render body: %w", err)
				return res
			}
			req.Body = io.NopCloser(strings.NewReader(bodyRendered))
			req.ContentLength = int64(len(bodyRendered))
		}
	}

	runStart := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Latency = time.Since(runStart)
		res.Error = err
		res.Success = false
		return res
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = fmt.Errorf("read response: %w", err)
		return res
	}

	res.Latency = time.Since(runStart)
	res.CompletedAt = time.Now()

	bodyString := string(bodyBytes)

	// Precompute JSON body if required
	var jsonBody interface{}
	var jsonErr error
	var parsed bool

	assertions := make([]AssertionResult, 0, len(cfg.Assertions))

	for _, assertion := range cfg.Assertions {
		result := AssertionResult{
			Kind: assertion.Kind,
			Op:   assertion.Op,
			Path: assertion.Path,
		}
		switch strings.ToLower(assertion.Kind) {
		case "status_code":
			expect, _ := toFloat(assertion.Value)
			actual := float64(resp.StatusCode)
			result.Passed = compareFloats(actual, expect, assertion.Op)
			if !result.Passed {
				result.Message = fmt.Sprintf("expected status %s %.0f, got %.0f", assertion.Op, expect, actual)
			}
		case "jsonpath":
			if !parsed {
				parsed = true
				jsonErr = json.Unmarshal(bodyBytes, &jsonBody)
			}
			if jsonErr != nil {
				result.Passed = false
				result.Message = fmt.Sprintf("parse json: %v", jsonErr)
				break
			}
			val, err := jsonpath.JsonPathLookup(jsonBody, assertion.Path)
			if err != nil {
				result.Passed = false
				result.Message = fmt.Sprintf("jsonpath lookup: %v", err)
			} else if strings.ToLower(assertion.Op) == "exists" {
				result.Passed = val != nil
				if !result.Passed {
					result.Message = "jsonpath value does not exist"
				}
			} else {
				expect := assertion.Value
				result.Passed = compareValues(val, expect, assertion.Op)
				if !result.Passed {
					result.Message = fmt.Sprintf("jsonpath value mismatch: got %v", val)
				}
			}
		case "body_contains":
			expect := fmt.Sprintf("%v", assertion.Value)
			switch strings.ToLower(assertion.Op) {
			case "regex":
				rx, err := regexp.Compile(expect)
				if err != nil {
					result.Passed = false
					result.Message = fmt.Sprintf("invalid regex %q: %v", expect, err)
				} else {
					result.Passed = rx.MatchString(bodyString)
					if !result.Passed {
						result.Message = "regex did not match body"
					}
				}
			case "contains":
				result.Passed = strings.Contains(bodyString, expect)
				if !result.Passed {
					result.Message = "string not found in body"
				}
			default:
				result.Passed = false
				result.Message = fmt.Sprintf("unsupported op %q", assertion.Op)
			}
		case "latency_ms":
			expect, _ := toFloat(assertion.Value)
			actual := float64(res.Latency / time.Millisecond)
			result.Passed = compareFloats(actual, expect, assertion.Op)
			if !result.Passed {
				result.Message = fmt.Sprintf("latency %.2fms not %s %.2fms", actual, assertion.Op, expect)
			}
		case "ssl_valid_days":
			if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
				result.Passed = false
				result.Message = "no tls connection"
			} else {
				cert := resp.TLS.PeerCertificates[0]
				days := time.Until(cert.NotAfter).Hours() / 24
				expect, _ := toFloat(assertion.Value)
				result.Passed = compareFloats(days, expect, assertion.Op)
				if !result.Passed {
					result.Message = fmt.Sprintf("cert valid for %.0f days", days)
				}
			}
		default:
			result.Passed = false
			result.Message = fmt.Sprintf("unsupported assertion %q", assertion.Kind)
		}
		assertions = append(assertions, result)
	}

	res.AssertionResults = assertions
	res.Success = allPassed(assertions)
	return res
}

func executePreAuth(ctx context.Context, cfg config.CheckConfig, env Environment, vars map[string]string, client *http.Client) error {
	flow := strings.ToLower(cfg.PreAuth.Flow)
	if flow != "http-token" {
		return fmt.Errorf("unsupported preauth flow %q", cfg.PreAuth.Flow)
	}
	reqCfg := cfg.PreAuth.Request
	urlStr := reqCfg.URL
	if urlStr == "" {
		urlStr = cfg.Target
	}

	renderCtx := render.TemplateContext{
		Secrets: env.Secrets,
		Vars:    vars,
		Data: map[string]interface{}{
			"check": map[string]interface{}{
				"id":     cfg.ID,
				"name":   cfg.Name,
				"target": cfg.Target,
			},
		},
	}

	renderedURL, err := env.TemplateEngine.RenderString(urlStr, renderCtx)
	if err != nil {
		return fmt.Errorf("render preauth url: %w", err)
	}
	method := reqCfg.Method
	if method == "" {
		method = "GET"
	}
	req, err := http.NewRequestWithContext(ctx, method, renderedURL, nil)
	if err != nil {
		return fmt.Errorf("build preauth request: %w", err)
	}

	if len(reqCfg.Headers) > 0 {
		headers, err := render.RenderMap(reqCfg.Headers, renderCtx, env.TemplateEngine)
		if err != nil {
			return fmt.Errorf("render preauth headers: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	if reqCfg.Body != "" {
		bodyRendered, err := env.TemplateEngine.RenderString(reqCfg.Body, renderCtx)
		if err != nil {
			return fmt.Errorf("render preauth body: %w", err)
		}
		req.Body = io.NopCloser(strings.NewReader(bodyRendered))
		req.ContentLength = int64(len(bodyRendered))
	}
	if reqCfg.Timeout != nil && reqCfg.Timeout.Set {
		ctx, cancel := context.WithTimeout(ctx, reqCfg.Timeout.Duration)
		defer cancel()
		req = req.WithContext(ctx)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("preauth request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("preauth read: %w", err)
	}
	if strings.ToLower(cfg.PreAuth.Capture.From) != "jsonpath" {
		return fmt.Errorf("unsupported capture from %q", cfg.PreAuth.Capture.From)
	}
	var jsonBody interface{}
	if err := json.Unmarshal(body, &jsonBody); err != nil {
		return fmt.Errorf("preauth json parse: %w", err)
	}
	val, err := jsonpath.JsonPathLookup(jsonBody, cfg.PreAuth.Capture.Path)
	if err != nil {
		return fmt.Errorf("preauth capture: %w", err)
	}
	str := fmt.Sprintf("%v", val)
	vars[cfg.PreAuth.Capture.As] = str
	return nil
}

func allPassed(results []AssertionResult) bool {
	for _, r := range results {
		if !r.Passed {
			return false
		}
	}
	return true
}

func runTCP(ctx context.Context, start time.Time, cfg config.CheckConfig, env Environment) Result {
	res := Result{
		CheckID:   cfg.ID,
		CheckName: cfg.Name,
		StartedAt: start,
		Metadata:  map[string]any{},
	}
	timeout := effectiveTimeout(cfg, env.Defaults)
	dialer := &net.Dialer{Timeout: timeout}
	runStart := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Target)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Latency = time.Since(runStart)
		res.Error = err
		res.Success = false
		return res
	}
	latency := time.Since(runStart)
	conn.Close()
	res.CompletedAt = time.Now()
	res.Latency = latency

	assertions := make([]AssertionResult, 0, len(cfg.Assertions))
	for _, assertion := range cfg.Assertions {
		result := AssertionResult{Kind: assertion.Kind, Op: assertion.Op}
		switch strings.ToLower(assertion.Kind) {
		case "tcp_connect":
			expect := fmt.Sprintf("%v", assertion.Value)
			result.Passed = strings.ToLower(expect) == "true"
			if !result.Passed {
				result.Message = "connection succeeded but expectation false"
			}
		case "latency_ms":
			expect, _ := toFloat(assertion.Value)
			actual := float64(latency / time.Millisecond)
			result.Passed = compareFloats(actual, expect, assertion.Op)
			if !result.Passed {
				result.Message = fmt.Sprintf("latency %.2fms not %s %.2fms", actual, assertion.Op, expect)
			}
		default:
			result.Passed = false
			result.Message = fmt.Sprintf("unsupported assertion %q", assertion.Kind)
		}
		assertions = append(assertions, result)
	}
	res.AssertionResults = assertions
	res.Success = allPassed(assertions)
	return res
}

func runICMP(ctx context.Context, start time.Time, cfg config.CheckConfig, env Environment) Result {
	res := Result{
		CheckID:   cfg.ID,
		CheckName: cfg.Name,
		StartedAt: start,
		Metadata:  map[string]any{},
	}
	pinger, err := ping.NewPinger(cfg.Target)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = fmt.Errorf("init pinger: %w", err)
		return res
	}
	pinger.SetPrivileged(true)
	timeout := effectiveTimeout(cfg, env.Defaults)
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	pinger.Count = 3
	pinger.Timeout = timeout
	pinger.Run() // blocking
	stats := pinger.Statistics()

	res.CompletedAt = time.Now()
	res.Latency = stats.AvgRtt
	res.Metadata["packet_loss"] = stats.PacketLoss
	res.Metadata["rtt_p95_ms"] = stats.AvgRtt.Seconds() * 1000 // approximate

	assertions := make([]AssertionResult, 0, len(cfg.Assertions))
	for _, assertion := range cfg.Assertions {
		result := AssertionResult{Kind: assertion.Kind, Op: assertion.Op}
		switch strings.ToLower(assertion.Kind) {
		case "packet_loss_percent":
			expect, _ := toFloat(assertion.Value)
			actual := stats.PacketLoss
			result.Passed = compareFloats(actual, expect, assertion.Op)
			if !result.Passed {
				result.Message = fmt.Sprintf("packet loss %.2f%% not %s %.2f", actual, assertion.Op, expect)
			}
		case "latency_ms_p95":
			expect, _ := toFloat(assertion.Value)
			actual := stats.AvgRtt.Seconds() * 1000
			result.Passed = compareFloats(actual, expect, assertion.Op)
			if !result.Passed {
				result.Message = fmt.Sprintf("latency %.2fms not %s %.2f", actual, assertion.Op, expect)
			}
		default:
			result.Passed = false
			result.Message = fmt.Sprintf("unsupported assertion %q", assertion.Kind)
		}
		assertions = append(assertions, result)
	}
	res.AssertionResults = assertions
	res.Success = allPassed(assertions)
	return res
}

func runDNS(ctx context.Context, start time.Time, cfg config.CheckConfig, env Environment) Result {
	res := Result{
		CheckID:   cfg.ID,
		CheckName: cfg.Name,
		StartedAt: start,
		Metadata:  map[string]any{},
	}
	client := &dnsclient.Client{}
	msg := new(dnsclient.Msg)
	msg.SetQuestion(dnsclient.Fqdn(cfg.Target), dnsTypeFromString(cfg.RecordType))
	server := cfg.Resolver
	if server == "" {
		server = "8.8.8.8:53"
	}
	resp, _, err := client.ExchangeContext(ctx, msg, server)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = err
		return res
	}
	res.CompletedAt = time.Now()
	if resp == nil || resp.Rcode != dnsclient.RcodeSuccess {
		res.Error = fmt.Errorf("dns error code %d", resp.Rcode)
		return res
	}
	answers := resp.Answer
	res.Metadata["answer_count"] = len(answers)

	assertions := make([]AssertionResult, 0, len(cfg.Assertions))
	for _, assertion := range cfg.Assertions {
		result := AssertionResult{Kind: assertion.Kind, Op: assertion.Op}
		switch strings.ToLower(assertion.Kind) {
		case "dns_answer":
			expectedList, _ := toStringSlice(assertion.Value)
			actualVals := extractAnswerStrings(answers)
			result.Passed = sliceContains(actualVals, expectedList)
			if !result.Passed {
				result.Message = fmt.Sprintf("expected any of %v, got %v", expectedList, actualVals)
			}
		case "ttl_seconds":
			expect, _ := toFloat(assertion.Value)
			if len(answers) == 0 {
				result.Passed = false
				result.Message = "no DNS answers"
			} else {
				actual := float64(answers[0].Header().Ttl)
				result.Passed = compareFloats(actual, expect, assertion.Op)
				if !result.Passed {
					result.Message = fmt.Sprintf("ttl %.0f not %s %.0f", actual, assertion.Op, expect)
				}
			}
		default:
			result.Passed = false
			result.Message = fmt.Sprintf("unsupported assertion %q", assertion.Kind)
		}
		assertions = append(assertions, result)
	}
	res.AssertionResults = assertions
	res.Success = allPassed(assertions)
	return res
}

func runTLS(ctx context.Context, start time.Time, cfg config.CheckConfig, env Environment) Result {
	res := Result{
		CheckID:   cfg.ID,
		CheckName: cfg.Name,
		StartedAt: start,
	}
	dialer := &net.Dialer{Timeout: effectiveTimeout(cfg, env.Defaults)}
	host, port, err := net.SplitHostPort(cfg.Target)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = fmt.Errorf("invalid target: %w", err)
		return res
	}
	serverName := cfg.SNI
	if serverName == "" {
		serverName = host
	}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, port), &tls.Config{ServerName: serverName})
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = err
		return res
	}
	defer conn.Close()
	state := conn.ConnectionState()
	res.CompletedAt = time.Now()
	res.Metadata = map[string]any{
		"negotiated_protocol": state.NegotiatedProtocol,
		"cipher_suite":        tls.CipherSuiteName(state.CipherSuite),
	}
	assertions := make([]AssertionResult, 0, len(cfg.Assertions))
	for _, assertion := range cfg.Assertions {
		result := AssertionResult{Kind: assertion.Kind, Op: assertion.Op}
		switch strings.ToLower(assertion.Kind) {
		case "ssl_valid_days":
			if len(state.PeerCertificates) == 0 {
				result.Passed = false
				result.Message = "no peer certificates"
			} else {
				exp := state.PeerCertificates[0].NotAfter
				days := time.Until(exp).Hours() / 24
				expect, _ := toFloat(assertion.Value)
				result.Passed = compareFloats(days, expect, assertion.Op)
				if !result.Passed {
					result.Message = fmt.Sprintf("cert expires in %.0f days", days)
				}
			}
		case "ssl_hostname_matches":
			expect := strings.ToLower(fmt.Sprintf("%v", assertion.Value)) == "true"
			if len(state.PeerCertificates) == 0 {
				result.Passed = !expect
				if expect {
					result.Message = "no certificates"
				}
			} else {
				err := state.PeerCertificates[0].VerifyHostname(serverName)
				result.Passed = (err == nil) == expect
				if !result.Passed && expect {
					result.Message = fmt.Sprintf("hostname verify: %v", err)
				}
			}
		default:
			result.Passed = false
			result.Message = fmt.Sprintf("unsupported assertion %q", assertion.Kind)
		}
		assertions = append(assertions, result)
	}
	res.AssertionResults = assertions
	res.Success = allPassed(assertions)
	return res
}

func runWHOIS(ctx context.Context, start time.Time, cfg config.CheckConfig, env Environment) Result {
	res := Result{
		CheckID:   cfg.ID,
		CheckName: cfg.Name,
		StartedAt: start,
	}
	domain := cfg.Target
	server, err := whoisServerForDomain(domain)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = err
		return res
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(server, "43"), effectiveTimeout(cfg, env.Defaults))
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = fmt.Errorf("dial whois: %w", err)
		return res
	}
	defer conn.Close()
	_, err = conn.Write([]byte(domain + "\r\n"))
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = fmt.Errorf("write whois: %w", err)
		return res
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, err := io.ReadAll(conn)
	if err != nil {
		res.CompletedAt = time.Now()
		res.Error = fmt.Errorf("read whois: %w", err)
		return res
	}
	res.CompletedAt = time.Now()
	res.Metadata = map[string]any{
		"raw": string(body),
	}

	expiration, err := extractExpiry(string(body))
	if err != nil {
		res.Error = err
		return res
	}

	assertions := make([]AssertionResult, 0, len(cfg.Assertions))
	for _, assertion := range cfg.Assertions {
		result := AssertionResult{Kind: assertion.Kind, Op: assertion.Op}
		switch strings.ToLower(assertion.Kind) {
		case "domain_expires_in_days":
			expect, _ := toFloat(assertion.Value)
			diff := time.Until(expiration).Hours() / 24
			result.Passed = compareFloats(diff, expect, assertion.Op)
			if !result.Passed {
				result.Message = fmt.Sprintf("domain expires in %.0f days", diff)
			}
		default:
			result.Passed = false
			result.Message = fmt.Sprintf("unsupported assertion %q", assertion.Kind)
		}
		assertions = append(assertions, result)
	}
	res.AssertionResults = assertions
	res.Success = allPassed(assertions)
	return res
}

func toFloat(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case string:
		f, err := strconv.ParseFloat(val, 64)
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

func compareFloats(actual, expected float64, op string) bool {
	switch strings.ToLower(op) {
	case "equals", "equal", "==":
		return actual == expected
	case "less_than", "<":
		return actual < expected
	case "greater_than", ">":
		return actual > expected
	case "not_equals", "!=":
		return actual != expected
	default:
		return false
	}
}

func compareValues(actual, expected interface{}, op string) bool {
	switch strings.ToLower(op) {
	case "equals", "equal", "==":
		return fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", expected)
	default:
		return false
	}
}

func extractAnswerStrings(rrs []dnsclient.RR) []string {
	values := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		if a, ok := rr.(*dnsclient.A); ok {
			values = append(values, a.A.String())
		} else {
			values = append(values, rr.String())
		}
	}
	return values
}

func sliceContains(actual, expected []string) bool {
	for _, exp := range expected {
		for _, act := range actual {
			if act == exp {
				return true
			}
		}
	}
	return false
}

func toStringSlice(v interface{}) ([]string, error) {
	switch val := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(val))
		for _, item := range val {
			out = append(out, fmt.Sprintf("%v", item))
		}
		return out, nil
	case []string:
		return val, nil
	default:
		return nil, fmt.Errorf("unexpected type %T for string slice", v)
	}
}

var whoisExpiryRegex = regexp.MustCompile(`(?i)Expiry Date:\s*(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)`)

func extractExpiry(body string) (time.Time, error) {
	match := whoisExpiryRegex.FindStringSubmatch(body)
	if len(match) < 2 {
		return time.Time{}, errors.New("could not locate expiry date")
	}
	t, err := time.Parse(time.RFC3339, match[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("parse expiry: %w", err)
	}
	return t, nil
}

var whoisServerCache = map[string]string{
	"com": "whois.verisign-grs.com",
	"net": "whois.verisign-grs.com",
	"org": "whois.pir.org",
	"io":  "whois.nic.io",
}

func whoisServerForDomain(domain string) (string, error) {
	etld, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return "", fmt.Errorf("public suffix: %w", err)
	}
	suffix, _ := publicsuffix.PublicSuffix(domain)
	server, ok := whoisServerCache[suffix]
	if !ok {
		server = "whois.iana.org"
	}
	if server == "whois.iana.org" {
		return server, fmt.Errorf("no specific whois server for %q", etld)
	}
	return server, nil
}

func dnsTypeFromString(t string) uint16 {
	switch strings.ToUpper(t) {
	case "A":
		return dnsclient.TypeA
	case "AAAA":
		return dnsclient.TypeAAAA
	case "CNAME":
		return dnsclient.TypeCNAME
	case "MX":
		return dnsclient.TypeMX
	default:
		return dnsclient.TypeA
	}
}
