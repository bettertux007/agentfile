package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
)

func TestClaudeCompileAndGolden(t *testing.T) {
	t.Parallel()

	authEnv, _ := spec.ParseEnvString("Bearer ${GITHUB_PAT}")
	argEnv, _ := spec.ParseEnvString("${PKG:-@mcp/fs}")
	cmdEnv, _ := spec.ParseEnvString("./check.sh")

	b := &ir.Bundle{
		Targets: []string{"claude"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "github",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://api.example.com/mcp?a=1&b=2"},
				Headers:   map[string]spec.EnvString{"Authorization": authEnv},
			},
			{
				Name:      "fs",
				Transport: "stdio",
				Command:   spec.EnvString{Raw: "npx"},
				Args:      []spec.EnvString{argEnv},
			},
		},
		Skills:        []ir.IRAsset{{Name: "tf", SourcePath: "/tmp/tf"}},
		Agents:        []ir.IRAgent{{Name: "sec", Description: "Security", Model: "sonnet", Content: []byte("Audit code.\n")}},
		Rules:         []ir.IRRule{{Name: "coding", Globs: []string{"**/*.go"}, Content: []byte("# Coding Standards\nAlways run go test.\n")}},
		AllowCommands: []string{"go test ./..."},
		DenyCommands:  []ir.SecurityControl{{Kind: "deny_command", Pattern: "rm -rf *"}},
		Hooks: []ir.IRHook{
			{Event: "PreToolUse", Matcher: "Bash", Command: cmdEnv, Enforced: true},
		},
	}

	adp := New()
	res, err := adp.Compile(b, adapters.ProbeResults{probeAgentsMD: true})
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != "AGENTS_MD_DOUBLE_LOAD" {
		t.Fatalf("expected AGENTS_MD_DOUBLE_LOAD warning, got %+v", res.Diagnostics)
	}

	byPath := make(map[string]adapters.ArtifactOp)
	for _, f := range res.Files {
		byPath[f.RelPath] = f
	}

	ruleOp, ok := byPath[".claude/rules/agentfile-coding.md"]
	if !ok || ruleOp.Kind != adapters.OpWriteDropIn {
		t.Fatalf("expected drop-in rule op, got %+v", ruleOp)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "rule.golden.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ruleOp.Content) != string(golden) {
		t.Fatalf("rule golden mismatch:\ngot:\n%s\nwant:\n%s", ruleOp.Content, golden)
	}

	mcpOp := byPath[".mcp.json"]
	if mcpOp.Kind != adapters.OpMergeJSONC {
		t.Fatalf("expected OpMergeJSONC for .mcp.json")
	}
	gh := mcpOp.JSONMerge.MapEntries["/mcpServers/github"].(map[string]any)
	hdrs := gh["headers"].(map[string]string)
	if hdrs["Authorization"] != "Bearer ${GITHUB_PAT}" {
		t.Fatalf("env var not preserved: %q", hdrs["Authorization"])
	}

	settingsOp := byPath[".claude/settings.json"]
	if settingsOp.JSONMerge.ArrayEntries["/permissions/deny"][0] != "Bash(rm -rf *)" {
		t.Fatalf("unexpected deny rule: %+v", settingsOp.JSONMerge.ArrayEntries)
	}
}

func TestClaudeCheckPolicyDetectsContradiction(t *testing.T) {
	t.Parallel()
	adp := New()
	b := &ir.Bundle{
		DenyCommands: []ir.SecurityControl{{Kind: "deny_command", Pattern: "rm -rf *"}},
	}
	merged := map[string][]byte{
		".claude/settings.json": []byte(`{
  // user runtime approval
  "permissions": {
    "allow": ["Bash(rm -rf *)"]
  }
}`),
	}
	diags := adp.CheckPolicy(b, merged)
	if len(diags) != 1 || diags[0].Level != adapters.DiagError {
		t.Fatalf("expected policy contradiction error, got %+v", diags)
	}

	// Verify overrides, metadata, and invalid JSONC handling in CheckPolicy.
	if adp.BinaryName() != "claude" || adp.SchemaContract().MinVersion == "" || len(adp.RequiredProbes()) != 2 {
		t.Fatalf("unexpected claude adapter metadata: probes=%d", len(adp.RequiredProbes()))
	}
	b.TargetsConfig = map[string]spec.TargetConfigSpec{
		"claude": {Overrides: map[string]any{"cleanupPeriodDays": 30}},
	}
	resOv, err := adp.Compile(b, adapters.ProbeResults{})
	if err != nil || len(resOv.Files) == 0 {
		t.Fatalf("Compile with overrides failed: %v", err)
	}
	badDiags := adp.CheckPolicy(b, map[string][]byte{".claude/settings.json": []byte("{invalid")})
	if len(badDiags) != 1 || badDiags[0].Code != "INVALID_JSONC" {
		t.Fatalf("expected INVALID_JSONC diagnostic, got %+v", badDiags)
	}
}

