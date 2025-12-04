package observability

import (
	"bufio"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// LoadDotEnv loads environment variables from a .env file when present.
// Missing files are ignored so production deployments relying on injected
// environment variables continue to work without noise.
func LoadDotEnv(logger *slog.Logger) {
	file, err := os.Open(".env")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			logger.Debug("no .env file found, skipping", "path", ".env")
			return
		}
		logger.Warn("failed to open .env file", "error", err)
		return
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			logger.Warn("failed to close .env file", "error", cerr)
		}
	}()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	var loaded bool
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			logger.Warn("skipping invalid .env entry (missing '=')", "line", lineNumber)
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			logger.Warn("skipping invalid .env line (missing key)", "line", lineNumber)
			continue
		}
		value = strings.TrimSpace(trimInlineComment(value))
		if parsed, err := decodeEnvValue(value); err == nil {
			if err := os.Setenv(key, parsed); err != nil {
				logger.Warn("failed to set env from .env line", "key", key, "line", lineNumber, "error", err)
				continue
			}
			loaded = true
		} else {
			logger.Warn("skipping invalid .env entry", "line", lineNumber, "error", err)
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("failed to read .env file", "error", err)
		return
	}
	if loaded {
		logger.Debug("loaded environment from .env")
	}
}

func trimInlineComment(value string) string {
	var (
		inSingle bool
		inDouble bool
	)
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '#':
			if !inSingle && !inDouble {
				return strings.TrimSpace(value[:i])
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		}
	}
	return value
}

func decodeEnvValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "'") {
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", err
		}
		return decoded, nil
	}
	return value, nil
}
