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

	"github.com/bettertux007/agentfile/internal/diff"
	"github.com/bettertux007/agentfile/internal/runner"
)

func TestInitSyncCleanEndToEnd(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
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
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
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
	if got := diff.RedactSecrets("Authorization: Bearer secret-tok-123"); !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "secret-tok-123") {
		t.Fatalf("secret not redacted: %q", got)
	}
}

func TestCLIInitFullCleanCacheDoctorHumanAndPrompts(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
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
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
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
		"AGENTFILE_NO_USER_CONFIG=1",
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
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
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

func TestStatusCleanDriftedCheckAndJSON(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_ALLOW_FILE_GIT", "1")
	cacheDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("AGENTFILE_CACHE_DIR", cacheDir)
	t.Setenv("HOME", homeDir)
	t.Setenv("CI", "true")

	// Seed global ~/.claude.json and ~/.claude/skills/personal; status MUST NOT scan $HOME.
	_ = os.WriteFile(filepath.Join(homeDir, ".claude.json"), []byte(`{"mcpServers":{"github":{"command":"gh"}}}`), 0o644)
	_ = os.MkdirAll(filepath.Join(homeDir, ".claude", "skills", "personal"), 0o755)
	_ = os.WriteFile(filepath.Join(homeDir, ".claude", "skills", "personal", "SKILL.md"), []byte("# P"), 0o644)

	// Poison binary on PATH that creates a marker if `claude --version` is invoked.
	binDir := t.TempDir()
	claudeProbeMarker := filepath.Join(t.TempDir(), "claude-called")
	_ = os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\ntouch "+claudeProbeMarker+"\necho '2.1.278'\n"), 0o755)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	proj := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "docs", "RULES.md"), []byte("Be concise.\n"), 0o644)
	manifest := `version: 1
targets: [claude, cursor]
mcp:
  github:
    transport: http
    url: "https://api.example.com/mcp"
rules:
  - name: style
    source: ./docs/RULES.md
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifest), 0o644)

	var out, errOut bytes.Buffer

	// 1. Before sync: local-only repo without Agentfile.lock -> not_synced, lock_status="not_required", --check exits 2.
	code := ExecuteWithIO([]string{"status", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("status before sync failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	var stBefore map[string]any
	if err := json.Unmarshal(out.Bytes(), &stBefore); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, out.String())
	}
	if stBefore["lock_status"] != "not_required" {
		t.Fatalf("expected lock_status=not_required for local-only repo, got %v", stBefore["lock_status"])
	}
	if _, err := os.Stat(claudeProbeMarker); err == nil {
		t.Fatalf("status must never invoke target binary --version")
	}
	if warns, ok := stBefore["warnings"].([]any); ok {
		for _, w := range warns {
			if strings.Contains(w.(string), "personal") || strings.Contains(w.(string), "global config") {
				t.Fatalf("status must not scan $HOME global assets/conflicts, got warning %v", w)
			}
		}
	}

	// 2. After sync: clean local-only repo without Agentfile.lock -> in_sync, lock_status="not_required", --check exits 0.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"status", "--check", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("status --check on clean local-only repo failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	var stClean map[string]any
	if err := json.Unmarshal(out.Bytes(), &stClean); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, out.String())
	}
	if stClean["manifest_valid"] != true || stClean["lock_status"] != "not_required" || stClean["drifted"] != false {
		t.Fatalf("unexpected clean status report: %+v", stClean)
	}
	perTarget, _ := stClean["per_target"].([]any)
	if len(perTarget) != 2 {
		t.Fatalf("expected 2 per_target entries, got %d", len(perTarget))
	}
	for _, pt := range perTarget {
		m := pt.(map[string]any)
		if m["status"] != "in_sync" {
			t.Fatalf("expected target %v in_sync, got %+v", m["target"], m)
		}
		if m["target"] == "claude" && m["isolation_capability"] != "supported" {
			t.Fatalf("expected claude isolation_capability=supported, got %v", m["isolation_capability"])
		}
		if m["target"] == "cursor" && m["isolation_capability"] != "unsupported" {
			t.Fatalf("expected cursor isolation_capability=unsupported, got %v", m["isolation_capability"])
		}
	}

	// 3. Modify synced target file -> drifted, --check exits 2, without --check exits 0.
	// Unmodified target (claude) must remain in_sync while modified target (cursor) reports drifted.
	_ = os.WriteFile(filepath.Join(proj, ".cursor", "rules", "agentfile-style.mdc"), []byte("tampered\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("status without --check on drifted repo should exit 0, got %d (%s)", code, errOut.String())
	}
	var stDrifted map[string]any
	_ = json.Unmarshal(out.Bytes(), &stDrifted)
	if stDrifted["drifted"] != true {
		t.Fatalf("expected drifted=true, got %+v", stDrifted)
	}
	driftedTargets, _ := stDrifted["per_target"].([]any)
	targetStatusMap := make(map[string]map[string]any, len(driftedTargets))
	for _, pt := range driftedTargets {
		m := pt.(map[string]any)
		targetStatusMap[m["target"].(string)] = m
	}
	if targetStatusMap["claude"]["status"] != "in_sync" {
		t.Fatalf("expected unmodified target claude to remain in_sync, got %+v", targetStatusMap["claude"])
	}
	if targetStatusMap["cursor"]["status"] != "drifted" {
		t.Fatalf("expected modified target cursor to report drifted, got %+v", targetStatusMap["cursor"])
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--check", "--targets=cursor"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected status --check --targets=cursor exit 2 on drift, got %d", code)
	}
	// Restore clean state via sync.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync", "--force"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("resync failed: %s", errOut.String())
	}

	// 4. Pending commit journal -> commit_journal_pending: true, --check exits 2.
	journalPath := filepath.Join(proj, ".agentfile", "commit-journal.json")
	_ = os.WriteFile(journalPath, []byte(`{"backup_dir":".agentfile/backups/old"}`), 0o644)
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"status", "--check", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 2 {
		t.Fatalf("expected status --check exit 2 on pending commit journal, got %d", code)
	}
	var stJournal map[string]any
	if err := json.Unmarshal(out.Bytes(), &stJournal); err != nil {
		t.Fatalf("expected valid JSON before exit 2: %v\n%s", err, out.String())
	}
	if stJournal["commit_journal_pending"] != true {
		t.Fatalf("expected commit_journal_pending=true, got %+v", stJournal)
	}
	_ = os.Remove(journalPath)

	// 5. Unhydrated remote dependency -> status reports unhydrated, executes ZERO git calls, touches ZERO .last_used, --check exits 2.
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "k8s"), 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "k8s", "SKILL.md"), []byte("# K8s\n"), 0o644)
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, b)
		}
	}
	runGit(workRepo, "init", "-b", "main")
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "init")
	runGit(tmp, "clone", "--bare", workRepo, bareRepo)

	remoteProj := t.TempDir()
	repoURL := "file://" + filepath.ToSlash(bareRepo)
	_ = os.WriteFile(filepath.Join(remoteProj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\nskills:\n  - name: k8s\n    source: \""+repoURL+"//skills/k8s\"\n"), 0o644)

	// Sync once to create Agentfile.lock and cache entry, then wipe cacheDir to simulate cold cache!
	if code := ExecuteWithIO([]string{"sync"}, remoteProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("remote sync failed: %s", errOut.String())
	}
	coldCache := t.TempDir()
	t.Setenv("AGENTFILE_CACHE_DIR", coldCache)

	// Put a poison git binary first on PATH to prove status executes zero git calls.
	gitPoisonDir := t.TempDir()
	gitMarker := filepath.Join(t.TempDir(), "git-called")
	_ = os.WriteFile(filepath.Join(gitPoisonDir, "git"), []byte("#!/bin/sh\ntouch "+gitMarker+"\nexit 99\n"), 0o755)
	t.Setenv("PATH", gitPoisonDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--json"}, remoteProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("status on unhydrated repo without --check should exit 0, got %d (%s)", code, errOut.String())
	}
	if _, err := os.Stat(gitMarker); err == nil {
		t.Fatalf("status must execute zero git calls")
	}
	if entries, err := os.ReadDir(coldCache); err == nil && len(entries) > 0 {
		t.Fatalf("status must not create cache entries or .last_used files, found: %v", entries)
	}
	var stUnhydrated map[string]any
	if err := json.Unmarshal(out.Bytes(), &stUnhydrated); err != nil {
		t.Fatalf("invalid unhydrated status JSON: %v\n%s", err, out.String())
	}
	uhDeps, _ := stUnhydrated["unhydrated_deps"].([]any)
	if len(uhDeps) != 1 || uhDeps[0] != "skill:k8s" {
		t.Fatalf("expected unhydrated_deps=[skill:k8s], got %v", stUnhydrated["unhydrated_deps"])
	}
	uhTargets := stUnhydrated["per_target"].([]any)
	if uhTargets[0].(map[string]any)["status"] != "unhydrated" {
		t.Fatalf("expected target status=unhydrated, got %+v", uhTargets[0])
	}

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--check", "--json"}, remoteProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected status --check exit 2 on unhydrated dep, got %d", code)
	}

	// Verify warm cache also never updates .last_used on status.
	t.Setenv("AGENTFILE_CACHE_DIR", cacheDir)
	extDir := filepath.Join(cacheDir, "extracted")
	items, _ := os.ReadDir(extDir)
	for _, it := range items {
		if strings.HasSuffix(it.Name(), ".last_used") {
			_ = os.Remove(filepath.Join(extDir, it.Name()))
		}
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--check"}, remoteProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("status --check with warm cache failed (%d): %s", code, errOut.String())
	}
	itemsAfter, _ := os.ReadDir(extDir)
	for _, it := range itemsAfter {
		if strings.HasSuffix(it.Name(), ".last_used") {
			t.Fatalf("status must not touch .last_used, found %s", it.Name())
		}
	}

	// 6. Invalid Agentfile.yaml -> --json still emits valid JSON and exits 2.
	badProj := t.TempDir()
	_ = os.WriteFile(filepath.Join(badProj, "Agentfile.yaml"), []byte("version: 99\ntargets: [claude]\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--json"}, badProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected status --json exit 2 on invalid manifest, got %d", code)
	}
	var stBad map[string]any
	if err := json.Unmarshal(out.Bytes(), &stBad); err != nil {
		t.Fatalf("expected valid JSON on invalid manifest error, got err=%v out=%s", err, out.String())
	}
	if stBad["manifest_valid"] != false {
		t.Fatalf("expected manifest_valid=false, got %+v", stBad)
	}

	// 7. user_config provenance in status --json without binary --version or $HOME scan.
	userCfgDir := t.TempDir()
	userCfgFile := filepath.Join(userCfgDir, "agentfile", "config.yaml")
	_ = os.MkdirAll(filepath.Dir(userCfgFile), 0o755)
	_ = os.WriteFile(userCfgFile, []byte("version: 1\nlink_mode: symlink\nload_global: false\n"), 0o644)
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "")
	t.Setenv("CI", "")
	t.Setenv("XDG_CONFIG_HOME", userCfgDir)

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("status with user_config failed (%d): %s", code, errOut.String())
	}
	var stUser map[string]any
	_ = json.Unmarshal(out.Bytes(), &stUser)
	eff := stUser["effective_config"].(map[string]any)
	srcs := eff["sources"].(map[string]any)
	if srcs["link_mode"] != "user_config" {
		t.Fatalf("expected link_mode source=user_config, got %+v", srcs)
	}
	pt := stUser["per_target"].([]any)[0].(map[string]any)
	if pt["load_global"] != false || pt["load_global_source"] != "user_config" {
		t.Fatalf("expected load_global=false from user_config, got %+v", pt)
	}
	if _, err := os.Stat(claudeProbeMarker); err == nil {
		t.Fatalf("status with user_config must not invoke claude --version")
	}

	// 8. Human output, unresolved conflict, and missing local dependency.
	conflictProj := t.TempDir()
	_ = os.MkdirAll(filepath.Join(conflictProj, ".claude", "rules"), 0o755)
	_ = os.WriteFile(filepath.Join(conflictProj, ".claude", "rules", "agentfile-style.md"), []byte("unmanaged"), 0o644)
	_ = os.MkdirAll(filepath.Join(conflictProj, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(conflictProj, "docs", "RULES.md"), []byte("Rule.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(conflictProj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\nrules:\n  - name: style\n    source: ./docs/RULES.md\n"), 0o644)
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("CI", "true")

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--json"}, conflictProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("status on conflict without --check failed (%d): %s", code, errOut.String())
	}
	var stConflict map[string]any
	_ = json.Unmarshal(out.Bytes(), &stConflict)
	ptConf := stConflict["per_target"].([]any)[0].(map[string]any)
	if ptConf["status"] != "conflict" {
		t.Fatalf("expected target status=conflict, got %+v", ptConf)
	}
	if code := ExecuteWithIO([]string{"status", "--check"}, conflictProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected status --check exit 2 on conflict, got %d", code)
	}

	// Missing local dependency.
	_ = os.Remove(filepath.Join(conflictProj, "docs", "RULES.md"))
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status"}, conflictProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("status human on missing local dep failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Missing local dependencies:") {
		t.Fatalf("expected Missing local dependencies in human status output:\n%s", out.String())
	}
	if code := ExecuteWithIO([]string{"status", "--check"}, conflictProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected status --check exit 2 on missing local dep, got %d", code)
	}
}

func TestDiffOfflineFetchStatNameOnlyCheckAndRedaction(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_ALLOW_FILE_GIT", "1")
	cacheDir := t.TempDir()
	t.Setenv("AGENTFILE_CACHE_DIR", cacheDir)
	t.Setenv("CI", "true")

	// 1. JSONC comment preservation, secret redaction, placeholder preservation, and secret rotation.
	proj := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj, "skills", "auditor"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "skills", "auditor", "SKILL.md"), []byte("# Auditor v1\n"), 0o644)
	// Existing .mcp.json with JSONC comment, literal secret, and rotated secret.
	existingMCP := `{
  "mcpServers": {
    "custom": {
      // custom user server comment
      "command": "node",
      "env": {
        "API_TOKEN": "raw-secret-123"
      }
    },
    "github": {
      "type": "http",
      "url": "https://api.example.com/mcp/",
      "headers": {
        "Authorization": "Bearer old_literal_secret_1"
      }
    }
  }
}
`
	_ = os.WriteFile(filepath.Join(proj, ".mcp.json"), []byte(existingMCP), 0o644)
	// Sync first with a literal secret in manifest (so github is owned and has old_literal_secret_1).
	manifestV1 := `version: 1
targets: [claude]
mcp:
  github:
    transport: http
    url: "https://api.example.com/mcp/"
    headers:
      Authorization: "Bearer old_literal_secret_1"
skills:
  - name: auditor
    source: ./skills/auditor
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifestV1), 0o644)
	var out, errOut bytes.Buffer
	if code := ExecuteWithIO([]string{"sync", "--force"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("initial sync failed (%d): %s", code, errOut.String())
	}

	// Now rotate github Authorization to a new literal secret AND add another server with ${GITHUB_PAT} placeholder.
	manifestV2 := `version: 1
targets: [claude]
mcp:
  github:
    transport: http
    url: "https://api.example.com/mcp/"
    headers:
      Authorization: "Bearer new_literal_secret_2"
  linear:
    transport: http
    url: "https://mcp.linear.app/sse"
    headers:
      Authorization: "Bearer ${LINEAR_PAT}"
skills:
  - name: auditor
    source: ./skills/auditor
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifestV2), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "skills", "auditor", "SKILL.md"), []byte("# Auditor v2\n"), 0o644)

	out.Reset()
	errOut.Reset()
	code := ExecuteWithIO([]string{"diff"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("diff failed (%d): %s", code, errOut.String())
	}
	diffOut := out.String()
	// Preserves JSONC comment.
	if !strings.Contains(diffOut, "// custom user server comment") {
		t.Fatalf("expected JSONC comment preserved in diff:\n%s", diffOut)
	}
	// Redacts all literal secrets (both context and rotated +/- lines).
	for _, leaked := range []string{"raw-secret-123", "old_literal_secret_1", "new_literal_secret_2"} {
		if strings.Contains(diffOut, leaked) {
			t.Fatalf("secret %q leaked in diff output:\n%s", leaked, diffOut)
		}
	}
	// Rotated secret shows - and + [REDACTED] lines!
	if !strings.Contains(diffOut, `-        "Authorization": "Bearer [REDACTED]"`) || !strings.Contains(diffOut, `+        "Authorization": "Bearer [REDACTED]"`) {
		t.Fatalf("expected rotated secret lines with - / + [REDACTED]:\n%s", diffOut)
	}
	// Placeholder ${LINEAR_PAT} preserved unmasked!
	if !strings.Contains(diffOut, `"Authorization": "Bearer ${LINEAR_PAT}"`) {
		t.Fatalf("expected ${LINEAR_PAT} preserved in diff:\n%s", diffOut)
	}
	// Tree diff included.
	if !strings.Contains(diffOut, ".claude/skills/auditor/SKILL.md") || !strings.Contains(diffOut, "+# Auditor v2") {
		t.Fatalf("expected skill tree diff in output:\n%s", diffOut)
	}

	// --name-only
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"diff", "--name-only"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("diff --name-only failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), ".mcp.json") || !strings.Contains(out.String(), ".claude/skills/auditor") {
		t.Fatalf("unexpected --name-only output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "@@ ") {
		t.Fatalf("--name-only must not emit hunks:\n%s", out.String())
	}

	// --stat
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"diff", "--stat"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("diff --stat failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), ".mcp.json") || !strings.Contains(out.String(), "changed") {
		t.Fatalf("unexpected --stat output:\n%s", out.String())
	}

	// --check exits 2 when diff is non-empty.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"diff", "--check"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected diff --check exit 2 when diff non-empty, got %d", code)
	}

	// Symlink diff when link_mode: symlink.
	_ = os.MkdirAll(filepath.Join(proj, ".agentfile"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".agentfile", "config.yaml"), []byte("version: 1\nlink_mode: symlink\non_conflict: overwrite\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"diff"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("diff with symlink mode failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "mode: dir") || !strings.Contains(out.String(), "mode: symlink") {
		t.Fatalf("expected dir->symlink mode change diff:\n%s", out.String())
	}

	// Sync to clean state, then diff --check exits 0.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"diff", "--check"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("expected diff --check exit 0 when clean, got %d (%s)\n%s", code, errOut.String(), out.String())
	}

	// 2. Unhydrated remote dependency: default diff exits 2 without --fetch, succeeds with --fetch without writing Agentfile.lock.
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "k8s"), 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "k8s", "SKILL.md"), []byte("# Remote K8s\n"), 0o644)
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, b)
		}
	}
	runGit(workRepo, "init", "-b", "main")
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "init")
	runGit(tmp, "clone", "--bare", workRepo, bareRepo)

	remoteProj := t.TempDir()
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	repoURL := "file://" + filepath.ToSlash(bareRepo)
	_ = os.WriteFile(filepath.Join(remoteProj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\nskills:\n  - name: k8s\n    source: \""+repoURL+"//skills/k8s\"\n"), 0o644)

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"diff"}, remoteProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected diff without --fetch on unhydrated dep to exit 2, got %d", code)
	}
	if !strings.Contains(errOut.String(), "skill:k8s") || !strings.Contains(errOut.String(), "--fetch") {
		t.Fatalf("expected actionable error naming skill:k8s and --fetch, got %q", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"diff", "--fetch"}, remoteProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("diff --fetch failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "+# Remote K8s") {
		t.Fatalf("expected fetched remote skill diff:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(remoteProj, "Agentfile.lock")); !os.IsNotExist(err) {
		t.Fatalf("diff --fetch must NOT write Agentfile.lock to disk")
	}
	// diff --fetch --check still exits 2 because Agentfile.lock is missing and workspace has diff.
	if code := ExecuteWithIO([]string{"diff", "--fetch", "--check"}, remoteProj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected diff --fetch --check exit 2, got %d", code)
	}

	// 3. Delete paths (ActionDeletePath for file/dir/symlink) and conflicting --stat --name-only.
	if code := ExecuteWithIO([]string{"diff", "--stat", "--name-only"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on --stat --name-only conflict, got %d", code)
	}
	// Remove skill from manifest on synced proj -> diff renders deletion of symlink/tree.
	manifestNoSkill := `version: 1
targets: [claude]
mcp:
  github:
    transport: http
    url: "https://api.example.com/mcp/"
    headers:
      Authorization: "Bearer new_literal_secret_2"
  linear:
    transport: http
    url: "https://mcp.linear.app/sse"
    headers:
      Authorization: "Bearer ${LINEAR_PAT}"
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifestNoSkill), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"diff"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("diff on skill deletion failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "-symlink ->") && !strings.Contains(out.String(), "+++ /dev/null") {
		t.Fatalf("expected deletion diff for removed skill:\n%s", out.String())
	}
}

