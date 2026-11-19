package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

type panicPrompter struct{}

func (panicPrompter) AskConflict(relPath, reason string) (ConflictChoice, error) {
	panic("Prompter must not be called when InspectConflicts is true: " + relPath + " (" + reason + ")")
}

func TestBuildPlanInspectConflicts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgAsk := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "ask", GitExclude: false}

	// First sync an owned file that will be pruned on the inspect pass so we test ActionDeletePath OldContent too.
	initPlan, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        &config.EffectiveConfig{LinkMode: "copy", OnConflict: "overwrite", GitExclude: false},
		CompiledResults: []*adapters.CompileResult{{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".claude/rules/agentfile-old.md", Kind: adapters.OpWriteDropIn, Content: []byte("old-owned-body\n")},
			},
		}},
	})
	if err != nil {
		t.Fatalf("init BuildPlan failed: %v", err)
	}
	if err := Commit(initPlan); err != nil {
		t.Fatalf("init Commit failed: %v", err)
	}

	// Create unmanaged .claude/rules/agentfile-r.md and pre-existing .mcp.json with 2 conflicting entries.
	_ = os.MkdirAll(filepath.Join(dir, ".claude", "rules"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".claude", "rules", "agentfile-r.md"), []byte("handwritten-rule\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"gh":{"url":"https://old-gh"},"db":{"url":"https://old-db"}}}`), 0o644)

	// Create an oversized (> 1 MiB) unmanaged file to ensure OldContent stays nil while OldKind == "file".
	bigBytes := make([]byte, maxOldContentBytes+1)
	for i := range bigBytes {
		bigBytes[i] = 'a'
	}
	_ = os.WriteFile(filepath.Join(dir, ".claude", "rules", "agentfile-big.md"), bigBytes, 0o644)

	compiled := []*adapters.CompileResult{{
		Target: "claude",
		Files: []adapters.ArtifactOp{
			{
				Target:  "claude",
				RelPath: ".mcp.json",
				Kind:    adapters.OpMergeJSONC,
				JSONMerge: &adapters.JSONMergeSpec{
					MapEntries: map[string]any{
						"/mcpServers/gh": map[string]any{"url": "https://new-gh"},
						"/mcpServers/db": map[string]any{"url": "https://new-db"},
					},
				},
			},
			{Target: "claude", RelPath: ".claude/rules/agentfile-r.md", Kind: adapters.OpWriteDropIn, Content: []byte("managed-rule\n")},
			{Target: "claude", RelPath: ".claude/rules/agentfile-big.md", Kind: adapters.OpWriteDropIn, Content: []byte("small-new\n")},
		},
	}}

	for _, policy := range []string{"ask", "fail"} {
		cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: policy, GitExclude: false}
		plan, err := BuildPlan(PlanInput{
			ProjectRoot:      dir,
			ActiveTargets:    []string{"claude"},
			Config:           cfg,
			CompiledResults:  compiled,
			Prompter:         panicPrompter{},
			InspectConflicts: true,
		})
		if err != nil {
			t.Fatalf("BuildPlan InspectConflicts with policy=%s failed: %v", policy, err)
		}
		if len(plan.Conflicts) != 3 {
			t.Fatalf("policy=%s: expected 3 deduplicated conflicts (.mcp.json, agentfile-r.md, agentfile-big.md), got %+v", policy, plan.Conflicts)
		}
		if len(plan.Summary.Conflicts) != len(plan.Conflicts) {
			t.Fatalf("policy=%s: Summary.Conflicts mismatch: %+v vs %+v", policy, plan.Summary.Conflicts, plan.Conflicts)
		}

		// Verify deduplication by RelPath.
		seen := map[string]bool{}
		for _, c := range plan.Conflicts {
			if c.Target != "claude" || c.Reason == "" {
				t.Fatalf("invalid conflict entry: %+v", c)
			}
			if seen[c.RelPath] {
				t.Fatalf("duplicate conflict for %s", c.RelPath)
			}
			seen[c.RelPath] = true
		}
		if !seen[".mcp.json"] || !seen[".claude/rules/agentfile-r.md"] || !seen[".claude/rules/agentfile-big.md"] {
			t.Fatalf("missing expected conflict paths: %+v", seen)
		}

		// Verify OldContent captured on ActionWriteFile and ActionDeletePath, and omitted when > 1 MiB.
		byPath := map[string]PlannedAction{}
		for _, a := range plan.Actions {
			byPath[a.RelPath] = a
		}
		if act, ok := byPath[".claude/rules/agentfile-r.md"]; !ok || act.Type != ActionWriteFile || string(act.OldContent) != "handwritten-rule\n" || act.OldKind != "file" {
			t.Fatalf("unexpected ActionWriteFile for agentfile-r.md: %+v", act)
		}
		if act, ok := byPath[".mcp.json"]; !ok || act.Type != ActionWriteFile || len(act.OldContent) == 0 || act.OldKind != "file" {
			t.Fatalf("unexpected ActionWriteFile for .mcp.json: %+v", act)
		}
		if act, ok := byPath[".claude/rules/agentfile-big.md"]; !ok || act.Type != ActionWriteFile || act.OldContent != nil || act.OldKind != "file" {
			t.Fatalf("expected nil OldContent for >1 MiB file, got len=%d kind=%q", len(act.OldContent), act.OldKind)
		}

		if len(plan.PruneOps) != 1 {
			t.Fatalf("expected 1 prune op for agentfile-old.md, got %d", len(plan.PruneOps))
		}
		delOp := plan.PruneOps[0]
		if delOp.Type != ActionDeletePath || delOp.RelPath != ".claude/rules/agentfile-old.md" || string(delOp.OldContent) != "old-owned-body\n" || delOp.OldKind != "file" {
			t.Fatalf("unexpected prune ActionDeletePath: %+v", delOp)
		}
	}

	// With OnConflict: "overwrite", plan.Conflicts remains empty even with InspectConflicts: true.
	_ = cfgAsk
	planOW, err := BuildPlan(PlanInput{
		ProjectRoot:      dir,
		ActiveTargets:    []string{"claude"},
		Config:           &config.EffectiveConfig{LinkMode: "copy", OnConflict: "overwrite", GitExclude: false},
		CompiledResults:  compiled,
		InspectConflicts: true,
	})
	if err != nil {
		t.Fatalf("BuildPlan overwrite failed: %v", err)
	}
	if len(planOW.Conflicts) != 0 || len(planOW.Summary.Conflicts) != 0 {
		t.Fatalf("expected empty Conflicts with OnConflict=overwrite, got %+v", planOW.Conflicts)
	}
}

