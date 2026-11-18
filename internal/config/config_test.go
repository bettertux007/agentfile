package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertux007/agentfile/internal/spec"
)

func TestResolveDefaultsTTYvsNonTTY(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	dir := t.TempDir()

	effTTY, err := Resolve(dir, CLIOverrides{}, func() bool { return true })
	if err != nil {
		t.Fatalf("Resolve TTY failed: %v", err)
	}
	if effTTY.LinkMode != "copy" || effTTY.OnConflict != "ask" || !effTTY.GitExclude {
		t.Fatalf("unexpected TTY defaults: %+v", effTTY)
	}

	effHeadless, err := Resolve(dir, CLIOverrides{}, func() bool { return false })
	if err != nil {
		t.Fatalf("Resolve non-TTY failed: %v", err)
	}
	if effHeadless.OnConflict != "fail" {
		t.Fatalf("expected fail on non-TTY, got %s", effHeadless.OnConflict)
	}

	t.Setenv("CI", "true")
	effCI, err := Resolve(dir, CLIOverrides{}, func() bool { return true })
	if err != nil {
		t.Fatalf("Resolve CI failed: %v", err)
	}
	if effCI.OnConflict != "fail" {
		t.Fatalf("expected fail when CI=true even with TTY, got %s", effCI.OnConflict)
	}
}

func TestResolveFileAndFlagPrecedence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".agentfile")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const cfgYAML = `
version: 1
link_mode: symlink
on_conflict: backup
git_exclude: false
`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	eff, err := Resolve(dir, CLIOverrides{
		Copy:          true,
		Force:         true,
		GitExclude:    true,
		GitExcludeSet: true,
	}, func() bool { return false })
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if eff.LinkMode != "copy" || eff.OnConflict != "overwrite" || !eff.GitExclude {
		t.Fatalf("expected CLI flags to override file config, got %+v", eff)
	}
}

func TestResolveRejectsConflictsAndUnknownFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, err := Resolve(dir, CLIOverrides{Copy: true, LinkMode: "symlink"}, nil)
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for --copy + --link-mode=symlink, got %v", err)
	}

	_, err = Resolve(dir, CLIOverrides{Force: true, OnConflict: "fail"}, nil)
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for --force + --on-conflict=fail, got %v", err)
	}

	cfgDir := filepath.Join(dir, ".agentfile")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("version: 1\nbogus: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(dir, CLIOverrides{}, nil)
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on unknown config field, got %v", err)
	}

	// Empty, multi-doc, bad version, and invalid link_mode/on_conflict.
	badConfigs := []string{
		"",
		"version: 1\n---\nversion: 1\n",
		"version: 2\n",
		"version: 1\nlink_mode: hardlink\n",
		"version: 1\non_conflict: ignore\n",
	}
	for _, bc := range badConfigs {
		_ = os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(bc), 0o644)
		if _, err := Resolve(dir, CLIOverrides{}, nil); !errors.As(err, &pe) {
			t.Fatalf("expected PolicyError for config %q, got %v", bc, err)
		}
	}
}

func TestResolveTargetLoadGlobalTruthTable(t *testing.T) {
	t.Parallel()
	bTrue := true
	bFalse := false

	// Default when omitted everywhere -> true, "default"
	got, src, warn, err := ResolveTargetLoadGlobal(spec.TargetConfigSpec{}, &EffectiveConfig{}, false, false, false)
	if err != nil || !got || src != SourceDefault || warn != "" {
		t.Fatalf("default failed: got=%v src=%s warn=%q err=%v", got, src, warn, err)
	}

	// Repo false beats local true with warning (no error).
	got, src, warn, err = ResolveTargetLoadGlobal(spec.TargetConfigSpec{LoadGlobal: &bFalse}, &EffectiveConfig{LoadGlobal: &bTrue}, false, false, false)
	if err != nil || got || src != SourceAgentfile || warn == "" {
		t.Fatalf("repo false vs local true failed: got=%v src=%s warn=%q err=%v", got, src, warn, err)
	}

	// Local false tightens repo true.
	got, src, warn, err = ResolveTargetLoadGlobal(spec.TargetConfigSpec{LoadGlobal: &bTrue}, &EffectiveConfig{LoadGlobal: &bFalse}, false, false, false)
	if err != nil || got || src != SourceLocalConfig || warn != "" {
		t.Fatalf("local false vs repo true failed: got=%v src=%s warn=%q err=%v", got, src, warn, err)
	}

	// CLI --load-global overrides repo false with warning.
	got, src, warn, err = ResolveTargetLoadGlobal(spec.TargetConfigSpec{LoadGlobal: &bFalse}, &EffectiveConfig{}, true, true, false)
	if err != nil || !got || src != SourceCLI || warn == "" {
		t.Fatalf("cli --load-global failed: got=%v src=%s warn=%q err=%v", got, src, warn, err)
	}

	// CLI --no-global overrides repo true.
	got, src, warn, err = ResolveTargetLoadGlobal(spec.TargetConfigSpec{LoadGlobal: &bTrue}, &EffectiveConfig{}, false, false, true)
	if err != nil || got || src != SourceCLI || warn != "" {
		t.Fatalf("cli --no-global failed: got=%v src=%s warn=%q err=%v", got, src, warn, err)
	}

	// Conflicting --load-global and --no-global -> PolicyError.
	if _, _, _, err := ResolveTargetLoadGlobal(spec.TargetConfigSpec{}, &EffectiveConfig{}, true, true, true); err == nil {
		t.Fatalf("expected error on conflicting CLI flags")
	}
}
