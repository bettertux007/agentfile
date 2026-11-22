package windsurf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
)

func TestWindsurfCompileGoldenAndModes(t *testing.T) {
	t.Parallel()
	b := &ir.Bundle{
		Targets: []string{"windsurf"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "github",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://api.example.com/mcp"},
			},
		},
		Skills: []ir.IRAsset{{Name: "k8s", SourcePath: "/store/k8s", IsRemote: true}},
		Agents: []ir.IRAgent{{Name: "sec", Content: []byte("Audit")}},
		Rules: []ir.IRRule{
			{Name: "coding", Description: "Go rules", Globs: []string{"**/*.go", "**/*.tf"}, AlwaysApply: false, Content: []byte("Always run go test.\n")},
			{Name: "general", Description: "General", AlwaysApply: false, Content: []byte("Be concise.\n")},
			{Name: "forced", Description: "Forced", Globs: []string{"**/*.md"}, AlwaysApply: true, Content: []byte("Check spelling.\n")},
		},
	}

	adp := New()
	if adp.Name() != "windsurf" || adp.BinaryName() != "windsurf" {
		t.Fatalf("unexpected windsurf metadata: %q / %q", adp.Name(), adp.BinaryName())
	}
	sc := adp.SchemaContract()
	if sc.MinVersion != "1.0.0" || sc.MaxTested != "1.x" || !sc.BinaryOptional {
		t.Fatalf("unexpected SchemaContract: %+v", sc)
	}
	if adp.RequiredProbes() != nil {
		t.Fatalf("expected nil RequiredProbes")
	}

	res, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}

	var sawMCPWarn, sawAgentWarn bool
	for _, d := range res.Diagnostics {
		if d.Level == adapters.DiagError {
			t.Fatalf("unexpected DiagError: %+v", d)
		}
		if d.Level == adapters.DiagWarning && d.Code == "PROJECT_MCP_UNSUPPORTED" {
			sawMCPWarn = true
		}
		if d.Level == adapters.DiagWarning && d.Code == "UNSUPPORTED_AGENT" {
			sawAgentWarn = true
		}
	}
	if !sawMCPWarn || !sawAgentWarn {
		t.Fatalf("expected PROJECT_MCP_UNSUPPORTED and UNSUPPORTED_AGENT warnings, got %+v", res.Diagnostics)
	}

	byPath := make(map[string]adapters.ArtifactOp)
	for _, f := range res.Files {
		if strings.Contains(f.RelPath, "mcp_config.json") {
			t.Fatalf("windsurf must never emit global or local mcp_config.json: %+v", f)
		}
		byPath[f.RelPath] = f
	}

	// 1. .agents/skills/k8s (Shared: true)
	skOp, ok := byPath[".agents/skills/k8s"]
	if !ok || skOp.Kind != adapters.OpMaterializeTree || !skOp.Shared || !skOp.IsRemote {
		t.Fatalf("expected Shared OpMaterializeTree for .agents/skills/k8s, got %+v", skOp)
	}

	// 2. .windsurf/rules/agentfile-coding.md golden (trigger: glob)
	golden, err := os.ReadFile(filepath.Join("testdata", "rule.golden.md"))
	if err != nil {
		t.Fatal(err)
	}
	codingOp, ok := byPath[".windsurf/rules/agentfile-coding.md"]
	if !ok || codingOp.Kind != adapters.OpWriteDropIn || codingOp.Shared || string(codingOp.Content) != string(golden) {
		t.Fatalf("windsurf rule golden mismatch:\ngot:\n%s\nwant:\n%s", codingOp.Content, golden)
	}

	// 3. always_on when len(Globs)==0 or AlwaysApply==true
	genOp := byPath[".windsurf/rules/agentfile-general.md"]
	if !strings.Contains(string(genOp.Content), "trigger: always_on") || strings.Contains(string(genOp.Content), "globs:") {
		t.Fatalf("expected trigger: always_on without globs when len(Globs)==0:\n%s", genOp.Content)
	}
	forcedOp := byPath[".windsurf/rules/agentfile-forced.md"]
	if !strings.Contains(string(forcedOp.Content), "trigger: always_on") || strings.Contains(string(forcedOp.Content), "globs:") {
		t.Fatalf("expected trigger: always_on without globs when AlwaysApply==true:\n%s", forcedOp.Content)
	}

	// 4. rules_mode: agents_md -> AGENTS.md (Shared: true)
	b.TargetsConfig = map[string]spec.TargetConfigSpec{
		"windsurf": {RulesMode: "agents_md"},
	}
	resAgents, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	var foundAgentsMD bool
	for _, f := range resAgents.Files {
		if f.RelPath == "AGENTS.md" {
			foundAgentsMD = true
			if f.Kind != adapters.OpManagedBlock || !f.Shared {
				t.Fatalf("expected Shared OpManagedBlock for AGENTS.md, got %+v", f)
			}
			wantBlock := adapters.FormatManagedRulesBlock(b.Rules)
			if string(f.Content) != string(wantBlock) {
				t.Fatalf("AGENTS.md content mismatch:\ngot:\n%s\nwant:\n%s", f.Content, wantBlock)
			}
		}
		if strings.HasPrefix(f.RelPath, ".windsurf/rules/") {
			t.Fatalf("did not expect .windsurf/rules/* when rules_mode=agents_md")
		}
	}
	if !foundAgentsMD {
		t.Fatalf("expected AGENTS.md when rules_mode=agents_md")
	}

	// 5. Empty rules omitted in agents_md mode
	b.Rules = nil
	resEmpty, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range resEmpty.Files {
		if f.RelPath == "AGENTS.md" {
			t.Fatalf("expected AGENTS.md omitted when len(bundle.Rules)==0")
		}
	}
}