func TestUserGlobalConfigDoctorRunAndSync(t *testing.T) {
	cacheDir := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("AGENTFILE_CACHE_DIR", cacheDir)
	_ = os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\necho '2.1.278'\n"), 0o755)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Ensure CI env vars don't interfere when testing XDG_CONFIG_HOME.
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("BUILDKITE", "")
	t.Setenv("GITLAB_CI", "")
	t.Setenv("CIRCLECI", "")
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "")
	t.Setenv("AGENTFILE_CONFIG_FILE", "")

	xdgDir := t.TempDir()
	xdgCfg := filepath.Join(xdgDir, "agentfile", "config.yaml")
	_ = os.MkdirAll(filepath.Dir(xdgCfg), 0o755)
	_ = os.WriteFile(xdgCfg, []byte("version: 1\nlink_mode: symlink\non_conflict: backup\nload_global: false\n"), 0o644)
	t.Setenv("XDG_CONFIG_HOME", xdgDir)

	proj := t.TempDir()
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\n"), 0o644)

	var out, errOut bytes.Buffer
	fpr := &fakeRunProc{}

	// 1. doctor --json exposes user_config_path, local_config_path, sources, and emits RUNTIME_ONLY_ISOLATION when load_global: false comes from user_config.
	if code := ExecuteWithIO([]string{"doctor", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("doctor with XDG user config failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	var docMap map[string]any
	if err := json.Unmarshal(out.Bytes(), &docMap); err != nil {
		t.Fatalf("invalid doctor JSON: %v", err)
	}
	eff := docMap["effective_config"].(map[string]any)
	if eff["user_config_path"] != xdgCfg {
		t.Fatalf("expected user_config_path=%q, got %v", xdgCfg, eff["user_config_path"])
	}
	srcs := eff["sources"].(map[string]any)
	if srcs["link_mode"] != "user_config" || srcs["on_conflict"] != "user_config" || srcs["load_global"] != "user_config" {
		t.Fatalf("unexpected sources in doctor report: %+v", srcs)
	}
	warns := docMap["warnings"].([]any)
	foundRuntimeOnly := false
	for _, w := range warns {
		if strings.Contains(w.(string), "RUNTIME_ONLY_ISOLATION") {
			foundRuntimeOnly = true
		}
	}
	if !foundRuntimeOnly {
		t.Fatalf("expected RUNTIME_ONLY_ISOLATION warning in doctor from user_config, got %+v", warns)
	}

	// 2. sync emits RUNTIME_ONLY_ISOLATION when load_global: false comes from user_config.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync with user config failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "RUNTIME_ONLY_ISOLATION") {
		t.Fatalf("expected RUNTIME_ONLY_ISOLATION in sync output from user_config:\n%s", out.String())
	}

	// 3. run --dry-run --json uses user_config load_global=false and source=user_config.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithRunner([]string{"run", "--dry-run", "--json", "claude", "-p", "hi"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 {
		t.Fatalf("run --dry-run failed (%d): %s", code, errOut.String())
	}
	var dryRep RunDryRunReport
	_ = json.Unmarshal(out.Bytes(), &dryRep)
	if dryRep.LoadGlobal || dryRep.LoadGlobalSource != "user_config" || dryRep.IsolationMode != "native_flags" {
		t.Fatalf("expected user_config isolation in run dry-run, got %+v", dryRep)
	}

	// 4. CI=true ignores ambient XDG user config.
	t.Setenv("CI", "true")
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("doctor in CI failed (%d): %s", code, errOut.String())
	}
	var docCI map[string]any
	_ = json.Unmarshal(out.Bytes(), &docCI)
	effCI := docCI["effective_config"].(map[string]any)
	srcsCI := effCI["sources"].(map[string]any)
	if srcsCI["link_mode"] != "default" || srcsCI["load_global"] != "default" {
		t.Fatalf("expected CI to ignore ambient user_config, got sources=%+v", srcsCI)
	}

	// 5. Explicit AGENTFILE_CONFIG_FILE overrides CI skip, and missing AGENTFILE_CONFIG_FILE fails exit 2.
	explicitCfg := filepath.Join(t.TempDir(), "custom.yaml")
	_ = os.WriteFile(explicitCfg, []byte("version: 1\non_conflict: overwrite\nload_global: false\n"), 0o644)
	t.Setenv("AGENTFILE_CONFIG_FILE", explicitCfg)

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync with AGENTFILE_CONFIG_FILE failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "on_conflict=overwrite") || !strings.Contains(out.String(), "RUNTIME_ONLY_ISOLATION") {
		t.Fatalf("expected user_config overwrite warning and RUNTIME_ONLY_ISOLATION:\n%s", out.String())
	}

	t.Setenv("AGENTFILE_CONFIG_FILE", filepath.Join(t.TempDir(), "nonexistent.yaml"))
	for _, cmdArgs := range [][]string{{"doctor"}, {"status"}, {"sync"}, {"run", "--dry-run", "claude"}} {
		out.Reset()
		errOut.Reset()
		if code := ExecuteWithRunner(cmdArgs, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 2 {
			t.Fatalf("expected exit 2 for %v when AGENTFILE_CONFIG_FILE missing, got %d", cmdArgs, code)
		}
	}

	// 6. local_config (.agentfile/config.yaml) load_global: false also triggers RUNTIME_ONLY_ISOLATION.
	t.Setenv("AGENTFILE_CONFIG_FILE", "")
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	_ = os.MkdirAll(filepath.Join(proj, ".agentfile"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".agentfile", "config.yaml"), []byte("version: 1\nload_global: false\n"), 0o644)

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync with local_config load_global=false failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "RUNTIME_ONLY_ISOLATION") {
		t.Fatalf("expected RUNTIME_ONLY_ISOLATION from local_config in sync:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("doctor with local_config load_global=false failed (%d): %s", code, errOut.String())
	}
	var docLocal map[string]any
	_ = json.Unmarshal(out.Bytes(), &docLocal)
	if docLocal["targets"].([]any)[0].(map[string]any)["load_global_source"] != "local_config" {
		t.Fatalf("expected load_global_source=local_config in doctor, got %+v", docLocal["targets"])
	}
	foundLocalRuntime := false
	for _, w := range docLocal["warnings"].([]any) {
		if strings.Contains(w.(string), "RUNTIME_ONLY_ISOLATION") {
			foundLocalRuntime = true
		}
	}
	if !foundLocalRuntime {
		t.Fatalf("expected RUNTIME_ONLY_ISOLATION from local_config in doctor, got %+v", docLocal["warnings"])
	}

	// 7. Verify redactEnvValue masks secret keys/tokens while preserving placeholders.
	if got := redactEnvValue("GITHUB_TOKEN", "ghp_secret123456"); got != "[REDACTED]" {
		t.Fatalf("expected GITHUB_TOKEN redacted, got %q", got)
	}
	if got := redactEnvValue("GITHUB_TOKEN", "${GITHUB_TOKEN}"); got != "${GITHUB_TOKEN}" {
		t.Fatalf("expected placeholder preserved, got %q", got)
	}
	if got := redactEnvValue("OPENCODE_CONFIG_DIR", "/tmp/empty-cfg"); got != "/tmp/empty-cfg" {
		t.Fatalf("expected non-secret env preserved, got %q", got)
	}
}