func TestBuildPlanOldKindAndLinkTransitions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	srcSkill := filepath.Join(dir, "src-skill")
	_ = os.MkdirAll(srcSkill, 0o755)
	_ = os.WriteFile(filepath.Join(srcSkill, "SKILL.md"), []byte("# Skill\n"), 0o644)

	// 1. Existing directory -> ActionSymlink captures OldKind: "dir".
	sk1Dir := filepath.Join(dir, ".claude", "skills", "sk1")
	_ = os.MkdirAll(sk1Dir, 0o755)
	_ = os.WriteFile(filepath.Join(sk1Dir, "old.txt"), []byte("old"), 0o644)

	planSym, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        &config.EffectiveConfig{LinkMode: "symlink", OnConflict: "overwrite"},
		CompiledResults: []*adapters.CompileResult{{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".claude/skills/sk1", Kind: adapters.OpMaterializeTree, SourcePath: srcSkill},
			},
		}},
	})
	if err != nil {
		t.Fatalf("BuildPlan dir->symlink failed: %v", err)
	}
	if len(planSym.Actions) != 1 || planSym.Actions[0].Type != ActionSymlink || planSym.Actions[0].OldKind != "dir" || planSym.Actions[0].OldLinkTarget != "" {
		t.Fatalf("expected ActionSymlink with OldKind=dir, got %+v", planSym.Actions)
	}

	// Commit so sk1 becomes an owned symlink, then test symlink -> ActionCopyTree and symlink -> ActionDeletePath.
	if err := Commit(planSym); err != nil {
		t.Fatalf("Commit symlink failed: %v", err)
	}

	// 2. Existing symlink -> ActionCopyTree captures OldKind: "symlink" and OldLinkTarget.
	planCopy, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        &config.EffectiveConfig{LinkMode: "copy", OnConflict: "overwrite"},
		CompiledResults: []*adapters.CompileResult{{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".claude/skills/sk1", Kind: adapters.OpMaterializeTree, SourcePath: srcSkill},
			},
		}},
	})
	if err != nil {
		t.Fatalf("BuildPlan symlink->copy failed: %v", err)
	}
	if len(planCopy.Actions) != 1 || planCopy.Actions[0].Type != ActionCopyTree || planCopy.Actions[0].OldKind != "symlink" || planCopy.Actions[0].OldLinkTarget != srcSkill {
		t.Fatalf("expected ActionCopyTree with OldKind=symlink OldLinkTarget=%q, got %+v", srcSkill, planCopy.Actions)
	}

	// 3. Existing symlink -> ActionDeletePath (when pruned) captures OldKind: "symlink" and OldLinkTarget.
	planPruneSym, err := BuildPlan(PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"claude"},
		Config:          &config.EffectiveConfig{LinkMode: "symlink", OnConflict: "overwrite"},
		CompiledResults: []*adapters.CompileResult{{Target: "claude", Files: nil}},
	})
	if err != nil {
		t.Fatalf("BuildPlan prune symlink failed: %v", err)
	}
	if len(planPruneSym.PruneOps) != 1 || planPruneSym.PruneOps[0].Type != ActionDeletePath || planPruneSym.PruneOps[0].OldKind != "symlink" || planPruneSym.PruneOps[0].OldLinkTarget != srcSkill {
		t.Fatalf("expected ActionDeletePath with OldKind=symlink OldLinkTarget=%q, got %+v", srcSkill, planPruneSym.PruneOps)
	}
}

