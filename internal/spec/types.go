package spec

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// PolicyError represents a validation, security policy, or lockfile mismatch
// error that maps to CLI exit code 2.
type PolicyError struct {
	Message string
	Err     error
}

func (e *PolicyError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *PolicyError) Unwrap() error {
	return e.Err
}

// NewPolicyError creates a PolicyError with the given message.
func NewPolicyError(format string, args ...any) error {
	return &PolicyError{Message: fmt.Sprintf(format, args...)}
}

// ProjectSpec is the parsed representation of Agentfile.yaml (version 1).
type ProjectSpec struct {
	Version       int                         `yaml:"version"`
	Extends       ExtendsList                 `yaml:"extends,omitempty"`
	Targets       []string                    `yaml:"targets"`
	MCP           map[string]MCPServerSpec    `yaml:"mcp,omitempty"`
	Skills        []AssetSpec                 `yaml:"skills,omitempty"`
	Agents        []AgentSpec                 `yaml:"agents,omitempty"`
	Rules         []RuleSpec                  `yaml:"rules,omitempty"`
	Settings      SettingsSpec                `yaml:"settings,omitempty"`
	TargetsConfig map[string]TargetConfigSpec `yaml:"targets_config,omitempty"`
}

// ExtendsList accepts either a single SourceSpec (string or {git, ref, path}) or a non-empty list of SourceSpecs.
type ExtendsList []SourceSpec

// UnmarshalYAML decodes a scalar/mapping source or a sequence of sources into ExtendsList.
func (el *ExtendsList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		if len(value.Content) == 0 {
			return NewPolicyError("extends cannot be an empty list")
		}
		out := make([]SourceSpec, 0, len(value.Content))
		for _, child := range value.Content {
			var src SourceSpec
			if err := src.UnmarshalYAML(child); err != nil {
				return err
			}
			out = append(out, src)
		}
		*el = out
		return nil
	case yaml.ScalarNode, yaml.MappingNode:
		var src SourceSpec
		if err := src.UnmarshalYAML(value); err != nil {
			return err
		}
		*el = ExtendsList{src}
		return nil
	default:
		return NewPolicyError("extends must be a source or list of sources")
	}
}

// MCPServerSpec defines an MCP server entry.
type MCPServerSpec struct {
	Transport string               `yaml:"transport"`
	Command   EnvString            `yaml:"command,omitempty"`
	Args      []EnvString          `yaml:"args,omitempty"`
	Env       map[string]EnvString `yaml:"env,omitempty"`
	URL       EnvString            `yaml:"url,omitempty"`
	Headers   map[string]EnvString `yaml:"headers,omitempty"`
}

// AssetSpec defines a named dependency-backed skill.
type AssetSpec struct {
	Name   string     `yaml:"name"`
	Source SourceSpec `yaml:"source"`
}

// AgentSpec defines a subagent Markdown artifact.
type AgentSpec struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description,omitempty"`
	Model       string     `yaml:"model,omitempty"`
	Source      SourceSpec `yaml:"source"`
}

// RuleSpec defines an instruction / rule Markdown artifact.
type RuleSpec struct {
	Name        string     `yaml:"name"`
	Source      SourceSpec `yaml:"source"`
	Description string     `yaml:"description,omitempty"`
	Globs       []string   `yaml:"globs,omitempty"`
	AlwaysApply bool       `yaml:"always_apply,omitempty"`
}

// SettingsSpec holds permissions and lifecycle hooks.
type SettingsSpec struct {
	Permissions PermissionsSpec       `yaml:"permissions,omitempty"`
	Hooks       map[string][]HookSpec `yaml:"hooks,omitempty"`
}

// PermissionsSpec defines command allow/deny patterns.
type PermissionsSpec struct {
	AllowCommands []string `yaml:"allow_commands,omitempty"`
	DenyCommands  []string `yaml:"deny_commands,omitempty"`
}

// HookSpec defines a lifecycle hook command.
type HookSpec struct {
	Matcher string    `yaml:"matcher,omitempty"`
	Command EnvString `yaml:"command"`
	Enforce bool      `yaml:"enforce,omitempty"`
}

// TargetConfigSpec holds per-target overrides and downgrade behavior.
type TargetConfigSpec struct {
	OnUnsupported          string         `yaml:"on_unsupported,omitempty"`
	OnIsolationUnsupported string         `yaml:"on_isolation_unsupported,omitempty"`
	LoadGlobal             *bool          `yaml:"load_global,omitempty"`
	RulesMode              string         `yaml:"rules_mode,omitempty"`
	Overrides              map[string]any `yaml:"overrides,omitempty"`
}

// SourceSpec represents either a string source or a structured Git reference.
type SourceSpec struct {
	Raw          string
	IsStructured bool
	Git          string `yaml:"git"`
	Ref          string `yaml:"ref"`
	Path         string `yaml:"path"`
}

