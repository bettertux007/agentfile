package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/pelletier/go-toml/v2"
)

func renderTableEntriesToTOML(t *testing.T, entries map[string]any) []byte {
	t.Helper()
	doc := make(map[string]any)
	for dotted, val := range entries {
		parts := strings.Split(dotted, ".")
		cur := doc
		for i := 0; i < len(parts)-1; i++ {
			seg := parts[i]
			next, ok := cur[seg].(map[string]any)
			if !ok {
				next = make(map[string]any)
				cur[seg] = next
			}
			cur = next
		}
		cur[parts[len(parts)-1]] = val
	}
	out, err := toml.Marshal(doc)
	if err != nil {
		t.Fatalf("toml.Marshal failed: %v", err)
	}
	return out
}

func TestCodexCompileGoldenSharedAndMCP(t *testing.T) {
	t.Parallel()

	ghTokenEnv, _ := spec.ParseEnvString("${GITHUB_TOKEN}")
	oaiKeyEnv, _ := spec.ParseEnvString("${OPENAI_API_KEY}")
	ghPatBearer, _ := spec.ParseEnvString("Bearer ${GITHUB_PAT}")
	apiKeyEnv, _ := spec.ParseEnvString("${KEY}")

	b := &ir.Bundle{
		Targets: []string{"codex"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "filesystem",
				Transport: "stdio",
				Command:   spec.EnvString{Raw: "npx"},
				Args: []spec.EnvString{
					{Raw: "-y"},
					{Raw: "@modelcontextprotocol/server-filesystem"},
					{Raw: "./docs"},
				},
				Env: map[string]spec.EnvString{
					"LOG_LEVEL":      {Raw: "info"},
					"GITHUB_TOKEN":   ghTokenEnv,
					"OPENAI_API_KEY": oaiKeyEnv,
				},
			},
			{
				Name:      "github",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://api.githubcopilot.com/mcp/"},
				Headers: map[string]spec.EnvString{
					"Authorization": ghPatBearer,
					"X-API-Key":     apiKeyEnv,
					"X-Client":      {Raw: "agentfile"},
				},
			},
		},
		Skills: []ir.IRAsset{
			{Name: "k8s-hardening", SourcePath: "/store/k8s", IsRemote: true},
		},
		Agents: []ir.IRAgent{
			{Name: "sec", Content: []byte("Audit")},
		},
		Rules: []ir.IRRule{
			{Name: "coding", Description: "Go rules", Globs: []string{"**/*.go"}, Content: []byte("Always run go test.\n")},
		},
		TargetsConfig: map[string]spec.TargetConfigSpec{
			"codex": {
				RulesMode: "agents_md",
				Overrides: map[string]any{
					"model":           "o3",
					"approval_policy": "on-request",
				},
			},
		},
	}

	adp := New()
	if adp.Name() != "codex" || adp.BinaryName() != "codex" {
		t.Fatalf("unexpected adapter name/binary: %q / %q", adp.Name(), adp.BinaryName())
	}
	if adp.SchemaContract().MinVersion == "" || adp.SchemaContract().BinaryOptional {
		t.Fatalf("unexpected schema contract: %+v", adp.SchemaContract())
	}
	if adp.RequiredProbes() != nil {
		t.Fatalf("expected nil RequiredProbes, got %+v", adp.RequiredProbes())
	}

	res, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}

	// Only advisory DiagWarning for agents should be present.
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Level != adapters.DiagWarning || res.Diagnostics[0].Code != "UNSUPPORTED_AGENT" {
		t.Fatalf("expected 1 UNSUPPORTED_AGENT warning, got %+v", res.Diagnostics)
	}

	byPath := make(map[string]adapters.ArtifactOp)
	for _, f := range res.Files {
		byPath[f.RelPath] = f
	}

	// 1. .codex/config.toml golden check
	cfgOp, ok := byPath[".codex/config.toml"]
	if !ok || cfgOp.Kind != adapters.OpMergeTOML || cfgOp.TOMLMerge == nil {
		t.Fatalf("expected OpMergeTOML for .codex/config.toml, got %+v", cfgOp)
	}
	gotTOML := renderTableEntriesToTOML(t, cfgOp.TOMLMerge.TableEntries)
	goldenTOML, err := os.ReadFile(filepath.Join("testdata", "config.golden.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotTOML) != string(goldenTOML) {
		t.Fatalf(".codex/config.toml golden mismatch:\ngot:\n%s\nwant:\n%s", gotTOML, goldenTOML)
	}

	// 2. AGENTS.md shared managed block golden check
	agentsOp, ok := byPath["AGENTS.md"]
	if !ok || agentsOp.Kind != adapters.OpManagedBlock || !agentsOp.Shared {
		t.Fatalf("expected Shared OpManagedBlock for AGENTS.md, got %+v", agentsOp)
	}
	goldenMD, err := os.ReadFile(filepath.Join("testdata", "AGENTS.golden.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(agentsOp.Content) != string(goldenMD) {
		t.Fatalf("AGENTS.md golden mismatch:\ngot:\n%s\nwant:\n%s", agentsOp.Content, goldenMD)
	}

	// 3. .agents/skills/<name> shared materialize tree check
	skillOp, ok := byPath[".agents/skills/k8s-hardening"]
	if !ok || skillOp.Kind != adapters.OpMaterializeTree || !skillOp.Shared || skillOp.SourcePath != "/store/k8s" || !skillOp.IsRemote {
		t.Fatalf("expected Shared OpMaterializeTree for .agents/skills/k8s-hardening, got %+v", skillOp)
	}
}

func TestCodexEmptyRulesAndEmptyConfigOmitted(t *testing.T) {
	t.Parallel()
	adp := New()
	b := &ir.Bundle{
		Targets: []string{"codex"},
	}
	res, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	if len(res.Files) != 0 || len(res.Diagnostics) != 0 {
		t.Fatalf("expected no files and no diagnostics for empty bundle, got files=%+v diags=%+v", res.Files, res.Diagnostics)
	}
}

func TestCodexFailClosedAndWarnDowngrades(t *testing.T) {
	t.Parallel()
	adp := New()

	cmdVar, _ := spec.ParseEnvString("${MY_BIN}")
	argVar, _ := spec.ParseEnvString("--token=${MY_TOK}")
	urlVar, _ := spec.ParseEnvString("https://${HOST}/mcp")
	envDefault, _ := spec.ParseEnvString("${LOG_LEVEL:-info}")
	envMixed, _ := spec.ParseEnvString("prefix-${FOO}")
	hdrBearerDefault, _ := spec.ParseEnvString("Bearer ${GH_PAT:-none}")
	hdrVarDefault, _ := spec.ParseEnvString("${API_KEY:-none}")
	hdrMixed, _ := spec.ParseEnvString("prefix-${BAR}")

	b := &ir.Bundle{
		Targets: []string{"codex"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "legacy-sse",
				Transport: "sse",
				URL:       spec.EnvString{Raw: "https://sse.example.com/events"},
			},
			{
				Name:      "stdio-bad",
				Transport: "stdio",
				Command:   cmdVar,
				Args:      []spec.EnvString{argVar},
				Env: map[string]spec.EnvString{
					"LOG_LEVEL": envDefault,
					"MIXED":     envMixed,
				},
			},
			{
				Name:      "http-bad",
				Transport: "http",
				URL:       urlVar,
				Headers: map[string]spec.EnvString{
					"Authorization": hdrBearerDefault,
					"X-API-Key":     hdrVarDefault,
					"X-Mixed":       hdrMixed,
				},
			},
		},
		AllowCommands: []string{"go test ./..."},
		DenyCommands:  []ir.SecurityControl{{Kind: "deny_command", Pattern: "rm -rf *"}},
		Hooks: []ir.IRHook{
			{Event: "PreToolUse", Command: spec.EnvString{Raw: "./c.sh"}, Enforced: true},
			{Event: "SessionStart", Command: spec.EnvString{Raw: "echo hi"}, Enforced: false},
		},
		Rules: []ir.IRRule{{Name: "r1", Content: []byte("Stay safe.\n")}},
		TargetsConfig: map[string]spec.TargetConfigSpec{
			"codex": {RulesMode: "dropin"},
		},
	}

	resFail, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}

	errCodes := make(map[string]int)
	warnCodes := make(map[string]int)
	for _, d := range resFail.Diagnostics {
		switch d.Level {
		case adapters.DiagError:
			errCodes[d.Code]++
		case adapters.DiagWarning:
			warnCodes[d.Code]++
		}
	}

	for _, wantCode := range []string{
		"UNSUPPORTED_MCP_TRANSPORT",
		"UNSUPPORTED_ENV_INTERPOLATION",
		"UNSUPPORTED_ENV_DEFAULT",
		"UNSUPPORTED_DENY_COMMAND",
		"UNSUPPORTED_HOOK",
		"UNSUPPORTED_RULES_MODE",
	} {
		if errCodes[wantCode] == 0 {
			t.Fatalf("expected DiagError with code %q, got errors=%v warnings=%v", wantCode, errCodes, warnCodes)
		}
	}
	if warnCodes["UNSUPPORTED_ALLOW_COMMANDS"] == 0 || warnCodes["UNSUPPORTED_HOOK"] == 0 {
		t.Fatalf("expected advisory warnings for allow_commands and non-enforced hook, got %v", warnCodes)
	}

	// Switch to on_unsupported: warn -> zero DiagErrors, SSE server omitted, defaults stripped, mixed dropped.
	b.TargetsConfig["codex"] = spec.TargetConfigSpec{
		OnUnsupported: "warn",
		RulesMode:     "dropin",
	}
	resWarn, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile warn mode failed: %v", err)
	}
	for _, d := range resWarn.Diagnostics {
		if d.Level == adapters.DiagError {
			t.Fatalf("unexpected DiagError in warn mode: %+v", d)
		}
	}

	var cfgOp adapters.ArtifactOp
	for _, f := range resWarn.Files {
		if f.RelPath == ".codex/config.toml" {
			cfgOp = f
		}
	}
	entries := cfgOp.TOMLMerge.TableEntries
	if _, exists := entries["mcp_servers.legacy-sse"]; exists {
		t.Fatalf("expected sse server omitted in warn mode")
	}
	stdioEntry := entries["mcp_servers.stdio-bad"].(map[string]any)
	if stdioEntry["command"] != "${MY_BIN}" {
		t.Fatalf("expected raw command kept in warn mode, got %v", stdioEntry["command"])
	}
	envVars := stdioEntry["env_vars"].([]string)
	if len(envVars) != 1 || envVars[0] != "LOG_LEVEL" {
		t.Fatalf("expected LOG_LEVEL in env_vars after default stripped, got %v", envVars)
	}
	if _, hasEnv := stdioEntry["env"]; hasEnv {
		t.Fatalf("expected mixed env omitted, got %v", stdioEntry["env"])
	}

	httpEntry := entries["mcp_servers.http-bad"].(map[string]any)
	if httpEntry["bearer_token_env_var"] != "GH_PAT" {
		t.Fatalf("expected bearer_token_env_var=GH_PAT after default stripped, got %v", httpEntry["bearer_token_env_var"])
	}
	envHdrs := httpEntry["env_http_headers"].(map[string]string)
	if envHdrs["X-API-Key"] != "API_KEY" || len(envHdrs) != 1 {
		t.Fatalf("expected X-API-Key=API_KEY only in env_http_headers, got %v", envHdrs)
	}
	if _, hasStatic := httpEntry["http_headers"]; hasStatic {
		t.Fatalf("expected mixed header omitted, got %v", httpEntry["http_headers"])
	}
}

