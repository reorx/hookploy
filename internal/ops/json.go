package ops

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/reorx/hookploy/internal/model"
)

// stepJSON is the wire/DB form of a Step: {"op": "...", "args": {...}} plus
// the modifiers the engine enforces. All but op are omitted when unset, so a
// step without modifiers keeps its pre-modifier bytes.
type stepJSON struct {
	Op      string          `json:"op"`
	Args    json.RawMessage `json:"args,omitempty"`
	Timeout model.Duration  `json:"timeout,omitempty"`
	Retries *int            `json:"retries,omitempty"`
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
	return json.Marshal(stepJSON{Op: s.Op, Args: args, Timeout: model.Duration(s.Timeout), Retries: s.Retries})
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
	// On is config-only and never on the wire: a decoded step is one the
	// scheduler already resolved targeting for, so it runs where it landed.
	s.Op, s.Args, s.On, s.Line = raw.Op, args, nil, 0
	s.Timeout, s.Retries = time.Duration(raw.Timeout), raw.Retries
	return nil
}
