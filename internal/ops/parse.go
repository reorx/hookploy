package ops

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseStep parses one pipeline entry. Two forms:
//
//   - compose.pull                      (string = zero-arg op)
//   - compose.up: { services: [web] }   (single-key map = op with args)
func ParseStep(node *yaml.Node) (Step, error) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return newStep(node.Value, node, nil)
	case yaml.MappingNode:
		if len(node.Content) != 2 {
			return Step{}, fmt.Errorf("line %d: op step must be a single-key map", node.Line)
		}
		return newStep(node.Content[0].Value, node.Content[0], node.Content[1])
	default:
		return Step{}, fmt.Errorf("line %d: op step must be a string or a single-key map", node.Line)
	}
}

func newStep(name string, nameNode, argsNode *yaml.Node) (Step, error) {
	construct, ok := registry[name]
	if !ok {
		return Step{}, fmt.Errorf("line %d: unknown op %q", nameNode.Line, name)
	}
	args := construct()
	if argsNode != nil {
		if err := decodeStrict(argsNode, args); err != nil {
			return Step{}, fmt.Errorf("op %s: %w", name, err)
		}
	}
	if d, ok := args.(defaulter); ok {
		d.setDefaults()
	}
	if err := args.Validate(); err != nil {
		return Step{}, fmt.Errorf("line %d: %w", nameNode.Line, err)
	}
	return Step{Op: name, Args: args, Line: nameNode.Line}, nil
}

// decodeStrict decodes a mapping node into out, rejecting unknown keys
// (yaml.Node.Decode alone is not strict).
func decodeStrict(node *yaml.Node, out any) error {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping", node.Line)
	}
	allowed := yamlFieldSet(reflect.TypeOf(out).Elem())
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if !allowed[key.Value] {
			return fmt.Errorf("line %d: unknown field %q", key.Line, key.Value)
		}
	}
	return node.Decode(out)
}

// YAMLFieldSet returns the yaml keys accepted by the struct pointed to by
// out. Shared with package config for strict node decoding.
func YAMLFieldSet(out any) map[string]bool {
	return yamlFieldSet(reflect.TypeOf(out).Elem())
}

// yamlFieldSet collects the yaml key names a struct accepts.
func yamlFieldSet(t reflect.Type) map[string]bool {
	set := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		tag := f.Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		switch name {
		case "-":
			continue
		case "":
			name = strings.ToLower(f.Name)
		}
		set[name] = true
	}
	return set
}
