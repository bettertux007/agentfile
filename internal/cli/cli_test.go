package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/runner"
)

func TestInitSyncCleanEndToEnd(t *testing.T) {
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	proj := t.TempDir()
	var out, errOut bytes.Buffer

	// 1. init creates Agentfile.yaml; second init without --force exits 2.
	code := ExecuteWithIO([]string{"init", "--minimal"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("init failed (%d): %s", code, errOut.String())
	}
	code = ExecuteWithIO([]string{"init", "--minimal"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 2 {
		t.Fatalf("expected exit 2 when Agentfile.yaml exists, got %d", code)
	}

	// 2. Write manifest with local skill, rule, mcp, and existing hand-written .mcp.json.
	_ = os.MkdirAll(filepath.Join(proj, "skills", "auditor"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "skills", "auditor", "SKILL.md"), []byte("# Auditor\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "docs", "RULES.md"), []byte("Use gofmt.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, ".mcp.json"), []byte("{\n  // user server\n  \"mcpServers\": {\"local\": {\"command\": \"echo\"}}\n}\n"), 0o644)

	manifest := `version: 1
targets:
  - claude
  - opencode
mcp:
  github:
    transport: http
    url: "https://api.example.com/mcp/"
    headers:
      Authorization: "Bearer ${GITHUB_PAT}"
skills:
  - name: auditor
    source: ./skills/auditor
rules:
  - name: style
    source: ./docs/RULES.md
    globs: ["**/*.go"]
`
	if err := os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Init git repo to verify exclude management.
	gitInit := exec.Command("git", "init")
	gitInit.Dir = proj
	if b, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, b)
	}

	// 3. Dry-run JSON outputs planned changes without mutating disk.
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"sync", "--dry-run", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("sync --dry-run failed (%d): %s", code, errOut.String())
	}
	var drySum map[string]any
	if err := json.Unmarshal(out.Bytes(), &drySum); err != nil {
		t.Fatalf("invalid dry-run JSON: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(proj, "opencode.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write opencode.json")
	}

	// 4. Actual sync writes targets, preserves user .mcp.json key, and updates .git/info/exclude.
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("sync failed (%d): %s", code, errOut.String())
	}
	mcpBytes, _ := os.ReadFile(filepath.Join(proj, ".mcp.json"))
	if !strings.Contains(string(mcpBytes), `"local"`) || !strings.Contains(string(mcpBytes), "${GITHUB_PAT}") {
		t.Fatalf("unexpected .mcp.json:\n%s", mcpBytes)
	}
	ocBytes, _ := os.ReadFile(filepath.Join(proj, "opencode.json"))
	if !strings.Contains(string(ocBytes), "{env:GITHUB_PAT}") {
		t.Fatalf("expected {env:GITHUB_PAT} in opencode.json:\n%s", ocBytes)
	}

	// 5. Unknown flag exits 2.
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"sync", "--bogus-flag"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 2 {
		t.Fatalf("expected exit 2 on unknown flag, got %d", code)
	}

	// 6. Clean removes managed entries while preserving user's "local" server in .mcp.json.
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"clean"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("clean failed (%d): %s", code, errOut.String())
	}
	mcpAfterClean, _ := os.ReadFile(filepath.Join(proj, ".mcp.json"))
	if !strings.Contains(string(mcpAfterClean), `"local"`) || strings.Contains(string(mcpAfterClean), `"github"`) {
		t.Fatalf("expected user server preserved and github removed after clean:\n%s", mcpAfterClean)
	}
	if _, err := os.Stat(filepath.Join(proj, "opencode.json")); !os.IsNotExist(err) {
		t.Fatalf("expected agentfile-created opencode.json removed after clean")
	}
}