func TestClaudePlanLaunchAndGlobalConfigSpecs(t *testing.T) {
	t.Parallel()
	adp := New()
	if len(adp.GlobalConfigSpecs()) != 5 {
		t.Fatalf("expected 5 global config specs, got %d", len(adp.GlobalConfigSpecs()))
	}
	if adp.IsSessionArgs([]string{"--help"}) || adp.IsSessionArgs([]string{"mcp", "list"}) || !adp.IsSessionArgs([]string{"-p", "--help"}) {
		t.Fatalf("unexpected IsSessionArgs classification")
	}

	b := &ir.Bundle{Targets: []string{"claude"}}

	// 1. Native isolation with existing .mcp.json
	lp, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false, UserArgs: []string{"-p", "hi"}}, adapters.LaunchFacts{
		ProjectRoot:   "/proj",
		BinaryVersion: "2.1.278 (Claude Code)",
		Probes:        adapters.ProbeResults{probeProjectMCPMap: true},
		CacheRoot:     "/cache",
	})
	if err != nil || lp.Mode != adapters.IsolationNativeFlags || lp.NeedEmptyMCP {
		t.Fatalf("unexpected PlanLaunch with .mcp.json: %+v err=%v", lp, err)
	}
	if len(lp.PrependedArgs) != 5 || lp.PrependedArgs[4] != filepath.Join("/proj", ".mcp.json") {
		t.Fatalf("unexpected PrependedArgs: %v", lp.PrependedArgs)
	}

	// 2. Without .mcp.json -> NeedEmptyMCP = true
	lpEmpty, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{
		ProjectRoot:   "/proj",
		BinaryVersion: "2.1.0",
		CacheRoot:     "/cache",
	})
	if !lpEmpty.NeedEmptyMCP || lpEmpty.PrependedArgs[4] != filepath.Join("/cache", "empty-mcp.json") {
		t.Fatalf("expected empty-mcp.json fallback, got %+v", lpEmpty)
	}

	// 3. GlobalHits["skills"] fails closed by default, warns under on_isolation_unsupported: warn
	lpSkillsFail, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{
		BinaryVersion: "2.1.0",
		GlobalHits:    map[string]bool{"skills": true},
		CacheRoot:     "/cache",
	})
	if lpSkillsFail.Mode != adapters.IsolationUnsupported || len(lpSkillsFail.Diagnostics) == 0 || lpSkillsFail.Diagnostics[0].Level != adapters.DiagError {
		t.Fatalf("expected fail-closed error when global skills exist, got %+v", lpSkillsFail)
	}

	b.TargetsConfig = map[string]spec.TargetConfigSpec{"claude": {OnIsolationUnsupported: "warn"}}
	lpSkillsWarn, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{
		BinaryVersion: "2.1.0",
		GlobalHits:    map[string]bool{"skills": true},
		CacheRoot:     "/cache",
	})
	if lpSkillsWarn.Mode != adapters.IsolationPartialWarn || len(lpSkillsWarn.PrependedArgs) == 0 {
		t.Fatalf("expected IsolationPartialWarn with flags kept, got %+v", lpSkillsWarn)
	}

	// 4. Old version < 2.1.0 fails closed (or omits flags under warn)
	b.TargetsConfig = nil
	lpOld, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{BinaryVersion: "1.9.0"})
	if lpOld.Mode != adapters.IsolationUnsupported {
		t.Fatalf("expected IsolationUnsupported for 1.9.0, got %+v", lpOld)
	}

	// 5. Forbidden user flag rejected
	lpForb, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false, UserArgs: []string{"--setting-sources=user"}}, adapters.LaunchFacts{BinaryVersion: "2.1.0"})
	if lpForb.Mode != adapters.IsolationUnsupported || len(lpForb.Diagnostics) == 0 {
		t.Fatalf("expected forbidden flag error, got %+v", lpForb)
	}

	// 6. LoadGlobal=true -> IsolationNone; non-session `mcp` -> IsolationNone + warning; old version under warn -> IsolationPartialWarn.
	lpTrue, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: true}, adapters.LaunchFacts{})
	if lpTrue.Mode != adapters.IsolationNone || len(lpTrue.PrependedArgs) != 0 {
		t.Fatalf("expected IsolationNone when LoadGlobal=true, got %+v", lpTrue)
	}
	lpMCP, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false, UserArgs: []string{"--verbose", "mcp", "list"}}, adapters.LaunchFacts{})
	if lpMCP.Mode != adapters.IsolationNone || len(lpMCP.Diagnostics) != 1 || lpMCP.Diagnostics[0].Level != adapters.DiagWarning {
		t.Fatalf("expected NON_SESSION_SUBCOMMAND warning on mcp, got %+v", lpMCP)
	}
	b.TargetsConfig = map[string]spec.TargetConfigSpec{"claude": {OnIsolationUnsupported: "warn"}}
	lpOldWarn, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{BinaryVersion: "bad-ver"})
	if lpOldWarn.Mode != adapters.IsolationPartialWarn || len(lpOldWarn.PrependedArgs) != 0 {
		t.Fatalf("expected IsolationPartialWarn with empty PrependedArgs on bad version, got %+v", lpOldWarn)
	}
}
