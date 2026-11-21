package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var validSlugRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

var knownTargets = map[string]bool{
	"claude":   true,
	"opencode": true,
	"gemini":   true,
	"cursor":   true,
	"codex":    true,
	"copilot":  true,
	"windsurf": true,
}

// ParseFile reads and validates a standalone Agentfile.yaml from disk.
func ParseFile(path string) (*ProjectSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(bytes.NewReader(data))
}

// ParseRawFile reads an Agentfile.yaml from disk and validates raw layer rules
// (allowing targets to be omitted when extends is non-empty).
func ParseRawFile(path string) (*ProjectSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseRaw(bytes.NewReader(data))
}

// ParseBaseFile reads an inherited base Agentfile.yaml from disk and validates
// layer fields (allowing targets to be omitted in base manifests).
func ParseBaseFile(path string) (*ProjectSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseBase(bytes.NewReader(data))
}

// Parse strictly decodes and validates a standalone Agentfile.yaml v1 stream.
func Parse(r io.Reader) (*ProjectSpec, error) {
	spec, err := decodeYAMLSpec(r)
	if err != nil {
		return nil, err
	}
	if err := Validate(spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// ParseRaw strictly decodes an Agentfile.yaml v1 stream and allows targets to be
// omitted when extends is non-empty.
func ParseRaw(r io.Reader) (*ProjectSpec, error) {
	spec, err := decodeYAMLSpec(r)
	if err != nil {
		return nil, err
	}
	if err := ValidateRaw(spec, false); err != nil {
		return nil, err
	}
	return spec, nil
}

// ParseBase strictly decodes an inherited base manifest stream (targets optional).
func ParseBase(r io.Reader) (*ProjectSpec, error) {
	spec, err := decodeYAMLSpec(r)
	if err != nil {
		return nil, err
	}
	if err := ValidateRaw(spec, true); err != nil {
		return nil, err
	}
	return spec, nil
}

func decodeYAMLSpec(r io.Reader) (*ProjectSpec, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var spec ProjectSpec
	if err := dec.Decode(&spec); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, NewPolicyError("Agentfile.yaml is empty")
		}
		var pe *PolicyError
		if errors.As(err, &pe) {
			return nil, pe
		}
		return nil, NewPolicyError("invalid Agentfile.yaml: %v", err)
	}

	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, NewPolicyError("Agentfile.yaml must contain a single YAML document")
	}
	return &spec, nil
}

// Validate checks structural and security invariants on a standalone or effective ProjectSpec.
func Validate(s *ProjectSpec) error {
	return ValidateEffective(s)
}

// ValidateEffective checks structural and security invariants on a post-merge ProjectSpec.
func ValidateEffective(s *ProjectSpec) error {
	if s == nil {
		return NewPolicyError("ProjectSpec is nil")
	}
	if err := ValidateRaw(s, true); err != nil {
		return err
	}
	if len(s.Targets) == 0 {
		return NewPolicyError("Agentfile.yaml must specify at least one target in 'targets'")
	}
	return nil
}

