package ops

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// OnKey is the reserved step key that restricts an op to some instances.
// It is not an op name, so it can never collide with the vocabulary.
const OnKey = "on"

// ParseStep parses one pipeline entry. Three forms:
//
//   - compose.pull                      (string = zero-arg op)
//   - compose.up: { services: [web] }   (single-key map = op with args)
//   - compose.run: { ... }              (op name + `on:` = op restricted to
//     on: [main]                         the named instances)
func ParseStep(node *yaml.Node) (Step, error) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return newStep(node.Value, node, nil)
	case yaml.MappingNode:
		return parseMapStep(node)
	default:
		return Step{}, fmt.Errorf("line %d: op step must be a string or a map", node.Line)
	}
}

// parseMapStep handles the map forms: exactly one op name, plus an optional
// `on:` in either key order.
func parseMapStep(node *yaml.Node) (Step, error) {
	var nameNode, argsNode, onNode *yaml.Node
	for i := 0; i < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if key.Value == OnKey {
			if onNode != nil {
				return Step{}, fmt.Errorf("line %d: duplicate %q key", key.Line, OnKey)
			}
			onNode = val
			continue
		}
		if nameNode != nil {
			return Step{}, fmt.Errorf(
				"line %d: an op step map takes one op name plus an optional %q, so %q has no place here",
				key.Line, OnKey, key.Value)
		}
		nameNode, argsNode = key, val
	}
	if nameNode == nil {
		return Step{}, fmt.Errorf("line %d: op step names no op", node.Line)
	}
	on, err := parseOn(onNode)
	if err != nil {
		return Step{}, err
	}
	step, err := newStep(nameNode.Value, nameNode, argsNode)
	if err != nil {
		return Step{}, err
	}
	step.On = on
	return step, nil
}

// parseOn normalizes the `on:` value: one instance name or a list of them.
func parseOn(node *yaml.Node) ([]string, error) {
	if node == nil {
		return nil, nil
	}
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	empty := fmt.Errorf("line %d: %q must name at least one instance", node.Line, OnKey)
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			return nil, empty
		}
		return []string{node.Value}, nil
	case yaml.SequenceNode:
		var names []string
		if err := node.Decode(&names); err != nil {
			return nil, fmt.Errorf("line %d: %q: %w", node.Line, OnKey, err)
		}
		if len(names) == 0 {
			return nil, empty
		}
		return names, nil
	default:
		return nil, fmt.Errorf("line %d: %q must be an instance name or a list of instance names", node.Line, OnKey)
	}
}

// isNull reports whether a node is an explicit or implicit YAML null, which
// is what `- image.pin:` leaves behind when the op takes no args but needs
// an `on:` next to it.
func isNull(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == "!!null"
}

func newStep(name string, nameNode, argsNode *yaml.Node) (Step, error) {
	construct, ok := registry[name]
	if !ok {
		return Step{}, fmt.Errorf("line %d: unknown op %q", nameNode.Line, name)
	}
	args := construct()
	if argsNode != nil && !isNull(argsNode) {
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
