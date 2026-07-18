// Package ops defines the typed vocabulary of deploy operations: parsing
// from YAML (both step forms), validation, the JSON snapshot format stored
// in the DB / sent to edges, and payload interpolation.
package ops

import (
	"errors"
	"time"

	"github.com/reorx/hookploy/internal/model"
)

// Args is the typed parameter struct of one op.
type Args interface {
	// Validate checks required fields and value formats.
	Validate() error
}

// defaulter lets an op fill documented defaults after decoding.
type defaulter interface{ setDefaults() }

// Step is one entry of a deploy/task pipeline.
type Step struct {
	Op   string
	Args Args
	Line int // source line in hookploy.yaml; 0 when restored from JSON
}

// ImagePin locks the deploy to an image digest. Zero parameters; the
// post-deploy verification is implicit (see PRD §4 模式 op 语义).
type ImagePin struct{}

func (*ImagePin) Validate() error { return nil }

// ImageExtract copies a path out of an image into the service dir with a
// near-atomic directory swap.
type ImageExtract struct {
	From  string `yaml:"from" json:"from"`
	To    string `yaml:"to" json:"to"`
	Image string `yaml:"image,omitempty" json:"image,omitempty"`
	Pull  bool   `yaml:"pull,omitempty" json:"pull,omitempty"`
}

func (a *ImageExtract) Validate() error {
	if a.From == "" {
		return errors.New("image.extract: \"from\" is required")
	}
	if a.To == "" {
		return errors.New("image.extract: \"to\" is required")
	}
	return nil
}

// ArtifactExtract downloads an artifact, verifies its sha256, unpacks it and
// swaps it into place.
type ArtifactExtract struct {
	URL    string `yaml:"url" json:"url"`
	SHA256 string `yaml:"sha256" json:"sha256"`
	To     string `yaml:"to" json:"to"`
}

func (a *ArtifactExtract) Validate() error {
	if a.URL == "" {
		return errors.New("artifact.extract: \"url\" is required")
	}
	if a.SHA256 == "" {
		return errors.New("artifact.extract: \"sha256\" is required (artifacts come from public URLs)")
	}
	if a.To == "" {
		return errors.New("artifact.extract: \"to\" is required")
	}
	return nil
}

// ComposePull is `docker compose pull`.
type ComposePull struct {
	Services []string `yaml:"services,omitempty" json:"services,omitempty"`
}

func (*ComposePull) Validate() error { return nil }

// ComposeUp is `docker compose up -d`.
type ComposeUp struct {
	ForceRecreate bool     `yaml:"force_recreate,omitempty" json:"force_recreate,omitempty"`
	Services      []string `yaml:"services,omitempty" json:"services,omitempty"`
}

func (*ComposeUp) Validate() error { return nil }

// ComposeRun is `docker compose run --rm` (one-off container).
type ComposeRun struct {
	Service string   `yaml:"service" json:"service"`
	Argv    []string `yaml:"argv" json:"argv"`
}

func (a *ComposeRun) Validate() error {
	if a.Service == "" {
		return errors.New("compose.run: \"service\" is required")
	}
	if len(a.Argv) == 0 {
		return errors.New("compose.run: \"argv\" is required")
	}
	return nil
}

// ComposeExec is `docker compose exec -T` (inside a running container).
type ComposeExec struct {
	Service string   `yaml:"service" json:"service"`
	Argv    []string `yaml:"argv" json:"argv"`
}

func (a *ComposeExec) Validate() error {
	if a.Service == "" {
		return errors.New("compose.exec: \"service\" is required")
	}
	if len(a.Argv) == 0 {
		return errors.New("compose.exec: \"argv\" is required")
	}
	return nil
}

// ComposeRestart is `docker compose restart`.
type ComposeRestart struct {
	Services []string `yaml:"services,omitempty" json:"services,omitempty"`
}

func (*ComposeRestart) Validate() error { return nil }

// EnvRequire asserts that keys in an env file have non-empty values.
type EnvRequire struct {
	File string   `yaml:"file" json:"file"`
	Keys []string `yaml:"keys" json:"keys"`
}

func (a *EnvRequire) Validate() error {
	if a.File == "" {
		return errors.New("env.require: \"file\" is required")
	}
	if len(a.Keys) == 0 {
		return errors.New("env.require: \"keys\" is required")
	}
	return nil
}

// EnvWrite upserts KEY=VALUE lines into a file.
type EnvWrite struct {
	File string            `yaml:"file" json:"file"`
	Set  map[string]string `yaml:"set" json:"set"`
}

func (a *EnvWrite) Validate() error {
	if a.File == "" {
		return errors.New("env.write: \"file\" is required")
	}
	if len(a.Set) == 0 {
		return errors.New("env.write: \"set\" is required")
	}
	return nil
}

// Healthcheck polls an HTTP endpoint until healthy or retries exhausted.
type Healthcheck struct {
	URL      string         `yaml:"url" json:"url"`
	Expect   int            `yaml:"expect,omitempty" json:"expect,omitempty"`
	Retries  int            `yaml:"retries,omitempty" json:"retries,omitempty"`
	Interval model.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
}

func (a *Healthcheck) Validate() error {
	if a.URL == "" {
		return errors.New("healthcheck: \"url\" is required")
	}
	return nil
}

func (a *Healthcheck) setDefaults() {
	if a.Expect == 0 {
		a.Expect = 200
	}
	if a.Retries == 0 {
		a.Retries = 5
	}
	if a.Interval == 0 {
		a.Interval = model.Duration(3 * time.Second)
	}
}

// Run executes one command in the service dir (argv array, no shell).
// Escape hatch for cases the typed vocabulary does not cover.
type Run struct {
	Argv []string `yaml:"argv" json:"argv"`
}

func (a *Run) Validate() error {
	if len(a.Argv) == 0 {
		return errors.New("run: \"argv\" is required")
	}
	return nil
}

// registry maps op names to constructors of their Args type.
var registry = map[string]func() Args{
	"image.pin":        func() Args { return &ImagePin{} },
	"image.extract":    func() Args { return &ImageExtract{} },
	"artifact.extract": func() Args { return &ArtifactExtract{} },
	"compose.pull":     func() Args { return &ComposePull{} },
	"compose.up":       func() Args { return &ComposeUp{} },
	"compose.run":      func() Args { return &ComposeRun{} },
	"compose.exec":     func() Args { return &ComposeExec{} },
	"compose.restart":  func() Args { return &ComposeRestart{} },
	"env.require":      func() Args { return &EnvRequire{} },
	"env.write":        func() Args { return &EnvWrite{} },
	"healthcheck":      func() Args { return &Healthcheck{} },
	"run":              func() Args { return &Run{} },
}
