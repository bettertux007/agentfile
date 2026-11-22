package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditInventoryFindingsStrictAndRedaction(t *testing.T) {
	setupHermeticTestEnv(t)

	proj := t.TempDir()
	skillDir := filepath.Join(proj, "skills", "pay")
	_ = os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Pay\n"), 0o644)
	_ = os.WriteFile(filepath.Join(skillDir, "scripts", "check.sh"), []byte("#!/bin/sh\necho ok\n"), 0o755)

	// 1. Manifest with literal secret -> error finding, always exits 2, secret redacted in JSON.
	withSecret := `version: 1
targets: [claude]
mcp:
  bad:
    transport: http
    url: "http://mcp.internal.example.com/v1"
    headers:
      Authorization: "Bearer ghp_1234567890abcdefSECRET"
skills:
  - name: pay
    source: ./skills/pay
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(withSecret), 0o644)

	var out, errOut bytes.Buffer
	code := ExecuteWithIO([]string{"audit", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 2 {
		t.Fatalf("expected audit exit 2 on literal secret, got %d (err=%s)", code, errOut.String())
	}
	if strings.Contains(out.String(), "ghp_1234567890abcdefSECRET") {
		t.Fatalf("raw secret leaked in audit --json output:\n%s", out.String())
	}
	var rep AuditReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal AuditReport failed: %v\n%s", err, out.String())
	}
	if rep.Summary.Errors == 0 || rep.Summary.Passed {
		t.Fatalf("expected error findings and Passed=false, got %+v", rep.Summary)
	}

	// 2. Replace literal secret with ${GITHUB_PAT}, add stdio MCP (npx -y unpinned) + PreToolUse hook.
	// Plain `audit` exits 0 (warnings only + inventory), while `audit --strict` exits 2.
	withWarnings := `version: 1
targets: [claude]
mcp:
  remote:
    transport: http
    url: "http://mcp.internal.example.com/v1"
    headers:
      Authorization: "Bearer ${GITHUB_PAT}"
  fs:
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "."]
skills:
  - name: pay
    source: ./skills/pay
settings:
  hooks:
    PreToolUse:
      - matcher: "Bash"
        command: "./scripts/guard.sh"
        enforce: true
targets_config:
  claude:
    load_global: false
    on_unsupported: warn
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(withWarnings), 0o644)

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"audit", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("expected plain audit exit 0 when only warnings exist, got %d (err=%s\nout=%s)", code, errOut.String(), out.String())
	}
	var warnRep AuditReport
	if err := json.Unmarshal(out.Bytes(), &warnRep); err != nil {
		t.Fatalf("unmarshal warning AuditReport failed: %v", err)
	}
	if warnRep.Summary.Errors != 0 || warnRep.Summary.Warnings == 0 || !warnRep.Summary.Passed {
		t.Fatalf("expected 0 errors, >0 warnings, Passed=true: %+v", warnRep.Summary)
	}
	if len(warnRep.Inventory.Hooks) != 1 || len(warnRep.Inventory.StdioMCPs) != 1 || len(warnRep.Inventory.SkillScripts) != 1 {
		t.Fatalf("expected hooks, stdio_mcps, and skill_scripts in inventory: %+v", warnRep.Inventory)
	}
	seenRules := make(map[string]bool)
	for _, f := range warnRep.Findings {
		seenRules[f.RuleID] = true
	}
	for _, want := range []string{"CLEARTEXT_HTTP_MCP", "UNPINNED_PACKAGE_RUNNER", "POLICY_DOWNGRADE_WARN"} {
		if !seenRules[want] {
			t.Fatalf("expected rule_id %s in findings: %+v", want, warnRep.Findings)
		}
	}

	// Strict mode promotes warnings to exit code 2.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"audit", "--strict"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected audit --strict exit 2 on warnings, got %d", code)
	}

	// 3. Clean strict-passing manifest (pinned local skill, https MCP, load_global: false, pinned npx @1.0.0) exits 0 under --strict!
	cleanStrict := `version: 1
targets: [claude]
mcp:
  remote:
    transport: http
    url: "https://mcp.internal.example.com/v1"
    headers:
      Authorization: "Bearer ${GITHUB_PAT}"
  fs:
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem@1.0.0", "."]
skills:
  - name: pay
    source: ./skills/pay
settings:
  hooks:
    PreToolUse:
      - matcher: "Bash"
        command: "./scripts/guard.sh"
        enforce: true
targets_config:
  claude:
    load_global: false
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(cleanStrict), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"audit", "--strict", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("expected clean manifest with hooks/stdio MCP inventory to pass audit --strict (exit 0), got %d:\n%s\n%s", code, out.String(), errOut.String())
	}

	// 4. Test audit on remote refs (FLOATING_REF vs MUTABLE_TAG vs 40-char SHA), uvx unpinned runner,
	// overrides secret, inherited override warning, and human output formatting.
	baseDir := filepath.Join(proj, "base")
	_ = os.MkdirAll(baseDir, 0o755)
	_ = os.WriteFile(filepath.Join(baseDir, "Agentfile.yaml"), []byte("version: 1\nskills:\n  - name: pay\n    source: ../skills/pay\n"), 0o644)

	complexAudit := `version: 1