func TestCheckWorkspaceDrift(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: false}
	res := []*adapters.CompileResult{{
		Target: "claude",
		Files: []adapters.ArtifactOp{
			{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("v1\n")},
		},
	}}

	// 1. Before initial sync: Actions > 0 and missing state.json -> drifted.
	planInit, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude"}, Config: cfg, CompiledResults: res})
	if err != nil {
		t.Fatal(err)
	}
	if drifted, reason := CheckWorkspaceDrift(dir, planInit); !drifted || reason == "" {
		t.Fatalf("expected drift before initial sync, got drifted=%v reason=%q", drifted, reason)
	}
	if err := Commit(planInit); err != nil {
		t.Fatal(err)
	}

	// 2. Clean workspace -> no drift.
	planClean, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude"}, Config: cfg, CompiledResults: res})
	if err != nil {
		t.Fatal(err)
	}
	if drifted, reason := CheckWorkspaceDrift(dir, planClean); drifted || reason != "" {
		t.Fatalf("expected no drift on clean workspace, got drifted=%v reason=%q", drifted, reason)
	}

	// 3. Actions > 0 (content modified in manifest) -> drifted.
	planMod, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        cfg,
		CompiledResults: []*adapters.CompileResult{{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("v2\n")},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if drifted, reason := CheckWorkspaceDrift(dir, planMod); !drifted || reason == "" {
		t.Fatalf("expected drift on Actions > 0, got drifted=%v reason=%q", drifted, reason)
	}

	// 4. PruneOps > 0 -> drifted.
	planPrune, err := BuildPlan(PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"claude"},
		Config:          cfg,
		CompiledResults: []*adapters.CompileResult{{Target: "claude", Files: nil}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if drifted, reason := CheckWorkspaceDrift(dir, planPrune); !drifted || reason == "" {
		t.Fatalf("expected drift on PruneOps > 0, got drifted=%v reason=%q", drifted, reason)
	}

	// 5. Conflicts > 0 -> drifted (even if we construct a conflict-only plan or via InspectConflicts).
	_ = os.WriteFile(filepath.Join(dir, ".claude", "rules", "r2.md"), []byte("unmanaged"), 0o644)
	planConflict, err := BuildPlan(PlanInput{
		ProjectRoot:      dir,
		ActiveTargets:    []string{"claude"},
		Config:           cfg,
		InspectConflicts: true,
		CompiledResults: []*adapters.CompileResult{{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("v1\n")},
				{Target: "claude", RelPath: ".claude/rules/r2.md", Kind: adapters.OpWriteDropIn, Content: []byte("managed\n")},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if drifted, reason := CheckWorkspaceDrift(dir, planConflict); !drifted || reason == "" {
		t.Fatalf("expected drift on Conflicts > 0, got drifted=%v reason=%q", drifted, reason)
	}
	_ = os.Remove(filepath.Join(dir, ".claude", "rules", "r2.md"))

	// 6. Pending commit-journal.json -> drifted.
	_ = writeFileAtomic(JournalPath(dir), []byte(`{"backup_dir":".agentfile/backups/old"}`), 0o644)
	if drifted, reason := CheckWorkspaceDrift(dir, planClean); !drifted || reason == "" {
		t.Fatalf("expected drift on pending commit-journal.json, got drifted=%v reason=%q", drifted, reason)
	}
	_ = os.Remove(JournalPath(dir))

	// 7. ExcludeFingerprint mismatch -> drifted.
	cfgGitExclude := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: true}
	planExcl, err := BuildPlan(PlanInput{ProjectRoot: dir, ActiveTargets: []string{"claude"}, Config: cfgGitExclude, CompiledResults: res})
	if err != nil {
		t.Fatal(err)
	}
	if drifted, reason := CheckWorkspaceDrift(dir, planExcl); !drifted || reason == "" {
		t.Fatalf("expected drift on ExcludeFingerprint mismatch, got drifted=%v reason=%q", drifted, reason)
	}

	// 8. Missing .agentfile/state.json -> drifted.
	_ = os.Remove(StatePath(dir))
	if drifted, reason := CheckWorkspaceDrift(dir, planClean); !drifted || reason == "" {
		t.Fatalf("expected drift on missing state.json, got drifted=%v reason=%q", drifted, reason)
	}
}

func TestCommitSidecarWritesAndRollback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	yamlPath := filepath.Join(dir, "Agentfile.yaml")
	lockPath := filepath.Join(dir, "Agentfile.lock")
	rulePath := filepath.Join(dir, ".claude", "rules", "r1.md")
	_ = os.MkdirAll(filepath.Dir(rulePath), 0o755)
	_ = os.WriteFile(yamlPath, []byte("version: 1\n# orig yaml\n"), 0o644)
	_ = os.WriteFile(lockPath, []byte("version: 1\n# orig lock\n"), 0o644)
	_ = os.WriteFile(rulePath, []byte("orig rule\n"), 0o644)

	cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "overwrite", GitExclude: false}
	plan, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        cfg,
		CompiledResults: []*adapters.CompileResult{{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("new rule\n")},
			},
		}},
	})
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	plan.SidecarWrites = []SidecarWrite{
		{RelPath: "Agentfile.yaml", Content: []byte("version: 1\n# new yaml\n"), Mode: 0o644},
		{RelPath: "Agentfile.lock", Content: []byte("version: 1\n# new lock\n"), Mode: 0o644},
		{RelPath: "new-sidecar.txt", Content: []byte("created\n"), Mode: 0o644},
	}

	injectedErr := errors.New("simulated sidecar write failure")
	failedOnce := false
	var observedJournal []byte
	var observedBackupYAML []byte
	var observedBackupLock []byte

	err = CommitWithDeps(plan, FSDeps{
		Rename: func(oldpath, newpath string) error {
			if filepath.Base(newpath) == "Agentfile.lock" && !failedOnce {
				failedOnce = true
				observedJournal, _ = os.ReadFile(JournalPath(dir))
				// Read backup contents right before failing.
				bakRoot := filepath.Join(dir, ".agentfile", "backups")
				entries, _ := os.ReadDir(bakRoot)
				if len(entries) > 0 {
					observedBackupYAML, _ = os.ReadFile(filepath.Join(bakRoot, entries[0].Name(), "Agentfile.yaml"))
					observedBackupLock, _ = os.ReadFile(filepath.Join(bakRoot, entries[0].Name(), "Agentfile.lock"))
				}
				return injectedErr
			}
			return os.Rename(oldpath, newpath)
		},
		Sleep: func(time.Duration) {},
	})
	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected injectedErr, got %v", err)
	}

	// Verify commit-journal.json and .agentfile/backups/ captured sidecars before mutation.
	if len(observedJournal) == 0 {
		t.Fatalf("expected commit-journal.json to exist during sidecar write")
	}
	var cj CommitJournal
	if err := json.Unmarshal(observedJournal, &cj); err != nil {
		t.Fatalf("unmarshal observed commit-journal.json: %v", err)
	}
	bakMap := map[string]bool{}
	for _, b := range cj.Backups {
		bakMap[b.OrigRelPath] = true
	}
	if !bakMap["Agentfile.yaml"] || !bakMap["Agentfile.lock"] {
		t.Fatalf("expected Agentfile.yaml and Agentfile.lock in commit-journal backups, got %+v", cj.Backups)
	}
	createdMap := map[string]bool{}
	for _, cp := range cj.CreatedPaths {
		createdMap[cp] = true
	}
	if !createdMap["new-sidecar.txt"] {
		t.Fatalf("expected new-sidecar.txt in commit-journal created_paths, got %+v", cj.CreatedPaths)
	}
	if string(observedBackupYAML) != "version: 1\n# orig yaml\n" {
		t.Fatalf("expected Agentfile.yaml backed up prior to mutation, got %q", observedBackupYAML)
	}
	if string(observedBackupLock) != "version: 1\n# orig lock\n" {
		t.Fatalf("expected Agentfile.lock backed up prior to mutation, got %q", observedBackupLock)
	}

	// Verify rollback restored workspace files AND Agentfile.yaml / Agentfile.lock and removed new-sidecar.txt.
	if got, _ := os.ReadFile(rulePath); string(got) != "orig rule\n" {
		t.Fatalf("expected r1.md rolled back to 'orig rule\\n', got %q", got)
	}
	if got, _ := os.ReadFile(yamlPath); string(got) != "version: 1\n# orig yaml\n" {
		t.Fatalf("expected Agentfile.yaml rolled back, got %q", got)
	}
	if got, _ := os.ReadFile(lockPath); string(got) != "version: 1\n# orig lock\n" {
		t.Fatalf("expected Agentfile.lock rolled back, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "new-sidecar.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected new-sidecar.txt deleted on rollback")
	}
	if _, err := os.Stat(JournalPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected commit-journal.json removed after rollback")
	}

	// Now commit successfully, then verify no-op Actions with non-empty SidecarWrites does NOT fast-exit.
	plan.SidecarWrites = plan.SidecarWrites[:2]
	if err := Commit(plan); err != nil {
		t.Fatalf("Commit with SidecarWrites failed: %v", err)
	}
	if got, _ := os.ReadFile(yamlPath); string(got) != "version: 1\n# new yaml\n" {
		t.Fatalf("expected Agentfile.yaml updated, got %q", got)
	}
	if got, _ := os.ReadFile(lockPath); string(got) != "version: 1\n# new lock\n" {
		t.Fatalf("expected Agentfile.lock updated, got %q", got)
	}
	st, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Targets["claude"].Artifacts["Agentfile.yaml"]; ok {
		t.Fatalf("SidecarWrites must never be added to state.json")
	}
	for _, ex := range plan.AllExcludePath {
		if ex == "Agentfile.yaml" || ex == "Agentfile.lock" {
			t.Fatalf("SidecarWrites must never be added to AllExcludePath: %v", plan.AllExcludePath)
		}
	}

	// Sidecar-only commit when Actions and PruneOps are empty must NOT take no-op fast exit.
	sidecarOnlyPlan, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"claude"},
		Config:        cfg,
		CompiledResults: []*adapters.CompileResult{{
			Target: "claude",
			Files: []adapters.ArtifactOp{
				{Target: "claude", RelPath: ".claude/rules/r1.md", Kind: adapters.OpWriteDropIn, Content: []byte("new rule\n")},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sidecarOnlyPlan.Actions) != 0 || len(sidecarOnlyPlan.PruneOps) != 0 {
		t.Fatalf("expected 0 actions/prunes, got %d/%d", len(sidecarOnlyPlan.Actions), len(sidecarOnlyPlan.PruneOps))
	}
	sidecarOnlyPlan.SidecarWrites = []SidecarWrite{
		{RelPath: "Agentfile.yaml", Content: []byte("version: 1\n# sidecar-only\n"), Mode: 0o644},
	}
	if err := Commit(sidecarOnlyPlan); err != nil {
		t.Fatalf("sidecar-only Commit failed: %v", err)
	}
	if got, _ := os.ReadFile(yamlPath); string(got) != "version: 1\n# sidecar-only\n" {
		t.Fatalf("expected sidecar-only Commit not to fast-exit, got %q", got)
	}

	// Sidecar path escaping project root must fail closed with PolicyError.
	sidecarOnlyPlan.SidecarWrites = []SidecarWrite{
		{RelPath: "../escape.yaml", Content: []byte("bad"), Mode: 0o644},
	}
	err = Commit(sidecarOnlyPlan)
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on escaping sidecar path, got %v", err)
	}
}

