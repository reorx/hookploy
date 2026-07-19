// Package config loads, normalizes and statically validates hookploy.yaml.
// Normalization turns the `server: x` sugar into the canonical
// one-instance/one-wave form so the scheduler only ever sees one shape.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/ops"
)

// Config is the normalized, validated configuration.
type Config struct {
	Path           string
	Listen         Listen
	DB             string
	Servers        map[string]*Server
	DefaultTimeout time.Duration
	Services       map[string]*Service
	ServiceNames   []string // sorted, for stable listings
}

type Listen struct {
	HTTP string `yaml:"http"`
	GRPC string `yaml:"grpc"`
}

type Server struct {
	Name  string
	Local bool
}

// Instance is one deployment target of a service.
type Instance struct {
	Name   string
	Server string
	Dir    string
}

// Service is a normalized service definition: always instances + rollout.
type Service struct {
	Name      string
	Image     string
	Webhook   bool
	Timeout   time.Duration
	Deploy    []ops.Step
	Tasks     map[string][]ops.Step
	Instances []Instance
	Rollout   [][]string // waves of instance names
}

// Instance returns the named instance, or nil.
func (s *Service) Instance(name string) *Instance {
	for i := range s.Instances {
		if s.Instances[i].Name == name {
			return &s.Instances[i]
		}
	}
	return nil
}

const defaultTimeout = 10 * time.Minute

// Load reads, decodes, normalizes and validates a hookploy.yaml.
// All errors are prefixed with the file path; op/step errors carry lines.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Path = path
	if cfg.DB == "" {
		cfg.DB = filepath.Join(filepath.Dir(path), "hookploy.db")
	}
	return cfg, nil
}

func parse(data []byte) (*Config, error) {
	raw, err := decode(data)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Listen:         raw.Listen,
		DB:             raw.DB,
		Servers:        map[string]*Server{},
		DefaultTimeout: defaultTimeout,
		Services:       map[string]*Service{},
	}
	if cfg.Listen.HTTP == "" {
		cfg.Listen.HTTP = "127.0.0.1:9100"
	}
	if cfg.Listen.GRPC == "" {
		cfg.Listen.GRPC = "127.0.0.1:9101"
	}
	if raw.Defaults.Timeout != 0 {
		cfg.DefaultTimeout = time.Duration(raw.Defaults.Timeout)
	}
	for name, def := range raw.Servers {
		cfg.Servers[name] = &Server{Name: name, Local: def.Local}
	}

	for name, rs := range raw.Services {
		svc, err := normalizeService(name, rs, cfg)
		if err != nil {
			return nil, err
		}
		cfg.Services[name] = svc
		cfg.ServiceNames = append(cfg.ServiceNames, name)
	}
	sort.Strings(cfg.ServiceNames)
	return cfg, nil
}

func normalizeService(name string, rs *rawService, cfg *Config) (*Service, error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("service %q: %s", name, fmt.Sprintf(format, args...))
	}

	svc := &Service{
		Name:    name,
		Image:   rs.Image,
		Webhook: rs.Webhook == nil || *rs.Webhook,
		Timeout: cfg.DefaultTimeout,
	}
	if rs.Timeout != 0 {
		svc.Timeout = time.Duration(rs.Timeout)
	}

	// instances / server sugar
	switch {
	case rs.Server != "" && len(rs.Instances) > 0:
		return nil, fail("\"server\" and \"instances\" are mutually exclusive")
	case rs.Server != "":
		if rs.Dir == "" {
			return nil, fail("\"dir\" is required")
		}
		svc.Instances = []Instance{{Name: name, Server: rs.Server, Dir: rs.Dir}}
	case len(rs.Instances) > 0:
		for _, ri := range rs.Instances {
			inst := Instance{Name: ri.Name, Server: ri.Server, Dir: ri.Dir}
			if inst.Dir == "" {
				inst.Dir = rs.Dir
			}
			if inst.Server == "" {
				return nil, fail("instance %q: \"server\" is required", ri.Name)
			}
			if inst.Dir == "" {
				return nil, fail("instance %q: \"dir\" is required (no service-level dir)", ri.Name)
			}
			svc.Instances = append(svc.Instances, inst)
		}
	default:
		return nil, fail("one of \"server\" or \"instances\" is required")
	}
	for _, inst := range svc.Instances {
		if _, ok := cfg.Servers[inst.Server]; !ok {
			return nil, fail("instance %q references unknown server %q", inst.Name, inst.Server)
		}
	}

	// rollout
	if len(rs.Rollout) > 0 {
		if rs.Server != "" {
			return nil, fail("\"rollout\" requires \"instances\"")
		}
		seen := map[string]bool{}
		for _, wave := range rs.Rollout {
			for _, iname := range wave {
				if svc.Instance(iname) == nil {
					return nil, fail("rollout references unknown instance %q", iname)
				}
				if seen[iname] {
					return nil, fail("rollout must list instance %q exactly once", iname)
				}
				seen[iname] = true
			}
			svc.Rollout = append(svc.Rollout, wave)
		}
		for _, inst := range svc.Instances {
			if !seen[inst.Name] {
				return nil, fail("rollout must list instance %q exactly once", inst.Name)
			}
		}
	} else {
		// default: one wave per instance, declaration order
		for _, inst := range svc.Instances {
			svc.Rollout = append(svc.Rollout, []string{inst.Name})
		}
	}

	// pipelines
	if len(rs.Deploy) == 0 {
		return nil, fail("\"deploy\" pipeline is required")
	}
	deploySteps, err := parsePipeline(rs.Deploy)
	if err != nil {
		return nil, fmt.Errorf("service %q deploy: %w", name, err)
	}
	svc.Deploy = deploySteps
	if err := validatePipeline(svc, svc.Deploy); err != nil {
		return nil, fmt.Errorf("service %q deploy: %w", name, err)
	}
	if len(rs.Tasks) > 0 {
		svc.Tasks = map[string][]ops.Step{}
		for tname, nodes := range rs.Tasks {
			steps, err := parsePipeline(nodes)
			if err != nil {
				return nil, fmt.Errorf("service %q task %q: %w", name, tname, err)
			}
			if err := validatePipeline(svc, steps); err != nil {
				return nil, fmt.Errorf("service %q task %q: %w", name, tname, err)
			}
			svc.Tasks[tname] = steps
		}
	}
	return svc, nil
}

// validatePipeline enforces cross-op rules: image.pin needs a service image
// and a later compose.up (the built-in verification must have a target —
// a "white pin" is statically impossible).
func validatePipeline(svc *Service, steps []ops.Step) error {
	pinIdx := -1
	for i, s := range steps {
		if s.Op == "image.pin" {
			if svc.Image == "" {
				return fmt.Errorf("line %d: image.pin requires the service to declare \"image\"", s.Line)
			}
			pinIdx = i
		}
	}
	if pinIdx >= 0 {
		hasUpAfter := false
		for _, s := range steps[pinIdx+1:] {
			if s.Op == "compose.up" {
				hasUpAfter = true
			}
		}
		if !hasUpAfter {
			return fmt.Errorf("line %d: image.pin requires a compose.up later in the pipeline (its verification runs after the last compose.up)", steps[pinIdx].Line)
		}
	}
	return nil
}

var _ = model.Duration(0)
