package copilot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
)

func TestCopilotCompileGoldenAndModes(t *testing.T) {
	t.Parallel()
	authEnv, _ := spec.ParseEnvString("Bearer ${GITHUB_PAT}")
	b := &ir.Bundle{
		Targets: []string{"copilot"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "github",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://api.example.com/mcp"},
				Headers:   map[string]spec.EnvString{"Authorization": authEnv},
			},
			{
				Name:      "filesystem",
				Transport: "stdio",
				Command:   spec.EnvString{Raw: "npx"},
				Args:      []spec.EnvString{{Raw: "-y"}, {Raw: "@mcp/fs"}},
			},
		},
		Skills: []ir.IRAsset{{Name: "k8s", SourcePath: "/store/k8s", IsRemote: true}},
		Agents: []ir.IRAgent{{Name: "sec", Content: []byte("Audit")}},
		Rules:  []ir.IRRule{{Name: "coding", Description: "Go rules", Globs: []string{"**/*.go"}, Content: []byte("Always run go test.\n")}},
	}

	adp := New()
	if adp.Name() != "copilot" || adp.BinaryName() != "code" {
		t.Fatalf("unexpected copilot metadata: %q / %q", adp.Name(), adp.BinaryName())
	}
	sc := adp.SchemaContract()
	if sc.MinVersion != "1.99.0" || sc.MaxTested != "1.x" || !sc.BinaryOptional {
		t.Fatalf("unexpected SchemaContract: %+v", sc)
	}
	if adp.RequiredProbes() != nil {
		t.Fatalf("expected nil RequiredProbes")
	}

	res, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Level != adapters.DiagWarning || res.Diagnostics[0].Code != "UNSUPPORTED_AGENT" {
		t.Fatalf("expected only UNSUPPORTED_AGENT warning, got %+v", res.Diagnostics)
	}

	byPath := make(map[string]adapters.ArtifactOp)
	for _, f := range res.Files {
		byPath[f.RelPath] = f
	}

	// 1. .vscode/mcp.json under /servers/
	mcpOp, ok := byPath[".vscode/mcp.json"]
	if !ok || mcpOp.Kind != adapters.OpMergeJSONC || mcpOp.Shared || mcpOp.JSONMerge == nil {
		t.Fatalf("expected non-shared OpMergeJSONC for .vscode/mcp.json, got %+v", mcpOp)
	}
	gh := mcpOp.JSONMerge.MapEntries["/servers/github"].(map[string]any)
	if gh["type"] != "http" || gh["headers"].(map[string]string)["Authorization"] != "Bearer ${env:GITHUB_PAT}" {
		t.Fatalf("unexpected github server in .vscode/mcp.json: %+v", gh)
	}
	fs := mcpOp.JSONMerge.MapEntries["/servers/filesystem"].(map[string]any)
	if fs["type"] != "stdio" || fs["command"] != "npx" {
		t.Fatalf("unexpected filesystem server in .vscode/mcp.json: %+v", fs)
	}

	// 2. .agents/skills/k8s (Shared: true)
	skOp, ok := byPath[".agents/skills/k8s"]
	if !ok || skOp.Kind != adapters.OpMaterializeTree || !skOp.Shared || !skOp.IsRemote {
		t.Fatalf("expected Shared OpMaterializeTree for .agents/skills/k8s, got %+v", skOp)
	}

	// 3. Default rules_mode -> .github/copilot-instructions.md (Shared: false)
	golden, err := os.ReadFile(filepath.Join("testdata", "copilot.golden.md"))
	if err != nil {
		t.Fatal(err)
	}
	ruleOp, ok := byPath[".github/copilot-instructions.md"]
	if !ok || ruleOp.Kind != adapters.OpManagedBlock || ruleOp.Shared || string(ruleOp.Content) != string(golden) {
		t.Fatalf("unexpected .github/copilot-instructions.md: %+v", ruleOp)
	}

	// 4. Explicit rules_mode: copilot -> .github/copilot-instructions.md (Shared: false)
	b.TargetsConfig = map[string]spec.TargetConfigSpec{
		"copilot": {RulesMode: "copilot"},
	}
	resCopilot, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	foundCopilotInstr := false
	for _, f := range resCopilot.Files {
		if f.RelPath == ".github/copilot-instructions.md" && !f.Shared {
			foundCopilotInstr = true
		}
	}
	if !foundCopilotInstr {
		t.Fatalf("expected .github/copilot-instructions.md when rules_mode=copilot")
	}

	// 5. rules_mode: agents_md -> AGENTS.md (Shared: true)
	b.TargetsConfig = map[string]spec.TargetConfigSpec{
		"copilot": {RulesMode: "agents_md"},
	}
	resAgents, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	var foundAgentsMD bool
	for _, f := range resAgents.Files {
		if f.RelPath == "AGENTS.md" {
			foundAgentsMD = true
			if f.Kind != adapters.OpManagedBlock || !f.Shared || string(f.Content) != string(golden) {
				t.Fatalf("unexpected AGENTS.md op: %+v", f)
			}
		}
		if f.RelPath == ".github/copilot-instructions.md" {
			t.Fatalf("did not expect .github/copilot-instructions.md when rules_mode=agents_md")
		}
	}
	if !foundAgentsMD {
		t.Fatalf("expected AGENTS.md when rules_mode=agents_md")
	}

	// 6. Empty rules omitted in both modes
	b.Rules = nil
	resEmpty, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range resEmpty.Files {
		if f.RelPath == "AGENTS.md" || f.RelPath == ".github/copilot-instructions.md" {
			t.Fatalf("expected rules file omitted when len(bundle.Rules)==0, got %+v", f)
		}
	}
}

