package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/diff"
	"github.com/bettertux007/agentfile/internal/engine"
	"github.com/bettertux007/agentfile/internal/runner"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/schemas"
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
	writeTestVersionStub(binDir, "claude", "claude 1.2.3")
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
	fakeClaude := writeTestVersionStub(binDir, "claude", "2.1.278 (Claude Code)")
	_ = fakeClaude
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
	writeTestVersionStub(binDir, "opencode", "opencode v2.3.0")
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
	if runtime.GOOS == "windows" {
		stub := filepath.Join(binDir, "claude.bat")
		bat := "@echo off\r\nif \"%~1\"==\"--version\" (\r\n  echo 2.1.278\r\n  exit /b 0\r\n)\r\necho ACTIVE=%AGENTFILE_ACTIVE% ARGS=%*\r\nexit /b 7\r\n"
		_ = os.WriteFile(stub, []byte(bat), 0o755)
	} else {
		stub := filepath.Join(binDir, "claude")
		script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '2.1.278'; exit 0; fi\necho \"ACTIVE=$AGENTFILE_ACTIVE ARGS=$*\"\nexit 7\n"
		_ = os.WriteFile(stub, []byte(script), 0o755)
	}

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
	writeTestVersionStub(binDir, "claude", "2.1.278")
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
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("BUILDKITE", "")
	t.Setenv("GITLAB_CI", "")
	t.Setenv("CIRCLECI", "")
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
	writeTestVersionStub(binDir, "claude", "2.1.278")
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

func TestAddRemoveUpgradeEndToEnd(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_ALLOW_FILE_GIT", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	// Set up bare Git repository with pinned tag v1.0.0 and floating main branch.
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "k8s"), 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "k8s", "SKILL.md"), []byte("# K8s v1\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "ops"), 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "ops", "SKILL.md"), []byte("# Ops v1\n"), 0o644)

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	runGit(workRepo, "init", "-b", "main")
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "v1")
	v1SHA := runGit(workRepo, "rev-parse", "HEAD")
	runGit(workRepo, "tag", "-a", "v1.0.0", "-m", "v1.0.0")
	runGit(tmp, "clone", "--bare", workRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)

	proj := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj, "skills", "local-skill"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "skills", "local-skill", "SKILL.md"), []byte("# Local Skill\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(proj, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "agents", "reviewer.md"), []byte("# Reviewer\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "docs", "RULES.md"), []byte("Keep code clean.\n"), 0o644)

	// Fail closed when Agentfile.yaml is missing.
	var out, errOut bytes.Buffer
	if code := ExecuteWithIO([]string{"add", "skill", "s", "./skills/local-skill"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 when Agentfile.yaml missing on add, got %d", code)
	}
	if code := ExecuteWithIO([]string{"remove", "skill", "s"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 when Agentfile.yaml missing on remove, got %d", code)
	}

	// Hand-commented Agentfile.yaml.
	initialYAML := `# Top manifest comment
version: 1
targets:
  - claude # keep target comment
  - cursor
# Skills block comment
skills:
  - name: k8s # pinned skill
    source: "` + repoURL + `#v1.0.0//skills/k8s"
`
	manifestPath := filepath.Join(proj, "Agentfile.yaml")
	if err := os.WriteFile(manifestPath, []byte(initialYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Initial sync locks skill:k8s.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("initial sync failed (%d): %s", code, errOut.String())
	}

	// 1. add skill (floating branch), add agent, add rule preserving comments & formatting.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "ops", repoURL + "#main//skills/ops"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("add skill ops failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "agent", "reviewer", "./agents/reviewer.md", "--description", "Code reviewer", "--model", "sonnet"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("add agent failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "rule", "style", "./docs/RULES.md", "--description", "Style", "--globs", "**/*.go", "--always-apply"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("add rule failed (%d): %s", code, errOut.String())
	}

	// 2. add mcp: transport inference + colon splitting + --env-ref + --header-env.
	// Conflicting or missing transport args -> exit 2.
	badMCPCalls := [][]string{
		{"add", "mcp", "bad1"},
		{"add", "mcp", "bad2", "--url", "https://api.example.com/mcp", "--", "npx", "mcp"},
		{"add", "mcp", "bad3", "--env", "KEY="},
		{"add", "mcp", "bad4", "--url", "https://api.example.com/mcp", "--header", "Authorization:"},
		{"add", "mcp", "bad5", "--env", "TOKEN=ghp_1234567890abcdef", "--", "npx", "mcp"},
		{"add", "mcp", "bad6", "--url", "https://api.example.com/mcp", "--header", "Authorization: Bearer rawtok"},
		{"add", "mcp", "bad7", "--url", "https://u:pass@host/mcp"},
		{"add", "mcp", "bad8", "--env-ref", "FOO:-ghp_1234567890abcdef", "--", "npx", "mcp"},
		{"add", "mcp", "bad9", "--env-ref", "FOO}bar", "--", "npx", "mcp"},
		{"add", "mcp", "bad10", "--url", "https://api.example.com/mcp", "--header-env", "TOKEN:-ghp_1234567890abcdef:Authorization"},
		{"add", "mcp", "bad11", "--url", "https://api.example.com/mcp", "--header-env", "FOO}bar:Authorization"},
	}
	for _, args := range badMCPCalls {
		out.Reset()
		errOut.Reset()
		if code := ExecuteWithIO(args, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
			t.Fatalf("expected exit 2 for %v, got %d (err=%s)", args, code, errOut.String())
		}
	}

	// Valid http MCP with inferred transport, --header colon splitting, --header-env Authorization & custom header.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{
		"add", "mcp", "github",
		"--url", "https://api.githubcopilot.com/mcp/",
		"--header", "X-Custom: a:b",
		"--header-env", "GITHUB_PAT:Authorization",
		"--header-env", "API_KEY:X-API-Key",
	}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("add mcp github failed (%d): %s", code, errOut.String())
	}

	// Valid stdio MCP with inferred transport and --env-ref.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{
		"add", "mcp", "fs",
		"--env", "LOG_LEVEL=info",
		"--env-ref", "FOO",
		"--", "npx", "-y", "@mcp/fs",
	}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("add mcp fs failed (%d): %s", code, errOut.String())
	}

	// Verify comments preserved and MCP entries lowered/written correctly.
	manifestAfterAdd, _ := os.ReadFile(manifestPath)
	mStr := string(manifestAfterAdd)
	for _, want := range []string{
		"# Top manifest comment",
		"- claude # keep target comment",
		"# Skills block comment",
		"- name: k8s # pinned skill",
		"transport: http",
		"X-Custom: a:b",
		"Authorization: Bearer ${GITHUB_PAT}",
		"X-API-Key: ${API_KEY}",
		"transport: stdio",
		"FOO: ${FOO}",
		"LOG_LEVEL: info",
	} {
		if !strings.Contains(mStr, want) {
			t.Fatalf("expected %q in Agentfile.yaml:\n%s", want, mStr)
		}
	}

	// Duplicate add without --replace fails exit 2; with --replace updates in place.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "ops", "./skills/local-skill"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on duplicate add without --replace, got %d", code)
	}
	// Replace local skill -> add local-skill with --no-sync, then replace it and sync.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "local-skill", "./skills/local-skill", "--no-sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("add skill --no-sync failed (%d): %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "skills", "local-skill")); !os.IsNotExist(err) {
		t.Fatalf("add --no-sync must not materialize workspace files")
	}
	// External local path fails without --allow-external-local and succeeds with it (--no-sync).
	extSkill := filepath.Join(t.TempDir(), "ext-skill")
	_ = os.MkdirAll(extSkill, 0o755)
	_ = os.WriteFile(filepath.Join(extSkill, "SKILL.md"), []byte("# Ext\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "local-skill", extSkill, "--replace", "--no-sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 for external local path without --allow-external-local, got %d", code)
	}
	// Replace local-skill back to ./skills/local-skill with sync.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "local-skill", "./skills/local-skill", "--replace"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("add skill --replace failed (%d): %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "skills", "local-skill", "SKILL.md")); err != nil {
		t.Fatalf("expected replaced skill materialized: %v", err)
	}
	// Remove local-skill so remaining assertions stay clean.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"remove", "skill", "local-skill"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("remove local-skill failed (%d): %s", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "mcp", "fs", "--replace", "--env-ref", "BAR", "--", "node", "server.js"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("add mcp --replace failed (%d): %s", code, errOut.String())
	}
	manifestReplaced, _ := os.ReadFile(manifestPath)
	if !strings.Contains(string(manifestReplaced), "command: node") || !strings.Contains(string(manifestReplaced), "BAR: ${BAR}") {
		t.Fatalf("expected replaced mcp fs:\n%s", manifestReplaced)
	}

	// 3. Pre-write safety: unreachable Git ref leaves Agentfile.yaml and Agentfile.lock untouched.
	beforeFailManifest, _ := os.ReadFile(manifestPath)
	beforeFailLock, _ := os.ReadFile(filepath.Join(proj, "Agentfile.lock"))
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "broken", repoURL + "#nonexistent-ref//skills/k8s"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 && code != 1 {
		t.Fatalf("expected non-zero exit on unreachable ref, got %d", code)
	}
	afterFailManifest, _ := os.ReadFile(manifestPath)
	afterFailLock, _ := os.ReadFile(filepath.Join(proj, "Agentfile.lock"))
	if string(beforeFailManifest) != string(afterFailManifest) || string(beforeFailLock) != string(afterFailLock) {
		t.Fatalf("failed add mutated Agentfile.yaml or Agentfile.lock on disk")
	}

	// 4. Push new commit to main in bareRepo and test `upgrade` + `sync --update`.
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "ops", "SKILL.md"), []byte("# Ops v2\n"), 0o644)
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "v2")
	v2SHA := runGit(workRepo, "rev-parse", "HEAD")
	runGit(workRepo, "push", bareRepo, "main")

	// upgrade --dry-run --json reports pinned k8s unchanged and ops updated without modifying lockfile.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"upgrade", "--dry-run", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("upgrade --dry-run --json failed (%d): %s", code, errOut.String())
	}
	var upDry UpgradeReport
	if err := json.Unmarshal(out.Bytes(), &upDry); err != nil {
		t.Fatalf("invalid UpgradeReport JSON: %v\n%s", err, out.String())
	}
	if len(upDry.Deltas) != 2 {
		t.Fatalf("expected 2 deltas, got %+v", upDry.Deltas)
	}
	lockDuringDry, _ := os.ReadFile(filepath.Join(proj, "Agentfile.lock"))
	if !strings.Contains(string(lockDuringDry), v1SHA) || strings.Contains(string(lockDuringDry), v2SHA) {
		t.Fatalf("upgrade --dry-run must not mutate Agentfile.lock")
	}

	// Human upgrade on all dependencies prints pinned vs updated LockDelta lines.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"upgrade"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("upgrade failed (%d): %s", code, errOut.String())
	}
	upOut := out.String()
	if !strings.Contains(upOut, "unchanged (pinned to v1.0.0)") || !strings.Contains(upOut, v1SHA+".."+v2SHA) {
		t.Fatalf("expected pinned and SHA delta lines in upgrade output:\n%s", upOut)
	}

	// Push v3 and test `upgrade skill ops` (2-arg form) and `sync --update=skill:ops --json`.
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "ops", "SKILL.md"), []byte("# Ops v3\n"), 0o644)
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "v3")
	v3SHA := runGit(workRepo, "rev-parse", "HEAD")
	runGit(workRepo, "push", bareRepo, "main")

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"upgrade", "skill", "ops"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("upgrade skill ops failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), v2SHA+".."+v3SHA) {
		t.Fatalf("expected %s..%s in two-arg upgrade output:\n%s", v2SHA, v3SHA, out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"upgrade", "skill:ops"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("upgrade skill:ops failed (%d): %s", code, errOut.String())
	}

	// Push v4 and test `sync --update=skill:ops` human and JSON deltas.
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "ops", "SKILL.md"), []byte("# Ops v4\n"), 0o644)
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "v4")
	v4SHA := runGit(workRepo, "rev-parse", "HEAD")
	runGit(workRepo, "push", bareRepo, "main")

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync", "--update=skill:ops", "--dry-run", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync --update --dry-run --json failed (%d): %s", code, errOut.String())
	}
	var syncDryJSON map[string]any
	if err := json.Unmarshal(out.Bytes(), &syncDryJSON); err != nil {
		t.Fatalf("invalid sync --update JSON: %v\n%s", err, out.String())
	}
	if _, ok := syncDryJSON["deltas"]; !ok {
		t.Fatalf("expected deltas field in sync --update --json: %+v", syncDryJSON)
	}

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync", "--update=skill:ops"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync --update failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), v3SHA+".."+v4SHA) {
		t.Fatalf("expected LockDelta line in sync --update human output:\n%s", out.String())
	}

	// Invalid upgrade invocations exit 2.
	for _, badUp := range [][]string{
		{"upgrade", "a", "b", "c"},
		{"upgrade", "badformat"},
		{"upgrade", "skill:nonexistent"},
		{"upgrade", "mcp:github"},
	} {
		out.Reset()
		errOut.Reset()
		if code := ExecuteWithIO(badUp, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
			t.Fatalf("expected exit 2 for %v, got %d", badUp, code)
		}
	}

	// 5. remove --no-sync updates only Agentfile.yaml (lockfile & workspace untouched).
	lockBeforeNoSync, _ := os.ReadFile(filepath.Join(proj, "Agentfile.lock"))
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"remove", "agent", "reviewer", "--no-sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("remove --no-sync failed (%d): %s", code, errOut.String())
	}
	lockAfterNoSync, _ := os.ReadFile(filepath.Join(proj, "Agentfile.lock"))
	if string(lockBeforeNoSync) != string(lockAfterNoSync) {
		t.Fatalf("remove --no-sync must not modify Agentfile.lock")
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "agents", "reviewer.md")); err != nil {
		t.Fatalf("remove --no-sync must leave workspace artifact untouched until sync: %v", err)
	}

	// remove with sync prunes workspace + removes key from Agentfile.lock so next sync exits 0.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"remove", "skill", "k8s"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("remove skill k8s failed (%d): %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "skills", "k8s")); !os.IsNotExist(err) {
		t.Fatalf("expected .claude/skills/k8s pruned after remove")
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "agents", "reviewer.md")); !os.IsNotExist(err) {
		t.Fatalf("expected previously removed agent pruned after sync")
	}
	lockAfterRemove, _ := os.ReadFile(filepath.Join(proj, "Agentfile.lock"))
	if strings.Contains(string(lockAfterRemove), "skill:k8s") {
		t.Fatalf("expected skill:k8s removed from Agentfile.lock:\n%s", lockAfterRemove)
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("subsequent sync after remove failed (%d): %s", code, errOut.String())
	}

	// Remove last remote dependency skill:ops -> Agentfile.lock reconciled to empty and status --check exits 0.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"remove", "skill", "ops"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("remove last remote skill ops failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--check"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("status --check after removing all remote deps failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	// upgrade with 0 remote deps reports cleanly.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"upgrade"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("upgrade on local-only repo failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "No remote dependencies to upgrade.") {
		t.Fatalf("expected 'No remote dependencies to upgrade.', got:\n%s", out.String())
	}

	// remove missing entry exits 2.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"remove", "skill", "does-not-exist"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 removing missing entry, got %d", code)
	}
}

