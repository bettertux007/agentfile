package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/adapters/claude"
	"github.com/bettertux007/agentfile/internal/spec"
)

type mockProcessRunner struct {
	bin  string
	argv []string
	env  []string
	dir  string
	err  error
}

func (m *mockProcessRunner) Run(bin string, argv []string, env []string, dir string) error {
	m.bin = bin
	m.argv = append([]string(nil), argv...)
	m.env = append([]string(nil), env...)
	m.dir = dir
	return m.err
}

func TestGatherFactsAndExecuteClaude(t *testing.T) {
	homeDir := t.TempDir()
	xdgDir := t.TempDir()
	cacheDir := t.TempDir()
	binDir := t.TempDir()
	projDir := t.TempDir()

	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", xdgDir)
	t.Setenv("AGENTFILE_CACHE_DIR", cacheDir)

	// Stub claude binary on PATH.
	fakeClaude := filepath.Join(binDir, "claude")
	_ = os.WriteFile(fakeClaude, []byte("#!/bin/sh\necho '2.1.278 (Claude Code)'\n"), 0o755)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Empty .claude/skills dir should NOT trigger GlobalHits["skills"], whereas non-empty .claude.json SHOULD trigger "mcp".
	_ = os.MkdirAll(filepath.Join(homeDir, ".claude", "skills"), 0o755)
	_ = os.WriteFile(filepath.Join(homeDir, ".claude.json"), []byte(`{"mcpServers":{}}`), 0o644)

	adp := claude.New()
	facts, err := GatherFacts(context.Background(), projDir, adp, nil, false, []string{"-p", "hi"})
	if err != nil {
		t.Fatalf("GatherFacts failed: %v", err)
	}
	if facts.GlobalHits["skills"] {
		t.Fatalf("empty ~/.claude/skills should not count as hit")
	}
	if !facts.GlobalHits["mcp"] {
		t.Fatalf("expected ~/.claude.json to set GlobalHits[mcp]=true")
	}
	if facts.BinaryVersion != "2.1.278 (Claude Code)" {
		t.Fatalf("unexpected BinaryVersion: %q", facts.BinaryVersion)
	}

	plan := &adapters.LaunchPlan{
		Target:        "claude",
		Mode:          adapters.IsolationNativeFlags,
		PrependedArgs: []string{"--setting-sources", "project,local", "--strict-mcp-config", "--mcp-config", filepath.Join(cacheDir, "empty-mcp.json")},
		NeedEmptyMCP:  true,
	}
	var stderr bytes.Buffer
	mpr := &mockProcessRunner{}
	if err := Execute(context.Background(), mpr, projDir, adp, plan, []string{"-p", "hi"}, false, cacheDir, &stderr, true); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if mpr.bin != fakeClaude || mpr.dir != projDir {
		t.Fatalf("unexpected exec target: bin=%s dir=%s", mpr.bin, mpr.dir)
	}
	if len(mpr.argv) != 7 || mpr.argv[len(mpr.argv)-1] != "hi" {
		t.Fatalf("unexpected argv: %v", mpr.argv)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "empty-mcp.json")); err != nil {
		t.Fatalf("expected empty-mcp.json created: %v", err)
	}
	envJoined := strings.Join(mpr.env, "\n")
	if !strings.Contains(envJoined, "AGENTFILE_ACTIVE=1") || !strings.Contains(envJoined, "AGENTFILE_ISOLATION_MODE=native_flags") {
		t.Fatalf("missing AGENTFILE_* env vars")
	}
	if !strings.Contains(stderr.String(), "agentfile: target=claude load_global=false mode=native_flags") {
		t.Fatalf("missing stderr isolation notice: %q", stderr.String())
	}

	// Missing binary on PATH returns PolicyError.
	t.Setenv("PATH", t.TempDir())
	err = Execute(context.Background(), mpr, projDir, adp, plan, nil, false, cacheDir, &stderr, false)
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on missing binary, got %v", err)
	}
}

func TestResolveGlobalPathAndExitCodeError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
	p := ResolveGlobalPath("/home/u", adapters.GlobalConfigSpec{RelPath: "opencode/opencode.json", UseXDGConfig: true})
	if p != filepath.Join("/custom/xdg", "opencode", "opencode.json") {
		t.Fatalf("unexpected XDG resolved path: %s", p)
	}

	e := &ExitCodeError{Code: 7, Err: errors.New("boom")}
	if e.Error() != "boom" || e.Unwrap() == nil {
		t.Fatalf("unexpected ExitCodeError formatting")
	}
	e2 := &ExitCodeError{Code: 3}
	if e2.Error() != "process exited with status 3" {
		t.Fatalf("unexpected default ExitCodeError message: %s", e2.Error())
	}

	// BuildEnv replaces existing AGENTFILE_* entries cleanly.
	t.Setenv("AGENTFILE_ACTIVE", "old")
	env := BuildEnv("opencode", true, adapters.IsolationNone, map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE_SKILLS": "1"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "AGENTFILE_ACTIVE=old") || !strings.Contains(joined, "AGENTFILE_ACTIVE=1") || !strings.Contains(joined, "AGENTFILE_TARGET=opencode") || !strings.Contains(joined, "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1") {
		t.Fatalf("expected BuildEnv to replace AGENTFILE_ACTIVE=old and include extra overrides")
	}

	// EnsureEmptyMCPConfig is idempotent and IsNonEmptyPath handles missing/empty/populated files & dirs.
	cRoot := t.TempDir()
	p1, err := EnsureEmptyMCPConfig(cRoot)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := EnsureEmptyMCPConfig(cRoot)
	if err != nil || p1 != p2 {
		t.Fatalf("EnsureEmptyMCPConfig second call failed: %v", err)
	}
	emptyFile := filepath.Join(cRoot, "empty.txt")
	_ = os.WriteFile(emptyFile, nil, 0o644)
	if IsNonEmptyPath(emptyFile) || IsNonEmptyPath(filepath.Join(cRoot, "missing")) {
		t.Fatalf("expected false for empty/missing file")
	}
	dsOnly := filepath.Join(cRoot, "ds-dir")
	_ = os.MkdirAll(dsOnly, 0o755)
	_ = os.WriteFile(filepath.Join(dsOnly, ".DS_Store"), []byte("x"), 0o644)
	if IsNonEmptyPath(dsOnly) {
		t.Fatalf("expected false for .DS_Store-only directory")
	}

	// ProbeBinaryVersion on failing binary returns "".
	badBin := filepath.Join(cRoot, "fail-bin")
	_ = os.WriteFile(badBin, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if got := ProbeBinaryVersion(context.Background(), badBin); got != "" {
		t.Fatalf("expected empty version on failing binary, got %q", got)
	}
}
