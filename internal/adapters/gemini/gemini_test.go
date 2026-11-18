package gemini

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
)

func TestGeminiCompileFailClosedAndWarnGolden(t *testing.T) {
	t.Parallel()
	authEnv, _ := spec.ParseEnvString("Bearer ${GEMINI_KEY}")
	b := &ir.Bundle{
		Targets: []string{"gemini"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "api",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://api.example.com/mcp"},
				Headers:   map[string]spec.EnvString{"Authorization": authEnv},
			},
		},
		Skills:       []ir.IRAsset{{Name: "k8s", SourcePath: "/tmp/k8s"}},
		Agents:       []ir.IRAgent{{Name: "sec", Content: []byte("Audit")}},
		Rules:        []ir.IRRule{{Name: "coding", Description: "Go rules", Globs: []string{"**/*.go"}, Content: []byte("Always run go test.\n")}},
		DenyCommands: []ir.SecurityControl{{Kind: "deny_command", Pattern: "rm -rf *"}},
	}

	adp := New()
	resFail, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	hasErr := false
	for _, d := range resFail.Diagnostics {
		if d.Level == adapters.DiagError {
			hasErr = true
		}
	}
	if !hasErr {
		t.Fatalf("expected fail-closed DiagError on deny_commands, got %+v", resFail.Diagnostics)
	}

	// Enable on_unsupported: warn -> degrades to warnings and emits GEMINI.md matching golden.
	b.TargetsConfig = map[string]spec.TargetConfigSpec{
		"gemini": {OnUnsupported: "warn"},
	}
	resWarn, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile warn mode failed: %v", err)
	}
	byPath := make(map[string]adapters.ArtifactOp)
	for _, f := range resWarn.Files {
		byPath[f.RelPath] = f
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "gemini.golden.md"))
	if err != nil {
		t.Fatal(err)
	}
	if byPath["GEMINI.md"].Kind != adapters.OpManagedBlock || string(byPath["GEMINI.md"].Content) != string(golden) {
		t.Fatalf("GEMINI.md golden mismatch:\ngot:\n%s\nwant:\n%s", byPath["GEMINI.md"].Content, golden)
	}
}

func TestGeminiStdioEnvDefaultAndOverrides(t *testing.T) {
	t.Parallel()
	adp := New()
	if adp.BinaryName() != "gemini" || adp.SchemaContract().MinVersion == "" || adp.RequiredProbes() != nil {
		t.Fatalf("unexpected gemini metadata")
	}
	defEnv, _ := spec.ParseEnvString("${LOG_LEVEL:-info}")
	b := &ir.Bundle{
		Targets: []string{"gemini"},
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
		Hooks:         []ir.IRHook{{Event: "PreToolUse", Command: spec.EnvString{Raw: "./c.sh"}, Enforced: true}},
		TargetsConfig: map[string]spec.TargetConfigSpec{
			"gemini": {OnUnsupported: "warn", Overrides: map[string]any{"model": "gemini-2.5-pro"}},
		},
	}
	res, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	if len(res.Diagnostics) < 3 {
		t.Fatalf("expected warnings for env default, allow_commands, and hook, got %+v", res.Diagnostics)
	}
	if diags := adp.CheckPolicy(b, nil); len(diags) != 0 {
		t.Fatalf("expected nil CheckPolicy diagnostics, got %+v", diags)
	}
}

func TestGeminiPlanLaunchAndGlobalConfigSpecs(t *testing.T) {
	t.Parallel()
	adp := New()
	if len(adp.GlobalConfigSpecs()) != 3 {
		t.Fatalf("expected 3 global config specs, got %d", len(adp.GlobalConfigSpecs()))
	}
	if adp.IsSessionArgs([]string{"mcp"}) || adp.IsSessionArgs([]string{"--help"}) || !adp.IsSessionArgs([]string{"-p", "hi"}) {
		t.Fatalf("unexpected IsSessionArgs")
	}

	b := &ir.Bundle{
		Targets: []string{"gemini"},
		MCP:     []ir.IRMCPServer{{Name: "zeta"}, {Name: "alpha"}},
	}
	lpFail, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if lpFail.Mode != adapters.IsolationUnsupported || lpFail.Diagnostics[0].Level != adapters.DiagError {
		t.Fatalf("expected fail-closed error, got %+v", lpFail)
	}

	b.TargetsConfig = map[string]spec.TargetConfigSpec{"gemini": {OnIsolationUnsupported: "warn"}}
	lpWarn, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if lpWarn.Mode != adapters.IsolationPartialWarn || len(lpWarn.PrependedArgs) != 2 || lpWarn.PrependedArgs[1] != "alpha,zeta" {
		t.Fatalf("expected --allowed-mcp-server-names alpha,zeta, got %+v", lpWarn)
	}

	// Empty MCPs omits --allowed-mcp-server-names.
	b.MCP = nil
	lpEmpty, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if lpEmpty.Mode != adapters.IsolationPartialWarn || len(lpEmpty.PrependedArgs) != 0 {
		t.Fatalf("expected no --allowed-mcp-server-names when MCPs empty, got %+v", lpEmpty)
	}

	// Forbidden --allowed-mcp-server-names rejected; LoadGlobal=true -> IsolationNone.
	b.MCP = []ir.IRMCPServer{{Name: "a"}}
	lpForb, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false, UserArgs: []string{"--allowed-mcp-server-names=x"}}, adapters.LaunchFacts{})
	if lpForb.Mode != adapters.IsolationUnsupported {
		t.Fatalf("expected IsolationUnsupported for forbidden flag, got %+v", lpForb)
	}
	lpTrue, _ := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: true}, adapters.LaunchFacts{})
	if lpTrue.Mode != adapters.IsolationNone {
		t.Fatalf("expected IsolationNone when LoadGlobal=true, got %+v", lpTrue)
	}
}