func TestCodexCheckPolicy(t *testing.T) {
	t.Parallel()
	adp := New()

	bNoDeny := &ir.Bundle{}
	if diags := adp.CheckPolicy(bNoDeny, map[string][]byte{".codex/config.toml": []byte("approval_policy = 'never'\n")}); len(diags) != 0 {
		t.Fatalf("expected no policy error without deny_commands, got %+v", diags)
	}

	bDeny := &ir.Bundle{
		DenyCommands: []ir.SecurityControl{{Kind: "deny_command", Pattern: "rm -rf *"}},
	}

	// Missing config.toml -> nil
	if diags := adp.CheckPolicy(bDeny, map[string][]byte{}); len(diags) != 0 {
		t.Fatalf("expected nil when config.toml absent, got %+v", diags)
	}

	// Safe config -> nil
	if diags := adp.CheckPolicy(bDeny, map[string][]byte{".codex/config.toml": []byte("approval_policy = 'on-request'\nsandbox_mode = 'workspace-write'\n")}); len(diags) != 0 {
		t.Fatalf("expected nil for safe config, got %+v", diags)
	}

	// approval_policy = "never" -> POLICY_CONTRADICTION
	diagsNever := adp.CheckPolicy(bDeny, map[string][]byte{".codex/config.toml": []byte("approval_policy = 'never'\n")})
	if len(diagsNever) != 1 || diagsNever[0].Level != adapters.DiagError || diagsNever[0].Code != "POLICY_CONTRADICTION" {
		t.Fatalf("expected POLICY_CONTRADICTION for approval_policy=never, got %+v", diagsNever)
	}

	// sandbox_mode = "danger-full-access" -> POLICY_CONTRADICTION
	diagsDanger := adp.CheckPolicy(bDeny, map[string][]byte{".codex/config.toml": []byte("sandbox_mode = 'danger-full-access'\n")})
	if len(diagsDanger) != 1 || diagsDanger[0].Level != adapters.DiagError || diagsDanger[0].Code != "POLICY_CONTRADICTION" {
		t.Fatalf("expected POLICY_CONTRADICTION for sandbox_mode=danger-full-access, got %+v", diagsDanger)
	}

	// Invalid TOML -> INVALID_TOML
	diagsBad := adp.CheckPolicy(bDeny, map[string][]byte{".codex/config.toml": []byte("not = [valid")})
	if len(diagsBad) != 1 || diagsBad[0].Level != adapters.DiagError || diagsBad[0].Code != "INVALID_TOML" {
		t.Fatalf("expected INVALID_TOML error, got %+v", diagsBad)
	}
}

