package ddl

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// MarshalManifest renders a manifest back to YAML — the inverse of ParseManifest —
// so a generated plan can be materialized as a reviewable manifest file. Each
// operation is emitted with its "operation" discriminator first, then only its
// meaningful fields: empty strings, null pointers, false flags, and empty option
// blocks are omitted, so the output stays clean and round-trips through
// ParseManifest unchanged.
func MarshalManifest(m *Manifest) ([]byte, error) {
	operations := &yaml.Node{Kind: yaml.SequenceNode}
	for i, op := range m.Operations {
		node, err := operationNode(op)
		if err != nil {
			return nil, fmt.Errorf("operation %d (%s): %w", i, op.CommandType(), err)
		}
		operations.Content = append(operations.Content, node)
	}

	root := &yaml.Node{Kind: yaml.MappingNode}
	if m.Description != "" {
		addPair(root, "description", scalarNode(m.Description))
	}
	if m.Database != "" {
		addPair(root, "database", scalarNode(m.Database))
	}
	if m.OnFailure != "" {
		addPair(root, "on_failure", scalarNode(string(m.OnFailure)))
	}
	if m.Intent != "" {
		addPair(root, "intent", scalarNode(string(m.Intent)))
	}
	if m.AbortBlockingResumable {
		addPair(root, "abort_blocking_resumable", scalarNode("true"))
	}
	if m.Window != nil {
		win := &yaml.Node{Kind: yaml.MappingNode}
		addPair(win, "start", scalarNode(m.Window.Start))
		addPair(win, "end", scalarNode(m.Window.End))
		if len(m.Window.Days) > 0 {
			days := &yaml.Node{Kind: yaml.SequenceNode}
			for _, d := range m.Window.Days {
				days.Content = append(days.Content, scalarNode(d))
			}
			addPair(win, "days", days)
		}
		addPair(root, "window", win)
	}
	if len(m.IgnoreBlockedSessions) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for i, s := range m.IgnoreBlockedSessions {
			node, err := ignoredSessionNode(s)
			if err != nil {
				return nil, fmt.Errorf("ignore_blocked_sessions[%d]: %w", i, err)
			}
			seq.Content = append(seq.Content, node)
		}
		addPair(root, "ignore_blocked_sessions", seq)
	}
	addPair(root, "operations", operations)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close manifest encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// operationNode encodes one operation to a mapping node, compacts away empty
// fields, and prepends the "operation" discriminator.
func operationNode(op Operation) (*yaml.Node, error) {
	var node yaml.Node
	if err := node.Encode(op); err != nil {
		return nil, err
	}
	compact(&node)
	node.Content = append([]*yaml.Node{scalarNode("operation"), scalarNode(manifestDiscriminator(op))}, node.Content...)
	return &node, nil
}

// manifestDiscriminator returns the value ParseManifest matches on to decode op. It is
// CommandType for every operation except shrink: shrink's CommandType folds the file type
// in (shrink_data / shrink_log), but the manifest discriminator is plain "shrink" with the
// type carried by a separate field. Using CommandType here wrote "operation: shrink_data",
// which no longer parses — so MarshalManifest must map back to the parse discriminator to
// stay a true inverse of ParseManifest.
func manifestDiscriminator(op Operation) string {
	if _, ok := op.(Shrink); ok {
		return "shrink"
	}
	return op.CommandType()
}

// ignoredSessionNode encodes one ignore_blocked_sessions entry, dropping its unset
// fields (nil session_id and empty regexps) so the rendered rule stays minimal and
// round-trips through ParseManifest unchanged.
func ignoredSessionNode(s IgnoredSession) (*yaml.Node, error) {
	var node yaml.Node
	if err := node.Encode(s); err != nil {
		return nil, err
	}
	compact(&node)
	return &node, nil
}

// compact drops mapping entries whose value carries no information: a null, an
// empty string, a false boolean, or a mapping that became empty after its own
// compaction. This is safe for the generated operation set, where an omitted field
// decodes back to the same zero value (auto / off / whole-index).
func compact(n *yaml.Node) {
	if n.Kind != yaml.MappingNode {
		return
	}
	kept := n.Content[:0:0]
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		switch {
		case val.Kind == yaml.MappingNode:
			compact(val)
			if len(val.Content) == 0 {
				continue
			}
		case val.Kind == yaml.SequenceNode:
			if len(val.Content) == 0 {
				continue // an empty list carries no information and decodes back to nil
			}
		case isEmptyScalar(val):
			continue
		}
		kept = append(kept, key, val)
	}
	n.Content = kept
}

// isEmptyScalar reports whether a scalar node carries no information worth writing.
func isEmptyScalar(v *yaml.Node) bool {
	if v.Kind != yaml.ScalarNode {
		return false
	}
	switch {
	case v.Tag == "!!null":
		return true
	case v.Value == "":
		return true
	case v.Tag == "!!bool" && v.Value == "false":
		return true
	default:
		return false
	}
}

func addPair(m *yaml.Node, key string, val *yaml.Node) {
	m.Content = append(m.Content, scalarNode(key), val)
}

// scalarNode builds a scalar node, letting the encoder choose quoting/tagging.
func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}
