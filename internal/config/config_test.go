package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bettertux007/agentfile/internal/spec"
)

func TestResolveDefaultsTTYvsNonTTY(t *testing.T) {
	clearTestEnv(t)
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
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

	eff, err := ResolveWithOptions(dir, CLIOverrides{
		Copy:          true,
		Force:         true,
		GitExclude:    true,
		GitExcludeSet: true,
	}, func() bool { return false }, ResolveOptions{DisableUserConfig: true})
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

	_, err := ResolveWithOptions(dir, CLIOverrides{Copy: true, LinkMode: "symlink"}, nil, ResolveOptions{DisableUserConfig: true})
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for --copy + --link-mode=symlink, got %v", err)
	}

	_, err = ResolveWithOptions(dir, CLIOverrides{Force: true, OnConflict: "fail"}, nil, ResolveOptions{DisableUserConfig: true})
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
	_, err = ResolveWithOptions(dir, CLIOverrides{}, nil, ResolveOptions{DisableUserConfig: true})
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
		if _, err := ResolveWithOptions(dir, CLIOverrides{}, nil, ResolveOptions{DisableUserConfig: true}); !errors.As(err, &pe) {
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

func clearTestEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AGENTFILE_NO_USER_CONFIG",
		"AGENTFILE_CONFIG_FILE",
		"XDG_CONFIG_HOME",
		"CI",
		"GITHUB_ACTIONS",
		"BUILDKITE",
		"GITLAB_CI",
		"CIRCLECI",
	} {
		t.Setenv(k, "")
	}
}

func TestDefaultUserConfigPath(t *testing.T) {
	// 1. AGENTFILE_NO_USER_CONFIG=1 disables user config and ignores AGENTFILE_CONFIG_FILE.
	clearTestEnv(t)
	t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")
	t.Setenv("AGENTFILE_CONFIG_FILE", "/missing/file")
	p, ok, err := DefaultUserConfigPath()
	if err != nil || ok || p != "" {
		t.Fatalf("AGENTFILE_NO_USER_CONFIG=1 expected (\"\", false, nil), got (%q, %v, %v)", p, ok, err)
	}

	// 2. AGENTFILE_CONFIG_FILE=/missing/file fails closed with PolicyError.
	clearTestEnv(t)
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	t.Setenv("AGENTFILE_CONFIG_FILE", missing)
	_, _, err = DefaultUserConfigPath()
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for missing AGENTFILE_CONFIG_FILE, got %v", err)
	}

	// 2b. Existing AGENTFILE_CONFIG_FILE succeeds even when CI=true.
	clearTestEnv(t)
	t.Setenv("CI", "true")
	explicit := filepath.Join(t.TempDir(), "custom-config.yaml")
	if err := os.WriteFile(explicit, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFILE_CONFIG_FILE", explicit)
	p, ok, err = DefaultUserConfigPath()
	if err != nil || !ok || p != explicit {
		t.Fatalf("explicit AGENTFILE_CONFIG_FILE in CI expected (%q, true, nil), got (%q, %v, %v)", explicit, p, ok, err)
	}

	// 3. CI=true without AGENTFILE_CONFIG_FILE skips ambient $HOME/.config.
	clearTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CI", "true")
	p, ok, err = DefaultUserConfigPath()
	if err != nil || ok || p != "" {
		t.Fatalf("CI=true without AGENTFILE_CONFIG_FILE expected (\"\", false, nil), got (%q, %v, %v)", p, ok, err)
	}

	// 4. XDG_CONFIG_HOME overrides $HOME/.config.
	clearTestEnv(t)
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	p, ok, err = DefaultUserConfigPath()
	wantXDG := filepath.Join(xdg, "agentfile", "config.yaml")
	if err != nil || !ok || p != wantXDG {
		t.Fatalf("XDG_CONFIG_HOME expected (%q, true, nil), got (%q, %v, %v)", wantXDG, p, ok, err)
	}

	// 5. $HOME/.config/agentfile/config.yaml when XDG_CONFIG_HOME is unset.
	clearTestEnv(t)
	t.Setenv("HOME", home)
	p, ok, err = DefaultUserConfigPath()
	wantHome := filepath.Join(home, ".config", "agentfile", "config.yaml")
	if err != nil || !ok || p != wantHome {
		t.Fatalf("HOME default expected (%q, true, nil), got (%q, %v, %v)", wantHome, p, ok, err)
	}

	// 6. Empty $HOME falls back to os.UserConfigDir().
	clearTestEnv(t)
	t.Setenv("HOME", "")
	p, ok, err = DefaultUserConfigPath()
	if udir, uerr := os.UserConfigDir(); uerr == nil && udir != "" {
		wantFallback := filepath.Join(udir, "agentfile", "config.yaml")
		if err != nil || !ok || p != wantFallback {
			t.Fatalf("empty HOME fallback expected (%q, true, nil), got (%q, %v, %v)", wantFallback, p, ok, err)
		}
	} else {
		if err != nil || ok || p != "" {
			t.Fatalf("empty HOME with unavailable os.UserConfigDir expected (\"\", false, nil), got (%q, %v, %v)", p, ok, err)
		}
	}
}

