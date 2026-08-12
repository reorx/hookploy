// Package config loads, normalizes and statically validates hookploy.yaml.
// Normalization turns the `server: x` sugar into the canonical
// one-instance/one-wave form so the scheduler only ever sees one shape.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/ops"
)

// Config is the normalized, validated configuration.
type Config struct {
	Path           string
	Listen         Listen
	DB             string
	WebUI          bool
	Github         Github
	Notify         Notify
	Servers        map[string]*Server
	DefaultTimeout time.Duration
	Services       map[string]*Service
	ServiceNames   []string // sorted, for stable listings
}

// Github holds the GitHub integration settings. The webhook endpoint
// (POST /github/webhook) stays closed until WebhookSecret is set.
type Github struct {
	WebhookSecret string
}

// Notify holds the deploy-notification settings. Exactly one backend is
// live at a time (Provider); an empty Provider disables notifications
// wholesale, the way an empty Github.WebhookSecret closes the GitHub
// endpoint. Credentials are read at send time and never surface in any DTO.
type Notify struct {
	Provider string // "" (off) | "telegram"
	BaseURL  string // deploy-detail link prefix; "" omits the link
	Events   []model.EventKind
	// EdgeOfflineAfter is how long an edge must be gone before edge.offline
	// fires. Always positive after parseNotify.
	EdgeOfflineAfter time.Duration
	Telegram         Telegram
}

// Wants reports whether the global policy asks for kind. It is the
// node-scoped counterpart of ServiceNotify.Wants: main.started and the
// edge.* pair belong to no service, so this list is their whole policy.
func (n Notify) Wants(kind model.EventKind) bool {
	for _, k := range n.Events {
		if k == kind {
			return true
		}
	}
	return false
}

// Telegram holds the Bot API credentials of the telegram provider.
type Telegram struct {
	BotToken string
	ChatID   string
}

// ServiceNotify is one service's *resolved* notification policy: the global
// notify block already merged with the service's override, so nothing
// downstream has to know which value came from where. Same idea as the
// `server:` sugar — normalization collapses the shapes and the rest of the
// program only ever sees the canonical one.
type ServiceNotify struct {
	Enabled bool
	Events  map[model.EventKind]bool
}