func TestSyncDryRunJSONWarningRedaction(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	proj := t.TempDir()
	manifest := `version: 1
targets: [cursor]
mcp:
  api:
    transport: http
    url: "https://api.example.com/mcp"
    headers:
      Authorization: "Bearer ${TOKEN:-ghp_secret123456}"
settings:
  hooks:
    PostToolUse:
      - matcher: "Bash"
        command: "curl -H 'Authorization: Bearer raw-hook-secret-999' https://example.com"
targets_config:
  cursor:
    on_unsupported: warn
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifest), 0o644)

	var out, errOut bytes.Buffer
	if code := ExecuteWithIO([]string{"sync", "--dry-run", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync --dry-run --json failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	rawJSON := out.String()
	for _, leaked := range []string{"ghp_secret123456", "raw-hook-secret-999"} {
		if strings.Contains(rawJSON, leaked) {
			t.Fatalf("secret %q leaked in sync --dry-run --json output:\n%s", leaked, rawJSON)
		}
	}
	var sum map[string]any
	if err := json.Unmarshal(out.Bytes(), &sum); err != nil {
		t.Fatalf("invalid dry-run JSON: %v\n%s", err, rawJSON)
	}
	warns, _ := sum["warnings"].([]any)
	if len(warns) < 2 {
		t.Fatalf("expected redacted warnings in dry-run JSON, got %+v", warns)
	}
	joined := ""
	for _, w := range warns {
		joined += w.(string) + "\n"
	}
	if !strings.Contains(joined, "${TOKEN:-[REDACTED]}") || !strings.Contains(joined, "Bearer [REDACTED]") {
		t.Fatalf("expected redacted placeholders/headers in warnings:\n%s", joined)
	}
}
