package spec

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParseValidManifestAndEnvTokenization(t *testing.T) {
	t.Setenv("GITHUB_PAT", "SHOULD_NOT_EXPAND")

	const input = `
version: 1
targets:
  - claude
  - opencode
mcp:
  github:
    transport: http
    url: "https://api.githubcopilot.com/mcp/?a=1&b=2"
    headers:
      Authorization: "Bearer ${GITHUB_PAT}"
      X-Literal: "cost=$$5"
  fs:
    transport: stdio
    command: npx
    args: ["-y", "${MCP_PKG:-@modelcontextprotocol/server-filesystem}"]
    env:
      LOG_LEVEL: "info"
skills:
  - name: terraform-auditor
    source: ./local/skills/terraform-auditor
  - name: k8s-hardening
    source:
      git: https://github.com/org/agent-skills.git
      ref: v1.0.0
      path: skills/k8s-hardening
agents:
  - name: security-reviewer
    description: "Audits IaC"
    model: sonnet
    source: ./agents/security-reviewer.md
rules:
  - name: coding-standards
    source: ./docs/RULES.md
    globs: ["**/*.go"]
    always_apply: false
settings:
  permissions:
    allow_commands: ["go test ./..."]
    deny_commands: ["rm -rf *"]
  hooks:
    PreToolUse:
      - matcher: "Bash"
        command: "./scripts/check.sh"
        enforce: true
targets_config:
  opencode:
    on_unsupported: warn
`
	spec, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if spec.Version != 1 {
		t.Fatalf("unexpected version: %d", spec.Version)
	}
	auth := spec.MCP["github"].Headers["Authorization"]
	if auth.Raw != "Bearer ${GITHUB_PAT}" {
		t.Fatalf("env var expanded unexpectedly: %q", auth.Raw)
	}
	if len(auth.Segments) != 2 || auth.Segments[0].Literal != "Bearer " || auth.Segments[1].Var == nil || auth.Segments[1].Var.Name != "GITHUB_PAT" {
		t.Fatalf("unexpected env segments: %+v", auth.Segments)
	}
	lit := spec.MCP["github"].Headers["X-Literal"]
	if len(lit.Segments) != 1 || lit.Segments[0].Literal != "cost=$5" {
		t.Fatalf("expected $$ escape to yield literal $, got %+v", lit.Segments)
	}
	arg := spec.MCP["fs"].Args[1]
	if len(arg.Segments) != 1 || arg.Segments[0].Var == nil || !arg.Segments[0].Var.HasDefault || arg.Segments[0].Var.Default != "@modelcontextprotocol/server-filesystem" {
		t.Fatalf("unexpected default env segment: %+v", arg.Segments)
	}
	if len(spec.Skills) != 2 || !spec.Skills[1].Source.IsStructured || spec.Skills[1].Source.Path != "skills/k8s-hardening" {
		t.Fatalf("unexpected structured skill source: %+v", spec.Skills)
	}
	_ = os.Getenv("GITHUB_PAT")
}

func TestParseRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
	}{
		{"empty", ""},
		{"multi_doc", "version: 1\ntargets: [claude]\n---\nversion: 1\n"},
		{"unknown_field", "version: 1\ntargets: [claude]\nextra: true\n"},
		{"wrong_version_string", "version: \"1\"\ntargets: [claude]\n"},
		{"empty_targets", "version: 1\ntargets: []\n"},
		{"unknown_target", "version: 1\ntargets: [emacs]\n"},
		{"path_traversal_skill", "version: 1\ntargets: [claude]\nskills:\n  - name: ../evil\n    source: ./a\n"},
		{"dot_prefix_agent", "version: 1\ntargets: [claude]\nagents:\n  - name: .hidden\n    source: ./a.md\n"},
		{"unclosed_env", "version: 1\ntargets: [claude]\nmcp:\n  a:\n    transport: http\n    url: \"https://x/${FOO\"\n"},
		{"empty_env", "version: 1\ntargets: [claude]\nmcp:\n  a:\n    transport: http\n    url: \"https://x/${}\"\n"},
		{"stdio_with_url", "version: 1\ntargets: [claude]\nmcp:\n  a:\n    transport: stdio\n    command: npx\n    url: \"https://x\"\n"},
		{"invalid_on_unsupported", "version: 1\ntargets: [claude]\ntargets_config:\n  claude:\n    on_unsupported: ignore\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(strings.NewReader(tc.yaml))
			if err == nil {
				t.Fatalf("expected error for case %s", tc.name)
			}
			var pe *PolicyError
			if !errors.As(err, &pe) {
				t.Fatalf("expected PolicyError, got %T: %v", err, err)
			}
		})
	}

	if _, err := ParseFile("/nonexistent/Agentfile.yaml"); err == nil {
		t.Fatalf("expected error for missing file")
	}

	// Valid ParseFile + PolicyError formatting/unwrap + duplicate & hook validation.
	tmp := t.TempDir() + "/Agentfile.yaml"
	_ = os.WriteFile(tmp, []byte("version: 1\ntargets: [claude]\n"), 0o644)
	if ps, err := ParseFile(tmp); err != nil || len(ps.Targets) != 1 {
		t.Fatalf("ParseFile valid failed: %v", err)
	}
	pe := &PolicyError{Message: "wrapped", Err: errors.New("cause")}
	if pe.Error() != "wrapped: cause" || pe.Unwrap() == nil {
		t.Fatalf("unexpected PolicyError formatting: %s", pe.Error())
	}

	moreBad := []string{
		"version: 1\ntargets: [claude, claude]\n",
		"version: 1\ntargets: [claude]\nskills:\n  - name: a\n    source: ./a\n  - name: a\n    source: ./b\n",
		"version: 1\ntargets: [claude]\nagents:\n  - name: a\n    source: ./a\n  - name: a\n    source: ./b\n",
		"version: 1\ntargets: [claude]\nrules:\n  - name: a\n    source: ./a\n  - name: a\n    source: ./b\n",
		"version: 1\ntargets: [claude]\nsettings:\n  hooks:\n    PreToolUse:\n      - command: \"\"\n",
		"version: 1\ntargets: [claude]\ntargets_config:\n  unknown:\n    on_unsupported: warn\n",
		"version: 1\ntargets: [claude]\ntargets_config:\n  claude:\n    rules_mode: legacy\n",
		"version: 1\ntargets: [claude]\ntargets_config:\n  claude:\n    on_isolation_unsupported: ignore\n",
		"version: 1\ntargets: [claude]\nmcp:\n  a:\n    transport: http\n    url: \"https://x/${1BAD}\"\n",
		"version: 1\ntargets: [claude]\nmcp:\n  a:\n    transport: http\n    url: \"https://x\"\n    command: npx\n",
		"version: 1\ntargets: [claude]\nskills:\n  - name: a\n    source:\n      ref: v1\n",
		"version: 1\ntargets: [claude]\nskills:\n  - name: a\n    source:\n      git: https://x\n      bogus: 1\n",
	}
	for _, y := range moreBad {
		if _, err := Parse(strings.NewReader(y)); err == nil {
			t.Fatalf("expected error for:\n%s", y)
		}
	}

	// Valid load_global and on_isolation_unsupported fields.
	lgSpec, err := Parse(strings.NewReader("version: 1\ntargets: [claude]\ntargets_config:\n  claude:\n    load_global: false\n    on_isolation_unsupported: warn\n"))
	if err != nil {
		t.Fatalf("Parse load_global failed: %v", err)
	}
	if lgSpec.TargetsConfig["claude"].LoadGlobal == nil || *lgSpec.TargetsConfig["claude"].LoadGlobal != false || lgSpec.TargetsConfig["claude"].OnIsolationUnsupported != "warn" {
		t.Fatalf("unexpected load_global target config: %+v", lgSpec.TargetsConfig["claude"])
	}
}

func TestParseSprint2TargetsAndRulesModes(t *testing.T) {
	t.Parallel()

	const input = `
version: 1
targets:
  - claude
  - opencode
  - gemini
  - cursor
  - codex
  - copilot
  - windsurf
targets_config:
  claude:
    rules_mode: dropin
  codex:
    rules_mode: agents_md
  copilot:
    rules_mode: copilot
  windsurf:
    rules_mode: dropin
`
	ps, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse failed for 7 targets and Sprint 2 rules_mode values: %v", err)
	}
	if len(ps.Targets) != 7 {
		t.Fatalf("expected 7 targets, got %d", len(ps.Targets))
	}
	if ps.TargetsConfig["codex"].RulesMode != "agents_md" || ps.TargetsConfig["copilot"].RulesMode != "copilot" {
		t.Fatalf("unexpected rules_mode values: %+v", ps.TargetsConfig)
	}

	// Unknown targets and unknown rules_mode must still be rejected.
	badCases := []string{
		"version: 1\ntargets: [aider]\n",
		"version: 1\ntargets: [codex]\ntargets_config:\n  codex:\n    rules_mode: unknown_mode\n",
	}
	for _, y := range badCases {
		if _, err := Parse(strings.NewReader(y)); err == nil {
			t.Fatalf("expected PolicyError for:\n%s", y)
		}
	}
}

func TestParseRawAndParseBaseWithExtends(t *testing.T) {
	t.Parallel()

	// Child manifest with extends and omitted targets succeeds in ParseRaw, fails standalone Parse.
	rawYAML := "version: 1\nextends: ./base/Agentfile.yaml\n"
	if _, err := Parse(strings.NewReader(rawYAML)); err == nil {
		t.Fatalf("expected standalone Parse to reject omitted targets even when extends is present")
	}
	ps, err := ParseRaw(strings.NewReader(rawYAML))
	if err != nil {
		t.Fatalf("expected ParseRaw to accept omitted targets when extends is present: %v", err)
	}
	if len(ps.Extends) != 1 || ps.Extends[0].Raw != "./base/Agentfile.yaml" {
		t.Fatalf("unexpected extends: %+v", ps.Extends)
	}

	// Base manifest without targets or extends succeeds in ParseBase.
	baseYAML := "version: 1\nskills:\n  - name: sec\n    source: ./skills/sec\n"
	if _, err := ParseRaw(strings.NewReader(baseYAML)); err == nil {
		t.Fatalf("expected ParseRaw to reject omitted targets when extends is empty")
	}
	if _, err := ParseBase(strings.NewReader(baseYAML)); err != nil {
		t.Fatalf("expected ParseBase to accept base manifest without targets: %v", err)
	}

	// Empty extends list rejected.
	if _, err := ParseRaw(strings.NewReader("version: 1\nextends: []\ntargets: [claude]\n")); err == nil {
		t.Fatalf("expected error on empty extends list")
	}
}
