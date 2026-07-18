package engine

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/reorx/hookploy/internal/ops"
)

// envRequire asserts every key exists with a non-empty value.
func (e *Engine) envRequire(spec Spec, a *ops.EnvRequire) error {
	path, err := resolveWithin(spec.Dir, a.File)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("env.require: %w", err)
	}
	values := parseEnv(string(data))
	for _, key := range a.Keys {
		if v, ok := values[key]; !ok || strings.TrimSpace(v) == "" {
			return fmt.Errorf("env.require: key %q is missing or empty in %s", key, a.File)
		}
	}
	return nil
}

// envWrite upserts KEY=VALUE lines preserving unrelated content, writing
// atomically via tmp+rename.
func (e *Engine) envWrite(spec Spec, a *ops.EnvWrite) error {
	path, err := resolveWithin(spec.Dir, a.File)
	if err != nil {
		return err
	}
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	remaining := map[string]string{}
	for k, v := range a.Set {
		remaining[k] = v
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, found := strings.Cut(trimmed, "=")
		key = strings.TrimSpace(key)
		if !found {
			continue
		}
		if v, ok := remaining[key]; ok {
			lines[i] = key + "=" + v
			delete(remaining, key)
		}
	}
	keys := make([]string, 0, len(remaining))
	for k := range remaining {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, k+"="+remaining[k])
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseEnv(content string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		out[strings.TrimSpace(key)] = val
	}
	return out
}
