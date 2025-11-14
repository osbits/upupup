package render

// RenderMap applies templates to each value in a map.
func RenderMap(values map[string]string, ctx TemplateContext, engine *Engine) (map[string]string, error) {
	if len(values) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(values))
	for key, val := range values {
		rendered, err := engine.RenderString(val, ctx)
		if err != nil {
			return nil, err
		}
		out[key] = rendered
	}
	return out, nil
}
