package observability

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/rollbar/rollbar-go"
)

// SetupRollbar configures the Rollbar SDK if the access token is present.
// It returns a boolean indicating whether Rollbar was enabled and a cleanup
// function that should be deferred to flush pending items.
func SetupRollbar(logger *slog.Logger) (bool, func()) {
	token := strings.TrimSpace(os.Getenv("ROLLBAR_ACCESS_TOKEN"))
	if token == "" {
		rollbar.SetEnabled(false)
		logger.Info("rollbar disabled", "reason", "missing access token")
		return false, func() {}
	}

	rollbar.SetEnabled(true)
	rollbar.SetToken(token)

	env := strings.TrimSpace(os.Getenv("ROLLBAR_ENVIRONMENT"))
	if env == "" {
		env = "production"
	}
	rollbar.SetEnvironment(env)

	if codeVersion := strings.TrimSpace(os.Getenv("ROLLBAR_CODE_VERSION")); codeVersion != "" {
		rollbar.SetCodeVersion(codeVersion)
	}

	if host := strings.TrimSpace(os.Getenv("ROLLBAR_SERVER_HOST")); host != "" {
		rollbar.SetServerHost(host)
	} else if hostname, err := os.Hostname(); err == nil && hostname != "" {
		rollbar.SetServerHost(hostname)
	}

	if root := strings.TrimSpace(os.Getenv("ROLLBAR_SERVER_ROOT")); root != "" {
		rollbar.SetServerRoot(root)
	} else if wd, err := os.Getwd(); err == nil {
		rollbar.SetServerRoot(filepath.Clean(wd))
	}

	rollbar.SetCaptureIp(rollbar.CaptureIpAnonymize)

	logger.Info("rollbar enabled", "environment", env)

	return true, func() {
		rollbar.Wait()
	}
}

// CapturePanic reports panics to Rollbar when enabled.
func CapturePanic(logger *slog.Logger, enabled bool) func() {
	if !enabled {
		return func() {}
	}

	return func() {
		if rec := recover(); rec != nil {
			switch err := rec.(type) {
			case error:
				rollbar.Critical(err)
			default:
				rollbar.Critical(fmt.Errorf("panic: %v", rec))
			}
			logger.Error("panic captured", "panic", rec)
			panic(rec)
		}
	}
}
