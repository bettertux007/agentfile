package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/spec"
)

func TestPlanCommitPruneAndTargetIsolation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "local-skill")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Skill\n"), 0o644)

	cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: false}

	// Sync both claude and opencode initially.
	crClaude := &adapters.CompileResult{
		Target: "claude",
		Files: []adapters.ArtifactOp{
			{Target: "claude", RelPath: ".claude/rules/agentfile-r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("rule1\n")},
			{Target: "claude", RelPath: ".claude/skills/sk1", Kind: adapters.OpMaterializeTree, SourcePath: skillDir},
		},
	}
	crOpenCode := &adapters.CompileResult{
		Target: "opencode",
		Files: []adapters.ArtifactOp{
			{Target: "opencode", RelPath: ".opencode/agentfile-rules.md", Kind: adapters.OpWriteDropIn, Content: []byte("oc-rule\n")},
		},
	}

	plan1, err := BuildPlan(PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"claude", "opencode"},
		Config:          cfg,
		CompiledResults: []*adapters.CompileResult{crClaude, crOpenCode},
	})
	if err != nil {
		t.Fatalf("BuildPlan 1 failed: %v", err)
	}
	if err := Commit(plan1); err != nil {
		t.Fatalf("Commit 1 failed: %v", err)
	}

	// Sync only claude without rule r1 -> claude rule pruned, opencode file untouched!
	crClaude2 := &adapters.CompileResult{
		Target: "claude",
		Files: []adapters.ArtifactOp{
			{Target: "claude", RelPath: ".claude/skills/sk1", Kind: adapters.OpMaterializeTree, SourcePath: skillDir},
		},
	}
	plan2, err := BuildPlan(PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"claude"},
		Config:          cfg,
		CompiledResults: []*adapters.CompileResult{crClaude2},
	})
	if err != nil {
		t.Fatalf("BuildPlan 2 failed: %v", err)
	}
	if err := Commit(plan2); err != nil {
		t.Fatalf("Commit 2 failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".claude", "rules", "agentfile-r1.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected claude rule to be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "rules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected empty owned parent dir .claude/rules to be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, ".opencode", "agentfile-rules.md")); err != nil {
		t.Fatalf("expected opencode file to remain untouched during --targets claude: %v", err)
	}

	// Clean removes remaining tracked artifacts cleanly.
	sum, err := Clean(dir, nil, false)
	if err != nil {
		t.Fatalf("Clean failed: %v", err)
	}
	if len(sum.Pruned) != 2 {
		t.Fatalf("expected 2 pruned paths on Clean, got %v", sum.Pruned)
	}
}

func TestCommitRollbackOnMidFlightFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	origFile := filepath.Join(dir, ".claude", "rules", "r1.md")
	_ = os.MkdirAll(filepath.Dir(origFile), 0o755)
	_ = os.WriteFile(origFile, []byte("original"), 0o644)

	plan, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        &config.EffectiveConfig{LinkMode: "copy", OnConflict: "overwrite", GitExclude: false},
		CompiledResults: []*adapters.CompileResult{
			{
				Target: "claude",
				Files: []adapters.ArtifactOp{
					{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("updated")},
					{Target: "claude", RelPath: ".claude/rules/r2.md", Kind: adapters.OpWriteDropIn, Content: []byte("new")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}

	renameCalls := 0
	injectedErr := errors.New("simulated disk full")
	err = CommitWithDeps(plan, FSDeps{
		Rename: func(oldpath, newpath string) error {
			// Allow commit-journal rename and first file rename, fail on second file rename.
			if filepath.Base(newpath) == "r2.md" {
				return injectedErr
			}
			renameCalls++
			return os.Rename(oldpath, newpath)
		},
		Sleep: func(time.Duration) {},
	})
	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error, got %v", err)
	}

	// Rollback must restore original r1.md and delete r2.md and commit-journal.json.
	got, _ := os.ReadFile(origFile)
	if string(got) != "original" {
		t.Fatalf("expected r1.md rolled back to 'original', got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "rules", "r2.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected newly created r2.md deleted on rollback")
	}
	if _, err := os.Stat(JournalPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected commit journal cleaned after rollback")
	}
}

func TestRenameWithRetryTransientWindowsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	_ = os.WriteFile(src, []byte("ok"), 0o644)

	attempts := 0
	var sleeps []time.Duration
	err := renameWithRetry(src, dst, FSDeps{
		Rename: func(o, n string) error {
			attempts++
			if attempts < 3 {
				return errors.New("Access is denied")
			}
			return os.Rename(o, n)
		},
		Sleep: func(d time.Duration) { sleeps = append(sleeps, d) },
	})
	if err != nil {
		t.Fatalf("expected retry success, got %v", err)
	}
	if attempts != 3 || len(sleeps) != 2 || sleeps[0] != 10*time.Millisecond || sleeps[1] != 20*time.Millisecond {
		t.Fatalf("unexpected retry sequence: attempts=%d sleeps=%v", attempts, sleeps)
	}
}

func TestBuildPlanRejectsCrossTargetCollisionAndPolicyViolation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail"}

	_, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude", "opencode"},
		Config:        cfg,
		CompiledResults: []*adapters.CompileResult{
			{Target: "claude", Files: []adapters.ArtifactOp{{Target: "claude", RelPath: "shared.md", Kind: adapters.OpWriteOwned, Content: []byte("a")}}},
			{Target: "opencode", Files: []adapters.ArtifactOp{{Target: "opencode", RelPath: "Shared.md", Kind: adapters.OpWriteOwned, Content: []byte("b")}}},
		},
	})
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on case-insensitive collision, got %v", err)
	}

	_, err = BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        cfg,
		CompiledResults: []*adapters.CompileResult{
			{Target: "claude", Files: []adapters.ArtifactOp{{Target: "claude", RelPath: ".claude/settings.json", Kind: adapters.OpMergeJSONC, JSONMerge: &adapters.JSONMergeSpec{}}}},
		},
		ValidatePolicy: func(target string, merged map[string][]byte) []adapters.Diagnostic {
			return []adapters.Diagnostic{{Level: adapters.DiagError, Target: target, Message: "contradicts deny_commands"}}
		},
	})
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError from post-merge ValidatePolicy, got %v", err)
	}
}

type fixedPrompter struct {
	choice ConflictChoice
}

func (p fixedPrompter) AskConflict(_, _ string) (ConflictChoice, error) {
	return p.choice, nil
}

func TestSymlinkCopyTransitionsAskBackupAndJournalRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "local-skill")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Skill\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Agents\n"), 0o644)

	probes, err := CollectProbes(dir, []adapters.ProbeRequest{{Key: "agents", RelPath: "AGENTS.md"}, {Key: "missing", RelPath: "NOPE.md"}})
	if err != nil || !probes["agents"] || probes["missing"] {
		t.Fatalf("unexpected probe results: %+v err=%v", probes, err)
	}

	// Unmanaged file resolved via Ask -> ChoiceBackup.
	unmanaged := filepath.Join(dir, ".claude", "rules", "r1.md")
	_ = os.MkdirAll(filepath.Dir(unmanaged), 0o755)
	_ = os.WriteFile(unmanaged, []byte("handwritten"), 0o644)

	planAsk, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        &config.EffectiveConfig{LinkMode: "symlink", OnConflict: "ask", GitExclude: false},
		Prompter:      fixedPrompter{choice: ChoiceBackup},
		CompiledResults: []*adapters.CompileResult{
			{
				Target: "claude",
				Files: []adapters.ArtifactOp{
					{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("managed")},
					{Target: "claude", RelPath: ".claude/skills/sk1", Kind: adapters.OpMaterializeTree, SourcePath: skillDir},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan ask failed: %v", err)
	}
	if err := Commit(planAsk); err != nil {
		t.Fatalf("Commit ask failed: %v", err)
	}

	// Verify symlink created for skill, then switch link_mode -> copy (dir replaces symlink cleanly).
	skillOut := filepath.Join(dir, ".claude", "skills", "sk1")
	if info, lerr := os.Lstat(skillOut); lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected symlink at %s, got info=%v err=%v", skillOut, info, lerr)
	}

	planCopy, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: false},
		CompiledResults: []*adapters.CompileResult{
			{
				Target: "claude",
				Files: []adapters.ArtifactOp{
					{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("managed")},
					{Target: "claude", RelPath: ".claude/skills/sk1", Kind: adapters.OpMaterializeTree, SourcePath: skillDir},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan copy switch failed: %v", err)
	}
	if err := Commit(planCopy); err != nil {
		t.Fatalf("Commit copy switch failed: %v", err)
	}
	if info, lerr := os.Lstat(skillOut); lerr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected directory after symlink->copy transition, got info=%v err=%v", info, lerr)
	}

	// Simulate stale commit journal recovery.
	_ = os.WriteFile(filepath.Join(dir, "partial.tmp"), []byte("junk"), 0o644)
	_ = writeFileAtomic(JournalPath(dir), []byte(`{"backup_dir":".agentfile/backups/old","created_paths":["partial.tmp"]}`), 0o644)
	if err := RecoverJournal(dir); err != nil {
		t.Fatalf("RecoverJournal failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "partial.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected partial.tmp deleted by RecoverJournal")
	}

	// Invalid state.json version fails LoadState with PolicyError.
	_ = writeFileAtomic(StatePath(dir), []byte(`{"version":99,"targets":{}}`), 0o644)
	_, err = LoadState(dir)
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on bad state version, got %v", err)
	}
}

func TestIdempotentResyncAndJSONCPrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "sk")
	_ = os.MkdirAll(filepath.Join(skillDir, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, ".git", "HEAD"), []byte("ref"), 0o644)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Skill\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "GEMINI.md"), []byte("# User\n"), 0o644)

	cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: false}
	res := []*adapters.CompileResult{
		{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".mcp.json", Kind: adapters.OpMergeJSONC, JSONMerge: &adapters.JSONMergeSpec{MapEntries: map[string]any{"/mcpServers/gh": map[string]any{"url": "https://x"}}}},
				{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("r1")},
				{Target: "claude", RelPath: ".claude/skills/sk", Kind: adapters.OpMaterializeTree, SourcePath: skillDir},
			},
		},
		{
			Target: "gemini",
			Files: []adapters.ArtifactOp{
				{Target: "gemini", RelPath: "GEMINI.md", Kind: adapters.OpManagedBlock, Content: []byte("G1")},
			},
		},
	}

	p1, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude", "gemini"}, Config: cfg, CompiledResults: res})
	if err != nil || Commit(p1) != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	// Second sync with identical inputs must report Unchanged across all 4 artifacts.
	p2, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude", "gemini"}, Config: cfg, CompiledResults: res})
	if err != nil {
		t.Fatalf("second plan failed: %v", err)
	}
	if len(p2.Summary.Unchanged) != 4 || len(p2.Actions) != 0 {
		t.Fatalf("expected all 4 unchanged on idempotent resync, got summary=%+v actions=%d", p2.Summary, len(p2.Actions))
	}

	// Remove .mcp.json from claude ops -> Commit prunes .mcp.json cleanly.
	resNoMCP := []*adapters.CompileResult{
		{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("r1")},
				{Target: "claude", RelPath: ".claude/skills/sk", Kind: adapters.OpMaterializeTree, SourcePath: skillDir},
			},
		},
	}
	p3, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude"}, Config: cfg, CompiledResults: resNoMCP})
	if err != nil || Commit(p3) != nil {
		t.Fatalf("third sync failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected .mcp.json deleted on prune")
	}

	// File <-> Dir mismatch replacement and .DS_Store empty dir pruning.
	targetPath := filepath.Join(dir, ".claude", "skills", "sk")
	if err := os.RemoveAll(targetPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte("file-blocking-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	p4, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude"}, Config: &config.EffectiveConfig{LinkMode: "copy", OnConflict: "overwrite"}, CompiledResults: resNoMCP})
	if err != nil || Commit(p4) != nil {
		t.Fatalf("file->dir replacement failed: %v", err)
	}
	if info, err := os.Stat(targetPath); err != nil || !info.IsDir() {
		t.Fatalf("expected directory after file->dir replacement")
	}

	// removeIfEffectivelyEmpty removes directory containing only .DS_Store.
	dsDir := filepath.Join(dir, "ds-only")
	_ = os.MkdirAll(dsDir, 0o755)
	_ = os.WriteFile(filepath.Join(dsDir, ".DS_Store"), []byte("x"), 0o644)
	if !removeIfEffectivelyEmpty(dsDir) {
		t.Fatalf("expected ds-only directory to be removed")
	}
}