func TestAddRemoveDriftAndRollbackSafety(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_ALLOW_FILE_GIT", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "locked"), 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "locked", "SKILL.md"), []byte("# Locked\n"), 0o644)

	runGit := func(dir string, args ...string) {
		t.Helper()
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
	runGit(workRepo, "commit", "-m", "v1")
	runGit(workRepo, "tag", "-a", "v1.0.0", "-m", "v1")
	runGit(tmp, "clone", "--bare", workRepo, bareRepo)
	repoURL := "file://" + filepath.ToSlash(bareRepo)

	proj := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj, "skills", "local"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "skills", "local", "SKILL.md"), []byte("# Local\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(proj, "skills", "extra"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "skills", "extra", "SKILL.md"), []byte("# Extra\n"), 0o644)

	manifest := "version: 1\ntargets: [claude]\nskills:\n  - name: locked\n    source: \"" + repoURL + "#v1.0.0//skills/locked\"\n  - name: local\n    source: ./skills/local\n"
	manifestPath := filepath.Join(proj, "Agentfile.yaml")
	_ = os.WriteFile(manifestPath, []byte(manifest), 0o644)

	var out, errOut bytes.Buffer
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync failed (%d): %s", code, errOut.String())
	}

	// 1. Failing BuildPlan (conflict on disk with on_conflict=fail) leaves Agentfile.yaml and Agentfile.lock untouched.
	origYAML, _ := os.ReadFile(manifestPath)
	origLock, _ := os.ReadFile(filepath.Join(proj, "Agentfile.lock"))
	conflictDir := filepath.Join(proj, ".claude", "skills", "extra")
	_ = os.MkdirAll(conflictDir, 0o755)
	_ = os.WriteFile(filepath.Join(conflictDir, "UNMANAGED.md"), []byte("user"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "extra", "./skills/extra", "--on-conflict=fail"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on BuildPlan conflict failure, got %d", code)
	}
	afterConflictYAML, _ := os.ReadFile(manifestPath)
	afterConflictLock, _ := os.ReadFile(filepath.Join(proj, "Agentfile.lock"))
	if string(afterConflictYAML) != string(origYAML) || string(afterConflictLock) != string(origLock) {
		t.Fatalf("expected Agentfile.yaml and Agentfile.lock untouched on BuildPlan failure")
	}
	_ = os.RemoveAll(conflictDir)

	// 2. Mid-commit failure rolls back Agentfile.yaml + Agentfile.lock + workspace atomically.
	if os.Geteuid() != 0 && runtime.GOOS != "windows" {
		skillsDir := filepath.Join(proj, ".claude", "skills")
		if err := os.Chmod(skillsDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })

		out.Reset()
		errOut.Reset()
		if code := ExecuteWithIO([]string{"add", "skill", "extra", "./skills/extra"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code == 0 {
			t.Fatalf("expected commit failure when .claude/skills is read-only")
		}
		if err := os.Chmod(skillsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		afterRollbackYAML, _ := os.ReadFile(manifestPath)
		afterRollbackLock, _ := os.ReadFile(filepath.Join(proj, "Agentfile.lock"))
		if string(afterRollbackYAML) != string(origYAML) || string(afterRollbackLock) != string(origLock) {
			t.Fatalf("expected Agentfile.yaml and Agentfile.lock rolled back on commit failure")
		}
	}

	// 3. Unrelated locked dependency drift in Agentfile.yaml causes add and remove to fail exit 2 without mutating disk.
	driftedManifest := strings.ReplaceAll(manifest, "#v1.0.0//", "#v2.0.0//")
	_ = os.WriteFile(manifestPath, []byte(driftedManifest), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "extra", "./skills/extra"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on add when unrelated locked dep drifted, got %d (%s)", code, errOut.String())
	}
	afterDriftAdd, _ := os.ReadFile(manifestPath)
	if string(afterDriftAdd) != driftedManifest {
		t.Fatalf("expected Agentfile.yaml untouched when add failed due to unrelated lock drift")
	}

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"remove", "skill", "local"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on remove when unrelated locked dep drifted, got %d (%s)", code, errOut.String())
	}
	afterDriftRemove, _ := os.ReadFile(manifestPath)
	if string(afterDriftRemove) != driftedManifest {
		t.Fatalf("expected Agentfile.yaml untouched when remove failed due to unrelated lock drift")
	}
}

func TestDiffRendererSingleChunkTruncation(t *testing.T) {
	t.Parallel()
	const truncNotice = "... diff truncated (exceeded 10 MiB total diff limit)\n"
	const testLimit = 1024

	if def := (&diffRenderer{}).maxTotalBytes(); def != diff.MaxTotalDiffBytes {
		t.Fatalf("expected default maxTotalBytes=%d, got %d", diff.MaxTotalDiffBytes, def)
	}

	t.Run("preserves single DiffTrees chunk slightly over limit", func(t *testing.T) {
		r := diffRenderer{maxBytes: testLimit}
		header := "--- /dev/null\n+++ b/.claude/skills/big/SKILL.md\n@@ -0,0 +1,16 @@\n"
		line := "+" + strings.Repeat("a", 62) + "\n" // 64 bytes
		var b strings.Builder
		b.WriteString(header)
		for b.Len()+len(line) <= testLimit-10 {
			b.WriteString(line)
		}
		if rem := (testLimit - 10) - b.Len(); rem > 2 {
			b.WriteString("+" + strings.Repeat("a", rem-2) + "\n")
		}
		b.WriteString(truncNotice)
		chunk := b.String()
		if len(chunk) <= testLimit {
			t.Fatalf("test setup expected chunk > testLimit (%d), got %d", testLimit, len(chunk))
		}

		r.appendRendered(chunk, ".claude/skills/big", true)
		r.appendRendered("--- /dev/null\n+++ b/extra.txt\n@@ -0,0 +1,1 @@\n+extra\n", "extra.txt", false)

		got := r.diffOut.String()
		if !r.hasDiff || !r.truncated {
			t.Fatalf("expected hasDiff=true and truncated=true, got hasDiff=%v truncated=%v", r.hasDiff, r.truncated)
		}
		if !strings.Contains(got, "--- /dev/null\n+++ b/.claude/skills/big/SKILL.md\n") {
			t.Fatalf("expected single DiffTrees hunks preserved, got len=%d", len(got))
		}
		if strings.Count(got, truncNotice) != 1 {
			t.Fatalf("expected exactly 1 truncation notice, got %d", strings.Count(got, truncNotice))
		}
		if strings.Contains(got, "extra.txt") {
			t.Fatalf("expected subsequent chunk suppressed after truncation")
		}
		if len(r.fileStats) != 2 {
			t.Fatalf("expected stats to still record all changed files (2), got %d", len(r.fileStats))
		}
	})

	t.Run("caps raw single chunk exceeding limit", func(t *testing.T) {
		r := diffRenderer{maxBytes: testLimit}
		header := "--- /dev/null\n+++ b/raw.txt\n@@ -0,0 +1,20 @@\n"
		line := "+" + strings.Repeat("b", 62) + "\n"
		var b strings.Builder
		b.WriteString(header)
		for b.Len() <= testLimit+256 {
			b.WriteString(line)
		}

		r.appendRendered(b.String(), "raw.txt", false)
		r.appendRendered("--- /dev/null\n+++ b/later.txt\n@@ -0,0 +1,1 @@\n+later\n", "later.txt", false)

		got := r.diffOut.String()
		if !r.truncated {
			t.Fatalf("expected truncated=true")
		}
		if !strings.HasSuffix(got, truncNotice) || strings.Count(got, truncNotice) != 1 {
			t.Fatalf("expected single truncation notice suffix, got count=%d", strings.Count(got, truncNotice))
		}
		if len(got) > testLimit+len(truncNotice) {
			t.Fatalf("expected capped output <= testLimit+notice, got %d", len(got))
		}
		if !strings.Contains(got, "+++ b/raw.txt") || strings.Contains(got, "later.txt") {
			t.Fatalf("unexpected rendered diff bounds")
		}
	})

	t.Run("truncates on subsequent chunk exceeding limit", func(t *testing.T) {
		r := diffRenderer{maxBytes: testLimit}
		first := "--- /dev/null\n+++ b/first.txt\n@@ -0,0 +1,1 @@\n+hello\n"
		r.appendRendered(first, "first.txt", false)

		line := "+" + strings.Repeat("c", 62) + "\n"
		var b strings.Builder
		b.WriteString("--- /dev/null\n+++ b/second.txt\n@@ -0,0 +1,20 @@\n")
		for b.Len() <= testLimit {
			b.WriteString(line)
		}
		r.appendRendered(b.String(), "second.txt", false)

		got := r.diffOut.String()
		if !r.truncated {
			t.Fatalf("expected truncated=true on second chunk")
		}
		if !strings.Contains(got, "+++ b/first.txt") || strings.Contains(got, "+++ b/second.txt") {
			t.Fatalf("expected first chunk kept and second chunk excluded")
		}
		if strings.Count(got, truncNotice) != 1 {
			t.Fatalf("expected 1 truncation notice, got %d", strings.Count(got, truncNotice))
		}
	})
}

func TestStatusSharedArtifactAttributionAndVerifyOnDisk(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_ALLOW_FILE_GIT", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skill-src")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Auditor\n"), 0o644)

	cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: false}
	sharedOps := []*adapters.CompileResult{
		{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("Shared rule")},
				{Target: "claude", RelPath: ".agents/skills/auditor", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir},
			},
		},
		{
			Target: "cursor",
			Files: []adapters.ArtifactOp{
				{Target: "cursor", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("Shared rule")},
				{Target: "cursor", RelPath: ".agents/skills/auditor", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir},
			},
		},
	}

	// 1. Pre-existing unmanaged .agents/skills/auditor causes shared conflict attributed to both claude & cursor.
	unmanaged := filepath.Join(dir, ".agents", "skills", "auditor")
	_ = os.MkdirAll(unmanaged, 0o755)
	_ = os.WriteFile(filepath.Join(unmanaged, "SKILL.md"), []byte("user"), 0o644)

	planConf, err := engine.BuildPlan(engine.PlanInput{
		ProjectRoot:      dir,
		ActiveTargets:    []string{"claude", "cursor"},
		Config:           cfg,
		CompiledResults:  sharedOps,
		InspectConflicts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reports := []StatusTargetReport{
		{Target: "claude", Status: "not_synced"},
		{Target: "cursor", Status: "not_synced"},
	}
	applyPlanToTargetReports(reports, []string{"claude", "cursor"}, planConf, &engine.State{Version: engine.StateVersion, Targets: map[string]*engine.TargetState{}}, false, false)
	for _, r := range reports {
		if r.Status != "conflict" || len(r.Conflicts) != 1 || r.Conflicts[0].Target != "shared" {
			t.Fatalf("expected shared conflict attributed to %s, got %+v", r.Target, r)
		}
		if !slices.Contains(r.DriftedPaths, "AGENTS.md") {
			t.Fatalf("expected shared Action AGENTS.md attributed as drift for %s, got %+v", r.Target, r.DriftedPaths)
		}
	}

	// 2. Sync with overwrite, verify collectStatePaths includes Shared, and status attribution is in_sync.
	_ = os.RemoveAll(unmanaged)
	planSync, err := engine.BuildPlan(engine.PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"claude", "cursor"},
		Config:          cfg,
		CompiledResults: sharedOps,
	})
	if err != nil || engine.Commit(planSync) != nil {
		t.Fatalf("sync commit failed: %v", err)
	}
	st, err := engine.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	statePaths := collectStatePaths(st)
	if !slices.Contains(statePaths, "AGENTS.md") || !slices.Contains(statePaths, ".agents/skills/auditor") {
		t.Fatalf("expected collectStatePaths to include Shared paths, got %v", statePaths)
	}

	planClean, err := engine.BuildPlan(engine.PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"claude", "cursor"},
		Config:          cfg,
		CompiledResults: sharedOps,
	})
	if err != nil {
		t.Fatal(err)
	}
	reportsClean := []StatusTargetReport{
		{Target: "claude", Status: "not_synced"},
		{Target: "cursor", Status: "not_synced"},
	}
	applyPlanToTargetReports(reportsClean, []string{"claude", "cursor"}, planClean, st, true, false)
	for _, r := range reportsClean {
		if r.Status != "in_sync" || len(r.DriftedPaths) != 0 {
			t.Fatalf("expected in_sync for %s, got %+v", r.Target, r)
		}
	}

	// 3. Shared PruneOps (both targets stop emitting) attributes drift to both claude and cursor.
	planPrune, err := engine.BuildPlan(engine.PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude", "cursor"},
		Config:        cfg,
		CompiledResults: []*adapters.CompileResult{
			{Target: "claude", Files: nil},
			{Target: "cursor", Files: nil},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reportsPrune := []StatusTargetReport{
		{Target: "claude", Status: "not_synced"},
		{Target: "cursor", Status: "not_synced"},
	}
	applyPlanToTargetReports(reportsPrune, []string{"claude", "cursor"}, planPrune, st, true, false)
	for _, r := range reportsPrune {
		if r.Status != "drifted" || !slices.Contains(r.DriftedPaths, "AGENTS.md") || !slices.Contains(r.DriftedPaths, ".agents/skills/auditor") {
			t.Fatalf("expected shared PruneOps drift for %s, got %+v", r.Target, r)
		}
	}

	// 4.verifyTargetStateOnDisk detects shared disk drift for every owning target (including unhydrated status path).
	_ = os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("tampered\n"), 0o644)
	for _, tgt := range []string{"claude", "cursor"} {
		dpaths := verifyTargetStateOnDisk(dir, st, tgt)
		if !slices.Contains(dpaths, "AGENTS.md") {
			t.Fatalf("expected verifyTargetStateOnDisk(%s) to detect tampered shared AGENTS.md, got %v", tgt, dpaths)
		}
	}
}

