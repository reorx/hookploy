package httpapi

import (
	"encoding/json"
	"io"
	"sort"

	"github.com/reorx/hookploy/internal/config"
)

func newNDJSON(w io.Writer) func(v any) {
	enc := json.NewEncoder(w)
	return func(v any) { _ = enc.Encode(v) }
}

func sortedServerNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
