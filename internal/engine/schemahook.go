package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// schemahook.go validates agent output against the JSON Schema stored on the
// step's ResponseFormat and asks the engine to retry on violation. It implements
// the common subset of JSON Schema used for LLM structured output (type incl.
// type-arrays and null, required, properties, items, enum, additionalProperties,
// minimum/maximum) with no external dependency.

// SchemaValidationPostHook returns a PostHook that validates a structured/text
// result against the resolved ResponseFormat schema. On any violation it returns
// ErrRetryWith so the engine re-runs the agent with the violations as the reason.
// Steps without a schema (empty Schema) pass through untouched, so it is safe to
// register globally.
func SchemaValidationPostHook() PostHook {
	return func(_ context.Context, req *StepRequest, res Result) (Result, error) {
		if req.ResponseFormat == nil || len(req.ResponseFormat.Schema) == 0 {
			return res, nil
		}
		var val any
		switch r := res.(type) {
		case *StructuredResult:
			val = r.Data
		case *TextResult:
			if err := json.Unmarshal([]byte(extractJSONObject(r.Content)), &val); err != nil {
				return nil, ErrRetryWith(RetryAnnotation{
					Reason: "your output was not valid JSON; return ONLY the JSON object matching the schema",
				})
			}
		default:
			return res, nil
		}
		if errs := ValidateJSONSchema(req.ResponseFormat.Schema, val); len(errs) > 0 {
			return nil, ErrRetryWith(RetryAnnotation{
				Reason: "output failed schema validation: " + strings.Join(capStrings(errs, 6), "; "),
			})
		}
		return res, nil
	}
}

// ValidateJSONSchema validates value against a JSON Schema (the common LLM
// subset) and returns a list of human-readable violations ("" path = root).
func ValidateJSONSchema(schema map[string]any, value any) []string {
	var errs []string
	validateNode(schema, value, "", &errs)
	return errs
}

func validateNode(schema map[string]any, value any, path string, errs *[]string) {
	at := path
	if at == "" {
		at = "(root)"
	}

	// enum
	if raw, ok := schema["enum"]; ok {
		if opts, ok := raw.([]any); ok && !enumContains(opts, value) {
			*errs = append(*errs, fmt.Sprintf("%s: value not in enum", at))
		}
	}

	// type
	if t, ok := schema["type"]; ok && !typeMatches(t, value) {
		*errs = append(*errs, fmt.Sprintf("%s: wrong type (want %v)", at, t))
		return // further checks assume the right type
	}

	switch v := value.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		// required
		if reqd, ok := schema["required"].([]any); ok {
			for _, r := range reqd {
				name, _ := r.(string)
				if _, present := v[name]; !present {
					*errs = append(*errs, fmt.Sprintf("%s: missing required %q", at, name))
				}
			}
		}
		// additionalProperties: false
		if ap, ok := schema["additionalProperties"]; ok {
			if allow, isBool := ap.(bool); isBool && !allow {
				for k := range v {
					if _, declared := props[k]; !declared {
						*errs = append(*errs, fmt.Sprintf("%s: unexpected property %q", at, k))
					}
				}
			}
		}
		// recurse declared properties that are present
		for name, ps := range props {
			if sub, ok := ps.(map[string]any); ok {
				if cv, present := v[name]; present {
					validateNode(sub, cv, joinPath(path, name), errs)
				}
			}
		}
	case []any:
		if is, ok := schema["items"].(map[string]any); ok {
			for i, item := range v {
				validateNode(is, item, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
	case float64:
		if mn, ok := toNumber(schema["minimum"]); ok && v < mn {
			*errs = append(*errs, fmt.Sprintf("%s: below minimum %v", at, mn))
		}
		if mx, ok := toNumber(schema["maximum"]); ok && v > mx {
			*errs = append(*errs, fmt.Sprintf("%s: above maximum %v", at, mx))
		}
	}
}

func typeMatches(t any, value any) bool {
	switch tt := t.(type) {
	case string:
		return matchesOneType(tt, value)
	case []any:
		for _, x := range tt {
			if s, ok := x.(string); ok && matchesOneType(s, value) {
				return true
			}
		}
		return false
	}
	return true // unknown type spec → don't fail
}

func matchesOneType(t string, value any) bool {
	switch t {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == float64(int64(f))
	case "null":
		return value == nil
	}
	return true
}

func enumContains(opts []any, value any) bool {
	vb, _ := json.Marshal(value)
	for _, o := range opts {
		ob, _ := json.Marshal(o)
		if string(ob) == string(vb) {
			return true
		}
	}
	return false
}

func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// extractJSONObject returns the outermost {...} block from text, stripping code
// fences — best-effort for models that wrap JSON in prose/markdown.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			return s[i : j+1]
		}
	}
	return s
}

func capStrings(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(xs[:n:n], fmt.Sprintf("…(+%d more)", len(xs)-n))
}
