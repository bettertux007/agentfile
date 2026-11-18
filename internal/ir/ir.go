package ir

import "github.com/bettertux007/agentfile/internal/spec"

// Bundle is the normalized, target-agnostic intermediate representation
// produced from Agentfile.yaml and resolved dependencies.
type Bundle struct {
	Targets       []string
	MCP           []IRMCPServer
	Skills        []IRAsset
	Agents        []IRAgent
	Rules         []IRRule
	AllowCommands []string
	DenyCommands  []SecurityControl
	Hooks         []IRHook
	TargetsConfig map[string]spec.TargetConfigSpec
}

// IRMCPServer represents a normalized MCP server entry.
type IRMCPServer struct {
	Name      string
	Transport string // stdio | http | sse
	Command   spec.EnvString
	Args      []spec.EnvString
	Env       map[string]spec.EnvString
	URL       spec.EnvString
	Headers   map[string]spec.EnvString
}

// IRAsset represents a directory-backed skill asset.
type IRAsset struct {
	Name       string
	SourcePath string
	IsRemote   bool
	SkillMD    []byte
}

// IRAgent represents a subagent Markdown definition.
type IRAgent struct {
	Name        string
	Description string
	Model       string
	SourcePath  string
	IsRemote    bool
	Content     []byte
}

// IRRule represents an instruction/rule Markdown definition.
type IRRule struct {
	Name        string
	Description string
	Globs       []string
	AlwaysApply bool
	SourcePath  string
	IsRemote    bool
	Content     []byte
}

// SecurityControl represents an enforced security restriction (such as deny_commands).
type SecurityControl struct {
	Kind    string // "deny_command"
	Pattern string
}

// IRHook represents a lifecycle hook, classified as Enforced or advisory.
type IRHook struct {
	Event    string
	Matcher  string
	Command  spec.EnvString
	Enforced bool
}
