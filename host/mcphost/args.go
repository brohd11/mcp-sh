package mcphost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ParseArgs turns command-line arguments into a tool's input object, guided by its
// JSON Schema:
//
//	--name value | --name=value      coerced to the property's type
//	--flag | --no-flag               booleans
//	--tags a --tags b | --tags '[..]' arrays
//	--json '{...}' | --json -        the whole input (stdin with "-"); later flags override
//	VALUE                            the single required (or only) property
//
// A property with a configured default no longer counts as required (Server.CallTool
// fills it). Required properties are checked before calling, so mistakes fail fast
// with a usage message instead of a server error.
func ParseArgs(schema any, args []string, readStdin func() (string, error), defaults map[string]any) (map[string]any, error) {
	props := properties(schema)
	req := required(schema)
	for k := range defaults {
		if _, declared := props[k]; declared {
			delete(req, k)
		}
	}
	out := map[string]any{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") || a == "--" {
			if a == "--" {
				if i+1 < len(args) {
					return nil, fmt.Errorf("unexpected arguments after --: %q", args[i+1:])
				}
				continue
			}
			key, err := positionalKey(props, req, out)
			if err != nil {
				return nil, fmt.Errorf("unexpected argument %q: %w", a, err)
			}
			v, err := coerce(props[key], a)
			if err != nil {
				return nil, fmt.Errorf("--%s: %w", key, err)
			}
			out[key] = v
			continue
		}
		name, val, hasVal := strings.Cut(a[2:], "=")
		next := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("--%s needs a value", name)
			}
			i++
			return args[i], nil
		}

		if name == "json" {
			raw, err := next()
			if err != nil {
				return nil, err
			}
			if raw == "-" {
				if raw, err = readStdin(); err != nil {
					return nil, err
				}
			}
			obj, err := decodeJSON(raw)
			if err != nil {
				return nil, fmt.Errorf("--json: %w", err)
			}
			m, ok := obj.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("--json: expected a JSON object")
			}
			for k, v := range m {
				out[k] = v
			}
			continue
		}

		key, prop, ok := lookupProp(props, name)
		negated := false
		if !ok && strings.HasPrefix(name, "no-") {
			if k, p, found := lookupProp(props, name[3:]); found && hasType(p, "boolean") {
				key, prop, ok, negated = k, p, true, true
			}
		}
		if !ok {
			if len(props) > 0 {
				return nil, fmt.Errorf("unknown parameter --%s (have: %s)", name, strings.Join(paramNames(props), ", "))
			}
			key = name // schema without properties: pass through
		}
		if negated {
			if hasVal {
				return nil, fmt.Errorf("--%s takes no value", name)
			}
			out[key] = false
			continue
		}
		// A bare boolean flag means true, unless an explicit true/false follows.
		if hasType(prop, "boolean") && !hasVal {
			if i+1 < len(args) && (args[i+1] == "true" || args[i+1] == "false") {
				i++
				out[key] = args[i] == "true"
			} else {
				out[key] = true
			}
			continue
		}
		raw, err := next()
		if err != nil {
			return nil, err
		}
		if hasType(prop, "array") && !strings.HasPrefix(strings.TrimSpace(raw), "[") {
			item, err := coerce(items(prop), raw)
			if err != nil {
				return nil, fmt.Errorf("--%s: %w", name, err)
			}
			list, _ := out[key].([]any)
			out[key] = append(list, item)
			continue
		}
		v, err := coerce(prop, raw)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", name, err)
		}
		out[key] = v
	}
	var missing []string
	for k := range req {
		if _, ok := out[k]; !ok {
			missing = append(missing, "--"+k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// positionalKey picks the property a bare argument fills: the single required one, or
// the only property.
func positionalKey(props map[string]any, req map[string]bool, set map[string]any) (string, error) {
	candidates := []string{}
	for k := range req {
		candidates = append(candidates, k)
	}
	if len(candidates) != 1 {
		candidates = candidates[:0]
		if len(props) == 1 {
			for k := range props {
				candidates = append(candidates, k)
			}
		}
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("pass parameters as --name value")
	}
	if _, done := set[candidates[0]]; done {
		return "", fmt.Errorf("--%s is already set", candidates[0])
	}
	return candidates[0], nil
}

// lookupProp matches a flag to a property, treating '-' and '_' alike.
func lookupProp(props map[string]any, name string) (string, map[string]any, bool) {
	if p, ok := props[name]; ok {
		m, _ := p.(map[string]any)
		return name, m, true
	}
	norm := strings.ReplaceAll(name, "-", "_")
	for k, p := range props {
		if strings.ReplaceAll(k, "-", "_") == norm {
			m, _ := p.(map[string]any)
			return k, m, true
		}
	}
	return "", nil, false
}

func properties(schema any) map[string]any {
	m, _ := schema.(map[string]any)
	props, _ := m["properties"].(map[string]any)
	return props
}

func required(schema any) map[string]bool {
	m, _ := schema.(map[string]any)
	out := map[string]bool{}
	list, _ := m["required"].([]any)
	for _, r := range list {
		if s, ok := r.(string); ok {
			out[s] = true
		}
	}
	return out
}

func paramNames(props map[string]any) []string {
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, "--"+k)
	}
	sort.Strings(names)
	return names
}

// types lists a property's JSON types, looking through anyOf/oneOf (Optional[...]
// in Python servers becomes anyOf [{type: X}, {type: null}]).
func types(prop any) []string {
	m, _ := prop.(map[string]any)
	if m == nil {
		return nil
	}
	var out []string
	switch t := m["type"].(type) {
	case string:
		out = append(out, t)
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts, ok := m[key].([]any); ok {
			for _, alt := range alts {
				out = append(out, types(alt)...)
			}
		}
	}
	return out
}

func hasType(prop any, t string) bool {
	for _, x := range types(prop) {
		if x == t {
			return true
		}
	}
	return false
}

func items(prop any) any {
	m, _ := prop.(map[string]any)
	if it, ok := m["items"]; ok {
		return it
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts, ok := m[key].([]any); ok {
			for _, alt := range alts {
				if hasType(alt, "array") {
					return items(alt)
				}
			}
		}
	}
	return nil
}

// paramType is the type shown in help, e.g. "string", "integer", "array of string".
func paramType(prop any) string {
	var ts []string
	for _, t := range types(prop) {
		if t == "null" {
			continue
		}
		if t == "array" {
			if it := paramType(items(prop)); it != "value" {
				t = "array of " + it
			}
		}
		ts = append(ts, t)
	}
	if len(ts) == 0 {
		return "value"
	}
	return strings.Join(ts, "|")
}

// coerce converts a flag value to the first of the property's types it fits.
func coerce(prop any, raw string) (any, error) {
	ts := types(prop)
	if len(ts) == 0 {
		// Untyped: take valid JSON as JSON, anything else as a string.
		if v, err := decodeJSON(raw); err == nil {
			return v, nil
		}
		return raw, nil
	}
	for _, t := range ts {
		switch t {
		case "string":
			return raw, nil
		case "integer":
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
				return n, nil
			}
		case "number":
			if f, err := strconv.ParseFloat(raw, 64); err == nil {
				return f, nil
			}
		case "boolean":
			if b, err := strconv.ParseBool(raw); err == nil {
				return b, nil
			}
		case "object", "array":
			if v, err := decodeJSON(raw); err == nil {
				if _, isObj := v.(map[string]any); isObj == (t == "object") {
					return v, nil
				}
			}
		case "null":
			if raw == "null" {
				return nil, nil
			}
		}
	}
	return nil, fmt.Errorf("invalid value %q (expected %s)", raw, paramType(prop))
}

func decodeJSON(raw string) (any, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}
