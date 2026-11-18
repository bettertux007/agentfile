package cursor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
)

func TestCursorCompileGoldenAndAdvisoryAgents(t *testing.T) {
	t.Parallel()
	authEnv, _ := spec.ParseEnvString("Bearer ${GITHUB_PAT}")
	b := &ir.Bundle{
		Targets: []string{"cursor"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "github",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://api.example.com/mcp"},
				Headers:   map[string]spec.EnvString{"Authorization": authEnv},
			},
		},
		Skills: []ir.IRAsset{{Name: "tf", SourcePath: "./skills/tf", SkillMD: []byte("# Terraform Auditor\n")}},
		Agents: []ir.IRAgent{{Name: "sec", Content: []byte("Audit")}},
		Rules:  []ir.IRRule{{Name: "coding", Description: "Go rules", Globs: []string{"**/*.go", "**/*.tf"}, AlwaysApply: false, Content: []byte("Always run go test.\n")}},
	}

	adp := New()
	if !adp.SchemaContract().BinaryOptional {
		t.Fatalf("expected BinaryOptional=true for Cursor GUI tool")
	}
	res, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	// Advisory agent must emit Warning, not Error.
	for _, d := range res.Diagnostics {
		if d.Level == adapters.DiagError {
			t.Fatalf("unexpected DiagError for advisory agent: %+v", d)
		}
	}

	byPath := make(map[string]adapters.ArtifactOp)
	for _, f := range res.Files {
		byPath[f.RelPath] = f
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "rule.golden.mdc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(byPath[".cursor/rules/agentfile-coding.mdc"].Content) != string(golden) {
		t.Fatalf("mdc golden mismatch:\ngot:\n%s\nwant:\n%s", byPath[".cursor/rules/agentfile-coding.mdc"].Content, golden)
	}
	if _, ok := byPath[".cursor/rules/agentfile-skill-tf.mdc"]; !ok {
		t.Fatalf("expected cursor skill rule .cursor/rules/agentfile-skill-tf.mdc")
	}
	gh := byPath[".cursor/mcp.json"].JSONMerge.MapEntries["/mcpServers/github"].(map[string]any)
	if gh["headers"].(map[string]string)["Authorization"] != "Bearer ${env:GITHUB_PAT}" {
		t.Fatalf("expected ${env:GITHUB_PAT} in cursor mcp.json")
	}
}

func TestCursorStdioFailClosedAndWarnOverrides(t *testing.T) {
	t.Parallel()
	adp := New()
	if adp.BinaryName() != "cursor" || adp.RequiredProbes() != nil {
		t.Fatalf("unexpected cursor metadata")
	}
	defEnv, _ := spec.ParseEnvString("${LOG_LEVEL:-debug}")
	b := &ir.Bundle{
		Targets: []string{"cursor"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "fs",
				Transport: "stdio",
				Command:   spec.EnvString{Raw: "npx"},
				Args:      []spec.EnvString{{Raw: "-y"}},
				Env:       map[string]spec.EnvString{"LOG": defEnv},
			},
		},
		AllowCommands: []string{"go test ./..."},
		DenyCommands:  []ir.SecurityControl{{Kind: "deny_command", Pattern: "rm -rf *"}},
		Hooks:         []ir.IRHook{{Event: "PreToolUse", Command: spec.EnvString{Raw: "./c.sh"}, Enforced: true}},
	}
	resFail, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	errCount := 0
	for _, d := range resFail.Diagnostics {
		if d.Level == adapters.DiagError {
			errCount++
		}
	}
	if errCount != 3 {
		t.Fatalf("expected 3 DiagErrors (env default, deny_commands, enforced hook), got %+v", resFail.Diagnostics)
	}

	b.TargetsConfig = map[string]spec.TargetConfigSpec{
		"cursor": {OnUnsupported: "warn", Overrides: map[string]any{"custom": true}},
	}
	resWarn, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile warn failed: %v", err)
	}
	for _, d := range resWarn.Diagnostics {
		if d.Level == adapters.DiagError {
			t.Fatalf("unexpected DiagError in warn mode: %+v", d)
		}
	}
	if diags := adp.CheckPolicy(b, nil); len(diags) != 0 {
		t.Fatalf("expected nil CheckPolicy diagnostics, got %+v", diags)
	}
}

func TestCursorPlanLaunchAndGlobalConfigSpecs(t *testing.T) {
	t.Parallel()
	adp := New()
	if len(adp.GlobalConfigSpecs()) != 2 {
		t.Fatalf("expected 2 global config specs, got %d", len(adp.GlobalConfigSpecs()))
	}
	if adp.IsSessionArgs([]string{"--version"}) || !adp.IsSessionArgs([]string{"."}) {
		t.Fatalf("unexpected IsSessionArgs")
	}

	b := &ir.Bundle{Targets: []string{"cursor"}}
	lpFail, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if lpFail.Mode != adapters.IsolationUnsupported || lpFail.Diagnostics[0].Level != adapters.DiagError {
		t.Fatalf("expected fail-closed error, got %+v", lpFail)
	}

	b.TargetsConfig = map[string]spec.TargetConfigSpec{"cursor": {OnIsolationUnsupported: "warn"}}
	lpWarn, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if lpWarn.Mode != adapters.IsolationPartialWarn || len(lpWarn.Diagnostics) == 0 || lpWarn.Diagnostics[0].Level != adapters.DiagWarning {
		t.Fatalf("expected IsolationPartialWarn, got %+v", lpWarn)
	}
	lpTrue, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: true}, adapters.LaunchFacts{})
	if lpTrue.Mode != adapters.IsolationNone {
		t.Fatalf("expected IsolationNone when LoadGlobal=true, got %+v", lpTrue)
	}
}
