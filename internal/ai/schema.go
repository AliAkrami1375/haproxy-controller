package ai

// Small helpers for building JSON Schema tool-parameter objects without the
// visual noise of nested map literals.

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func enum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func arrayOf(item map[string]any, desc string) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": item}
}

// argString reads a string argument, tolerating a nil map or wrong type.
func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// argInt reads an integer argument. JSON numbers decode to float64.
func argInt(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if v == "" {
			return def
		}
		n := 0
		for _, r := range v {
			if r < '0' || r > '9' {
				return def
			}
			n = n*10 + int(r-'0')
		}
		return n
	}
	return def
}

// argBool reads a boolean argument, tolerating string forms.
func argBool(args map[string]any, key string, def bool) bool {
	if args == nil {
		return def
	}
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		switch v {
		case "true", "yes", "1", "on":
			return true
		case "false", "no", "0", "off":
			return false
		}
	}
	return def
}