func TestDoctorTOMLGlobalMCPShadowingAndInitDetection(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	// 1. Seed global ~/.codex/config.toml with a shadowing [mcp_servers.github] entry.
	_ = os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(homeDir, ".codex", "config.toml"), []byte("[mcp_servers.github]\nurl = \"https://example.com/mcp\"\n"), 0o644)

	// Stub codex, copilot, and windsurf on isolated PATH to test init detection.
	binDir := t.TempDir()
	for _, bin := range []string{"codex", "copilot", "windsurf"} {
		writeTestVersionStub(binDir, bin, bin+" 1.0.0")
	}
	t.Setenv("PATH", binDir)

	proj := t.TempDir()
	var out, errOut bytes.Buffer
	if code := ExecuteWithIO([]string{"init", "--minimal"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("init failed (%d): %s", code, errOut.String())
	}
	initManifest, _ := os.ReadFile(filepath.Join(proj, "Agentfile.yaml"))
	for _, tgt := range []string{"codex", "copilot", "windsurf"} {
		if !strings.Contains(string(initManifest), "- "+tgt) {
			t.Fatalf("expected detected target %q in Agentfile.yaml:\n%s", tgt, initManifest)
		}
	}

	// 2. Write manifest declaring codex, copilot, windsurf + github MCP and verify doctor catches TOML shadowing.
	_ = os.MkdirAll(filepath.Join(proj, "skills", "k8s"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "skills", "k8s", "SKILL.md"), []byte("# K8s\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "docs", "RULES.md"), []byte("Run tests.\n"), 0o644)

	manifest := `version: 1
targets: [codex, copilot, windsurf]
mcp:
  github:
    transport: http
    url: "https://api.example.com/mcp"
skills:
  - name: k8s
    source: ./skills/k8s
rules:
  - name: style
    source: ./docs/RULES.md
    globs: ["**/*.go"]
targets_config:
  copilot:
    rules_mode: agents_md
  windsurf:
    rules_mode: agents_md
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifest), 0o644)

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("3-target sync failed (%d): %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(proj, ".codex", "config.toml")); err != nil {
		t.Fatalf("expected .codex/config.toml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".vscode", "mcp.json")); err != nil {
		t.Fatalf("expected .vscode/mcp.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, "AGENTS.md")); err != nil {
		t.Fatalf("expected shared AGENTS.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".agents", "skills", "k8s", "SKILL.md")); err != nil {
		t.Fatalf("expected shared .agents/skills/k8s/SKILL.md: %v", err)
	}

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"doctor", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("doctor --json failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	var docRep DoctorReport
	if err := json.Unmarshal(out.Bytes(), &docRep); err != nil {
		t.Fatalf("invalid doctor JSON: %v\n%s", err, out.String())
	}
	if len(docRep.GlobalConflicts) != 1 || docRep.GlobalConflicts[0].ServerName != "github" || !strings.HasSuffix(docRep.GlobalConflicts[0].GlobalFile, filepath.Join(".codex", "config.toml")) {
		t.Fatalf("expected global TOML MCP conflict for github in .codex/config.toml, got %+v", docRep.GlobalConflicts)
	}
}

func TestInitImportEndToEnd(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	proj := t.TempDir()

	// Initialize git repo to verify vcs.SyncExcludes leaves .agentfile/imported/ trackable while excluding state.json.
	gitInit := exec.Command("git", "init")
	gitInit.Dir = proj
	if b, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, b)
	}

	// 1. Seed brownfield repo across all 7 tools.
	// MCP: .mcp.json (claude), opencode.json (opencode), .vscode/mcp.json (copilot), .codex/config.toml (codex),
	// plus .gemini/settings.json (gemini) and .cursor/mcp.json (cursor) for complete coverage.
	_ = os.WriteFile(filepath.Join(proj, ".mcp.json"), []byte(`{
  // comment in jsonc
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": {
        "Authorization": "Bearer ${GITHUB_PAT}",
        "X-Api-Key": "${API_KEY}"
      }
    },
    "fs": {
      "command": "npx",
      "args": ["-y", "@mcp/fs", "."],
      "env": {
        "FS_TOKEN": "${FS_TOKEN}"
      }
    }
  }
}
`), 0o644)

	_ = os.WriteFile(filepath.Join(proj, "opencode.json"), []byte(`{
  "mcp": {
    "github": {
      "type": "remote",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": {
        "Authorization": "Bearer {env:GITHUB_PAT}",
        "X-Api-Key": "{env:API_KEY}"
      }
    },
    "fs": {
      "type": "local",
      "command": ["npx", "-y", "@mcp/fs", "."],
      "environment": {
        "FS_TOKEN": "{env:FS_TOKEN}"
      }
    }
  },
  "permission": {
    "bash": {
      "go test *": "allow",
      "rm -rf *": "deny"
    }
  }
}
`), 0o644)

	_ = os.MkdirAll(filepath.Join(proj, ".vscode"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".vscode", "mcp.json"), []byte(`{
  "servers": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": {
        "Authorization": "Bearer ${env:GITHUB_PAT}",
        "X-Api-Key": "${env:API_KEY}"
      }
    }
  }
}
`), 0o644)

	_ = os.MkdirAll(filepath.Join(proj, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".codex", "config.toml"), []byte(`[mcp_servers.github]