func TestResolveFourLayerPrecedence(t *testing.T) {
	clearTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	userCfgDir := filepath.Join(home, ".config", "agentfile")
	if err := os.MkdirAll(userCfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userCfgPath := filepath.Join(userCfgDir, "config.yaml")

	writeUser := func(body string) {
		t.Helper()
		if err := os.WriteFile(userCfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeLocal := func(proj string, body string) {
		t.Helper()
		dir := filepath.Join(proj, ".agentfile")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A. user_config sets link_mode, on_conflict: overwrite, git_exclude: false; no local_config -> user_config wins + warning emitted.
	projA := t.TempDir()
	writeUser("version: 1\nlink_mode: symlink\non_conflict: overwrite\ngit_exclude: false\nload_global: false\n")
	effA, err := ResolveWithOptions(projA, CLIOverrides{}, func() bool { return true }, ResolveOptions{})
	if err != nil {
		t.Fatalf("ResolveWithOptions projA failed: %v", err)
	}
	if effA.LinkMode != "symlink" || effA.OnConflict != "overwrite" || effA.GitExclude {
		t.Fatalf("unexpected projA values: %+v", effA)
	}
	if !effA.UserConfigLoaded || effA.UserConfigPath != userCfgPath || effA.LocalConfigLoaded {
		t.Fatalf("unexpected projA metadata: %+v", effA)
	}
	if effA.FieldSources["link_mode"] != SourceUserConfig ||
		effA.FieldSources["on_conflict"] != SourceUserConfig ||
		effA.FieldSources["git_exclude"] != SourceUserConfig ||
		effA.FieldSources["load_global"] != SourceUserConfig {
		t.Fatalf("unexpected projA FieldSources: %+v", effA.FieldSources)
	}
	if len(effA.Warnings) != 1 {
		t.Fatalf("expected 1 overwrite warning from user_config, got %v", effA.Warnings)
	}
	// Resolve delegates cleanly to ResolveWithOptions with default options.
	effADelegated, err := Resolve(projA, CLIOverrides{}, func() bool { return true })
	if err != nil {
		t.Fatalf("Resolve projA failed: %v", err)
	}
	if effADelegated.LinkMode != effA.LinkMode || effADelegated.OnConflict != effA.OnConflict || effADelegated.UserConfigPath != effA.UserConfigPath {
		t.Fatalf("Resolve delegation mismatch: got %+v vs %+v", effADelegated, effA)
	}

	// B. local_config overrides user_config; no overwrite warning when local_config overrides on_conflict.
	projB := t.TempDir()
	writeLocal(projB, "version: 1\nlink_mode: copy\non_conflict: backup\ngit_exclude: true\nload_global: true\n")
	effB, err := ResolveWithOptions(projB, CLIOverrides{}, func() bool { return true }, ResolveOptions{})
	if err != nil {
		t.Fatalf("ResolveWithOptions projB failed: %v", err)
	}
	if effB.LinkMode != "copy" || effB.OnConflict != "backup" || !effB.GitExclude {
		t.Fatalf("unexpected projB values: %+v", effB)
	}
	if effB.FieldSources["link_mode"] != SourceLocalConfig ||
		effB.FieldSources["on_conflict"] != SourceLocalConfig ||
		effB.FieldSources["git_exclude"] != SourceLocalConfig ||
		effB.FieldSources["load_global"] != SourceLocalConfig {
		t.Fatalf("unexpected projB FieldSources: %+v", effB.FieldSources)
	}
	if len(effB.Warnings) != 0 {
		t.Fatalf("expected no overwrite warning when local_config overrides on_conflict, got %v", effB.Warnings)
	}

	// B2. local_config explicitly sets on_conflict: overwrite -> still no user_config overwrite warning.
	writeLocal(projB, "version: 1\non_conflict: overwrite\n")
	effB2, err := ResolveWithOptions(projB, CLIOverrides{}, func() bool { return true }, ResolveOptions{})
	if err != nil {
		t.Fatalf("ResolveWithOptions projB2 failed: %v", err)
	}
	if effB2.FieldSources["on_conflict"] != SourceLocalConfig || len(effB2.Warnings) != 0 {
		t.Fatalf("expected local_config source and no user_config overwrite warning, got src=%s warnings=%v", effB2.FieldSources["on_conflict"], effB2.Warnings)
	}

	// C. CLI overrides both local_config and user_config; no user_config overwrite warning when CLI overrides on_conflict.
	effC, err := ResolveWithOptions(projA, CLIOverrides{
		LinkMode:      "copy",
		OnConflict:    "fail",
		GitExclude:    true,
		GitExcludeSet: true,
	}, func() bool { return true }, ResolveOptions{})
	if err != nil {
		t.Fatalf("ResolveWithOptions projC failed: %v", err)
	}
	if effC.LinkMode != "copy" || effC.OnConflict != "fail" || !effC.GitExclude {
		t.Fatalf("unexpected projC values: %+v", effC)
	}
	if effC.FieldSources["link_mode"] != SourceCLI ||
		effC.FieldSources["on_conflict"] != SourceCLI ||
		effC.FieldSources["git_exclude"] != SourceCLI {
		t.Fatalf("unexpected projC FieldSources: %+v", effC.FieldSources)
	}
	if len(effC.Warnings) != 0 {
		t.Fatalf("expected no user_config overwrite warning when CLI overrides on_conflict, got %v", effC.Warnings)
	}

	// D. DisableUserConfig ignores user_config completely and falls back to defaults.
	effD, err := ResolveWithOptions(projA, CLIOverrides{}, func() bool { return true }, ResolveOptions{DisableUserConfig: true})
	if err != nil {
		t.Fatalf("ResolveWithOptions DisableUserConfig failed: %v", err)
	}
	if effD.UserConfigLoaded || effD.LinkMode != "copy" || effD.OnConflict != "ask" || !effD.GitExclude {
		t.Fatalf("unexpected DisableUserConfig result: %+v", effD)
	}
	if effD.FieldSources["link_mode"] != SourceDefault || effD.FieldSources["on_conflict"] != SourceDefault || effD.FieldSources["git_exclude"] != SourceDefault {
		t.Fatalf("unexpected DisableUserConfig FieldSources: %+v", effD.FieldSources)
	}

	// E. Invalid user_config fails with PolicyError (exit 2) even if local_config is valid.
	writeUser("version: 1\nlink_mode: hardlink\n")
	_, err = ResolveWithOptions(projB, CLIOverrides{}, nil, ResolveOptions{})
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for invalid user_config even when local_config is valid, got %v", err)
	}
}

func TestResolveTargetLoadGlobalFiveTiers(t *testing.T) {
	t.Parallel()
	bTrue := true
	bFalse := false

	type tc struct {
		name         string
		agentfile    *bool
		local        *bool
		user         *bool
		cliVal       bool
		cliSet       bool
		cliNoGlobal  bool
		want         bool
		wantSrc      string
		wantWarn     bool
		wantWarnSubs []string
		noWarnSubs   []string
	}

	cases := []tc{
		{
			name:    "default when omitted everywhere",
			want:    true,
			wantSrc: SourceDefault,
		},
		{
			name:    "user_config false overrides default",
			user:    &bFalse,
			want:    false,
			wantSrc: SourceUserConfig,
		},
		{
			name:    "user_config true explicit",
			user:    &bTrue,
			want:    true,
			wantSrc: SourceUserConfig,
		},
		{
			name:      "agentfile true overrides user_config false",
			agentfile: &bTrue,
			user:      &bFalse,
			want:      true,
			wantSrc:   SourceAgentfile,
		},
		{
			name:      "local_config false overrides agentfile true and user_config true",
			agentfile: &bTrue,
			local:     &bFalse,
			user:      &bTrue,
			want:      false,
			wantSrc:   SourceLocalConfig,
		},
		{
			name:    "local_config true overrides user_config false",
			local:   &bTrue,
			user:    &bFalse,
			want:    true,
			wantSrc: SourceLocalConfig,
		},
		{
			name:         "agentfile false beats local_config true with local_config warning only",
			agentfile:    &bFalse,
			local:        &bTrue,
			want:         false,
			wantSrc:      SourceAgentfile,
			wantWarn:     true,
			wantWarnSubs: []string{"local_config"},
			noWarnSubs:   []string{"user_config"},
		},
		{
			name:         "agentfile false beats user_config true with user_config warning only",
			agentfile:    &bFalse,
			user:         &bTrue,
			want:         false,
			wantSrc:      SourceAgentfile,
			wantWarn:     true,
			wantWarnSubs: []string{"user_config"},
			noWarnSubs:   []string{"local_config"},
		},
		{
			name:         "agentfile false beats both local_config true and user_config true with both named",
			agentfile:    &bFalse,
			local:        &bTrue,
			user:         &bTrue,
			want:         false,
			wantSrc:      SourceAgentfile,
			wantWarn:     true,
			wantWarnSubs: []string{"local_config", "user_config"},
		},
		{
			name:      "agentfile false suppresses user_config true warning when local_config explicitly sets false",
			agentfile: &bFalse,
			local:     &bFalse,
			user:      &bTrue,
			want:      false,
			wantSrc:   SourceAgentfile,
			wantWarn:  false,
		},
		{
			name:      "CLI --load-global overrides agentfile false with warning",
			agentfile: &bFalse,
			local:     &bFalse,
			user:      &bFalse,
			cliVal:    true,
			cliSet:    true,
			want:      true,
			wantSrc:   SourceCLI,
			wantWarn:  true,
		},
		{
			name:        "CLI --no-global overrides agentfile true and local/user true",
			agentfile:   &bTrue,
			local:       &bTrue,
			user:        &bTrue,
			cliNoGlobal: true,
			want:        false,
			wantSrc:     SourceCLI,
			wantWarn:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eff := &EffectiveConfig{
				UserLoadGlobal:  c.user,
				LocalLoadGlobal: c.local,
			}
			got, src, warn, err := ResolveTargetLoadGlobal(
				spec.TargetConfigSpec{LoadGlobal: c.agentfile},
				eff,
				c.cliVal,
				c.cliSet,
				c.cliNoGlobal,
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want || src != c.wantSrc {
				t.Fatalf("got (%v, %s), want (%v, %s)", got, src, c.want, c.wantSrc)
			}
			if c.wantWarn && warn == "" {
				t.Fatalf("expected non-empty warning, got empty")
			}
			if !c.wantWarn && warn != "" {
				t.Fatalf("expected no warning, got %q", warn)
			}
			for _, sub := range c.wantWarnSubs {
				if !containsSub(warn, sub) {
					t.Fatalf("expected warning %q to contain %q", warn, sub)
				}
			}
			for _, sub := range c.noWarnSubs {
				if containsSub(warn, sub) {
					t.Fatalf("expected warning %q NOT to contain %q", warn, sub)
				}
			}
		})
	}
}

func TestResolveOptionsExplicitAndIgnoreCI(t *testing.T) {
	clearTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CI", "true")

	ambientDir := filepath.Join(home, ".config", "agentfile")
	if err := os.MkdirAll(ambientDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ambientDir, "config.yaml"), []byte("version: 1\nlink_mode: symlink\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	proj := t.TempDir()

	// Under CI=true with IgnoreCI=false, ambient user config is skipped and on_conflict defaults to fail.
	effCI, err := ResolveWithOptions(proj, CLIOverrides{}, func() bool { return true }, ResolveOptions{})
	if err != nil {
		t.Fatalf("ResolveWithOptions under CI failed: %v", err)
	}
	if effCI.UserConfigLoaded || effCI.LinkMode != "copy" || effCI.OnConflict != "fail" {
		t.Fatalf("expected ambient user config skipped and on_conflict=fail under CI, got %+v", effCI)
	}

	// Under CI=true with IgnoreCI=true, ambient user config loads and on_conflict defaults to ask on TTY.
	effIgnore, err := ResolveWithOptions(proj, CLIOverrides{}, func() bool { return true }, ResolveOptions{IgnoreCI: true})
	if err != nil {
		t.Fatalf("ResolveWithOptions IgnoreCI failed: %v", err)
	}
	if !effIgnore.UserConfigLoaded || effIgnore.LinkMode != "symlink" || effIgnore.OnConflict != "ask" {
		t.Fatalf("expected IgnoreCI to load user config and default on_conflict=ask, got %+v", effIgnore)
	}

	// Explicit UserConfigPath overrides ambient discovery even when CI=true.
	explicitPath := filepath.Join(t.TempDir(), "explicit.yaml")
	if err := os.WriteFile(explicitPath, []byte("version: 1\non_conflict: backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	effExplicit, err := ResolveWithOptions(proj, CLIOverrides{}, func() bool { return false }, ResolveOptions{UserConfigPath: explicitPath})
	if err != nil {
		t.Fatalf("ResolveWithOptions explicit UserConfigPath failed: %v", err)
	}
	if !effExplicit.UserConfigLoaded || effExplicit.UserConfigPath != explicitPath || effExplicit.OnConflict != "backup" {
		t.Fatalf("unexpected explicit UserConfigPath result: %+v", effExplicit)
	}

	// Missing explicit UserConfigPath fails closed with PolicyError.
	var pe *spec.PolicyError
	if _, err := ResolveWithOptions(proj, CLIOverrides{}, nil, ResolveOptions{UserConfigPath: filepath.Join(t.TempDir(), "missing.yaml")}); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for missing UserConfigPath, got %v", err)
	}

	// AGENTFILE_CONFIG_FILE pointing to a directory fails closed with PolicyError.
	t.Setenv("AGENTFILE_CONFIG_FILE", t.TempDir())
	if _, _, err := DefaultUserConfigPath(); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError when AGENTFILE_CONFIG_FILE is a directory, got %v", err)
	}
}

func TestExplicitUserConfigErrorsReturnPolicyError(t *testing.T) {
	clearTestEnv(t)
	proj := t.TempDir()
	var pe *spec.PolicyError

	// 1. UserConfigPath pointing to a directory fails closed with PolicyError (exit 2).
	if _, err := ResolveWithOptions(proj, CLIOverrides{}, nil, ResolveOptions{UserConfigPath: t.TempDir()}); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError when UserConfigPath is a directory, got %T: %v", err, err)
	}

	// 2. AGENTFILE_CONFIG_FILE whose parent is a regular file (ENOTDIR from os.Stat) returns PolicyError.
	regularFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(regularFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFILE_CONFIG_FILE", filepath.Join(regularFile, "config.yaml"))
	if _, _, err := DefaultUserConfigPath(); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on ENOTDIR AGENTFILE_CONFIG_FILE, got %T: %v", err, err)
	}
	if _, err := ResolveWithOptions(proj, CLIOverrides{}, nil, ResolveOptions{}); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError from ResolveWithOptions on ENOTDIR AGENTFILE_CONFIG_FILE, got %T: %v", err, err)
	}

	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("skipping chmod 0000 permission tests when running as root or on Windows")
	}

	// 3. Unreadable AGENTFILE_CONFIG_FILE (0000 permissions) returns PolicyError from ResolveWithOptions.
	unreadable := filepath.Join(t.TempDir(), "unreadable.yaml")
	if err := os.WriteFile(unreadable, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	t.Setenv("AGENTFILE_CONFIG_FILE", unreadable)
	if _, err := ResolveWithOptions(proj, CLIOverrides{}, nil, ResolveOptions{}); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for unreadable AGENTFILE_CONFIG_FILE, got %T: %v", err, err)
	}

	// 4. Unreadable ResolveOptions.UserConfigPath (0000 permissions) returns PolicyError.
	t.Setenv("AGENTFILE_CONFIG_FILE", "")
	if _, err := ResolveWithOptions(proj, CLIOverrides{}, nil, ResolveOptions{UserConfigPath: unreadable}); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for unreadable UserConfigPath, got %T: %v", err, err)
	}

	// 5. AGENTFILE_CONFIG_FILE inside a 0000-permission parent directory (EACCES from os.Stat) returns PolicyError.
	lockedDir := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(lockedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockedDir, "config.yaml"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) })

	t.Setenv("AGENTFILE_CONFIG_FILE", filepath.Join(lockedDir, "config.yaml"))
	if _, _, err := DefaultUserConfigPath(); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for EACCES stat on AGENTFILE_CONFIG_FILE, got %T: %v", err, err)
	}
}

func containsSub(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexString(s, sub) >= 0)
}

func indexString(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
