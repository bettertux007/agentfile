package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitExec(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSyncAndClearExcludesWorktreeAndTracked(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainDir := filepath.Join(tmp, "main")
	wtDir := filepath.Join(tmp, "wt2")
	_ = os.MkdirAll(mainDir, 0o755)

	gitExec(t, mainDir, "init", "-b", "main")
	_ = os.WriteFile(filepath.Join(mainDir, "tracked.json"), []byte("{}"), 0o644)
	gitExec(t, mainDir, "add", "tracked.json")
	gitExec(t, mainDir, "commit", "-m", "init")

	// Add a linked worktree.
	gitExec(t, mainDir, "worktree", "add", "-b", "feat", wtDir)

	// Sync in main worktree (with one tracked file that must be skipped with a warning).
	warnings, err := SyncExcludes(mainDir, []string{".mcp.json", "tracked.json"}, true)
	if err != nil {
		t.Fatalf("SyncExcludes main failed: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "tracked.json") {
		t.Fatalf("expected tracked.json warning, got %v", warnings)
	}

	// Sync in linked worktree with a different file.
	if _, err := SyncExcludes(wtDir, []string{"opencode.json"}, true); err != nil {
		t.Fatalf("SyncExcludes wt2 failed: %v", err)
	}

	excludePath := filepath.Join(mainDir, ".git", "info", "exclude")
	raw, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	txt := string(raw)
	if !strings.Contains(txt, "/.mcp.json") || !strings.Contains(txt, "/opencode.json") || strings.Contains(txt, "tracked.json") {
		t.Fatalf("unexpected exclude contents:\n%s", txt)
	}

	// Clear main worktree excludes; wt2 block must remain intact.
	if _, err := ClearExcludes(mainDir); err != nil {
		t.Fatalf("ClearExcludes main failed: %v", err)
	}
	raw2, _ := os.ReadFile(excludePath)
	if strings.Contains(string(raw2), "/.mcp.json") || !strings.Contains(string(raw2), "/opencode.json") {
		t.Fatalf("expected wt2 excludes preserved after clearing main:\n%s", raw2)
	}

	// Remove wt2 directory & verify prunable worktree block gets pruned on next sync.
	if err := os.RemoveAll(wtDir); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncExcludes(mainDir, []string{".mcp.json"}, true); err != nil {
		t.Fatal(err)
	}
	raw3, _ := os.ReadFile(excludePath)
	if strings.Contains(string(raw3), "/opencode.json") {
		t.Fatalf("expected prunable wt2 block to be pruned:\n%s", raw3)
	}
}

func TestSyncExcludesNoOpOutsideGitRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	warnings, err := SyncExcludes(dir, []string{".mcp.json"}, true)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("expected clean no-op outside git repo, got warnings=%v err=%v", warnings, err)
	}
	if warnings, err := SyncExcludes(dir, []string{".mcp.json"}, false); err != nil || len(warnings) != 0 {
		t.Fatalf("expected clean no-op when disabled outside git repo: %v %v", warnings, err)
	}
}
