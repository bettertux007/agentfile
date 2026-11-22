package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"gopkg.in/yaml.v3"
)

// AddSkillInput describes a skill entry to add or replace in Agentfile.yaml.
type AddSkillInput struct {
	Name    string
	Source  string
	Replace bool
}

// AddAgentInput describes an agent entry to add or replace in Agentfile.yaml.
type AddAgentInput struct {
	Name        string
	Source      string
	Description string
	Model       string
	Replace     bool
}

// AddRuleInput describes a rule entry to add or replace in Agentfile.yaml.
type AddRuleInput struct {
	Name        string
	Source      string
	Description string
	Globs       []string
	AlwaysApply bool
	Replace     bool
}

// AddMCPInput describes an MCP server entry to add or replace in Agentfile.yaml.
type AddMCPInput struct {
	Name      string
	Transport string
	Command   string
	URL       string
	Args      []string
	Env       map[string]string
	Headers   map[string]string
	Replace   bool
}

// AddSkill adds or replaces a skill entry in Agentfile.yaml while preserving comments.
func AddSkill(yamlBytes []byte, in AddSkillInput) ([]byte, error) {
	entry := buildSkillNode(in)
	return addSequenceEntry(yamlBytes, "skills", "skill", in.Name, in.Replace, entry)
}

// AddAgent adds or replaces an agent entry in Agentfile.yaml while preserving comments.
func AddAgent(yamlBytes []byte, in AddAgentInput) ([]byte, error) {
	entry := buildAgentNode(in)
	return addSequenceEntry(yamlBytes, "agents", "agent", in.Name, in.Replace, entry)
}

// AddRule adds or replaces a rule entry in Agentfile.yaml while preserving comments.
func AddRule(yamlBytes []byte, in AddRuleInput) ([]byte, error) {
	entry := buildRuleNode(in)
	return addSequenceEntry(yamlBytes, "rules", "rule", in.Name, in.Replace, entry)
}

// AddMCP adds or replaces an MCP server entry in Agentfile.yaml while preserving comments.
func AddMCP(yamlBytes []byte, in AddMCPInput) ([]byte, error) {
	doc, root, err := parseManifestAST(yamlBytes)
	if err != nil {
		return nil, err
	}

	serverNode := buildMCPNode(in)
	idx := findMappingKeyIndex(root, "mcp")
	if idx < 0 {
		root.Style = 0
		mapNode := &yaml.Node{
			Kind:    yaml.MappingNode,
			Tag:     "!!map",
			Content: []*yaml.Node{strNode(in.Name), serverNode},
		}
		root.Content = append(root.Content, strNode("mcp"), mapNode)
	} else {
		keyNode := root.Content[idx]
		valNode := root.Content[idx+1]
		if err := rejectAnchorsAndMerges(keyNode, "mcp"); err != nil {
			return nil, err
		}
		if err := rejectAnchorsAndMerges(valNode, "mcp"); err != nil {
			return nil, err
		}

		if valNode.Kind == yaml.ScalarNode && valNode.Tag == "!!null" {
			valNode.Kind = yaml.MappingNode
			valNode.Tag = "!!map"
			valNode.Value = ""
			valNode.Style = 0
			valNode.Content = nil
		} else if valNode.Kind != yaml.MappingNode {
			return nil, NewPolicyError("mcp section must be a mapping")
		}
		if valNode.LineComment != "" && keyNode.LineComment == "" {
			keyNode.LineComment = valNode.LineComment
			valNode.LineComment = ""
		}
		valNode.Style = 0

		entryIdx := findMappingKeyIndex(valNode, in.Name)
		if entryIdx >= 0 {
			if !in.Replace {
				return nil, NewPolicyError("mcp %q already exists (use --replace to overwrite)", in.Name)
			}
			oldVal := valNode.Content[entryIdx+1]
			serverNode.FootComment = oldVal.FootComment
			valNode.Content[entryIdx+1] = serverNode
		} else {
			valNode.Content = append(valNode.Content, strNode(in.Name), serverNode)
		}
	}

	return encodeAndValidateAST(doc)
}