func TestConflictBackupAndJSONCRemovePartial(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Pre-existing unmanaged file + pre-existing .mcp.json with custom key.
	_ = os.MkdirAll(filepath.Join(dir, ".claude", "rules"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".claude", "rules", "r1.md"), []byte("user"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"gh":{"url":"https://old"}}}`), 0o644)

	res := []*adapters.CompileResult{
		{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".mcp.json", Kind: adapters.OpMergeJSONC, JSONMerge: &adapters.JSONMergeSpec{MapEntries: map[string]any{"/mcpServers/gh": map[string]any{"url": "https://new"}}}},
				{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("new")},
			},
		},
	}

	p, err := BuildPlan(PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"claude"},
		Config:          &config.EffectiveConfig{LinkMode: "copy", OnConflict: "backup"},
		CompiledResults: res,
	})
	if err != nil {
		t.Fatalf("BuildPlan backup failed: %v", err)
	}
	if err := Commit(p); err != nil {
		t.Fatalf("Commit backup failed: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ".agentfile", "backups"))
	if len(entries) == 0 {
		t.Fatalf("expected persistent backup directory under .agentfile/backups")
	}

	// Verify Clean updates .mcp.json when user adds another key before Clean.
	_ = os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"gh":{"url":"https://new"},"keep":{"command":"echo"}}}`), 0o644)
	sum, err := Clean(dir, nil, false)
	if err != nil || len(sum.Updated) != 1 {
		t.Fatalf("expected Clean to update .mcp.json, got sum=%+v err=%v", sum, err)
	}

	// Verify remote skill under link_mode=symlink emits warning and falls back to copy.
	remoteDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(remoteDir, "SKILL.md"), []byte("# Remote"), 0o644)
	pRemote, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        &config.EffectiveConfig{LinkMode: "symlink", OnConflict: "fail"},
		CompiledResults: []*adapters.CompileResult{{
			Target: "claude",
			Files:  []adapters.ArtifactOp{{Target: "claude", RelPath: ".claude/skills/r", Kind: adapters.OpMaterializeTree, SourcePath: remoteDir, IsRemote: true}},
		}},
	})
	if err != nil || len(pRemote.Summary.Warnings) == 0 {
		t.Fatalf("expected symlink fallback warning for remote dep: %v warnings=%v", err, pRemote.Summary.Warnings)
	}

	// Verify DiagError from adapter compilation aborts BuildPlan with PolicyError.
	_, err = BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail"},
		CompiledResults: []*adapters.CompileResult{{
			Target:      "claude",
			Diagnostics: []adapters.Diagnostic{{Level: adapters.DiagError, Target: "claude", Message: "fail closed"}},
		}},
	})
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on DiagError, got %v", err)
	}

	// Clean on empty directory with no state.json succeeds cleanly.
	emptyDir := t.TempDir()
	if sum, err := Clean(emptyDir, nil, false); err != nil || len(sum.Pruned) != 0 {
		t.Fatalf("expected clean no-op on empty dir: sum=%+v err=%v", sum, err)
	}

	// Empty BuildPlan with nil Config returns error.
	if _, err := BuildPlan(PlanInput{ProjectRoot: dir}); err == nil {
		t.Fatalf("expected error on nil Config")
	}
}

