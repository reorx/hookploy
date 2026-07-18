package ops

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

var payloadRef = regexp.MustCompile(`\$\{payload\.([A-Za-z0-9_][A-Za-z0-9_.\-]*?)(\?)?\}`)

// Interpolate returns a deep copy of steps with every ${payload.a.b} (and
// optional ${payload.a.b?}) reference in string-valued arg positions replaced
// by the payload value. Required references to missing keys fail fast.
// Interpolated values only ever land inside structured parameters (argv
// elements, map values, string fields) — never in a shell string.
func Interpolate(steps []Step, payload map[string]any) ([]Step, error) {
	out := make([]Step, len(steps))
	for i, s := range steps {
		// deep copy via the JSON snapshot format
		b, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		if err := out[i].UnmarshalJSON(b); err != nil {
			return nil, err
		}
		out[i].Line = s.Line
		if err := interpolateValue(reflect.ValueOf(out[i].Args).Elem(), payload); err != nil {
			return nil, fmt.Errorf("op %s: %w", s.Op, err)
		}
	}
	return out, nil
}

func interpolateValue(v reflect.Value, payload map[string]any) error {
	switch v.Kind() {
	case reflect.String:
		s, err := interpolateString(v.String(), payload)
		if err != nil {
			return err
		}
		v.SetString(s)
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := interpolateValue(v.Index(i), payload); err != nil {
				return err
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			elem := v.MapIndex(k)
			if elem.Kind() != reflect.String {
				continue
			}
			s, err := interpolateString(elem.String(), payload)
			if err != nil {
				return err
			}
			v.SetMapIndex(k, reflect.ValueOf(s))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := interpolateValue(v.Field(i), payload); err != nil {
				return err
			}
		}
	}
	return nil
}

func interpolateString(s string, payload map[string]any) (string, error) {
	var firstErr error
	out := payloadRef.ReplaceAllStringFunc(s, func(m string) string {
		groups := payloadRef.FindStringSubmatch(m)
		path, optional := groups[1], groups[2] == "?"
		val, ok, err := lookupPayload(payload, path)
		if err != nil && firstErr == nil {
			firstErr = err
			return ""
		}
		if !ok {
			if optional {
				return ""
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("payload key %q is required but missing", path)
			}
			return ""
		}
		return val
	})
	return out, firstErr
}

// lookupPayload resolves a dotted path against the payload, returning the
// scalar value as a string.
func lookupPayload(payload map[string]any, path string) (string, bool, error) {
	var cur any = payload
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur, ok = m[part]
		if !ok {
			return "", false, nil
		}
	}
	switch val := cur.(type) {
	case string:
		return val, true, nil
	case bool:
		return strconv.FormatBool(val), true, nil
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64), true, nil
	case json.Number:
		return val.String(), true, nil
	case nil:
		return "", true, nil
	default:
		return "", false, fmt.Errorf("payload key %q is not a scalar (got %T)", path, cur)
	}
}