url = "https://api.githubcopilot.com/mcp/"
bearer_token_env_var = "GITHUB_PAT"
env_http_headers = { "X-Api-Key" = "API_KEY" }

[mcp_servers.fs]
command = "npx"
args = ["-y", "@mcp/fs", "."]
env_vars = ["FS_TOKEN"]
`), 0o644)

	// Claude settings: allow, deny, hooks.
	_ = os.MkdirAll(filepath.Join(proj, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".claude", "settings.json"), []byte(`{
  "permissions": {
    "allow": ["Bash(go vet:*)"],
    "deny": ["Bash(rm -rf *)"]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "./scripts/guard.sh"
          }
        ]
      }
    ]
  }
}
`), 0o644)

	// Identical skill in .claude/skills/auditor and .agents/skills/auditor -> deduplicated cleanly.
	for _, skillDir := range []string{
		filepath.Join(proj, ".claude", "skills", "auditor"),
		filepath.Join(proj, ".agents", "skills", "auditor"),
	} {
		_ = os.MkdirAll(skillDir, 0o755)
		_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Auditor Skill\nAudit carefully.\n"), 0o644)
	}

	// Agent in .claude/agents/reviewer.md with frontmatter.
	_ = os.MkdirAll(filepath.Join(proj, ".claude", "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".claude", "agents", "reviewer.md"), []byte("---\ndescription: \"Reviews PRs\"\nmodel: sonnet\n---\n\nReview code for bugs.\n"), 0o644)

	// Cursor rule .cursor/rules/style.mdc.
	_ = os.MkdirAll(filepath.Join(proj, ".cursor", "rules"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".cursor", "rules", "style.mdc"), []byte("---\ndescription: Go style\nglobs: \"**/*.go\"\nalwaysApply: false\n---\n\nRun gofmt.\n"), 0o644)

	// Windsurf rule .windsurf/rules/sec.md.
	_ = os.MkdirAll(filepath.Join(proj, ".windsurf", "rules"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".windsurf", "rules", "sec.md"), []byte("---\ntrigger: always_on\ndescription: Security baseline\n---\n\nNever hardcode secrets.\n"), 0o644)

	// AGENTS.md with managed block markers and ## <name> sections.
	_ = os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte(`<!-- BEGIN AGENTFILE:v1 deadbeef -->
# Project Agent Instructions

## architecture

*Scope:* Architectural invariants
*Applies to:* `+"`internal/**/*.go`"+`