// UnmarshalYAML accepts either a scalar string or a {git, ref, path} mapping.
func (s *SourceSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var raw string
		if err := value.Decode(&raw); err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return NewPolicyError("source cannot be empty")
		}
		s.Raw = raw
		s.IsStructured = false
		return nil
	case yaml.MappingNode:
		type structured struct {
			Git  string `yaml:"git"`
			Ref  string `yaml:"ref"`
			Path string `yaml:"path"`
		}
		var st structured
		dec := yaml.NewDecoder(strings.NewReader(""))
		_ = dec
		// Decode strictly via mapping node check for unknown fields.
		for i := 0; i+1 < len(value.Content); i += 2 {
			k := value.Content[i].Value
			switch k {
			case "git", "ref", "path":
			default:
				return NewPolicyError("unknown field %q in structured source", k)
			}
		}
		if err := value.Decode(&st); err != nil {
			return err
		}
		st.Git = strings.TrimSpace(st.Git)
		st.Ref = strings.TrimSpace(st.Ref)
		st.Path = strings.TrimSpace(st.Path)
		if st.Git == "" {
			return NewPolicyError("structured source requires non-empty 'git' field")
		}
		s.IsStructured = true
		s.Git = st.Git
		s.Ref = st.Ref
		s.Path = st.Path
		return nil
	default:
		return NewPolicyError("source must be a string or {git, ref, path} mapping")
	}
}

// EnvRef represents a parsed ${VAR} or ${VAR:-default} placeholder.
type EnvRef struct {
	Name       string
	Default    string
	HasDefault bool
}

// EnvSegment is either literal text or an EnvRef placeholder.
type EnvSegment struct {
	Literal string
	Var     *EnvRef
}

// EnvString preserves raw text alongside tokenized environment placeholders.
type EnvString struct {
	Raw      string
	Segments []EnvSegment
}

// UnmarshalYAML decodes a scalar string and parses ${VAR} placeholders without expanding env vars.
func (e *EnvString) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return NewPolicyError("expected string value for env-interpolated field")
	}
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	parsed, err := ParseEnvString(raw)
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

// ParseEnvString tokenizes ${VAR}, ${VAR:-default}, and $$ escapes without calling os.ExpandEnv.
func ParseEnvString(raw string) (EnvString, error) {
	var segments []EnvSegment
	var lit strings.Builder

	flushLit := func() {
		if lit.Len() > 0 {
			segments = append(segments, EnvSegment{Literal: lit.String()})
			lit.Reset()
		}
	}

	for i := 0; i < len(raw); {
		if raw[i] == '$' {
			if i+1 < len(raw) && raw[i+1] == '$' {
				lit.WriteByte('$')
				i += 2
				continue
			}
			if i+1 < len(raw) && raw[i+1] == '{' {
				flushLit()
				end := strings.IndexByte(raw[i+2:], '}')
				if end < 0 {
					return EnvString{}, NewPolicyError("unclosed environment placeholder in %q", raw)
				}
				inner := raw[i+2 : i+2+end]
				if inner == "" {
					return EnvString{}, NewPolicyError("empty environment placeholder ${} in %q", raw)
				}
				var ref EnvRef
				if idx := strings.Index(inner, ":-"); idx >= 0 {
					ref.Name = inner[:idx]
					ref.Default = inner[idx+2:]
					ref.HasDefault = true
				} else {
					ref.Name = inner
				}
				if !isValidEnvVarName(ref.Name) {
					return EnvString{}, NewPolicyError("invalid environment variable name %q in %q", ref.Name, raw)
				}
				segments = append(segments, EnvSegment{Var: &ref})
				i = i + 2 + end + 1
				continue
			}
		}
		lit.WriteByte(raw[i])
		i++
	}
	flushLit()
	return EnvString{Raw: raw, Segments: segments}, nil
}

func isValidEnvVarName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlpha := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

// Lockfile represents Agentfile.lock (version 1).
type Lockfile struct {
	Version            int                  `yaml:"version"`
	Extends            map[string]LockEntry `yaml:"extends,omitempty"`
	Dependencies       map[string]LockEntry `yaml:"dependencies"`
	FromInitialCompose bool                 `yaml:"-"`
	ExtendsReconciled  bool                 `yaml:"-"`
}

// HasRemoteEntries reports whether the lockfile contains any pinned dependencies or remote extends bases.
func (lf *Lockfile) HasRemoteEntries() bool {
	return lf != nil && (len(lf.Dependencies) > 0 || len(lf.Extends) > 0)
}

// LockEntry records a pinned remote dependency.
type LockEntry struct {
	Source      string `yaml:"source"`
	Ref         string `yaml:"ref,omitempty"`
	CommitSHA   string `yaml:"commit_sha"`
	ContentHash string `yaml:"content_hash"`
}