// RemoveEntry removes a named entry from skills, agents, rules, or mcp in Agentfile.yaml.
func RemoveEntry(yamlBytes []byte, kind, name string) ([]byte, error) {
	section, isSeq, err := resolveKindSection(kind)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, NewPolicyError("%s name cannot be empty", kind)
	}

	doc, root, err := parseManifestAST(yamlBytes)
	if err != nil {
		return nil, err
	}

	secIdx := findMappingKeyIndex(root, section)
	if secIdx < 0 {
		return nil, NewPolicyError("%s %q not found", kind, name)
	}

	keyNode := root.Content[secIdx]
	valNode := root.Content[secIdx+1]
	if err := rejectAnchorsAndMerges(keyNode, section); err != nil {
		return nil, err
	}
	if err := rejectAnchorsAndMerges(valNode, section); err != nil {
		return nil, err
	}

	if isSeq {
		if valNode.Kind == yaml.ScalarNode && valNode.Tag == "!!null" {
			return nil, NewPolicyError("%s %q not found", kind, name)
		}
		if valNode.Kind != yaml.SequenceNode {
			return nil, NewPolicyError("%s section must be a sequence", section)
		}
		entryIdx := findSequenceEntryIndex(valNode, name)
		if entryIdx < 0 {
			return nil, NewPolicyError("%s %q not found", kind, name)
		}
		valNode.Content = append(valNode.Content[:entryIdx], valNode.Content[entryIdx+1:]...)
		if len(valNode.Content) == 0 {
			if nodeHasComments(keyNode) || nodeHasComments(valNode) {
				valNode.Kind = yaml.SequenceNode
				valNode.Tag = "!!seq"
				valNode.Style = yaml.FlowStyle
				valNode.Value = ""
				valNode.Content = nil
				if keyNode.LineComment != "" && valNode.LineComment == "" {
					valNode.LineComment = keyNode.LineComment
					keyNode.LineComment = ""
				}
			} else {
				root.Content = append(root.Content[:secIdx], root.Content[secIdx+2:]...)
			}
		}
	} else {
		if valNode.Kind == yaml.ScalarNode && valNode.Tag == "!!null" {
			return nil, NewPolicyError("%s %q not found", kind, name)
		}
		if valNode.Kind != yaml.MappingNode {
			return nil, NewPolicyError("%s section must be a mapping", section)
		}
		entryIdx := findMappingKeyIndex(valNode, name)
		if entryIdx < 0 {
			return nil, NewPolicyError("%s %q not found", kind, name)
		}
		valNode.Content = append(valNode.Content[:entryIdx], valNode.Content[entryIdx+2:]...)
		if len(valNode.Content) == 0 {
			if nodeHasComments(keyNode) || nodeHasComments(valNode) {
				valNode.Kind = yaml.MappingNode
				valNode.Tag = "!!map"
				valNode.Style = yaml.FlowStyle
				valNode.Value = ""
				valNode.Content = nil
				if keyNode.LineComment != "" && valNode.LineComment == "" {
					valNode.LineComment = keyNode.LineComment
					keyNode.LineComment = ""
				}
			} else {
				root.Content = append(root.Content[:secIdx], root.Content[secIdx+2:]...)
			}
		}
	}

	return encodeAndValidateAST(doc)
}

func addSequenceEntry(yamlBytes []byte, section, kind, name string, replace bool, entry *yaml.Node) ([]byte, error) {
	doc, root, err := parseManifestAST(yamlBytes)
	if err != nil {
		return nil, err
	}

	secIdx := findMappingKeyIndex(root, section)
	if secIdx < 0 {
		root.Style = 0
		seqNode := &yaml.Node{
			Kind:    yaml.SequenceNode,
			Tag:     "!!seq",
			Content: []*yaml.Node{entry},
		}
		root.Content = append(root.Content, strNode(section), seqNode)
	} else {
		keyNode := root.Content[secIdx]
		valNode := root.Content[secIdx+1]
		if err := rejectAnchorsAndMerges(keyNode, section); err != nil {
			return nil, err
		}
		if err := rejectAnchorsAndMerges(valNode, section); err != nil {
			return nil, err
		}

		if valNode.Kind == yaml.ScalarNode && valNode.Tag == "!!null" {
			valNode.Kind = yaml.SequenceNode
			valNode.Tag = "!!seq"
			valNode.Value = ""
			valNode.Style = 0
			valNode.Content = nil
		} else if valNode.Kind != yaml.SequenceNode {
			return nil, NewPolicyError("%s section must be a sequence", section)
		}
		if valNode.LineComment != "" && keyNode.LineComment == "" {
			keyNode.LineComment = valNode.LineComment
			valNode.LineComment = ""
		}
		valNode.Style = 0

		entryIdx := findSequenceEntryIndex(valNode, name)
		if entryIdx >= 0 {
			if !replace {
				return nil, NewPolicyError("%s %q already exists (use --replace to overwrite)", kind, name)
			}
			oldEntry := valNode.Content[entryIdx]
			entry.HeadComment = oldEntry.HeadComment
			entry.FootComment = oldEntry.FootComment
			valNode.Content[entryIdx] = entry
		} else {
			valNode.Content = append(valNode.Content, entry)
		}
	}

	return encodeAndValidateAST(doc)
}

