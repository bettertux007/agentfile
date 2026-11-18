package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
)

func TestOpenCodeFailClosedAndWarnOverride(t *testing.T) {
	t.Parallel()
	cmdEnv, _ := spec.ParseEnvString("./check.sh")
	defEnv, _ := spec.ParseEnvString("Bearer ${TOKEN:-fallback}")

	b := &ir.Bundle{
		Targets: []string{"opencode"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "api",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://api.example.com"},
				Headers:   map[string]spec.EnvString{"Authorization": defEnv},
			},
		},
		Hooks: []ir.IRHook{
			{Event: "PreToolUse", Matcher: "Bash", Command: cmdEnv, Enforced: true},
		},
	}

	adp := New()
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
	if errCount != 2 {
		t.Fatalf("expected 2 fail-closed DiagErrors (env default + enforced hook), got %+v", resFail.Diagnostics)
	}

	// With on_unsupported: warn, both degrade to warnings and env strips default.
	b.TargetsConfig = map[string]spec.TargetConfigSpec{
		"opencode": {OnUnsupported: "warn"},
	}
	resWarn, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile warn mode failed: %v", err)
	}
	for _, d := range resWarn.Diagnostics {
		if d.Level == adapters.DiagError {
			t.Fatalf("unexpected error in warn mode: %+v", d)
		}
	}
	var ocOp adapters.ArtifactOp
	for _, f := range resWarn.Files {
		if f.RelPath == "opencode.json" {
			ocOp = f
		}
	}
	apiCfg := ocOp.JSONMerge.MapEntries["/mcp/api"].(map[string]any)
	hdrs := apiCfg["headers"].(map[string]string)
	if hdrs["Authorization"] != "Bearer {env:TOKEN}" {
		t.Fatalf("expected {env:TOKEN}, got %q", hdrs["Authorization"])
	}
}

func TestOpenCodeRulesGoldenAndCheckPolicy(t *testing.T) {
	t.Parallel()
	adp := New()
	patEnv, _ := spec.ParseEnvString("Bearer ${GITHUB_PAT}")
	b := &ir.Bundle{
		Targets: []string{"opencode"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "github",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://api.githubcopilot.com/mcp/"},
				Headers:   map[string]spec.EnvString{"Authorization": patEnv},
			},
		},
		Rules: []ir.IRRule{
			{Name: "coding", Description: "Go rules", Globs: []string{"**/*.go"}, Content: []byte("Always run go test.\n")},
		},
		AllowCommands: []string{"go test ./..."},
		DenyCommands:  []ir.SecurityControl{{Kind: "deny_command", Pattern: "rm -rf *"}},
	}

	res, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	byPath := make(map[string]adapters.ArtifactOp)
	for _, f := range res.Files {
		byPath[f.RelPath] = f
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "rules.golden.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(byPath[".opencode/agentfile-rules.md"].Content) != string(golden) {
		t.Fatalf("rules golden mismatch:\ngot:\n%s\nwant:\n%s", byPath[".opencode/agentfile-rules.md"].Content, golden)
	}
	if byPath["opencode.json"].JSONMerge.MapEntries["/permission/bash/rm -rf *"] != "deny" {
		t.Fatalf("expected deny permission in opencode.json")
	}

	// Post-merge check catches contradictory allow.
	diags := adp.CheckPolicy(b, map[string][]byte{
		"opencode.json": []byte(`{"permission": {"bash": {"rm -rf *": "allow"}}}`),
	})
	if len(diags) != 1 || diags[0].Level != adapters.DiagError {
		t.Fatalf("expected POLICY_CONTRADICTION error, got %+v", diags)
	}
}

func TestOpenCodeStdioAgentsHooksAndOverrides(t *testing.T) {
	t.Parallel()
	adp := New()
	if adp.BinaryName() != "opencode" || adp.SchemaContract().MinVersion == "" || adp.RequiredProbes() != nil {
		t.Fatalf("unexpected opencode metadata")
	}
	b := &ir.Bundle{
		Targets: []string{"opencode"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "fs",
				Transport: "stdio",
				Command:   spec.EnvString{Raw: "npx"},
				Args:      []spec.EnvString{{Raw: "-y"}},
				Env:       map[string]spec.EnvString{"LOG": {Raw: "info"}},
			},
		},
		Agents:        []ir.IRAgent{{Name: "sec", Description: "Audit", Model: "sonnet", Content: []byte("Check code")}},
		AllowCommands: []string{"go test && rm -rf /"},
		DenyCommands:  []ir.SecurityControl{{Kind: "deny_command", Pattern: "curl | sh"}},
		Hooks:         []ir.IRHook{{Event: "SessionStart", Command: spec.EnvString{Raw: "echo hi"}, Enforced: false}},
		TargetsConfig: map[string]spec.TargetConfigSpec{
			"opencode": {
				OnUnsupported: "warn",
				Overrides:     map[string]any{"theme": "dark"},
			},
		},
	}
	res, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	if len(res.Diagnostics) < 3 {
		t.Fatalf("expected warnings for untranslatable commands and advisory hook, got %+v", res.Diagnostics)
	}
	badDiags := adp.CheckPolicy(b, map[string][]byte{"opencode.json": []byte("{bad")})
	if len(badDiags) != 1 || badDiags[0].Code != "INVALID_JSONC" {
		t.Fatalf("expected INVALID_JSONC, got %+v", badDiags)
	}
}