func TestCodexGlobalConfigSpecsIsSessionArgsAndPlanLaunch(t *testing.T) {
	t.Parallel()
	adp := New()

	specs := adp.GlobalConfigSpecs()
	if len(specs) != 3 {
		t.Fatalf("expected 3 GlobalConfigSpecs, got %d: %+v", len(specs), specs)
	}
	if specs[0].RelPath != ".codex/config.toml" || specs[1].RelPath != ".codex/AGENTS.md" || specs[2].RelPath != ".agents/skills" {
		t.Fatalf("unexpected GlobalConfigSpecs: %+v", specs)
	}

	if adp.IsSessionArgs([]string{"--help"}) ||
		adp.IsSessionArgs([]string{"-h"}) ||
		adp.IsSessionArgs([]string{"--version"}) ||
		adp.IsSessionArgs([]string{"-V"}) ||
		adp.IsSessionArgs([]string{"mcp", "list"}) ||
		adp.IsSessionArgs([]string{"login"}) ||
		adp.IsSessionArgs([]string{"-m", "o3", "logout"}) ||
		adp.IsSessionArgs([]string{"-m", "o3", "--help"}) {
		t.Fatalf("expected non-session args to return false")
	}
	if !adp.IsSessionArgs([]string{"-m", "--help"}) || !adp.IsSessionArgs([]string{"-m", "o3", "fix the tests"}) || !adp.IsSessionArgs([]string{"-c", "model=o3", "--", "--help"}) || !adp.IsSessionArgs(nil) {
		t.Fatalf("expected session args to return true")
	}

	b := &ir.Bundle{Targets: []string{"codex"}}

	// LoadGlobal=true -> IsolationNone
	lpGlobal, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: true}, adapters.LaunchFacts{})
	if err != nil || lpGlobal.Mode != adapters.IsolationNone || len(lpGlobal.Diagnostics) != 0 {
		t.Fatalf("expected IsolationNone when LoadGlobal=true, got %+v err=%v", lpGlobal, err)
	}

	// Non-session args -> IsolationNone
	lpNonSess, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false, UserArgs: []string{"--version"}}, adapters.LaunchFacts{})
	if err != nil || lpNonSess.Mode != adapters.IsolationNone {
		t.Fatalf("expected IsolationNone for --version, got %+v err=%v", lpNonSess, err)
	}

	// Default load_global=false -> fail-closed IsolationUnsupported
	lpFail, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if err != nil || lpFail.Mode != adapters.IsolationUnsupported || len(lpFail.Diagnostics) != 1 || lpFail.Diagnostics[0].Level != adapters.DiagError {
		t.Fatalf("expected IsolationUnsupported DiagError, got %+v err=%v", lpFail, err)
	}

	// on_isolation_unsupported: warn -> IsolationPartialWarn
	b.TargetsConfig = map[string]spec.TargetConfigSpec{
		"codex": {OnIsolationUnsupported: "warn"},
	}
	lpWarn, err := adp.PlanLaunch(b, adapters.LaunchOptions{LoadGlobal: false}, adapters.LaunchFacts{})
	if err != nil || lpWarn.Mode != adapters.IsolationPartialWarn || len(lpWarn.Diagnostics) != 1 || lpWarn.Diagnostics[0].Level != adapters.DiagWarning {
		t.Fatalf("expected IsolationPartialWarn DiagWarning, got %+v err=%v", lpWarn, err)
	}
}

func TestCodexDottedMCPServerName(t *testing.T) {
	t.Parallel()
	adp := New()
	b := &ir.Bundle{
		Targets: []string{"codex"},
		MCP: []ir.IRMCPServer{
			{
				Name:      "acme.github",
				Transport: "http",
				URL:       spec.EnvString{Raw: "https://acme.example.com/mcp"},
			},
		},
	}
	res, err := adp.Compile(b, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].TOMLMerge == nil {
		t.Fatalf("expected .codex/config.toml TOMLMerge op, got %+v", res.Files)
	}
	entries := res.Files[0].TOMLMerge.TableEntries
	if _, ok := entries[`mcp_servers."acme.github"`]; !ok {
		t.Fatalf("expected quoted dotted TOML key mcp_servers.\"acme.github\", got keys: %v", entries)
	}
}
