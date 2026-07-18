package ops

import (
	"encoding/json"
	"fmt"
)

// stepJSON is the wire/DB form of a Step: {"op": "...", "args": {...}}.
type stepJSON struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

func (s Step) MarshalJSON() ([]byte, error) {
	var args json.RawMessage
	if s.Args != nil {
		b, err := json.Marshal(s.Args)
		if err != nil {
			return nil, err
		}
		if string(b) != "{}" {
			args = b
		}
	}
	return json.Marshal(stepJSON{Op: s.Op, Args: args})
}

func (s *Step) UnmarshalJSON(b []byte) error {
	var raw stepJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	construct, ok := registry[raw.Op]
	if !ok {
		return fmt.Errorf("unknown op %q", raw.Op)
	}
	args := construct()
	if len(raw.Args) > 0 {
		if err := json.Unmarshal(raw.Args, args); err != nil {
			return fmt.Errorf("op %s: %w", raw.Op, err)
		}
	}
	if d, ok := args.(defaulter); ok {
		d.setDefaults()
	}
	s.Op, s.Args, s.Line = raw.Op, args, 0
	return nil
}