func parseManifestAST(yamlBytes []byte) (*yaml.Node, *yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(yamlBytes))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, NewPolicyError("Agentfile.yaml is empty")
		}
		return nil, nil, NewPolicyError("invalid Agentfile.yaml: %v", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, nil, NewPolicyError("Agentfile.yaml must contain a single YAML document")
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, NewPolicyError("Agentfile.yaml root must be a YAML mapping")
	}
	return &doc, doc.Content[0], nil
}

func encodeAndValidateAST(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode Agentfile.yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	out := buf.Bytes()
	if _, err := ParseRaw(bytes.NewReader(out)); err != nil {
		return nil, err
	}
	return out, nil
}

func resolveKindSection(kind string) (section string, isSeq bool, err error) {
	switch kind {
	case "skill":
		return "skills", true, nil
	case "agent":
		return "agents", true, nil
	case "rule":
		return "rules", true, nil
	case "mcp":
		return "mcp", false, nil
	default:
		return "", false, NewPolicyError("unsupported entry kind %q (expected skill, agent, rule, or mcp)", kind)
	}
}

func rejectAnchorsAndMerges(node *yaml.Node, section string) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode {
		return NewPolicyError("%s contains YAML alias which is not supported for automated edits", section)
	}
	if node.Anchor != "" {
		return NewPolicyError("%s contains YAML anchor (&%s) which is not supported for automated edits", section, node.Anchor)
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			k := node.Content[i]
			if k.Tag == "!!merge" || (k.Kind == yaml.ScalarNode && k.Value == "<<") {
				return NewPolicyError("%s contains YAML merge key (<<) which is not supported for automated edits", section)
			}
		}
	}
	for _, child := range node.Content {
		if err := rejectAnchorsAndMerges(child, section); err != nil {
			return err
		}
	}
	return nil
}

func findMappingKeyIndex(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			return i
		}
	}
	return -1
}

func findSequenceEntryIndex(seq *yaml.Node, name string) int {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return -1
	}
	for i, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		nameIdx := findMappingKeyIndex(item, "name")
		if nameIdx >= 0 {
			v := item.Content[nameIdx+1]
			if v.Kind == yaml.ScalarNode && v.Value == name {
				return i
			}
		}
	}
	return -1
}

func nodeHasComments(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	return n.HeadComment != "" || n.LineComment != "" || n.FootComment != ""
}

func strNode(v string) *yaml.Node {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: v,
	}
}

func strSliceNode(items []string) *yaml.Node {
	seq := &yaml.Node{
		Kind:    yaml.SequenceNode,
		Tag:     "!!seq",
		Content: make([]*yaml.Node, 0, len(items)),
	}
	for _, item := range items {
		seq.Content = append(seq.Content, strNode(item))
	}
	return seq
}

func sortedStrMapNode(m map[string]string) *yaml.Node {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	mp := &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: make([]*yaml.Node, 0, len(keys)*2),
	}
	for _, k := range keys {
		mp.Content = append(mp.Content, strNode(k), strNode(m[k]))
	}
	return mp
}

func buildSkillNode(in AddSkillInput) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			strNode("name"), strNode(in.Name),
			strNode("source"), strNode(in.Source),
		},
	}
}

func buildAgentNode(in AddAgentInput) *yaml.Node {
	content := []*yaml.Node{
		strNode("name"), strNode(in.Name),
	}
	if in.Description != "" {
		content = append(content, strNode("description"), strNode(in.Description))
	}
	if in.Model != "" {
		content = append(content, strNode("model"), strNode(in.Model))
	}
	content = append(content, strNode("source"), strNode(in.Source))
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: content,
	}
}

func buildRuleNode(in AddRuleInput) *yaml.Node {
	content := []*yaml.Node{
		strNode("name"), strNode(in.Name),
		strNode("source"), strNode(in.Source),
	}
	if in.Description != "" {
		content = append(content, strNode("description"), strNode(in.Description))
	}
	if len(in.Globs) > 0 {
		content = append(content, strNode("globs"), strSliceNode(in.Globs))
	}
	if in.AlwaysApply {
		content = append(content, strNode("always_apply"), &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!bool",
			Value: "true",
		})
	}
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: content,
	}
}

func buildMCPNode(in AddMCPInput) *yaml.Node {
	content := []*yaml.Node{
		strNode("transport"), strNode(in.Transport),
	}
	if in.Command != "" {
		content = append(content, strNode("command"), strNode(in.Command))
	}
	if len(in.Args) > 0 {
		content = append(content, strNode("args"), strSliceNode(in.Args))
	}
	if len(in.Env) > 0 {
		content = append(content, strNode("env"), sortedStrMapNode(in.Env))
	}
	if in.URL != "" {
		content = append(content, strNode("url"), strNode(in.URL))
	}
	if len(in.Headers) > 0 {
		content = append(content, strNode("headers"), sortedStrMapNode(in.Headers))
	}
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: content,
	}
}