func TestCopilotFailClosedWarnAndOverrides(t *testing.T) {
	t.Parallel()
	adp := New()
	defEnv, _ := spec.ParseEnvString("${LOG_LEVEL:-debug}")
	b := &ir.Bundle{
		Targets: []string{"copilot"},
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
		Rules:         []ir.IRRule{{Name: "r", Content: []byte("Rule.\n")}},
		TargetsConfig: map[string]spec.TargetConfigSpec{
			"copilot": {RulesMode: "dropin"},
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
	for _, code := range []string{"UNSUPPORTED_ENV_DEFAULT", "UNSUPPORTED_DENY_COMMAND", "UNSUPPORTED_HOOK", "UNSUPPORTED_RULES_MODE"} {
		if errCodes[code] == 0 {
			t.Fatalf("expected DiagError %q, got %+v", code, resFail.Diagnostics)
		}
	}

	b.TargetsConfig["copilot"] = spec.TargetConfigSpec{
		OnUnsupported: "warn",
		RulesMode:     "dropin",
		Overrides:     map[string]any{"inputs": []any{}},
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
	var mcpOp adapters.ArtifactOp
	for _, f := range resWarn.Files {
		if f.RelPath == ".vscode/mcp.json" {
			mcpOp = f
		}
	}
	if _, ok := mcpOp.JSONMerge.MapEntries["/inputs"]; !ok {
		t.Fatalf("expected overrides /inputs in .vscode/mcp.json: %+v", mcpOp.JSONMerge.MapEntries)
	}
	if diags := adp.CheckPolicy(b, nil); len(diags) != 0 {
		t.Fatalf("expected nil CheckPolicy diagnostics, got %+v", diags)
	}
}

func TestCopilotPlanLaunchAndGlobalConfigSpecs(t *testing.T) {
	t.Parallel()
	adp := New()
	specs := adp.GlobalConfigSpecs()
	if len(specs) != 2 || specs[0].RelPath != ".copilot/skills" {
		t.Fatalf("unexpected GlobalConfigSpecs: %+v", specs)
	}
	if adp.IsSessionArgs([]string{"--version"}) || adp.IsSessionArgs([]string{"-h"}) || !adp.IsSessionArgs([]string{"."}) {
		t.Fatalf("unexpected IsSessionArgs")
	}

	b := &ir.Bundle{Targets: []string{"copilot"}}
	lpFail, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if err != nil || lpFail.Mode != adapters.IsolationUnsupported || len(lpFail.Diagnostics) == 0 || lpFail.Diagnostics[0].Level != adapters.DiagError {
		t.Fatalf("expected fail-closed error, got %+v err=%v", lpFail, err)
	}

	b.TargetsConfig = map[string]spec.TargetConfigSpec{"copilot": {OnIsolationUnsupported: "warn"}}
	lpWarn, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if err != nil || lpWarn.Mode != adapters.IsolationPartialWarn || len(lpWarn.Diagnostics) == 0 || lpWarn.Diagnostics[0].Level != adapters.DiagWarning {
		t.Fatalf("expected IsolationPartialWarn, got %+v err=%v", lpWarn, err)
	}

	lpTrue, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: true}, adapters.LaunchFacts{})
	if err != nil || lpTrue.Mode != adapters.IsolationNone {
		t.Fatalf("expected IsolationNone when LoadGlobal=true, got %+v err=%v", lpTrue, err)
	}
}

func TestCopilotDeterministicDiagnosticsOrder(t *testing.T) {
	t.Parallel()
	adp := New()
	alphaDef, _ := spec.ParseEnvString("${ALPHA_VAR:-a}")
	zuluDef, _ := spec.ParseEnvString("${ZULU_VAR:-z}")
	hdrAlpha, _ := spec.ParseEnvString("${HDR_A:-ha}")
	hdrZulu, _ := spec.ParseEnvString("${HDR_Z:-hz}")

	b := &ir.Bundle{
		Targets: []string{"copilot"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "s1",
				Transport: "stdio",
				Command:   spec.EnvString{Raw: "npx"},
				Env: map[string]spec.EnvString{
					"ZULU":  zuluDef,
					"ALPHA": alphaDef,
				},
			},
			{
				Name:      "s2",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://example.com"},
				Headers: map[string]spec.EnvString{
					"X-Zulu":  hdrZulu,
					"X-Alpha": hdrAlpha,
				},
			},
		},
	}

	var baseline []string
	for iter := 0; iter < 15; iter++ {
		res, err := adp.Compile(b, nil)
		if err != nil {
			t.Fatalf("Compile failed: %v", err)
		}
		msgs := make([]string, 0, len(res.Diagnostics))
		for _, d := range res.Diagnostics {
			msgs = append(msgs, d.Message)
		}
		if len(msgs) != 4 {
			t.Fatalf("expected 4 diagnostics, got %v", msgs)
		}
		if iter == 0 {
			baseline = msgs
			if !strings.Contains(msgs[0], "ALPHA_VAR") || !strings.Contains(msgs[1], "ZULU_VAR") || !strings.Contains(msgs[2], "HDR_A") || !strings.Contains(msgs[3], "HDR_Z") {
				t.Fatalf("expected sorted diagnostic order ALPHA_VAR, ZULU_VAR, HDR_A, HDR_Z; got %v", msgs)
			}
		} else {
			for i := range msgs {
				if msgs[i] != baseline[i] {
					t.Fatalf("non-deterministic diagnostic order on iter %d: %v vs %v", iter, msgs, baseline)
				}
			}
		}
	}
}