func TestCommitFastExitRespectsGitExcludeAndStaleJournal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res := []*adapters.CompileResult{{
		Target: "claude",
		Files:  []adapters.ArtifactOp{{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("r1")}},
	}}
	cfgTrue := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: true}
	p1, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude"}, Config: cfgTrue, CompiledResults: res})
	if err != nil || Commit(p1) != nil {
		t.Fatalf("p1 commit failed: %v", err)
	}

	// Changing git_exclude from true -> false changes ExcludeFingerprint and must NOT fast-exit (updates state.json).
	cfgFalse := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: false}
	p2, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude"}, Config: cfgFalse, CompiledResults: res})
	if err != nil {
		t.Fatal(err)
	}
	if err := Commit(p2); err != nil {
		t.Fatalf("p2 commit failed: %v", err)
	}
	stAfter, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if stAfter.ExcludeFingerprint != p2.NextState.ExcludeFingerprint {
		t.Fatalf("expected ExcludeFingerprint updated in state.json")
	}

	// Clean re-run with identical ExcludeFingerprint takes fast-exit without invoking Rename.
	p3, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude"}, Config: cfgFalse, CompiledResults: res})
	if err != nil {
		t.Fatal(err)
	}
	err = CommitWithDeps(p3, FSDeps{
		Rename: func(_, _ string) error {
			return errors.New("unexpected Rename during no-op fast exit")
		},
	})
	if err != nil {
		t.Fatalf("expected clean no-op fast exit, got %v", err)
	}

	// If a stale commit-journal.json exists, Commit must NOT take the fast exit and must recover/remove it.
	_ = os.WriteFile(filepath.Join(dir, "stale.tmp"), []byte("x"), 0o644)
	_ = writeFileAtomic(JournalPath(dir), []byte(`{"backup_dir":".agentfile/backups/old","created_paths":["stale.tmp"]}`), 0o644)
	p4, _ := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude"}, Config: cfgFalse, CompiledResults: res})
	if err := Commit(p4); err != nil {
		t.Fatalf("Commit with stale journal failed: %v", err)
	}
	if _, err := os.Stat(JournalPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale journal recovered and removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale.tmp deleted during journal recovery")
	}

	// Verify StatesEqual covers nil/pointer/record diffs cleanly.
	if !StatesEqual(nil, nil) || StatesEqual(nil, stAfter) {
		t.Fatalf("unexpected nil StatesEqual result")
	}
	stA := cloneState(stAfter)
	stA.Targets["claude"].OwnedDirs = nil
	stB := cloneState(stAfter)
	stB.Targets["claude"].OwnedDirs = []string{}
	if !StatesEqual(stA, stB) {
		t.Fatalf("expected nil vs empty OwnedDirs to compare equal")
	}
	stB.Targets["claude"].OwnedDirs = []string{".claude"}
	if StatesEqual(stA, stB) {
		t.Fatalf("expected OwnedDirs diff to compare unequal")
	}
}

func TestPlanManagedBlockPrunePartialCleanAndConflictPolicies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "GEMINI.md"), []byte("# User Intro\n"), 0o644)

	cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: false}
	plan1, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"gemini", "claude"},
		Config:        cfg,
		CompiledResults: []*adapters.CompileResult{
			{Target: "gemini", Files: []adapters.ArtifactOp{{Target: "gemini", RelPath: "GEMINI.md", Kind: adapters.OpManagedBlock, Content: []byte("Rule 1")}}},
			{Target: "claude", Files: []adapters.ArtifactOp{{Target: "claude", RelPath: ".claude/rules/r.md", Kind: adapters.OpWriteDropIn, Content: []byte("Claude rule")}}},
		},
	})
	if err != nil || Commit(plan1) != nil {
		t.Fatalf("plan1/commit1 failed: %v", err)
	}

	// Prune gemini managed block when rules become empty.
	plan2, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"gemini"},
		Config:        cfg,
		CompiledResults: []*adapters.CompileResult{
			{Target: "gemini", Files: nil},
		},
	})
	if err != nil || Commit(plan2) != nil {
		t.Fatalf("plan2/commit2 failed: %v", err)
	}
	gemBytes, _ := os.ReadFile(filepath.Join(dir, "GEMINI.md"))
	if string(gemBytes) != "# User Intro\n" {
		t.Fatalf("expected GEMINI.md restored to user content, got %q", gemBytes)
	}

	// Partial Clean --targets claude (dry-run then real).
	dry, err := Clean(dir, []string{"claude"}, true)
	if err != nil || len(dry.Pruned) != 1 {
		t.Fatalf("unexpected dry clean: %+v err=%v", dry, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "rules", "r.md")); err != nil {
		t.Fatalf("dry clean must not delete files")
	}
	if _, err := Clean(dir, []string{"claude"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "rules", "r.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected claude rule deleted by Clean")
	}

	// Conflict policy branches: skip, abort, nil prompter, invalid policy.
	if ow, _, err := resolveConflictChoice("a", "r", "ask", fixedPrompter{choice: ChoiceSkip}); err != nil || ow {
		t.Fatalf("expected skip to return false, nil")
	}
	if _, _, err := resolveConflictChoice("a", "r", "ask", fixedPrompter{choice: ChoiceAbort}); err == nil {
		t.Fatalf("expected abort error")
	}
	if _, _, err := resolveConflictChoice("a", "r", "ask", nil); err == nil {
		t.Fatalf("expected error with nil prompter")
	}
	if _, _, err := resolveConflictChoice("a", "r", "bogus", nil); err == nil {
		t.Fatalf("expected error on unknown conflict policy")
	}
}