// ValidateRaw checks structural and security invariants on a single manifest layer.
// When allowEmptyTargets is false, targets may only be omitted if len(s.Extends) > 0.
func ValidateRaw(s *ProjectSpec, allowEmptyTargets bool) error {
	if s == nil {
		return NewPolicyError("ProjectSpec is nil")
	}
	if s.Version != 1 {
		return NewPolicyError("unsupported Agentfile.yaml version %d (expected 1)", s.Version)
	}
	if !allowEmptyTargets && len(s.Targets) == 0 && len(s.Extends) == 0 {
		return NewPolicyError("Agentfile.yaml must specify at least one target in 'targets'")
	}
	seenTargets := make(map[string]bool, len(s.Targets))
	for _, t := range s.Targets {
		if !knownTargets[t] {
			return NewPolicyError("unknown target %q (supported: claude, opencode, gemini, cursor, codex, copilot, windsurf)", t)
		}
		if seenTargets[t] {
			return NewPolicyError("duplicate target %q", t)
		}
		seenTargets[t] = true
	}

	for name, srv := range s.MCP {
		if !validSlugRe.MatchString(name) {
			return NewPolicyError("invalid mcp server name %q (must match %s)", name, validSlugRe.String())
		}
		switch srv.Transport {
		case "stdio":
			if strings.TrimSpace(srv.Command.Raw) == "" {
				return NewPolicyError("mcp.%s: stdio transport requires non-empty 'command'", name)
			}
			if strings.TrimSpace(srv.URL.Raw) != "" || len(srv.Headers) > 0 {
				return NewPolicyError("mcp.%s: stdio transport cannot specify 'url' or 'headers'", name)
			}
		case "http", "sse":
			if strings.TrimSpace(srv.URL.Raw) == "" {
				return NewPolicyError("mcp.%s: %s transport requires non-empty 'url'", name, srv.Transport)
			}
			if strings.TrimSpace(srv.Command.Raw) != "" || len(srv.Args) > 0 || len(srv.Env) > 0 {
				return NewPolicyError("mcp.%s: %s transport cannot specify 'command', 'args', or 'env'", name, srv.Transport)
			}
		default:
			return NewPolicyError("mcp.%s: unsupported transport %q (expected stdio, http, or sse)", name, srv.Transport)
		}
	}

	seenSkills := make(map[string]bool, len(s.Skills))
	for _, sk := range s.Skills {
		if !validSlugRe.MatchString(sk.Name) {
			return NewPolicyError("invalid skill name %q (must match %s)", sk.Name, validSlugRe.String())
		}
		if seenSkills[sk.Name] {
			return NewPolicyError("duplicate skill name %q", sk.Name)
		}
		seenSkills[sk.Name] = true
	}

	seenAgents := make(map[string]bool, len(s.Agents))
	for _, ag := range s.Agents {
		if !validSlugRe.MatchString(ag.Name) {
			return NewPolicyError("invalid agent name %q (must match %s)", ag.Name, validSlugRe.String())
		}
		if seenAgents[ag.Name] {
			return NewPolicyError("duplicate agent name %q", ag.Name)
		}
		seenAgents[ag.Name] = true
	}

	seenRules := make(map[string]bool, len(s.Rules))
	for _, rl := range s.Rules {
		if !validSlugRe.MatchString(rl.Name) {
			return NewPolicyError("invalid rule name %q (must match %s)", rl.Name, validSlugRe.String())
		}
		if seenRules[rl.Name] {
			return NewPolicyError("duplicate rule name %q", rl.Name)
		}
		seenRules[rl.Name] = true
	}

	for event, hooks := range s.Settings.Hooks {
		if strings.TrimSpace(event) == "" {
			return NewPolicyError("hook event name cannot be empty")
		}
		for i, h := range hooks {
			if strings.TrimSpace(h.Command.Raw) == "" {
				return NewPolicyError("settings.hooks.%s[%d]: command cannot be empty", event, i)
			}
		}
	}

	for target, cfg := range s.TargetsConfig {
		if !knownTargets[target] {
			return NewPolicyError("unknown target %q in targets_config", target)
		}
		switch cfg.OnUnsupported {
		case "", "fail", "warn":
		default:
			return NewPolicyError("targets_config.%s.on_unsupported: invalid value %q (expected fail or warn)", target, cfg.OnUnsupported)
		}
		switch cfg.OnIsolationUnsupported {
		case "", "fail", "warn":
		default:
			return NewPolicyError("targets_config.%s.on_isolation_unsupported: invalid value %q (expected fail or warn)", target, cfg.OnIsolationUnsupported)
		}
		switch cfg.RulesMode {
		case "", "dropin", "agents_md", "copilot":
		default:
			return NewPolicyError("targets_config.%s.rules_mode: invalid value %q (expected dropin, agents_md, or copilot)", target, cfg.RulesMode)
		}
	}

	return nil
}