func TestMultiTargetGitRepoDoctorAndFailClosed(t *testing.T) {
	cacheDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("AGENTFILE_CACHE_DIR", cacheDir)
	t.Setenv("AGENTFILE_ALLOW_FILE_GIT", "1")
	t.Setenv("HOME", homeDir)
	t.Setenv("CI", "true")

	// Seed a global ~/.claude.json with a shadowing MCP server.
	_ = os.WriteFile(filepath.Join(homeDir, ".claude.json"), []byte(`{"mcpServers":{"github":{"command":"gh"}}}`), 0o644)

	// Create bare git repo with a skill.
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "k8s"), 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "k8s", "SKILL.md"), []byte("# K8s Hardening\n"), 0o644)
	runGitCLI := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGitCLI(workRepo, "init", "-b", "main")
	runGitCLI(workRepo, "add", ".")
	runGitCLI(workRepo, "commit", "-m", "init")
	runGitCLI(workRepo, "tag", "-a", "v1.0.0", "-m", "v1")
	runGitCLI(tmp, "clone", "--bare", workRepo, bareRepo)

	proj := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "docs", "RULES.md"), []byte("Run tests.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "GEMINI.md"), []byte("# Existing Gemini Notes\n"), 0o644)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	failManifest := "version: 1\ntargets: [claude, opencode, gemini, cursor]\n" +
		"mcp:\n  github:\n    transport: http\n    url: \"https://api.example.com/mcp\"\n" +
		"skills:\n  - name: k8s\n    source: \"" + repoURL + "#v1.0.0//skills/k8s\"\n" +
		"rules:\n  - name: coding\n    source: ./docs/RULES.md\n    globs: [\"**/*.go\"]\n" +
		"settings:\n  permissions:\n    deny_commands: [\"rm -rf *\"]\n"

	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(failManifest), 0o644)

	var out, errOut bytes.Buffer
	code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 2 {
		t.Fatalf("expected exit 2 on fail-closed deny_commands without on_unsupported: warn, got %d (%s)", code, errOut.String())
	}

	okManifest := failManifest + "\ntargets_config:\n  opencode:\n    on_unsupported: warn\n  gemini:\n    on_unsupported: warn\n  cursor:\n    on_unsupported: warn\n"
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(okManifest), 0o644)

	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("4-target sync failed (%d): %s", code, errOut.String())
	}

	// Verify GEMINI.md preserved existing notes + managed block.
	gemMD, _ := os.ReadFile(filepath.Join(proj, "GEMINI.md"))
	if !strings.Contains(string(gemMD), "# Existing Gemini Notes") || !strings.Contains(string(gemMD), "<!-- BEGIN AGENTFILE:v1") {
		t.Fatalf("unexpected GEMINI.md:\n%s", gemMD)
	}
	if _, err := os.Stat(filepath.Join(proj, ".cursor", "rules", "agentfile-coding.mdc")); err != nil {
		t.Fatalf("expected cursor rule file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, "Agentfile.lock")); err != nil {
		t.Fatalf("expected Agentfile.lock created: %v", err)
	}

	// Run doctor --json and verify manifest valid, lock ok, and global conflict detected.
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"doctor", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("doctor failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	var docRep DoctorReport
	if err := json.Unmarshal(out.Bytes(), &docRep); err != nil {
		t.Fatalf("invalid doctor JSON: %v\n%s", err, out.String())
	}
	if !docRep.ManifestValid || docRep.LockStatus != "ok" || len(docRep.GlobalConflicts) != 1 || docRep.GlobalConflicts[0].ServerName != "github" {
		t.Fatalf("unexpected doctor report: %+v", docRep)
	}

	// Clean dry-run JSON + actual clean preserves # Existing Gemini Notes.
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"clean", "--dry-run", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("clean --dry-run failed: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"clean"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("clean failed: %s", errOut.String())
	}
	gemAfterClean, _ := os.ReadFile(filepath.Join(proj, "GEMINI.md"))
	if !strings.Contains(string(gemAfterClean), "# Existing Gemini Notes") || strings.Contains(string(gemAfterClean), "BEGIN AGENTFILE") {
		t.Fatalf("expected user GEMINI.md preserved and block removed:\n%s", gemAfterClean)
	}

	// Verify RedactSecrets masks bearer/token values.
	if got := RedactSecrets("Authorization: Bearer secret-tok-123"); !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "secret-tok-123") {
		t.Fatalf("secret not redacted: %q", got)
	}
}