func TestSharedArtifactOwnershipLifecycleAndValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "local-skill")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Auditor\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Existing Agents\n"), 0o644)

	cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail", GitExclude: false}

	// 1. Two adapters emit identical Shared: true OpManagedBlock (AGENTS.md) and OpMaterializeTree (.agents/skills/auditor).
	resCodex := &adapters.CompileResult{
		Target: "codex",
		Files: []adapters.ArtifactOp{
			{Target: "codex", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("Shared rules")},
			{Target: "codex", RelPath: ".agents/skills/auditor", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir},
		},
	}
	resCopilot := &adapters.CompileResult{
		Target: "copilot",
		Files: []adapters.ArtifactOp{
			{Target: "copilot", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("Shared rules")},
			{Target: "copilot", RelPath: ".agents/skills/auditor", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir},
			{Target: "copilot", RelPath: ".github/copilot-instructions.md", Kind: adapters.OpWriteDropIn, Content: []byte("Copilot only\n")},
		},
	}

	plan1, err := BuildPlan(PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"codex", "copilot"},
		Config:          cfg,
		CompiledResults: []*adapters.CompileResult{resCodex, resCopilot},
	})
	if err != nil {
		t.Fatalf("BuildPlan shared failed: %v", err)
	}
	// Must emit a single action per shared artifact (2 shared + 1 copilot-only = 3).
	if len(plan1.Actions) != 3 {
		t.Fatalf("expected 3 planned actions (deduped shared ops), got %d: %+v", len(plan1.Actions), plan1.Actions)
	}
	for _, a := range plan1.Actions {
		if (a.RelPath == "AGENTS.md" || a.RelPath == ".agents/skills/auditor") && a.Target != "shared" {
			t.Fatalf("expected shared PlannedAction.Target == 'shared', got %q for %s", a.Target, a.RelPath)
		}
	}
	if len(plan1.NextState.Shared) != 2 {
		t.Fatalf("expected 2 entries in NextState.Shared, got %+v", plan1.NextState.Shared)
	}
	if gotOwners := plan1.NextState.Shared["AGENTS.md"].Owners; len(gotOwners) != 2 || gotOwners[0] != "codex" || gotOwners[1] != "copilot" {
		t.Fatalf("unexpected AGENTS.md owners: %v", gotOwners)
	}
	if gotOwners := plan1.NextState.Shared[".agents/skills/auditor"].Owners; len(gotOwners) != 2 || gotOwners[0] != "codex" || gotOwners[1] != "copilot" {
		t.Fatalf("unexpected .agents/skills/auditor owners: %v", gotOwners)
	}
	if len(plan1.NextState.SharedOwnedDirs) == 0 {
		t.Fatalf("expected .agents/skills and .agents in SharedOwnedDirs")
	}
	if len(plan1.AllExcludePath) != 3 {
		t.Fatalf("expected 3 paths in AllExcludePath (including Shared), got %v", plan1.AllExcludePath)
	}
	for _, td := range plan1.NextState.Targets["codex"].OwnedDirs {
		if td == ".agents" || td == ".agents/skills" {
			t.Fatalf("shared dirs must not be placed in target OwnedDirs: %v", plan1.NextState.Targets["codex"].OwnedDirs)
		}
	}

	if err := Commit(plan1); err != nil {
		t.Fatalf("Commit shared failed: %v", err)
	}
	agentsBytes, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(agentsBytes), "# Existing Agents") || !strings.Contains(string(agentsBytes), "Shared rules") {
		t.Fatalf("unexpected AGENTS.md content:\n%s", agentsBytes)
	}

	// Idempotent resync reports all 3 artifacts unchanged and 0 actions.
	planIdem, err := BuildPlan(PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"codex", "copilot"},
		Config:          cfg,
		CompiledResults: []*adapters.CompileResult{resCodex, resCopilot},
	})
	if err != nil {
		t.Fatalf("idempotent BuildPlan failed: %v", err)
	}
	if len(planIdem.Actions) != 0 || len(planIdem.Summary.Unchanged) != 3 {
		t.Fatalf("expected 0 actions and 3 unchanged on resync, got actions=%d summary=%+v", len(planIdem.Actions), planIdem.Summary)
	}

	// Subset sync (--targets codex) where codex STILL emits shared ops preserves copilot in Owners.
	planSubsetKeep, err := BuildPlan(PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"codex"},
		Config:          cfg,
		CompiledResults: []*adapters.CompileResult{resCodex},
	})
	if err != nil {
		t.Fatalf("subset-keep BuildPlan failed: %v", err)
	}
	if got := planSubsetKeep.NextState.Shared["AGENTS.md"].Owners; len(got) != 2 || got[0] != "codex" || got[1] != "copilot" {
		t.Fatalf("expected AGENTS.md owners=[codex copilot] preserved on subset sync, got %v", got)
	}

	// 2. Subset sync (--targets codex) where codex stops emitting shared ops keeps shared artifacts alive for copilot.
	planSubset, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"codex"},
		Config:        cfg,
		CompiledResults: []*adapters.CompileResult{
			{Target: "codex", Files: nil},
		},
	})
	if err != nil {
		t.Fatalf("subset BuildPlan failed: %v", err)
	}
	if len(planSubset.PruneOps) != 0 {
		t.Fatalf("expected 0 PruneOps while copilot still owns shared artifacts, got %+v", planSubset.PruneOps)
	}
	if got := planSubset.NextState.Shared["AGENTS.md"].Owners; len(got) != 1 || got[0] != "copilot" {
		t.Fatalf("expected AGENTS.md owners=[copilot] after codex dropped it, got %v", got)
	}
	if err := Commit(planSubset); err != nil {
		t.Fatalf("subset Commit failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents", "skills", "auditor", "SKILL.md")); err != nil {
		t.Fatalf("expected shared skill preserved while copilot owns it: %v", err)
	}

	// Re-sync codex + copilot so both own shared artifacts again, then test subset Clean.
	planRestore, err := BuildPlan(PlanInput{
		ProjectRoot:     dir,
		ActiveTargets:   []string{"codex", "copilot"},
		Config:          cfg,
		CompiledResults: []*adapters.CompileResult{resCodex, resCopilot},
	})
	if err != nil || Commit(planRestore) != nil {
		t.Fatalf("restore shared ownership failed: %v", err)
	}

	// Subset Clean("codex") removes codex from Owners without deleting shared files.
	cleanCodex, err := Clean(dir, []string{"codex"}, false)
	if err != nil {
		t.Fatalf("Clean(codex) failed: %v", err)
	}
	if len(cleanCodex.Pruned) != 0 || len(cleanCodex.Updated) != 0 {
		t.Fatalf("expected no shared files pruned/updated on Clean(codex), got %+v", cleanCodex)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents", "skills", "auditor", "SKILL.md")); err != nil {
		t.Fatalf("expected shared skill kept after Clean(codex): %v", err)
	}
	stAfterCodexClean, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := stAfterCodexClean.Shared["AGENTS.md"].Owners; len(got) != 1 || got[0] != "copilot" {
		t.Fatalf("expected AGENTS.md owners=[copilot] after Clean(codex), got %v", got)
	}

	// Subset Clean("copilot") removes last owner -> prunes shared block in AGENTS.md, deletes .agents/skills/auditor & SharedOwnedDirs, removes state.json.
	cleanCopilot, err := Clean(dir, []string{"copilot"}, false)
	if err != nil {
		t.Fatalf("Clean(copilot) failed: %v", err)
	}
	if len(cleanCopilot.Pruned) != 2 || len(cleanCopilot.Updated) != 1 {
		t.Fatalf("expected 2 pruned (.agents/skills/auditor, copilot-instructions.md) and 1 updated (AGENTS.md), got %+v", cleanCopilot)
	}
	agentsAfterClean, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if string(agentsAfterClean) != "# Existing Agents\n" {
		t.Fatalf("expected AGENTS.md restored to original content, got %q", agentsAfterClean)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected empty SharedOwnedDirs .agents to be pruned")
	}
	if _, err := os.Stat(StatePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected state.json removed after all targets and shared artifacts cleaned")
	}
}