Keep adapters I/O-free.
<!-- END AGENTFILE:v1 -->
`), 0o644)

	// Symlink escaping repo root MUST be ignored without failing or reading external files.
	outsideDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(outsideDir, "SKILL.md"), []byte("secret external"), 0o644)
	_ = os.Symlink(outsideDir, filepath.Join(proj, ".claude", "skills", "escaped-skill"))

	var out, errOut bytes.Buffer

	// 2. init --import --dry-run --json emits preview without creating Agentfile.yaml or .agentfile/imported/.
	code := ExecuteWithIO([]string{"init", "--import", "--dry-run", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("init --import --dry-run --json failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	if _, err := os.Stat(filepath.Join(proj, "Agentfile.yaml")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not create Agentfile.yaml")
	}
	if _, err := os.Stat(filepath.Join(proj, ".agentfile", "imported")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not create .agentfile/imported")
	}
	var dryRep map[string]any
	if err := json.Unmarshal(out.Bytes(), &dryRep); err != nil {
		t.Fatalf("invalid init --import --dry-run --json output: %v\n%s", err, out.String())
	}
	if dryRep["dry_run"] != true {
		t.Fatalf("expected dry_run=true, got %+v", dryRep)
	}

	// 3. init --import writes .agentfile/imported/ and valid Agentfile.yaml.
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"init", "--import"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("init --import failed (%d): %s\n%s", code, errOut.String(), out.String())
	}

	manifestPath := filepath.Join(proj, "Agentfile.yaml")
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read Agentfile.yaml: %v", err)
	}
	if err := schemas.ValidateAgentfileV1(rawManifest); err != nil {
		t.Fatalf("generated Agentfile.yaml failed JSON schema: %v\n%s", err, rawManifest)
	}
	ps, err := spec.ParseFile(manifestPath)
	if err != nil {
		t.Fatalf("generated Agentfile.yaml failed spec.ParseFile: %v\n%s", err, rawManifest)
	}

	// Verify env normalization (${VAR}, no {env:VAR} or ${env:VAR}).
	mStr := string(rawManifest)
	if strings.Contains(mStr, "{env:") {
		t.Fatalf("expected {env:VAR} and ${env:VAR} normalized to ${VAR}, got:\n%s", mStr)
	}
	if !strings.Contains(mStr, "Bearer ${GITHUB_PAT}") || !strings.Contains(mStr, "FS_TOKEN: ${FS_TOKEN}") || !strings.Contains(mStr, "X-Api-Key: ${API_KEY}") {
		t.Fatalf("expected normalized env placeholders in Agentfile.yaml:\n%s", mStr)
	}
	if _, ok := ps.MCP["github"]; !ok || ps.MCP["github"].Transport != "http" {
		t.Fatalf("expected deduplicated github http MCP, got %+v", ps.MCP)
	}
	if _, ok := ps.MCP["fs"]; !ok || ps.MCP["fs"].Transport != "stdio" {
		t.Fatalf("expected deduplicated fs stdio MCP, got %+v", ps.MCP)
	}

	// Verify skill deduplication + escaping symlink ignored.
	if len(ps.Skills) != 1 || ps.Skills[0].Name != "auditor" || ps.Skills[0].Source.Raw != "./.agentfile/imported/skills/auditor" {
		t.Fatalf("expected single deduplicated auditor skill from .agentfile/imported, got %+v", ps.Skills)
	}
	if _, err := os.Stat(filepath.Join(proj, ".agentfile", "imported", "skills", "auditor", "SKILL.md")); err != nil {
		t.Fatalf("expected extracted skill SKILL.md: %v", err)
	}

	// Verify agent extracted cleanly with frontmatter stripped from body.
	if len(ps.Agents) != 1 || ps.Agents[0].Name != "reviewer" || ps.Agents[0].Description != "Reviews PRs" || ps.Agents[0].Model != "sonnet" {
		t.Fatalf("expected imported reviewer agent, got %+v", ps.Agents)
	}
	agentBody, _ := os.ReadFile(filepath.Join(proj, ".agentfile", "imported", "agents", "reviewer.md"))
	if strings.Contains(string(agentBody), "---") || !strings.Contains(string(agentBody), "Review code for bugs.") {
		t.Fatalf("expected clean normalized agent body in .agentfile/imported/agents/reviewer.md, got:\n%s", agentBody)
	}

	// Verify rules extracted cleanly (cursor style.mdc, windsurf sec.md, AGENTS.md architecture).
	if len(ps.Rules) != 3 {
		t.Fatalf("expected 3 imported rules (architecture, sec, style), got %+v", ps.Rules)
	}
	ruleByName := make(map[string]spec.RuleSpec)
	for _, r := range ps.Rules {
		ruleByName[r.Name] = r
	}
	if r, ok := ruleByName["style"]; !ok || r.Description != "Go style" || !slices.Equal(r.Globs, []string{"**/*.go"}) || r.AlwaysApply {
		t.Fatalf("unexpected imported style rule: %+v", r)
	}
	if r, ok := ruleByName["sec"]; !ok || r.Description != "Security baseline" || !r.AlwaysApply {
		t.Fatalf("unexpected imported sec rule: %+v", r)
	}
	if r, ok := ruleByName["architecture"]; !ok || r.Description != "Architectural invariants" || !slices.Equal(r.Globs, []string{"internal/**/*.go"}) {
		t.Fatalf("unexpected imported architecture rule: %+v", r)
	}
	archBody, _ := os.ReadFile(filepath.Join(proj, ".agentfile", "imported", "rules", "architecture.md"))
	if strings.Contains(string(archBody), "BEGIN AGENTFILE") || strings.Contains(string(archBody), "*Scope:*") || !strings.Contains(string(archBody), "Keep adapters I/O-free.") {
		t.Fatalf("expected stripped clean rule body in .agentfile/imported/rules/architecture.md, got:\n%s", archBody)
	}

	// Verify permissions and hooks.
	if !slices.Contains(ps.Settings.Permissions.AllowCommands, "go vet") || !slices.Contains(ps.Settings.Permissions.AllowCommands, "go test *") {
		t.Fatalf("expected allow_commands imported, got %+v", ps.Settings.Permissions.AllowCommands)
	}
	if len(ps.Settings.Permissions.DenyCommands) != 1 || ps.Settings.Permissions.DenyCommands[0] != "rm -rf *" {
		t.Fatalf("expected deduplicated deny_commands [rm -rf *], got %+v", ps.Settings.Permissions.DenyCommands)
	}
	if len(ps.Settings.Hooks["PreToolUse"]) != 1 || ps.Settings.Hooks["PreToolUse"][0].Command.Raw != "./scripts/guard.sh" {
		t.Fatalf("expected imported PreToolUse hook, got %+v", ps.Settings.Hooks)
	}

	// Verify detected targets include claude, opencode, cursor, codex, copilot, windsurf.
	for _, wantTgt := range []string{"claude", "opencode", "cursor", "codex", "copilot", "windsurf"} {
		if !slices.Contains(ps.Targets, wantTgt) {
			t.Fatalf("expected detected target %q in %v", wantTgt, ps.Targets)
		}
	}

	// 4. Follow-up sync --link-mode=symlink --force and clean leave .agentfile/imported/ intact,
	// and vcs.SyncExcludes keeps .agentfile/imported/ non-ignored while excluding .agentfile/state.json.
	// Adjust targets_config on_unsupported: warn so multi-target sync succeeds cleanly with deny_commands/hooks.
	for _, tgt := range []string{"opencode", "cursor", "codex", "copilot", "windsurf"} {
		if ps.TargetsConfig == nil {
			ps.TargetsConfig = make(map[string]spec.TargetConfigSpec)
		}
		// Keep test simple: overwrite manifest targets to [claude, codex] via init --import --force --targets claude,codex
		_ = tgt
	}
	out.Reset()
	errOut.Reset()
	code = ExecuteWithIO([]string{"init", "--import", "--force", "--targets", "claude,codex"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("init --import --force --targets claude,codex failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	psMigrated, err := spec.ParseFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(psMigrated.Targets, []string{"claude", "codex"}) {
		t.Fatalf("expected targets [claude codex], got %v", psMigrated.Targets)
	}
	// Cursor/Windsurf rules were still migrated even though targets is [claude, codex].
	if len(psMigrated.Rules) != 3 {
		t.Fatalf("expected Cursor/Windsurf rules migrated with --targets claude,codex, got %d", len(psMigrated.Rules))
	}

	// Add on_unsupported: warn for codex so sync with deny_commands/PreToolUse succeeds.
	migratedBytes, _ := os.ReadFile(manifestPath)
	migratedBytes = append(migratedBytes, []byte("\ntargets_config:\n  codex:\n    on_unsupported: warn\n")...)
	_ = os.WriteFile(manifestPath, migratedBytes, 0o644)

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync", "--link-mode=symlink", "--force"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync --link-mode=symlink failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	// Verify .agentfile/state.json is ignored by git, while .agentfile/imported/ is NOT ignored!
	checkIgnore := func(rel string) bool {
		cmd := exec.Command("git", "check-ignore", "-q", rel)
		cmd.Dir = proj
		return cmd.Run() == nil
	}
	if !checkIgnore(".agentfile/state.json") {
		t.Fatalf("expected .agentfile/state.json to be git-excluded")
	}
	if checkIgnore(".agentfile/imported/skills/auditor/SKILL.md") {
		t.Fatalf("expected .agentfile/imported/ NOT to be git-excluded")
	}

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"clean"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("clean failed (%d): %s", code, errOut.String())
	}
	for _, importedRel := range []string{
		".agentfile/imported/skills/auditor/SKILL.md",
		".agentfile/imported/agents/reviewer.md",
		".agentfile/imported/rules/style.md",
		".agentfile/imported/rules/sec.md",
		".agentfile/imported/rules/architecture.md",
	} {
		if _, err := os.Stat(filepath.Join(proj, filepath.FromSlash(importedRel))); err != nil {
			t.Fatalf("expected %s to remain intact after sync + clean: %v", importedRel, err)
		}
	}
}

func TestInitImportFailClosedAndEdgeCases(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	var out, errOut bytes.Buffer

	// 1. Flag validation errors -> exit 2.
	empty := t.TempDir()
	for _, args := range [][]string{
		{"init", "--import", "--minimal"},
		{"init", "--import", "--full"},
		{"init", "--dry-run"},
		{"init", "--json"},
		{"init", "--import", "--targets", "unknown_tool"},
	} {
		out.Reset()
		errOut.Reset()
		if code := ExecuteWithIO(args, empty, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
			t.Fatalf("expected exit 2 for %v, got %d (err=%s)", args, code, errOut.String())
		}
	}

	// 2. Empty repo (zero importable configs) -> exit 2.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"init", "--import"}, empty, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on empty repo, got %d", code)
	}

	// 3. Existing Agentfile.yaml without --force -> exit 2.
	existing := t.TempDir()
	_ = os.WriteFile(filepath.Join(existing, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\n"), 0o644)
	_ = os.WriteFile(filepath.Join(existing, ".mcp.json"), []byte(`{"mcpServers":{"fs":{"command":"npx"}}}`), 0o644)
	if code := ExecuteWithIO([]string{"init", "--import"}, existing, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 when Agentfile.yaml exists without --force, got %d", code)
	}

	// 4. Literal secrets in MCP & hooks -> exit 2 without writing anything.
	secretCases := []struct {
		name      string
		wantInErr []string
		setup     func(dir string)
	}{
		{
			name:      "ghp token in mcp env",
			wantInErr: []string{".mcp.json", "mcp.gh.env.GH_TOKEN"},
			setup: func(dir string) {
				_ = os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"gh":{"command":"npx","env":{"GH_TOKEN":"ghp_1234567890abcdef"}}}}`), 0o644)
			},
		},
		{
			name:      "Bearer rawtok in mcp headers",
			wantInErr: []string{".mcp.json", "mcp.gh.headers.Authorization"},
			setup: func(dir string) {
				_ = os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"gh":{"type":"http","url":"https://api.example.com","headers":{"Authorization":"Bearer rawtok"}}}}`), 0o644)
			},
		},
		{
			name:      "literal secret in hook command",
			wantInErr: []string{".claude/settings.json", "hooks.PostToolUse"},
			setup: func(dir string) {
				_ = os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
				_ = os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"curl -H 'Authorization: Bearer rawtok' https://example.com"}]}]}}`), 0o644)
			},
		},
	}
	for _, tc := range secretCases {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			tc.setup(d)
			out.Reset()
			errOut.Reset()
			if code := ExecuteWithIO([]string{"init", "--import"}, d, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
				t.Fatalf("expected exit 2 for %s, got %d (out=%s err=%s)", tc.name, code, out.String(), errOut.String())
			}
			for _, want := range tc.wantInErr {
				if !strings.Contains(errOut.String(), want) {
					t.Fatalf("expected %q in error for %s, got: %s", want, tc.name, errOut.String())
				}
			}
			if _, err := os.Stat(filepath.Join(d, "Agentfile.yaml")); !os.IsNotExist(err) {
				t.Fatalf("must not write Agentfile.yaml on secret rejection")
			}
		})
	}

	// 5. Divergent MCP definition across tools -> exit 2.
	divMCP := t.TempDir()
	_ = os.WriteFile(filepath.Join(divMCP, ".mcp.json"), []byte(`{"mcpServers":{"gh":{"type":"http","url":"https://a.example.com"}}}`), 0o644)
	_ = os.WriteFile(filepath.Join(divMCP, "opencode.json"), []byte(`{"mcp":{"gh":{"type":"remote","url":"https://b.example.com"}}}`), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"init", "--import"}, divMCP, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on divergent MCP definition, got %d (%s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), ".mcp.json") || !strings.Contains(errOut.String(), "opencode.json") {
		t.Fatalf("expected conflicting file paths in MCP error: %s", errOut.String())
	}

	// 6. Divergent skill definition across tools -> exit 2.
	divSkill := t.TempDir()
	_ = os.MkdirAll(filepath.Join(divSkill, ".claude", "skills", "auditor"), 0o755)
	_ = os.WriteFile(filepath.Join(divSkill, ".claude", "skills", "auditor", "SKILL.md"), []byte("v1\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(divSkill, ".agents", "skills", "auditor"), 0o755)
	_ = os.WriteFile(filepath.Join(divSkill, ".agents", "skills", "auditor", "SKILL.md"), []byte("v2\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"init", "--import"}, divSkill, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on divergent skill definition, got %d (%s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), ".claude/skills/auditor") || !strings.Contains(errOut.String(), ".agents/skills/auditor") {
		t.Fatalf("expected conflicting skill paths in error: %s", errOut.String())
	}

	// 7. Divergent rule & agent definitions across tools -> exit 2.
	divRule := t.TempDir()
	_ = os.MkdirAll(filepath.Join(divRule, ".claude", "rules"), 0o755)
	_ = os.WriteFile(filepath.Join(divRule, ".claude", "rules", "agentfile-style.md"), []byte("Rule v1.\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(divRule, ".cursor", "rules"), 0o755)
	_ = os.WriteFile(filepath.Join(divRule, ".cursor", "rules", "agentfile-style.mdc"), []byte("---\nalwaysApply: false\n---\n\nRule v2.\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"init", "--import"}, divRule, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on divergent rule definition, got %d (%s)", code, errOut.String())
	}

	divAgent := t.TempDir()
	_ = os.MkdirAll(filepath.Join(divAgent, ".claude", "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(divAgent, ".claude", "agents", "rev.md"), []byte("v1\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(divAgent, ".opencode", "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(divAgent, ".opencode", "agents", "rev.md"), []byte("v2\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"init", "--import"}, divAgent, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on divergent agent definition, got %d (%s)", code, errOut.String())
	}
}

func TestInitImportGeminiCursorOpenCodeRulesAndUnstructuredFallbacks(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	proj := t.TempDir()

	// Gemini httpUrl + Cursor mcpServers + identical deduplication.
	_ = os.MkdirAll(filepath.Join(proj, ".gemini"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".gemini", "settings.json"), []byte(`{
  "mcpServers": {
    "linear": {
      "httpUrl": "https://mcp.linear.app/mcp",
      "headers": { "Authorization": "Bearer ${LINEAR_PAT}" }
    }
  }
}`), 0o644)

	_ = os.MkdirAll(filepath.Join(proj, ".cursor"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".cursor", "mcp.json"), []byte(`{
  "mcpServers": {
    "linear": {
      "url": "https://mcp.linear.app/mcp",
      "headers": { "Authorization": "Bearer ${env:LINEAR_PAT}" }
    }
  }
}`), 0o644)

	// .claude/rules/agentfile-coding.md (strips agentfile- prefix, maps paths -> globs).
	_ = os.MkdirAll(filepath.Join(proj, ".claude", "rules"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".claude", "rules", "agentfile-coding.md"), []byte("---\npaths:\n  - \"**/*.go\"\n---\n\nWrite unit tests.\n"), 0o644)

	// .opencode/agentfile-rules.md with ## coding (matching .claude/rules/agentfile-coding.md if Scope absent) + ## Go Code Style! (sanitized slug).
	_ = os.MkdirAll(filepath.Join(proj, ".opencode"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".opencode", "agentfile-rules.md"), []byte(`# Project Agent Instructions (Managed by agentfile)

## coding

*Applies to:* `+"`**/*.go`"+`

Write unit tests.

---

## Go Code Style!

Keep functions small.
`), 0o644)

	// Unstructured GEMINI.md and .github/copilot-instructions.md -> tool-derived fallback slugs.
	_ = os.WriteFile(filepath.Join(proj, "GEMINI.md"), []byte("Be concise with Gemini.\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(proj, ".github"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, ".github", "copilot-instructions.md"), []byte("Use idiomatic Go for Copilot.\n"), 0o644)

	// Escaping rule symlink must be ignored.
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "leak.md"), []byte("secret"), 0o644)
	_ = os.Symlink(filepath.Join(outside, "leak.md"), filepath.Join(proj, ".claude", "rules", "leak.md"))

	var out, errOut bytes.Buffer
	code := ExecuteWithIO([]string{"init", "--import", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false })
	if code != 0 {
		t.Fatalf("init --import --json failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	var sum ImportSummary
	if err := json.Unmarshal(out.Bytes(), &sum); err != nil {
		t.Fatalf("invalid ImportSummary JSON: %v\n%s", err, out.String())
	}
	for _, wantTgt := range []string{"claude", "opencode", "gemini", "cursor", "copilot"} {
		if !slices.Contains(sum.Targets, wantTgt) {
			t.Fatalf("expected target %q in %v", wantTgt, sum.Targets)
		}
	}
	for _, wantRule := range []string{"coding", "go-code-style", "gemini-instructions", "copilot-instructions"} {
		if !slices.Contains(sum.Rules, wantRule) {
			t.Fatalf("expected rule %q in %v", wantRule, sum.Rules)
		}
	}
	if slices.Contains(sum.Rules, "leak") {
		t.Fatalf("escaping symlink rule should have been ignored, got %v", sum.Rules)
	}
	if _, err := spec.ParseFile(filepath.Join(proj, "Agentfile.yaml")); err != nil {
		t.Fatalf("generated Agentfile.yaml invalid: %v", err)
	}
}

func TestInitImportCursorSkillRuleAndMultiTargetRoundTrip(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	// 1. Sync a multi-target manifest matching examples/starter across all 7 targets (claude, opencode, gemini, cursor, codex, copilot, windsurf),
	//    plus rules covering always_apply=false with globs, always_apply=false without globs, and always_apply=true with globs,
	//    and a dotted MCP server name (acme.github).
	proj := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj, "skills", "terraform-auditor"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "skills", "terraform-auditor", "SKILL.md"), []byte("# Terraform Auditor\nAudit TF.\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(proj, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "agents", "security-reviewer.md"), []byte("Audit IaC.\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "docs", "CODING.md"), []byte("Run gofmt.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "docs", "GLOBAL.md"), []byte("Always be concise.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "docs", "SCOPED_ALWAYS.md"), []byte("Check SQL queries.\n"), 0o644)

	manifest := `version: 1
targets: [claude, opencode, gemini, cursor, codex, copilot, windsurf]
mcp:
  acme.github:
    transport: http
    url: "https://api.githubcopilot.com/mcp/"
    headers:
      Authorization: "Bearer ${GITHUB_PAT}"
skills:
  - name: terraform-auditor
    source: ./skills/terraform-auditor
agents:
  - name: security-reviewer
    description: "Audits IaC"
    model: sonnet
    source: ./agents/security-reviewer.md
rules:
  - name: coding-standards
    source: ./docs/CODING.md
    description: "Repository Go conventions"
    globs: ["**/*.go"]
    always_apply: false
  - name: global-baseline
    source: ./docs/GLOBAL.md
    description: "Global instructions"
    always_apply: false
  - name: sql-always
    source: ./docs/SCOPED_ALWAYS.md
    description: "SQL safety"
    globs: ["**/*.sql"]
    always_apply: true
targets_config:
  opencode:
    on_unsupported: warn
  gemini:
    on_unsupported: warn
  cursor:
    on_unsupported: warn
  codex:
    on_unsupported: warn
  copilot:
    on_unsupported: warn
  windsurf:
    on_unsupported: warn
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifest), 0o644)

	var out, errOut bytes.Buffer
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("initial 7-target sync failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	if _, err := os.Stat(filepath.Join(proj, ".cursor", "rules", "agentfile-skill-terraform-auditor.mdc")); err != nil {
		t.Fatalf("expected Cursor skill rule wrapper on disk: %v", err)
	}

	// Remove Agentfile.yaml and .agentfile/state.json to simulate importing a 7-target synced repo.
	_ = os.Remove(filepath.Join(proj, "Agentfile.yaml"))
	_ = os.Remove(filepath.Join(proj, ".agentfile", "state.json"))

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"init", "--import"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("init --import on 7-target synced repo failed (%d): %s\n%s", code, errOut.String(), out.String())
	}

	ps, err := spec.ParseFile(filepath.Join(proj, "Agentfile.yaml"))
	if err != nil {
		t.Fatalf("imported Agentfile.yaml invalid: %v", err)
	}

	// Cursor skill wrapper rule must NOT be imported as rule "skill-terraform-auditor".
	for _, r := range ps.Rules {
		if strings.HasPrefix(r.Name, "skill-") {
			t.Fatalf("unexpected imported Cursor skill wrapper as rule %q in %+v", r.Name, ps.Rules)
		}
	}
	if len(ps.Skills) != 1 || ps.Skills[0].Name != "terraform-auditor" {
		t.Fatalf("expected 1 skill terraform-auditor, got %+v", ps.Skills)
	}
	if _, ok := ps.MCP["acme.github"]; !ok {
		t.Fatalf("expected dotted MCP server acme.github imported, got %+v", ps.MCP)
	}

	// Reconciled rules must preserve rich metadata without false divergence.
	if len(ps.Rules) != 3 {
		t.Fatalf("expected 3 rules (coding-standards, global-baseline, sql-always), got %+v", ps.Rules)
	}
	byName := make(map[string]spec.RuleSpec, len(ps.Rules))
	for _, r := range ps.Rules {
		byName[r.Name] = r
	}
	if r := byName["coding-standards"]; r.Description != "Repository Go conventions" || !slices.Equal(r.Globs, []string{"**/*.go"}) || r.AlwaysApply {
		t.Fatalf("unexpected coding-standards rule after merge: %+v", r)
	}
	if r := byName["global-baseline"]; r.Description != "Global instructions" || len(r.Globs) != 0 || r.AlwaysApply {
		t.Fatalf("unexpected global-baseline rule after merge (explicit cursor false should win over windsurf always_on): %+v", r)
	}
	if r := byName["sql-always"]; r.Description != "SQL safety" || !slices.Equal(r.Globs, []string{"**/*.sql"}) || !r.AlwaysApply {
		t.Fatalf("unexpected sql-always rule after merge: %+v", r)
	}

	// Round-trip sync --force must succeed cleanly without self-collision!
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync", "--force"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("round-trip sync --force failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--check"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("round-trip status --check failed (%d): %s\n%s", code, errOut.String(), out.String())
	}
}

func TestInitImportRuleGenuineDivergenceAndEdgeCases(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("CI", "true")

	var out, errOut bytes.Buffer

	// 1. Explicit empty --targets "" must fail closed with exit 2.
	emptyTargetDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(emptyTargetDir, ".mcp.json"), []byte(`{"mcpServers":{"fs":{"command":"npx"}}}`), 0o644)
	if code := ExecuteWithIO([]string{"init", "--import", "--targets", ""}, emptyTargetDir, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 for --targets \"\", got %d (err=%s)", code, errOut.String())
	}

	// 2. Genuine rule metadata divergences must fail closed with exit 2.
	divCases := []struct {
		name  string
		setup func(dir string)
	}{
		{
			name: "conflicting description",
			setup: func(dir string) {
				_ = os.MkdirAll(filepath.Join(dir, ".cursor", "rules"), 0o755)
				_ = os.MkdirAll(filepath.Join(dir, ".windsurf", "rules"), 0o755)
				_ = os.WriteFile(filepath.Join(dir, ".cursor", "rules", "agentfile-style.mdc"), []byte("---\ndescription: Desc A\nalwaysApply: true\n---\n\nBody.\n"), 0o644)
				_ = os.WriteFile(filepath.Join(dir, ".windsurf", "rules", "agentfile-style.md"), []byte("---\ntrigger: always_on\ndescription: Desc B\n---\n\nBody.\n"), 0o644)
			},
		},
		{
			name: "conflicting globs",
			setup: func(dir string) {
				_ = os.MkdirAll(filepath.Join(dir, ".claude", "rules"), 0o755)
				_ = os.MkdirAll(filepath.Join(dir, ".cursor", "rules"), 0o755)
				_ = os.WriteFile(filepath.Join(dir, ".claude", "rules", "agentfile-style.md"), []byte("---\npaths:\n  - \"**/*.go\"\n---\n\nBody.\n"), 0o644)
				_ = os.WriteFile(filepath.Join(dir, ".cursor", "rules", "agentfile-style.mdc"), []byte("---\nglobs: \"**/*.py\"\nalwaysApply: false\n---\n\nBody.\n"), 0o644)
			},
		},
		{
			name: "conflicting explicit always_apply",
			setup: func(dir string) {
				_ = os.MkdirAll(filepath.Join(dir, ".cursor", "rules"), 0o755)
				_ = os.MkdirAll(filepath.Join(dir, ".windsurf", "rules"), 0o755)
				_ = os.WriteFile(filepath.Join(dir, ".cursor", "rules", "agentfile-style.mdc"), []byte("---\nglobs: \"**/*.go\"\nalwaysApply: true\n---\n\nBody.\n"), 0o644)
				_ = os.WriteFile(filepath.Join(dir, ".windsurf", "rules", "agentfile-style.md"), []byte("---\ntrigger: glob\nglobs:\n  - \"**/*.go\"\n---\n\nBody.\n"), 0o644)
			},
		},
	}
	for _, tc := range divCases {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			tc.setup(d)
			out.Reset()
			errOut.Reset()
			if code := ExecuteWithIO([]string{"init", "--import"}, d, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
				t.Fatalf("%s: expected exit 2, got %d (out=%s err=%s)", tc.name, code, out.String(), errOut.String())
			}
			if !strings.Contains(errOut.String(), "divergent definitions for rule") {
				t.Fatalf("%s: expected divergent rule error, got: %s", tc.name, errOut.String())
			}
		})
	}

	// 3. Codex case-folded authorization header with bearer_token_env_var normalizes cleanly without duplicate keys.
	codexAuth := t.TempDir()
	_ = os.WriteFile(filepath.Join(codexAuth, ".mcp.json"), []byte(`{"mcpServers":{"gh":{"type":"http","url":"https://api.example.com/mcp","headers":{"Authorization":"Bearer ${GH_PAT}"}}}}`), 0o644)
	_ = os.MkdirAll(filepath.Join(codexAuth, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(codexAuth, ".codex", "config.toml"), []byte("[mcp_servers.gh]\nurl = \"https://api.example.com/mcp\"\nbearer_token_env_var = \"GH_PAT\"\nhttp_headers = { authorization = \"Bearer ${GH_PAT}\" }\n"), 0o644)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"init", "--import"}, codexAuth, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("expected case-folded authorization normalization to succeed, got %d (%s)", code, errOut.String())
	}
	psAuth, err := spec.ParseFile(filepath.Join(codexAuth, "Agentfile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(psAuth.MCP["gh"].Headers) != 1 || psAuth.MCP["gh"].Headers["Authorization"].Raw != "Bearer ${GH_PAT}" {
		t.Fatalf("expected single canonical Authorization header, got %+v", psAuth.MCP["gh"].Headers)
	}
}

func TestStatusUnhydratedTamperedCodexTOMLAndCleanSubsetStatusCheck(t *testing.T) {
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_ALLOW_FILE_GIT", "1")
	t.Setenv("CI", "true")

	cacheDir := t.TempDir()
	t.Setenv("AGENTFILE_CACHE_DIR", cacheDir)

	// Set up bare remote skill repo.
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

	proj := t.TempDir()
	runGit(proj, "init", "-b", "main")
	repoURL := "file://" + filepath.ToSlash(bareRepo)
	manifest := `version: 1
targets: [codex, copilot]
mcp:
  github:
    transport: http
    url: "https://api.example.com/mcp"
skills:
  - name: k8s
    source: "` + repoURL + `//skills/k8s"
`
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(manifest), 0o644)

	var out, errOut bytes.Buffer
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync failed (%d): %s", code, errOut.String())
	}

	// 1. Clean subset codex, then status --targets copilot --check MUST exit 0 (ExcludeFingerprint updated).
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"clean", "--targets", "codex"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("clean --targets codex failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--targets", "copilot", "--check"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("status --targets copilot --check after clean --targets codex failed (%d): %s\n%s", code, errOut.String(), out.String())
	}

	// 2. Re-sync codex, switch to cold cache (unhydrated), tamper .codex/config.toml -> status must report drift!
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("re-sync failed (%d): %s", code, errOut.String())
	}
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	_ = os.WriteFile(filepath.Join(proj, ".codex", "config.toml"), []byte("[mcp_servers.github]\nurl = \"https://tampered.example.com/\"\n"), 0o644)

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"status", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("status --json failed (%d): %s", code, errOut.String())
	}
	var rep StatusReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, out.String())
	}
	if !rep.Drifted {
		t.Fatalf("expected Drifted=true on tampered .codex/config.toml while unhydrated, got %+v", rep)
	}
	var codexRep *StatusTargetReport
	for i := range rep.PerTarget {
		if rep.PerTarget[i].Target == "codex" {
			codexRep = &rep.PerTarget[i]
		}
	}
	if codexRep == nil || !slices.Contains(codexRep.DriftedPaths, ".codex/config.toml") {
		t.Fatalf("expected .codex/config.toml in codex DriftedPaths while unhydrated, got %+v", codexRep)
	}
}

func setupHermeticTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CI", "true")
}

func writeTestVersionStub(dir, name, version string) string {
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, name+".bat")
		_ = os.WriteFile(p, []byte("@echo off\r\necho "+version+"\r\n"), 0o755)
		return p
	}
	p := filepath.Join(dir, name)
	_ = os.WriteFile(p, []byte("#!/bin/sh\necho '"+version+"'\n"), 0o755)
	return p
}

func TestSprint3ExtendsAddRemoveUpgradeAndRunFastPath(t *testing.T) {
	setupHermeticTestEnv(t)
	t.Setenv("AGENTFILE_ALLOW_FILE_GIT", "1")

	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work-base")
	bareRepo := filepath.Join(tmp, "base.git")
	binDir := filepath.Join(tmp, "bin")
	_ = os.MkdirAll(binDir, 0o755)
	writeTestVersionStub(binDir, "claude", "2.1.278 (Claude Code)")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_ = os.MkdirAll(filepath.Join(workRepo, "base", "skills", "sec-audit"), 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "base", "skills", "sec-audit", "SKILL.md"), []byte("# Security Audit v1\n"), 0o644)
	baseYAML := `version: 1
targets: [claude, opencode]
mcp:
  github:
    transport: http
    url: "https://api.githubcopilot.com/mcp/"
skills:
  - name: sec-audit
    source: ./skills/sec-audit
settings:
  permissions:
    deny_commands: ["rm -rf *"]
targets_config:
  claude:
    load_global: false
`
	_ = os.WriteFile(filepath.Join(workRepo, "base", "Agentfile.yaml"), []byte(baseYAML), 0o644)

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
	runGit(workRepo, "commit", "-m", "v1 base")
	runGit(tmp, "clone", "--bare", workRepo, bareRepo)

	proj := t.TempDir()
	runGit(proj, "init", "-b", "main")
	_ = os.MkdirAll(filepath.Join(proj, "skills", "payments"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "skills", "payments", "SKILL.md"), []byte("# Payments\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(proj, "skills", "extra"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "skills", "extra", "SKILL.md"), []byte("# Extra\n"), 0o644)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	childYAML := "version: 1\nextends: \"" + repoURL + "#main//base/Agentfile.yaml\"\ntargets: [claude]\nskills:\n  - name: payments\n    source: ./skills/payments\n"
	_ = os.WriteFile(filepath.Join(proj, "Agentfile.yaml"), []byte(childYAML), 0o644)

	var out, errOut bytes.Buffer
	// 1. Sync composes remote base + child, writes Agentfile.lock with extends, and populates .agentfile/run-cache/claude.json.
	if code := ExecuteWithIO([]string{"sync"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("sync with extends failed (%d): %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "skills", "sec-audit", "SKILL.md")); err != nil {
		t.Fatalf("expected inherited skill sec-audit materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "skills", "payments", "SKILL.md")); err != nil {
		t.Fatalf("expected child skill payments materialized: %v", err)
	}

	// 2. Warm `agentfile run --dry-run --json claude -p "hi"` hits fast path!
	out.Reset()
	errOut.Reset()
	fpr := &fakeRunProc{}
	if code := ExecuteWithRunner([]string{"run", "--dry-run", "--json", "claude", "-p", "hi"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 {
		t.Fatalf("run --dry-run failed (%d): %s", code, errOut.String())
	}
	var dryRep RunDryRunReport
	if err := json.Unmarshal(out.Bytes(), &dryRep); err != nil {
		t.Fatalf("unmarshal RunDryRunReport failed: %v", err)
	}
	if !dryRep.FastPathHit || dryRep.SyncAction != "fast_path" || dryRep.LoadGlobal != false {
		t.Fatalf("expected FastPathHit=true, SyncAction=fast_path, LoadGlobal=false (inherited from base), got %+v", dryRep)
	}

	// 3. `agentfile add skill extra ./skills/extra` preserves inherited sec-audit; `remove skill sec-audit` fails closed with origin error.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "extra", "./skills/extra"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("add skill extra failed (%d): %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "skills", "sec-audit", "SKILL.md")); err != nil {
		t.Fatalf("expected inherited sec-audit skill retained after add skill extra: %v", err)
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"remove", "skill", "sec-audit"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 when removing inherited-only skill sec-audit, got %d", code)
	}
	if !strings.Contains(errOut.String(), "inherited from") {
		t.Fatalf("expected origin attribution in remove error, got: %s", errOut.String())
	}

	// 4. `add skill sec-audit ./skills/extra` without --replace fails advising --replace; with --replace overrides locally;
	// then `remove skill sec-audit` removes local override and reverts to inherited base!
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "sec-audit", "./skills/extra"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 when shadowing inherited skill without --replace, got %d", code)
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"add", "skill", "sec-audit", "./skills/extra", "--replace"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("expected add --replace on inherited skill to succeed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"remove", "skill", "sec-audit"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("expected remove of local override to succeed and revert to base (%d): %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "reverting to inherited definition") {
		t.Fatalf("expected revert notice on stderr, got: %s", errOut.String())
	}

	// 5. Commit new revision to remote base repo and run `agentfile upgrade extends` -> updates extends lock entry.
	_ = os.WriteFile(filepath.Join(workRepo, "base", "skills", "sec-audit", "SKILL.md"), []byte("# Security Audit v2\n"), 0o644)
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "v2 base")
	runGit(workRepo, "push", bareRepo, "main:main")

	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"upgrade", "extends", "--json"}, proj, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("upgrade extends failed (%d): %s", code, errOut.String())
	}
	var upgRep UpgradeReport
	if err := json.Unmarshal(out.Bytes(), &upgRep); err != nil {
		t.Fatalf("unmarshal UpgradeReport failed: %v", err)
	}
	if len(upgRep.Deltas) == 0 || upgRep.Deltas[0].Action != "updated" {
		t.Fatalf("expected updated extends delta, got %+v", upgRep.Deltas)
	}

	// 6. Subdirectory --project-root and --if-configured passthrough on unconfigured target (opencode).
	subDir := filepath.Join(proj, "services", "api")
	_ = os.MkdirAll(subDir, 0o755)
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithRunner([]string{"run", "--if-configured", "--project-root", proj, "--dry-run", "--json", "opencode", "-p", "hi"}, subDir, strings.NewReader(""), &out, &errOut, func() bool { return false }, fpr); code != 0 {
		t.Fatalf("run --if-configured opencode failed (%d): %s", code, errOut.String())
	}
	var passRep RunDryRunReport
	_ = json.Unmarshal(out.Bytes(), &passRep)
	if passRep.SyncAction != "passthrough" {
		t.Fatalf("expected passthrough SyncAction for unconfigured target under --if-configured, got %+v", passRep)
	}
}
