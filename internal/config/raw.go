package config

import (
	"bytes"
	"fmt"

	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/ops"
	"gopkg.in/yaml.v3"
)

// rawFile is the on-disk shape before normalization. Decoding is strict:
// unknown fields are rejected with their line number.
type rawFile struct {
	Listen   Listen                 `yaml:"listen"`
	DB       string                 `yaml:"db"`
	WebUI    *bool                  `yaml:"webui"`
	Github   rawGithub              `yaml:"github"`
	Servers  map[string]rawServer   `yaml:"servers"`
	Defaults rawDefaults            `yaml:"defaults"`
	Services map[string]*rawService `yaml:"services"`
}

type rawGithub struct {
	WebhookSecret string `yaml:"webhook_secret"`
}

type rawServer struct {
	Local bool `yaml:"local"`
}

type rawDefaults struct {
	Timeout model.Duration `yaml:"timeout"`
}

type rawService struct {
	Server     string                 `yaml:"server"`
	Dir        string                 `yaml:"dir"`
	Image      string                 `yaml:"image"`
	Webhook    *bool                  `yaml:"webhook"`
	GithubRepo string                 `yaml:"github_repo"`
	Timeout    model.Duration         `yaml:"timeout"`
	Deploy     []yaml.Node            `yaml:"deploy"`
	Tasks      map[string][]yaml.Node `yaml:"tasks"`
	Instances  []rawInstance          `yaml:"instances"` // order preserved
	Rollout    [][]string             `yaml:"rollout"`
}

type rawInstance struct {
	Name   string
	Server string
	Dir    string
}

// instancesNode decodes the `instances:` mapping preserving declaration order.
func (rs *rawService) UnmarshalYAML(node *yaml.Node) error {
	type plain struct {
		Server     string                 `yaml:"server"`
		Dir        string                 `yaml:"dir"`
		Image      string                 `yaml:"image"`
		Webhook    *bool                  `yaml:"webhook"`
		GithubRepo string                 `yaml:"github_repo"`
		Timeout    model.Duration         `yaml:"timeout"`
		Deploy     []yaml.Node            `yaml:"deploy"`
		Tasks      map[string][]yaml.Node `yaml:"tasks"`
		Instances  yaml.Node              `yaml:"instances"`
		Rollout    []yaml.Node            `yaml:"rollout"`
	}
	var p plain
	if err := decodeStrictNode(node, &p); err != nil {
		return err
	}
	rs.Server, rs.Dir, rs.Image, rs.Webhook, rs.Timeout = p.Server, p.Dir, p.Image, p.Webhook, p.Timeout
	rs.GithubRepo = p.GithubRepo
	rs.Deploy, rs.Tasks = p.Deploy, p.Tasks

	if p.Instances.Kind != 0 {
		if p.Instances.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: \"instances\" must be a mapping", p.Instances.Line)
		}
		for i := 0; i < len(p.Instances.Content); i += 2 {
			key, val := p.Instances.Content[i], p.Instances.Content[i+1]
			var body struct {
				Server string `yaml:"server"`
				Dir    string `yaml:"dir"`
			}
			if err := decodeStrictNode(val, &body); err != nil {
				return fmt.Errorf("instance %q: %w", key.Value, err)
			}
			rs.Instances = append(rs.Instances, rawInstance{Name: key.Value, Server: body.Server, Dir: body.Dir})
		}
	}

	// rollout: scalar = single-instance wave, sequence = parallel wave
	for i := range p.Rollout {
		wnode := &p.Rollout[i]
		switch wnode.Kind {
		case yaml.ScalarNode:
			rs.Rollout = append(rs.Rollout, []string{wnode.Value})
		case yaml.SequenceNode:
			var wave []string
			if err := wnode.Decode(&wave); err != nil {
				return err
			}
			rs.Rollout = append(rs.Rollout, wave)
		default:
			return fmt.Errorf("line %d: rollout wave must be an instance name or a list of names", wnode.Line)
		}
	}
	return nil
}

// decodeStrictNode decodes a mapping node into out rejecting unknown keys.
func decodeStrictNode(node *yaml.Node, out any) error {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping", node.Line)
	}
	// Re-encode this subtree and run it through a strict decoder: this gives
	// KnownFields behavior with line numbers relative to the subtree, so we
	// prefix key errors with the original line instead.
	allowed := ops.YAMLFieldSet(out)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if !allowed[key.Value] {
			return fmt.Errorf("line %d: unknown field %q", key.Line, key.Value)
		}
	}
	return node.Decode(out)
}

func decode(data []byte) (*rawFile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawFile
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	return &raw, nil
}

// parsePipeline parses a list of step nodes.
func parsePipeline(nodes []yaml.Node) ([]ops.Step, error) {
	var steps []ops.Step
	for i := range nodes {
		s, err := ops.ParseStep(&nodes[i])
		if err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, nil
}
