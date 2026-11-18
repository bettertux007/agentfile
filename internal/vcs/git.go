package vcs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/lock"
)

const (
	beginPrefix = "# BEGIN AGENTFILE:"
	endPrefix   = "# END AGENTFILE:"
)

type repoContext struct {
	topLevel   string
	commonDir  string
	worktreeID string
	activeIDs  map[string]bool
}

// SyncExcludes updates the current worktree's managed block in <git-common-dir>/info/exclude,
// skipping paths already tracked by Git and pruning stale worktree blocks.
func SyncExcludes(projectRoot string, relPaths []string, enabled bool) ([]string, error) {
	if !enabled {
		return ClearExcludes(projectRoot)
	}
	rc, ok, err := inspectRepo(projectRoot)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	var warnings []string
	tracked, err := findTrackedPaths(rc.topLevel, relPaths)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("could not inspect git tracked files: %v", err))
	}

	var entries []string
	seen := make(map[string]bool)
	addEntry := func(p string) {
		posix := filepath.ToSlash(filepath.Clean(p))
		posix = strings.TrimPrefix(posix, "./")
		if posix == "" || posix == "." {
			return
		}
		if !seen[posix] {
			seen[posix] = true
			entries = append(entries, escapeGitignorePattern("/"+posix))
		}
	}

	// Always exclude agentfile local runtime/config state when git_exclude is active.
	addEntry(".agentfile/")

	for _, p := range relPaths {
		posix := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(p)), "./")
		if tracked[posix] {
			warnings = append(warnings, fmt.Sprintf("path %s is already tracked by Git; skipping exclude entry", posix))
			continue
		}
		addEntry(posix)
	}
	sort.Strings(entries)

	w, err := updateExcludeFile(rc, entries)
	warnings = append(warnings, w...)
	return warnings, err
}

// ClearExcludes removes the current worktree's managed block from <git-common-dir>/info/exclude.
func ClearExcludes(projectRoot string) ([]string, error) {
	rc, ok, err := inspectRepo(projectRoot)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return updateExcludeFile(rc, nil)
}

func updateExcludeFile(rc *repoContext, entries []string) ([]string, error) {
	lockPath := filepath.Join(rc.commonDir, "agentfile-exclude.lock")
	l, err := lock.Acquire(lockPath)
	if err != nil {
		if os.IsPermission(err) {
			return []string{fmt.Sprintf("git common dir %s is not writable; skipping info/exclude update", rc.commonDir)}, nil
		}
		return nil, err
	}
	defer func() { _ = l.Close() }()

	excludePath := filepath.Join(rc.commonDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		if os.IsPermission(err) {
			return []string{fmt.Sprintf("cannot create %s; skipping info/exclude update", filepath.Dir(excludePath))}, nil
		}
		return nil, err
	}

	var existing string
	if b, err := os.ReadFile(excludePath); err == nil {
		existing = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	kept := stripBlocks(existing, rc.worktreeID, rc.activeIDs)
	var out strings.Builder
	out.WriteString(strings.TrimRight(kept, "\n"))
	if len(entries) > 0 {
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(beginPrefix)
		out.WriteString(rc.worktreeID)
		out.WriteByte('\n')
		for _, e := range entries {
			out.WriteString(e)
			out.WriteByte('\n')
		}
		out.WriteString(endPrefix)
		out.WriteString(rc.worktreeID)
		out.WriteByte('\n')
	} else if out.Len() > 0 {
		out.WriteByte('\n')
	}

	tmp, err := os.CreateTemp(filepath.Dir(excludePath), ".exclude-*")
	if err != nil {
		if os.IsPermission(err) {
			return []string{fmt.Sprintf("%s is read-only; skipping info/exclude update", excludePath)}, nil
		}
		return nil, err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(out.String()); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpPath, excludePath); err != nil {
		return nil, err
	}
	cleanup = false
	return nil, nil
}

func stripBlocks(content, currentID string, activeIDs map[string]bool) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var kept []string
	var skipID string

	for _, line := range lines {
		if skipID != "" {
			if strings.TrimSpace(line) == endPrefix+skipID {
				skipID = ""
			}
			continue
		}
		if strings.HasPrefix(line, beginPrefix) {
			id := strings.TrimSpace(strings.TrimPrefix(line, beginPrefix))
			if id == currentID || !activeIDs[id] {
				skipID = id
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func inspectRepo(projectRoot string) (*repoContext, bool, error) {
	topOut, err := runGit(projectRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, false, nil
	}
	topLevel := strings.TrimSpace(string(topOut))

	commonOut, err := runGit(projectRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		// Fallback for older git versions without --path-format=absolute.
		commonOut, err = runGit(projectRoot, "rev-parse", "--git-common-dir")
		if err != nil {
			return nil, false, nil
		}
	}
	commonDir := strings.TrimSpace(string(commonOut))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Clean(filepath.Join(projectRoot, commonDir))
	}

	wtID, err := computeWorktreeID(topLevel)
	if err != nil {
		return nil, false, err
	}

	active := map[string]bool{wtID: true}
	wtOut, err := runGit(projectRoot, "worktree", "list", "--porcelain", "-z")
	if err == nil {
		for _, block := range bytes.Split(wtOut, []byte{0, 0}) {
			var wtPath string
			prunable := false
			for _, field := range bytes.Split(block, []byte{0}) {
				line := string(field)
				if strings.HasPrefix(line, "worktree ") {
					wtPath = strings.TrimPrefix(line, "worktree ")
				} else if line == "prunable" || strings.HasPrefix(line, "prunable ") {
					prunable = true
				}
			}
			if wtPath != "" && !prunable {
				if id, err := computeWorktreeID(wtPath); err == nil {
					active[id] = true
				}
			}
		}
	}

	return &repoContext{
		topLevel:   topLevel,
		commonDir:  commonDir,
		worktreeID: wtID,
		activeIDs:  active,
	}, true, nil
}

func computeWorktreeID(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return hex.EncodeToString(sum[:])[:12], nil
}

func findTrackedPaths(topLevel string, relPaths []string) (map[string]bool, error) {
	tracked := make(map[string]bool)
	if len(relPaths) == 0 {
		return tracked, nil
	}
	args := []string{"ls-files", "-z", "--"}
	cleanInputs := make([]string, 0, len(relPaths))
	for _, p := range relPaths {
		posix := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(p)), "./")
		if posix != "" && posix != "." {
			cleanInputs = append(cleanInputs, posix)
			args = append(args, posix)
		}
	}
	if len(cleanInputs) == 0 {
		return tracked, nil
	}

	out, err := runGit(topLevel, args...)
	if err != nil {
		return tracked, err
	}
	var trackedFiles []string
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) > 0 {
			trackedFiles = append(trackedFiles, string(rec))
		}
	}
	for _, in := range cleanInputs {
		for _, tf := range trackedFiles {
			if tf == in || strings.HasPrefix(tf, strings.TrimSuffix(in, "/")+"/") {
				tracked[in] = true
				break
			}
		}
	}
	return tracked, nil
}

func escapeGitignorePattern(p string) string {
	// Escape Gitignore special leading characters and metacharacters.
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if (i == 0 && (c == '#' || c == '!')) || c == '*' || c == '?' || c == '[' || c == ']' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

func runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