func TestCLIInitFullCleanCacheDoctorHumanAndPrompts(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("AGENTFILE_CACHE_DIR", cacheDir)
	t.Setenv("CI", "true")

	proj := t.TempDir()
	var out, errOut bytes.Buffer

	// init --full works, --minimal + --full fails with 2.
	if code := ExecuteWithIO([]string{"init", "--minimal", "--full"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 for --minimal --full, got %d", code)
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"init", "--full"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("init --full failed (%d): %s", code, errOut.String())
	}

	// doctor human output on valid minimal manifest.
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("doctor human failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), "Manifest valid: true") {
		t.Fatalf("unexpected doctor human output:\n%s", out.String())
	}

	// clean --cache --json with bad max-age -> exit 2, valid -> exit 0.
	if code := ExecuteWithIO([]string{"clean", "--cache", "--max-age=bad"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on bad --max-age, got %d", code)
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"clean", "--cache", "--max-age=1h", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("clean --cache failed (%d): %s", code, errOut.String())
	}

	// Test ttyPrompter interactive choices.
	p := newTTYPrompter(strings.NewReader("o\nb\ns\na\n"), &out)
	for _, want := range []string{"overwrite", "backup", "skip", "abort"} {
		got, err := p.AskConflict("file.txt", "exists")
		if err != nil || string(got) != want {
			t.Fatalf("expected %s, got %s (err=%v)", want, got, err)
		}
	}

	// Doctor on empty project with no Agentfile.yaml exits 2.
	emptyProj := t.TempDir()
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor", "--json"}, emptyProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 for missing Agentfile.yaml in doctor, got %d", code)
	}

	// Fake binary on PATH for doctor version probing.
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	_ = os.WriteFile(fakeClaude, []byte("#!/bin/sh\necho 'claude 1.2.3'\n"), 0o755)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("doctor with fake claude failed (%d): %s", code, errOut.String())
	}
	var rep DoctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Targets) != 1 || !rep.Targets[0].BinaryFound || rep.Targets[0].Version != "claude 1.2.3" {
		t.Fatalf("expected probed binary version, got %+v", rep.Targets)
	}

	// Sync --force and --targets works cleanly.
	_ = os.WriteFile(filepath.Join(proj, ".claude", "agents", "orphan.md"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync", "--targets=claude", "--force"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync --force failed (%d): %s", code, errOut.String())
	}

	// Clean human output and Execute(--help) smoke test.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"clean"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("clean human failed (%d): %s", code, errOut.String())
	}
	if code := Execute([]string{"--help"}); code != 0 {
		t.Fatalf("Execute(--help) failed: %d", code)
	}
	if code := Execute([]string{"--version"}); code != 0 {
		t.Fatalf("Execute(--version) failed: %d", code)
	}

	// Sync human output with hooks, allow_commands, deny_commands, and stdio MCP.
	proj2 := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj2, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(proj2, "docs", "R.md"), []byte("# R\n"), 0o644)
	manifest2 := `version: 1
targets: [claude]
mcp:
  fs:
    transport: stdio
    command: npx
    args: ["-y", "@mcp/fs"]
rules:
  - name: r
    source: ./docs/R.md
settings:
  permissions:
    allow_commands: ["go test ./..."]
    deny_commands: ["rm -rf *"]
  hooks:
    PreToolUse:
      - matcher: "Bash"
        command: "./c.sh"
        enforce: true
`
	_ = os.WriteFile(filepath.Join(proj2, "Agentfile.yaml"), []byte(manifest2), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj2, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync human failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Security / executable items:") || !strings.Contains(out.String(), "stdio mcp:") {
		t.Fatalf("expected security highlights in human sync output:\n%s", out.String())
	}

	// Clean --targets=claude exits 0 and missing Agentfile.yaml sync exits 2.
	if code := ExecuteWithIO([]string{"clean", "--targets=claude"}, proj2, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("clean --targets failed: %d", code)
	}
	if code := ExecuteWithIO([]string{"sync"}, t.TempDir(), strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 when Agentfile.yaml missing, got %d", code)
	}

	// Doctor on invalid Agentfile.yaml (missing targets + out-of-sync lock) exits 2 with SchemaErrors.
	badProj := t.TempDir()
	_ = os.WriteFile(filepath.Join(badProj, "Agentfile.yaml"), []byte("version: 2\ntargets: [claude]\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor"}, badProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on invalid manifest in doctor, got %d", code)
	}

	// Doctor on out-of-sync lockfile exits 2 and reports LockStatus=out_of_sync.
	_ = os.WriteFile(filepath.Join(badProj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\nskills:\n  - name: s\n    source: https://github.com/org/repo.git#v2.0.0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(badProj, "Agentfile.lock"), []byte("version: 1\ndependencies:\n  skill:s:\n    source: https://github.com/org/repo.git\n    ref: v1.0.0\n    commit_sha: "+strings.Repeat("a", 40)+"\n    content_hash: sha256:"+strings.Repeat("1", 64)+"\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor", "--json"}, badProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on out-of-sync lockfile in doctor, got %d", code)
	}
	var badRep DoctorReport
	_ = json.Unmarshal(out.Bytes(), &badRep)
	if badRep.LockStatus != "out_of_sync" {
		t.Fatalf("expected out_of_sync lock status, got %+v", badRep)
	}

	// Doctor with missing lockfile and remote rule warns cleanly.
	_ = os.Remove(filepath.Join(badProj, "Agentfile.lock"))
	_ = os.WriteFile(filepath.Join(badProj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\nrules:\n  - name: r\n    source: https://github.com/org/repo.git#v1.0.0\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor", "--json"}, badProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("expected exit 0 with missing lockfile warning, got %d (%s)", code, errOut.String())
	}

	// Verify sync --update updates lockfile after tag bump on bare git repo.
	// Also test invalid YAML in Agentfile.yaml -> exit 2.
	_ = os.WriteFile(filepath.Join(badProj, "Agentfile.yaml"), []byte("invalid: [yaml\n"), 0o644)
	if code := ExecuteWithIO([]string{"sync"}, badProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on invalid Agentfile.yaml, got %d", code)
	}
}

type fakeRunProc struct {
	called bool
	bin    string
	argv   []string
	env    []string
	dir    string
	err    error
}

func (f *fakeRunProc) Run(bin string, argv []string, env []string, dir string) error {
	f.called = true
	f.bin = bin
	f.argv = append([]string(nil), argv...)
	f.env = append([]string(nil), env...)
	f.dir = dir
	return f.err
}

func TestRunIsolationForwardingDryRunAndSyncModes(t *testing.T) {
	homeDir := t.TempDir()
	cacheDir := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("AGENTFILE_CACHE_DIR", cacheDir)
	t.Setenv("CI", "true")

	// Stub claude >= 2.1.0 on PATH.
	fakeClaude := filepath.Join(binDir, "claude")
	_ = os.WriteFile(fakeClaude, []byte("#!/bin/sh\necho '2.1.278 (Claude Code)'\n"), 0o755)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	proj := t.TempDir()
	manifest := `version: 1
targets: [claude, cursor]
mcp:
  github:
    transport: http
    url: "https://api.example.com/mcp"
targets_config:
  claude:
    load_global: false
  cursor:
    load_global: false
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifest), 0o644)

	var out, errOut bytes.Buffer
	fpr := &fakeRunProc{}

	// 1. `agentfile run --help` prints run help, while `agentfile run claude --help` and `-v` forward verbatim!
	if code := ExecuteWithRunner([]string{"run", "--help"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 || fpr.called {
		t.Fatalf("run --help failed: code=%d called=%v", code, fpr.called)
	}
	if !strings.Contains(out.String(), "agentfile run") {
		t.Fatalf("expected run help text, got:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	fpr = &fakeRunProc{}
	if code := ExecuteWithRunner([]string{"run", "--no-sync", "claude", "--help", "-v"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 || !fpr.called {
		t.Fatalf("expected claude --help -v forwarded: code=%d called=%v err=%s", code, fpr.called, errOut.String())
	}
	if len(fpr.argv) != 2 || fpr.argv[0] != "--help" || fpr.argv[1] != "-v" {
		t.Fatalf("expected [--help -v] forwarded verbatim, got %v", fpr.argv)
	}

	// 2. `--dry-run --json` produces zero disk writes and reports isolated argv.
	out.Reset()
	errOut.Reset()
	fpr = &fakeRunProc{}
	if code := ExecuteWithRunner([]string{"run", "--dry-run", "--json", "claude", "-p", "hello"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 || fpr.called {
		t.Fatalf("run --dry-run --json failed: code=%d called=%v err=%s", code, fpr.called, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(proj, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("run --dry-run must not write .mcp.json")
	}
	var dryRep RunDryRunReport
	if err := json.Unmarshal(out.Bytes(), &dryRep); err != nil {
		t.Fatalf("invalid RunDryRunReport JSON: %v\n%s", err, out.String())
	}
	if dryRep.IsolationMode != "native_flags" || dryRep.LoadGlobal || dryRep.LoadGlobalSource != "agentfile" {
		t.Fatalf("unexpected RunDryRunReport: %+v", dryRep)
	}

	// 3. `--sync=check` fails before sync when .mcp.json hasn't been synced yet.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithRunner([]string{"run", "--sync=check", "claude", "-p", "hello"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 2 {
		t.Fatalf("expected exit 2 on --sync=check before sync, got %d (%s)", code, errOut.String())
	}

	// 4. Actual `run claude -p hello` syncs cleanly (stdout empty, stderr has notice) and passes isolation flags.
	out.Reset()
	errOut.Reset()
	fpr = &fakeRunProc{}
	if code := ExecuteWithRunner([]string{"run", "claude", "-p", "hello"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 || !fpr.called {
		t.Fatalf("run claude failed: code=%d err=%s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected empty stdout during run sync, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "load_global=false") {
		t.Fatalf("expected stderr notice, got %q", errOut.String())
	}
	if !strings.Contains(strings.Join(fpr.argv, " "), "--setting-sources project,local --strict-mcp-config --mcp-config "+filepath.Join(proj, ".mcp.json")+" -p hello") {
		t.Fatalf("unexpected isolated argv: %v", fpr.argv)
	}

	// 5. `--sync=check` now passes after sync!
	out.Reset()
	errOut.Reset()
	fpr = &fakeRunProc{}
	if code := ExecuteWithRunner([]string{"run", "--sync=check", "claude", "-p", "hello"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 || !fpr.called {
		t.Fatalf("expected --sync=check pass after sync: code=%d err=%s", code, errOut.String())
	}

	// 6. Cursor fails closed (exit 2); --json without --dry-run exits 2; unconfigured target exits 2; ExitCodeError propagates verbatim.
	if code := ExecuteWithRunner([]string{"run", "cursor"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 2 {
		t.Fatalf("expected exit 2 for un-isolatable cursor, got %d", code)
	}
	if code := ExecuteWithRunner([]string{"run", "--json", "claude", "-p", "hi"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 2 {
		t.Fatalf("expected exit 2 for --json without --dry-run, got %d", code)
	}
	if code := ExecuteWithRunner([]string{"run", "opencode"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 2 {
		t.Fatalf("expected exit 2 for unconfigured target opencode, got %d", code)
	}
	errOut.Reset()
	fprErr := &fakeRunProc{err: &runner.ExitCodeError{Code: 42}}
	if code := ExecuteWithRunner([]string{"run", "--no-sync", "claude", "-p", "hi"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fprErr); code != 42 {
		t.Fatalf("expected ExitCodeError 42 propagated verbatim, got %d", code)
	}

	// 7. Doctor detects non-empty ~/.claude/skills in GlobalAssets.
	_ = os.MkdirAll(filepath.Join(homeDir, ".claude", "skills", "personal"), 0o755)
	_ = os.WriteFile(filepath.Join(homeDir, ".claude", "skills", "personal", "SKILL.md"), []byte("# P"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("doctor failed: code=%d err=%s", code, errOut.String())
	}
	var docRep DoctorReport
	if err := json.Unmarshal(out.Bytes(), &docRep); err != nil {
		t.Fatal(err)
	}
	if len(docRep.GlobalAssets) == 0 || docRep.Targets[0].LoadGlobal || docRep.Targets[0].IsolationMode != "unsupported" {
		t.Fatalf("expected GlobalAssets and updated isolation_mode in doctor report: %+v", docRep)
	}

	// OpenCode dry-run with on_isolation_unsupported: warn emits OPENCODE_CONFIG_DIR and --standalone.
	ocProj := t.TempDir()
	_ = os.WriteFile(filepath.Join(binDir, "opencode"), []byte("#!/bin/sh\necho 'opencode v2.3.0'\n"), 0o755)
	_ = os.WriteFile(filepath.Join(ocProj, "Agentfile.yaml"), []byte("version: 1\ntargets: [opencode]\ntargets_config:\n  opencode:\n    load_global: false\n    on_isolation_unsupported: warn\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithRunner([]string{"run", "--dry-run", "--json", "opencode", "debug", "agents"}, ocProj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 {
		t.Fatalf("opencode dry-run failed: code=%d err=%s", code, errOut.String())
	}
	var ocDry RunDryRunReport
	_ = json.Unmarshal(out.Bytes(), &ocDry)
	if ocDry.EnvAddedRedacted["OPENCODE_DISABLE_CLAUDE_CODE_SKILLS"] != "1" || ocDry.EnvAddedRedacted["OPENCODE_CONFIG_DIR"] == "" {
		t.Fatalf("expected OPENCODE_* env vars in dry-run report: %+v", ocDry)
	}
}

func TestRunRealSubprocessExec(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		sep := slices.Index(os.Args, "--")
		os.Exit(Execute(os.Args[sep+1:]))
	}

	binDir := t.TempDir()
	proj := t.TempDir()
	cacheDir := t.TempDir()
	homeDir := t.TempDir()

	// Stub claude script that echoes args & AGENTFILE_ACTIVE and exits 7.
	stub := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '2.1.278'; exit 0; fi\necho \"ACTIVE=$AGENTFILE_ACTIVE ARGS=$*\"\nexit 7\n"
	_ = os.WriteFile(stub, []byte(script), 0o755)

	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\ntargets_config:\n  claude:\n    load_global: false\n"), 0o644)

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunRealSubprocessExec$", "--", "run", "--no-sync", "claude", "-p", "hi")
	cmd.Dir = proj
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"HOME="+homeDir,
		"AGENTFILE_CACHE_DIR="+cacheDir,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.Output()
	var ee *exec.ExitError
	if err == nil || !os.IsNotExist(err) && (!strings.Contains(err.Error(), "exit status 7")) {
		t.Fatalf("expected exit status 7 from stub claude, got err=%v out=%s", err, out)
	}
	_ = ee
	if !strings.Contains(string(out), "ACTIVE=1") || !strings.Contains(string(out), "--setting-sources project,local --strict-mcp-config --mcp-config") {
		t.Fatalf("unexpected real exec output: %s", out)
	}
}

func TestRunFlagConflictsHumanDryRunAndSyncWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	binDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\necho '2.1.278'\n"), 0o755)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	proj := t.TempDir()
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\ntargets_config:\n  claude:\n    load_global: false\n"), 0o644)

	var out, errOut bytes.Buffer
	fpr := &fakeRunProc{}

	// Sync emits RUNTIME_ONLY_ISOLATION notice when load_global: false is set.
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync failed: %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "RUNTIME_ONLY_ISOLATION") {
		t.Fatalf("expected RUNTIME_ONLY_ISOLATION warning in sync output:\n%s", out.String())
	}

	// Conflicting --sync + --no-sync, bad --sync mode, --load-global + --no-global, missing Agentfile.yaml.
	badInvocations := [][]string{
		{"run", "--sync=auto", "--no-sync", "claude"},
		{"run", "--sync=bogus", "claude"},
		{"run", "--load-global", "--no-global", "claude"},
	}
	for _, args := range badInvocations {
		if code := ExecuteWithRunner(args, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 2 {
			t.Fatalf("expected exit 2 for %v, got %d", args, code)
		}
	}
	if code := ExecuteWithRunner([]string{"run", "claude"}, t.TempDir(), strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 2 {
		t.Fatalf("expected exit 2 when Agentfile.yaml missing, got %d", code)
	}

	// Human dry-run output with --load-global override prints warning on stderr and command on stdout.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithRunner([]string{"run", "--dry-run", "--load-global", "claude", "-p", "hi"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 {
		t.Fatalf("human dry-run failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Run dry-run: target=claude") || !strings.Contains(errOut.String(), "overriding Agentfile.yaml load_global=false") {
		t.Fatalf("unexpected human dry-run output: out=%q err=%q", out.String(), errOut.String())
	}

	// Nested AGENTFILE_ACTIVE=1 warning.
	t.Setenv("AGENTFILE_ACTIVE", "1")
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithRunner([]string{"run", "--no-sync", "claude", "-p", "hi"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 {
		t.Fatalf("nested run failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "nested agentfile run detected") {
		t.Fatalf("expected nested warning, got %q", errOut.String())
	}
}