func TestOpenCodePlanLaunchAndGlobalConfigSpecs(t *testing.T) {
	t.Parallel()
	adp := New()
	if len(adp.GlobalConfigSpecs()) != 5 {
		t.Fatalf("expected 5 global config specs, got %d", len(adp.GlobalConfigSpecs()))
	}
	if adp.IsSessionArgs([]string{"--version"}) || adp.IsSessionArgs([]string{"auth"}) || !adp.IsSessionArgs([]string{"run", "hi"}) {
		t.Fatalf("unexpected IsSessionArgs")
	}

	b := &ir.Bundle{Targets: []string{"opencode"}}
	// Clean $HOME (no ~/.agents/skills) with >= 1.18.0 succeeds natively via OPENCODE_CONFIG_DIR + OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1 + --standalone!
	lpNative, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{
		BinaryVersion: "opencode v2.3.0",
		CacheRoot:     "/cache",
	})
	if err != nil || lpNative.Mode != adapters.IsolationNativeFlags || len(lpNative.PrependedArgs) != 1 || lpNative.PrependedArgs[0] != "--standalone" {
		t.Fatalf("expected IsolationNativeFlags with --standalone, got %+v err=%v", lpNative, err)
	}
	if lpNative.EnvOverrides["OPENCODE_DISABLE_CLAUDE_CODE_SKILLS"] != "1" || lpNative.EnvOverrides["OPENCODE_CONFIG_DIR"] == "" || len(lpNative.EnsureDirs) != 1 {
		t.Fatalf("expected OPENCODE_CONFIG_DIR + OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1 + EnsureDirs, got %+v", lpNative)
	}

	// Non-empty ~/.agents/skills fails closed unless on_isolation_unsupported: warn.
	lpFail, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{
		BinaryVersion: "opencode v2.3.0",
		CacheRoot:     "/cache",
		GlobalHits:    map[string]bool{"agents_skills": true},
	})
	if err != nil || lpFail.Mode != adapters.IsolationUnsupported || len(lpFail.Diagnostics) == 0 || lpFail.Diagnostics[0].Level != adapters.DiagError {
		t.Fatalf("expected fail-closed error when ~/.agents/skills exists, got %+v err=%v", lpFail, err)
	}

	b.TargetsConfig = map[string]spec.TargetConfigSpec{"opencode": {OnIsolationUnsupported: "warn"}}
	lpWarn, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{
		BinaryVersion: "opencode v2.3.0",
		CacheRoot:     "/cache",
		GlobalHits:    map[string]bool{"agents_skills": true},
	})
	if lpWarn.Mode != adapters.IsolationPartialWarn || len(lpWarn.PrependedArgs) != 1 || lpWarn.PrependedArgs[0] != "--standalone" || lpWarn.EnvOverrides["OPENCODE_CONFIG_DIR"] == "" {
		t.Fatalf("expected partial_warn with --standalone and EnvOverrides, got %+v", lpWarn)
	}

	// Forbidden --standalone rejected; old version < 1.18.0 degrades under warn; LoadGlobal=true returns IsolationNone.
	lpForb, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false, UserArgs: []string{"--standalone"}}, adapters.LaunchFacts{BinaryVersion: "opencode v2.3.0"})
	if lpForb.Mode != adapters.IsolationUnsupported {
		t.Fatalf("expected IsolationUnsupported for forbidden --standalone, got %+v", lpForb)
	}
	lpOld, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{BinaryVersion: "1.10.0"})
	if lpOld.Mode != adapters.IsolationPartialWarn || len(lpOld.PrependedArgs) != 0 {
		t.Fatalf("expected no --standalone on 1.10.0, got %+v", lpOld)
	}
	lpTrue, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: true}, adapters.LaunchFacts{})
	if lpTrue.Mode != adapters.IsolationNone {
		t.Fatalf("expected IsolationNone when LoadGlobal=true, got %+v", lpTrue)
	}
}