// Wants reports whether this service should be notified about kind.
func (n ServiceNotify) Wants(kind model.EventKind) bool {
	return n.Enabled && n.Events[kind]
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
	Name       string
	Image      string
	Webhook    bool
	GithubRepo string // owner/repo, links workflow_run events to this service
	Notify     ServiceNotify
	Timeout    time.Duration
	Deploy     []ops.Step
	Tasks      map[string][]ops.Step
	Instances  []Instance
	Rollout    [][]string // waves of instance names
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
		WebUI:          raw.WebUI == nil || *raw.WebUI,
		Github:         Github{WebhookSecret: raw.Github.WebhookSecret},
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
	notify, err := parseNotify(raw.Notify)
	if err != nil {
		return nil, fmt.Errorf("notify: %w", err)
	}
	cfg.Notify = notify
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

// ProviderTelegram is the only notification backend implemented so far. The
// notification-center backend lands as a second value here.
const ProviderTelegram = "telegram"

// defaultEdgeOfflineAfter is well past edgehub's 60s reconnect grace, so the
// stream flapping that grace exists to absorb never reaches Telegram.
const defaultEdgeOfflineAfter = 5 * time.Minute

// parseNotify validates the global notify block and fills its defaults. A
// telegram provider missing its credentials fails the load rather than
// silently going quiet at 3am: a notification channel that is configured but
// dead is worse than one that is plainly off.
func parseNotify(raw rawNotify) (Notify, error) {
	n := Notify{
		Provider:         raw.Provider,
		BaseURL:          strings.TrimRight(raw.BaseURL, "/"),
		Events:           model.DefaultEventKinds(),
		EdgeOfflineAfter: defaultEdgeOfflineAfter,
		Telegram:         Telegram{BotToken: raw.Telegram.BotToken, ChatID: raw.Telegram.ChatID},
	}
	// Zero means unset, the same convention defaults.timeout uses. A negative
	// one parses fine and would quietly turn outage alerting off — the same
	// "configured but dead" failure the credential check above exists to
	// prevent — so it fails the load instead.
	if d := time.Duration(raw.EdgeOfflineAfter); d < 0 {
		return Notify{}, fmt.Errorf("edge_offline_after must be positive, got %s", d)
	} else if d > 0 {
		n.EdgeOfflineAfter = d
	}
	switch raw.Provider {
	case "":
	case ProviderTelegram:
		if n.Telegram.BotToken == "" || n.Telegram.ChatID == "" {
			return Notify{}, fmt.Errorf("provider %q requires telegram.bot_token and telegram.chat_id", raw.Provider)
		}
	case "center":
		return Notify{}, fmt.Errorf("provider \"center\" is not implemented yet")
	default:
		return Notify{}, fmt.Errorf("provider must be %q or \"center\", got %q", ProviderTelegram, raw.Provider)
	}
	if raw.Events != nil {
		events, err := parseEventKinds(*raw.Events)
		if err != nil {
			return Notify{}, fmt.Errorf("events: %w", err)
		}
		n.Events = events
	}
	return n, nil
}

// parseEventKinds validates yaml event names against the vocabulary.
func parseEventKinds(names []string) ([]model.EventKind, error) {
	out := make([]model.EventKind, 0, len(names))
	for _, s := range names {
		k := model.EventKind(s)
		if !k.Valid() {
			return nil, fmt.Errorf("unknown event %q (want one of %v)", s, model.EventKinds())
		}
		out = append(out, k)
	}
	return out, nil
}

// resolveServiceNotify merges the global notify block with a service's
// override into the one policy the service actually runs under. An override
// replaces the event list rather than adding to it, so a service can be made
// quieter as well as louder.
//
// Node events are a service's business either way: naming one in an override
// fails the load, and inheriting one from the global list drops it silently.
// The asymmetry is deliberate — the override is something the user wrote and
// expects to work, while the inherited list is a default they never asked
// for and should not have to defend against.
func resolveServiceNotify(rs *rawService, cfg *Config) (ServiceNotify, error) {
	events := cfg.Notify.Events
	sn := ServiceNotify{Enabled: true}
	if rs.Notify != nil {
		if rs.Notify.Enabled != nil {
			sn.Enabled = *rs.Notify.Enabled
		}
		if rs.Notify.Events != nil {
			var err error
			if events, err = parseEventKinds(*rs.Notify.Events); err != nil {
				return ServiceNotify{}, fmt.Errorf("events: %w", err)
			}
			for _, k := range events {
				if k.Scope() == model.ScopeNode {
					return ServiceNotify{}, fmt.Errorf(
						"events: %q is about a node, not a deploy, and belongs in the top-level notify block", k)
				}
			}
		}
	}
	sn.Events = make(map[model.EventKind]bool, len(events))
	for _, k := range events {
		if k.Scope() == model.ScopeNode {
			continue
		}
		sn.Events[k] = true
	}
	return sn, nil
}

func normalizeService(name string, rs *rawService, cfg *Config) (*Service, error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("service %q: %s", name, fmt.Sprintf(format, args...))
	}

	svc := &Service{
		Name:       name,
		Image:      rs.Image,
		Webhook:    rs.Webhook == nil || *rs.Webhook,
		GithubRepo: rs.GithubRepo,
		Timeout:    cfg.DefaultTimeout,
	}
	if rs.Timeout != 0 {
		svc.Timeout = time.Duration(rs.Timeout)
	}
	notify, err := resolveServiceNotify(rs, cfg)
	if err != nil {
		return nil, fail("notify: %s", err)
	}
	svc.Notify = notify
	if svc.GithubRepo != "" {
		owner, repo, ok := strings.Cut(svc.GithubRepo, "/")
		if !ok || owner == "" || repo == "" ||
			strings.ContainsAny(svc.GithubRepo, " \t") || strings.Contains(repo, "/") {
			return nil, fail("github_repo must look like \"owner/repo\", got %q", svc.GithubRepo)
		}
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
			// A task already picks its target with --instance; a second way to
			// address instances would only be a way to disagree with the first.
			for _, s := range steps {
				if len(s.On) > 0 {
					return nil, fmt.Errorf(
						"service %q task %q: line %d: %q does not apply to tasks (a task picks its target with --instance)",
						name, tname, s.Line, ops.OnKey)
				}
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
// a "white pin" is statically impossible), and `on:` must name instances of
// this service without leaving any of them idle.
//
// The pin rule is checked per instance because `on:` makes the pipeline an
// instance actually runs a subset of the one written down — a compose.up
// targeted away from an instance cannot verify that instance's pin.
func validatePipeline(svc *Service, steps []ops.Step) error {
	for _, s := range steps {
		if s.Op == "image.pin" && svc.Image == "" {
			return fmt.Errorf("line %d: image.pin requires the service to declare \"image\"", s.Line)
		}
		for _, target := range s.On {
			if svc.Instance(target) == nil {
				return fmt.Errorf("line %d: %q references unknown instance %q", s.Line, ops.OnKey, target)
			}
		}
	}
	for _, inst := range svc.Instances {
		mine := ops.StepsFor(steps, inst.Name)
		// An instance every step is targeted away from would deploy nothing
		// at all: always a typo, never something worth expressing this way.
		if len(steps) > 0 && len(mine) == 0 {
			return fmt.Errorf("every step is restricted by %q, leaving instance %q with nothing to run", ops.OnKey, inst.Name)
		}
		if err := validatePin(mine); err != nil {
			return err
		}
	}
	return nil
}

// validatePin checks that an image.pin has a compose.up after it to verify.
func validatePin(steps []ops.Step) error {
	pinIdx := -1
	for i, s := range steps {
		if s.Op == "image.pin" {
			pinIdx = i
		}
	}
	if pinIdx < 0 {
		return nil
	}
	for _, s := range steps[pinIdx+1:] {
		if s.Op == "compose.up" {
			return nil
		}
	}
	return fmt.Errorf("line %d: image.pin requires a compose.up later in the pipeline (its verification runs after the last compose.up)", steps[pinIdx].Line)
}