func TestSharedArtifactCollisionAndMismatchPolicies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skillA := filepath.Join(dir, "skill-a")
	skillB := filepath.Join(dir, "skill-b")
	_ = os.MkdirAll(skillA, 0o755)
	_ = os.MkdirAll(skillB, 0o755)
	_ = os.WriteFile(filepath.Join(skillA, "SKILL.md"), []byte("# A"), 0o644)
	_ = os.WriteFile(filepath.Join(skillB, "SKILL.md"), []byte("# B"), 0o644)

	cfg := &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail"}

	cases := []struct {
		name string
		ops  []*adapters.CompileResult
	}{
		{
			name: "differ_in_kind",
			ops: []*adapters.CompileResult{
				{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("x")}}},
				{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: "AGENTS.md", Kind: adapters.OpWriteOwned, Shared: true, Content: []byte("x")}}},
			},
		},
		{
			name: "differ_in_content",
			ops: []*adapters.CompileResult{
				{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("x")}}},
				{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("y")}}},
			},
		},
		{
			name: "differ_in_source_path",
			ops: []*adapters.CompileResult{
				{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: ".agents/skills/s", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillA}}},
				{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: ".agents/skills/s", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillB}}},
			},
		},
		{
			name: "differ_in_is_remote",
			ops: []*adapters.CompileResult{
				{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: ".agents/skills/s", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillA, IsRemote: false}}},
				{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: ".agents/skills/s", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillA, IsRemote: true}}},
			},
		},
		{
			name: "differ_only_in_case",
			ops: []*adapters.CompileResult{
				{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("x")}}},
				{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: "agents.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("x")}}},
			},
		},
		{
			name: "mix_shared_true_and_false",
			ops: []*adapters.CompileResult{
				{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("x")}}},
				{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: false, Content: []byte("x")}}},
			},
		},
		{
			name: "mix_shared_false_and_true",
			ops: []*adapters.CompileResult{
				{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: false, Content: []byte("x")}}},
				{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: "AGENTS.md", Kind: adapters.OpManagedBlock, Shared: true, Content: []byte("x")}}},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildPlan(PlanInput{
				ProjectRoot:     dir,
				ActiveTargets:   []string{"codex", "copilot"},
				Config:          cfg,
				CompiledResults: tc.ops,
			})
			var pe *spec.PolicyError
			if !errors.As(err, &pe) {
				t.Fatalf("%s: expected *spec.PolicyError, got %v", tc.name, err)
			}
		})
	}
}

func TestSharedArtifactInspectConflictsAndPrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skill-src")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# S\n"), 0o644)

	// Pre-existing unmanaged .agents/skills/auditor directory triggers conflict.
	unmanagedSkill := filepath.Join(dir, ".agents", "skills", "auditor")
	_ = os.MkdirAll(unmanagedSkill, 0o755)
	_ = os.WriteFile(filepath.Join(unmanagedSkill, "SKILL.md"), []byte("handwritten"), 0o644)

	plan, err := BuildPlan(PlanInput{
		ProjectRoot:      dir,
		ActiveTargets:    []string{"codex", "copilot"},
		Config:           &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail"},
		InspectConflicts: true,
		CompiledResults: []*adapters.CompileResult{
			{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: ".agents/skills/auditor", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir}}},
			{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: ".agents/skills/auditor", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir}}},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan InspectConflicts failed: %v", err)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("expected 1 shared conflict, got %+v", plan.Conflicts)
	}
	c := plan.Conflicts[0]
	if c.Target != "shared" || c.RelPath != ".agents/skills/auditor" || len(c.Owners) != 2 || c.Owners[0] != "codex" || c.Owners[1] != "copilot" {
		t.Fatalf("unexpected shared PlanConflict: %+v", c)
	}

	// Sync with overwrite, then prune when both targets stop emitting shared artifact.
	planOW, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"codex", "copilot"},
		Config:        &config.EffectiveConfig{LinkMode: "copy", OnConflict: "overwrite"},
		CompiledResults: []*adapters.CompileResult{
			{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: ".agents/skills/auditor", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir}}},
			{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: ".agents/skills/auditor", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir}}},
		},
	})
	if err != nil || Commit(planOW) != nil {
		t.Fatalf("overwrite commit failed: %v", err)
	}

	// Now both targets stop emitting -> PruneOps emitted once with Target: "shared", and Commit prunes SharedOwnedDirs.
	planPrune, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"codex", "copilot"},
		Config:        &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail"},
		CompiledResults: []*adapters.CompileResult{
			{Target: "codex", Files: nil},
			{Target: "copilot", Files: nil},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan prune failed: %v", err)
	}
	if len(planPrune.PruneOps) != 1 || planPrune.PruneOps[0].Target != "shared" || planPrune.PruneOps[0].RelPath != ".agents/skills/auditor" {
		t.Fatalf("expected 1 shared PruneOp, got %+v", planPrune.PruneOps)
	}
	if err := Commit(planPrune); err != nil {
		t.Fatalf("Commit prune failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents", "skills", "auditor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected .agents/skills/auditor deleted on Commit(planPrune)")
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents")); err != nil {
		t.Fatalf("pre-existing user .agents directory must not be deleted by SharedOwnedDirs: %v", err)
	}

	// Now create a shared skill under a non-existent .agents/skills/fresh directory so agentfile owns .agents, then prune via BuildPlan+Commit.
	_ = os.RemoveAll(filepath.Join(dir, ".agents"))
	planFresh, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"codex", "copilot"},
		Config:        &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail"},
		CompiledResults: []*adapters.CompileResult{
			{Target: "codex", Files: []adapters.ArtifactOp{{Target: "codex", RelPath: ".agents/skills/fresh", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir}}},
			{Target: "copilot", Files: []adapters.ArtifactOp{{Target: "copilot", RelPath: ".agents/skills/fresh", Kind: adapters.OpMaterializeTree, Shared: true, SourcePath: skillDir}}},
		},
	})
	if err != nil || Commit(planFresh) != nil {
		t.Fatalf("fresh shared commit failed: %v", err)
	}
	planFreshPrune, err := BuildPlan(PlanInput{
		ProjectRoot:   dir,
		ActiveTargets: []string{"codex", "copilot"},
		Config:        &config.EffectiveConfig{LinkMode: "copy", OnConflict: "fail"},
		CompiledResults: []*adapters.CompileResult{
			{Target: "codex", Files: nil},
			{Target: "copilot", Files: nil},
		},
	})
	if err != nil || Commit(planFreshPrune) != nil {
		t.Fatalf("fresh shared prune commit failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected agentfile-created .agents SharedOwnedDirs pruned on Commit")
	}

	// Verify StatesEqual checks Shared and SharedOwnedDirs.
	s1 := &State{
		Version:         StateVersion,
		Targets:         map[string]*TargetState{},
		Shared:          map[string]SharedArtifactRecord{"AGENTS.md": {Record: ArtifactRecord{ArtifactType: TypeMarkdownBlock, BlockHash: "h1"}, Owners: []string{"codex"}}},
		SharedOwnedDirs: []string{".agents"},
	}
	s2 := cloneState(s1)
	if !StatesEqual(s1, s2) {
		t.Fatalf("expected cloned shared state to compare equal")
	}
	s2.Shared["AGENTS.md"] = SharedArtifactRecord{Record: ArtifactRecord{ArtifactType: TypeMarkdownBlock, BlockHash: "h1"}, Owners: []string{"codex", "copilot"}}
	if StatesEqual(s1, s2) {
		t.Fatalf("expected differing Shared Owners to compare unequal")
	}
	s3 := cloneState(s1)
	s3.SharedOwnedDirs = nil
	if StatesEqual(s1, s3) {
		t.Fatalf("expected differing SharedOwnedDirs to compare unequal")
	}
}
