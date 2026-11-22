package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/adapters/claude"
	"github.com/bettertux007/agentfile/internal/spec"
)

func writeStubVersionBin(dir, name, version string) string {
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, name+".bat")
		_ = os.WriteFile(p, []byte("@echo off\r\necho "+version+"\r\n"), 0o755)
		return p
	}
	p := filepath.Join(dir, name)
	_ = os.WriteFile(p, []byte("#!/bin/sh\necho '"+version+"'\n"), 0o755)
	return p
}

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
	fakeClaude := writeStubVersionBin(binDir, "claude", "2.1.278 (Claude Code)")
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
	if runtime.GOOS == "windows" {
		badBin += ".bat"
		_ = os.WriteFile(badBin, []byte("@echo off\r\nexit /b 1\r\n"), 0o755)
	} else {
		_ = os.WriteFile(badBin, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	}
	if got := ProbeBinaryVersion(context.Background(), badBin); got != "" {
		t.Fatalf("expected empty version on failing binary, got %q", got)
	}
}

func TestProbeBinaryVersionCachedAndExecuteWithOptions(t *testing.T) {
	cacheRoot := t.TempDir()
	binDir := t.TempDir()
	pkgPath := filepath.Join(binDir, "package.json")

	_ = os.WriteFile(pkgPath, []byte(`{"version":"2.1.0"}`), 0o644)
	binPath := writeStubVersionBin(binDir, "claude", "2.1.0")

	// 1. First probe populates bin-versions.json.
	v1 := ProbeBinaryVersionCached(context.Background(), cacheRoot, binPath, true)
	if v1 != "2.1.0" {
		t.Fatalf("expected 2.1.0, got %q", v1)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "bin-versions.json")); err != nil {
		t.Fatalf("expected bin-versions.json created: %v", err)
	}

	// 2. Overwrite script with identical size and mtime -> cache hit returns "2.1.0".
	origInfo, _ := os.Stat(binPath)
	writeStubVersionBin(binDir, "claude", "9.9.9")
	_ = os.Chtimes(binPath, origInfo.ModTime(), origInfo.ModTime())
	vHit := ProbeBinaryVersionCached(context.Background(), cacheRoot, binPath, true)
	if vHit != "2.1.0" {
		t.Fatalf("expected cached 2.1.0 on identical size/mtime, got %q", vHit)
	}

	// 3. useCache=false bypasses cache and reads "9.9.9".
	vBypass := ProbeBinaryVersionCached(context.Background(), cacheRoot, binPath, false)
	if vBypass != "9.9.9" {
		t.Fatalf("expected 9.9.9 when useCache=false, got %q", vBypass)
	}

	// 4. Touching package.json invalidates cache and updates to "9.9.9".
	_ = os.WriteFile(pkgPath, []byte(`{"version":"9.9.9","updated":true}`), 0o644)
	vInvalidate := ProbeBinaryVersionCached(context.Background(), cacheRoot, binPath, true)
	if vInvalidate != "9.9.9" {
		t.Fatalf("expected cache invalidation on package.json change, got %q", vInvalidate)
	}

	// 5. Dynamic version-manager shim path bypasses cache.
	shimDir := filepath.Join(t.TempDir(), ".asdf", "shims")
	_ = os.MkdirAll(shimDir, 0o755)
	shimBin := writeStubVersionBin(shimDir, "claude", "3.0.0")
	if got := ProbeBinaryVersionCached(context.Background(), cacheRoot, shimBin, true); got != "3.0.0" {
		t.Fatalf("expected shim probe 3.0.0, got %q", got)
	}

	// 6. ExecuteWithOptions sets AGENTFILE_ROOT, honors LaunchDir, and respects QuietBanner.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	mpr := &mockProcessRunner{}
	var stderr bytes.Buffer
	plan := &adapters.LaunchPlan{Target: "claude", Mode: adapters.IsolationNone}
	err := ExecuteWithOptions(context.Background(), mpr, ExecuteOptions{
		ProjectRoot: "/repo",
		LaunchDir:   "/repo/services/api",
		QuietBanner: true,
	}, claude.New(), plan, []string{"--version"}, true, cacheRoot, &stderr, false)
	if err != nil {
		t.Fatalf("ExecuteWithOptions failed: %v", err)
	}
	if mpr.dir != "/repo/services/api" {
		t.Fatalf("expected LaunchDir /repo/services/api, got %s", mpr.dir)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected QuietBanner to suppress stderr banner, got %q", stderr.String())
	}
	if !strings.Contains(strings.Join(mpr.env, "\n"), "AGENTFILE_ROOT=/repo") {
		t.Fatalf("expected AGENTFILE_ROOT=/repo in env")
	}
}
