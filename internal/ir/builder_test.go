package ir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
)

func TestBuildClassifiesSecurityAndLoadsContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skill")
	agentFile := filepath.Join(dir, "agent.md")
	ruleFile := filepath.Join(dir, "rule.md")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Skill"), 0o644)
	_ = os.WriteFile(agentFile, []byte("# Reviewer"), 0o644)
	_ = os.WriteFile(ruleFile, []byte("# Rules"), 0o644)

	ps := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude", "opencode"},
		Skills:  []spec.AssetSpec{{Name: "s1", Source: spec.SourceSpec{Raw: "./skill"}}},
		Agents:  []spec.AgentSpec{{Name: "a1", Description: "desc", Source: spec.SourceSpec{Raw: "./agent.md"}}},
		Rules:   []spec.RuleSpec{{Name: "r1", Globs: []string{"*.go"}, Source: spec.SourceSpec{Raw: "./rule.md"}}},
		Settings: spec.SettingsSpec{
			Permissions: spec.PermissionsSpec{
				AllowCommands: []string{"go test ./..."},
				DenyCommands:  []string{"rm -rf *"},
			},
			Hooks: map[string][]spec.HookSpec{
				"SessionStart": {{Command: spec.EnvString{Raw: "echo ok"}}},
				"PreToolUse":   {{Matcher: "Bash", Command: spec.EnvString{Raw: "./check.sh"}}},
			},
		},
	}

	deps := map[string]store.ResolvedAsset{
		"skill:s1": {Kind: "skill", Name: "s1", SourcePath: skillDir},
		"agent:a1": {Kind: "agent", Name: "a1", SourcePath: agentFile},
		"rule:r1":  {Kind: "rule", Name: "r1", SourcePath: ruleFile},
	}

	b, err := Build(ps, deps)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if len(b.DenyCommands) != 1 || b.DenyCommands[0].Pattern != "rm -rf *" {
		t.Fatalf("unexpected DenyCommands: %+v", b.DenyCommands)
	}
	if len(b.Hooks) != 2 || b.Hooks[0].Event != "PreToolUse" || !b.Hooks[0].Enforced {
		t.Fatalf("expected PreToolUse hook to be Enforced=true, got %+v", b.Hooks)
	}
	if b.Hooks[1].Event != "SessionStart" || b.Hooks[1].Enforced {
		t.Fatalf("expected SessionStart hook to be advisory, got %+v", b.Hooks[1])
	}
	if string(b.Agents[0].Content) != "# Reviewer" || string(b.Rules[0].Content) != "# Rules" {
		t.Fatalf("expected agent/rule markdown content to be loaded")
	}
}

func TestBuildValidationErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	subDir := filepath.Join(dir, "d")
	file := filepath.Join(dir, "f.md")
	_ = os.MkdirAll(subDir, 0o755)
	_ = os.WriteFile(file, []byte("x"), 0o644)

	if _, err := Build(nil, nil); err == nil {
		t.Fatalf("expected error for nil spec")
	}
	// File instead of dir for skill
	_, err := Build(&spec.ProjectSpec{
		Version: 1, Targets: []string{"claude"},
		Skills: []spec.AssetSpec{{Name: "s", Source: spec.SourceSpec{Raw: "./f.md"}}},
	}, map[string]store.ResolvedAsset{"skill:s": {SourcePath: file}})
	if err == nil {
		t.Fatalf("expected error when skill source is a file")
	}
	// Dir instead of file for agent
	_, err = Build(&spec.ProjectSpec{
		Version: 1, Targets: []string{"claude"},
		Agents: []spec.AgentSpec{{Name: "a", Source: spec.SourceSpec{Raw: "./d"}}},
	}, map[string]store.ResolvedAsset{"agent:a": {SourcePath: subDir}})
	if err == nil {
		t.Fatalf("expected error when agent source is a dir")
	}

	// Missing resolved dep for skill / agent / rule
	if _, err := Build(&spec.ProjectSpec{Version: 1, Targets: []string{"claude"}, Skills: []spec.AssetSpec{{Name: "s"}}}, map[string]store.ResolvedAsset{}); err == nil {
		t.Fatalf("expected error for missing skill dep")
	}
	if _, err := Build(&spec.ProjectSpec{Version: 1, Targets: []string{"claude"}, Agents: []spec.AgentSpec{{Name: "a"}}}, map[string]store.ResolvedAsset{}); err == nil {
		t.Fatalf("expected error for missing agent dep")
	}
	if _, err := Build(&spec.ProjectSpec{Version: 1, Targets: []string{"claude"}, Rules: []spec.RuleSpec{{Name: "r"}}}, map[string]store.ResolvedAsset{}); err == nil {
		t.Fatalf("expected error for missing rule dep")
	}

	// BuildForRun builds Bundle without reading skill/agent/rule files on disk.
	bFalse := false
	runB := BuildForRun(&spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		MCP:     map[string]spec.MCPServerSpec{"gh": {Transport: "http", URL: spec.EnvString{Raw: "https://x"}}},
		Skills:  []spec.AssetSpec{{Name: "missing-disk-skill", Source: spec.SourceSpec{Raw: "./does-not-exist"}}},
		TargetsConfig: map[string]spec.TargetConfigSpec{
			"claude": {LoadGlobal: &bFalse, OnIsolationUnsupported: "warn"},
		},
	})
	if len(runB.MCP) != 1 || runB.TargetsConfig["claude"].LoadGlobal == nil || *runB.TargetsConfig["claude"].LoadGlobal {
		t.Fatalf("unexpected BuildForRun output: %+v", runB)
	}
}
