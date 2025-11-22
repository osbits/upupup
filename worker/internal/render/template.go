package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"
)

// Engine renders template strings with helper functions.
type Engine struct{}

// TemplateContext provides data for template execution.
type TemplateContext struct {
	Secrets map[string]string
	Vars    map[string]string
	Data    map[string]interface{}
}

// New creates a new template engine.
func New() *Engine {
	return &Engine{}
}

// RenderString renders the provided template string with context.
func (e *Engine) RenderString(tmpl string, ctx TemplateContext) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	t, err := template.New("tpl").Funcs(template.FuncMap{
		"secret": func(key string) (string, error) {
			if ctx.Secrets == nil {
				return "", fmt.Errorf("no secrets available")
			}
			val, ok := ctx.Secrets[key]
			if !ok {
				return "", fmt.Errorf("secret %q not found", key)
			}
			return val, nil
		},
		"var": func(key string) (string, error) {
			if ctx.Vars == nil {
				return "", fmt.Errorf("vars not available")
			}
			val, ok := ctx.Vars[key]
			if !ok {
				return "", fmt.Errorf("var %q not defined", key)
			}
			return val, nil
		},
		"to_json": func(v interface{}) (string, error) {
			b, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
	}).Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx.Data); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	return buf.String(), nil
}