func TestWindsurfFailClosedAndWarn(t *testing.T) {
	t.Parallel()
	adp := New()
	b := &ir.Bundle{
		Targets:       []string{"windsurf"},
		AllowCommands: []string{"go test ./..."},
		DenyCommands:  []ir.SecurityControl{{Kind: "deny_command", Pattern: "rm -rf *"}},
		Hooks:         []ir.IRHook{{Event: "PreToolUse", Command: spec.EnvString{Raw: "./c.sh"}, Enforced: true}},
		Rules:         []ir.IRRule{{Name: "r", Content: []byte("Rule.\n")}},
		TargetsConfig: map[string]spec.TargetConfigSpec{
			"windsurf": {RulesMode: "copilot"},
		},
	}

	resFail, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	errCodes := make(map[string]int)
	for _, d := range resFail.Diagnostics {
		if d.Level == adapters.DiagError {
			errCodes[d.Code]++
		}
	}
	for _, code := range []string{"UNSUPPORTED_DENY_COMMAND", "UNSUPPORTED_HOOK", "UNSUPPORTED_RULES_MODE"} {
		if errCodes[code] == 0 {
			t.Fatalf("expected DiagError %q, got %+v", code, resFail.Diagnostics)
		}
	}

	b.TargetsConfig["windsurf"] = spec.TargetConfigSpec{
		OnUnsupported: "warn",
		RulesMode:     "copilot",
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

func TestWindsurfPlanLaunchAndGlobalConfigSpecs(t *testing.T) {
	t.Parallel()
	adp := New()
	specs := adp.GlobalConfigSpecs()
	if len(specs) != 2 || specs[0].RelPath != ".codeium/windsurf/mcp_config.json" || specs[1].RelPath != ".codeium/windsurf/memories/global_rules.md" {
		t.Fatalf("unexpected GlobalConfigSpecs: %+v", specs)
	}
	if adp.IsSessionArgs([]string{"--version"}) || adp.IsSessionArgs([]string{"-h"}) || !adp.IsSessionArgs([]string{"."}) {
		t.Fatalf("unexpected IsSessionArgs")
	}

	b := &ir.Bundle{Targets: []string{"windsurf"}}
	lpFail, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if err != nil || lpFail.Mode != adapters.IsolationUnsupported || len(lpFail.Diagnostics) == 0 || lpFail.Diagnostics[0].Level != adapters.DiagError {
		t.Fatalf("expected fail-closed error, got %+v err=%v", lpFail, err)
	}

	b.TargetsConfig = map[string]spec.TargetConfigSpec{"windsurf": {OnIsolationUnsupported: "warn"}}
	lpWarn, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if err != nil || lpWarn.Mode != adapters.IsolationPartialWarn || len(lpWarn.Diagnostics) == 0 || lpWarn.Diagnostics[0].Level != adapters.DiagWarning {
		t.Fatalf("expected IsolationPartialWarn, got %+v err=%v", lpWarn, err)
	}

	lpTrue, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: true}, adapters.LaunchFacts{})
	if err != nil || lpTrue.Mode != adapters.IsolationNone {
		t.Fatalf("expected IsolationNone when LoadGlobal=true, got %+v err=%v", lpTrue, err)
	}
}