extends: ./base/Agentfile.yaml
targets: [claude, cursor]
mcp:
  pytool:
    transport: stdio
    command: uvx
    args: ["--from", "my-pkg", "run"]
skills:
  - name: pay
    source: ./skills/pay
  - name: floating-sk
    source: "https://github.com/org/skills.git#main//skills/a"
  - name: tag-sk
    source: "https://github.com/org/skills.git#v1.2.0//skills/b"
  - name: sha-sk
    source: "https://github.com/org/skills.git#` + strings.Repeat("a", 40) + `//skills/c"
targets_config:
  claude:
    load_global: true
    on_isolation_unsupported: warn
    overrides:
      nested:
        api_key: "sk-1234567890abcdefSECRET"
      list:
        - "ghp_1234567890abcdefSECRET"
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(complexAudit), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"audit"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on overrides literal secret, got %d", code)
	}
	humanOut := out.String()
	for _, want := range []string{
		"Audit status: FAIL",
		"Extends layers:",
		"Executable inventory:",
		"FLOATING_REF",
		"MUTABLE_TAG",
		"UNPINNED_PACKAGE_RUNNER",
		"INHERITED_ENTRY_OVERRIDDEN",
		"GLOBAL_ISOLATION_OPEN",
		"LITERAL_SECRET",
	} {
		if !strings.Contains(humanOut, want) {
			t.Fatalf("expected %q in human audit output:\n%s", want, humanOut)
		}
	}
	if strings.Contains(humanOut, "sk-1234567890abcdefSECRET") || strings.Contains(humanOut, "ghp_1234567890abcdefSECRET") {
		t.Fatalf("raw secret leaked in human audit output:\n%s", humanOut)
	}

	// 5. Test --no-store, LOCK_INVALID, EXTENDS_CYCLE, and EXTERNAL_LOCAL_PATH.
	extDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(extDir, "SKILL.md"), []byte("# Ext\n"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\nskills:\n  - name: ext\n    source: \""+filepath.ToSlash(extDir)+"\"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.lock"), []byte("version: 99\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"audit", "--no-store", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on invalid lockfile, got %d", code)
	}
	if !strings.Contains(out.String(), "LOCK_INVALID") || !strings.Contains(out.String(), "EXTERNAL_LOCAL_PATH") || !strings.Contains(out.String(), `"store_status": "skipped"`) {
		t.Fatalf("expected LOCK_INVALID, EXTERNAL_LOCAL_PATH, and store_status=skipped:\n%s", out.String())
	}
	_ = os.Remove(filepath.Join(proj, "Agentfile.lock"))

	// Cycle in extends -> EXTENDS_CYCLE error finding.
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte("version: 1\nextends: ./Agentfile.yaml\ntargets: [claude]\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"audit", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 || !strings.Contains(out.String(), "EXTENDS_CYCLE") {
		t.Fatalf("expected EXTENDS_CYCLE finding, got code=%d:\n%s", code, out.String())
	}
}
