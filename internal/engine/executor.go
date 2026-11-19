package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bettertux007/agentfile/internal/lock"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/vcs"
)

// CommitJournal records pre-commit backups and newly created paths for deterministic rollback.
type CommitJournal struct {
	BackupDir    string        `json:"backup_dir"`
	Backups      []BackupEntry `json:"backups,omitempty"`
	CreatedPaths []string      `json:"created_paths,omitempty"`
}

// BackupEntry maps an original relative workspace path to its backup location.
type BackupEntry struct {
	OrigRelPath   string `json:"orig_rel_path"`
	BackupRelPath string `json:"backup_rel_path"`
	IsDir         bool   `json:"is_dir,omitempty"`
}

// FSDeps allows injecting rename/sleep hooks for Windows retry unit testing.
type FSDeps struct {
	Rename func(oldpath, newpath string) error
	Sleep  func(d time.Duration)
}

func defaultFSDeps() FSDeps {
	return FSDeps{
		Rename: os.Rename,
		Sleep:  time.Sleep,
	}
}

// JournalPath returns .agentfile/commit-journal.json.
func JournalPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".agentfile", "commit-journal.json")
}

// SyncLockPath returns .agentfile/sync.lock.
func SyncLockPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".agentfile", "sync.lock")
}

// RecoverJournal checks for an interrupted .agentfile/commit-journal.json and rolls it back.
func RecoverJournal(projectRoot string) error {
	jp := JournalPath(projectRoot)
	data, err := os.ReadFile(jp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var j CommitJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return fmt.Errorf("corrupt commit journal %s: %w", jp, err)
	}
	if err := rollbackJournal(projectRoot, &j, defaultFSDeps()); err != nil {
		return err
	}
	return os.Remove(jp)
}

// Commit applies a Plan atomically under .agentfile/sync.lock with deterministic rollback on error.
func Commit(plan *Plan) error {
	return CommitWithDeps(plan, defaultFSDeps())
}

// CommitWithDeps executes Commit using injectable filesystem hooks.
func CommitWithDeps(plan *Plan, deps FSDeps) error {
	if deps.Rename == nil {
		deps.Rename = os.Rename
	}
	if deps.Sleep == nil {
		deps.Sleep = time.Sleep
	}

	l, err := lock.Acquire(SyncLockPath(plan.ProjectRoot))
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()

	_, journalErr := os.Stat(JournalPath(plan.ProjectRoot))
	journalExists := journalErr == nil

	// Recover any leftover interrupted transaction before starting.
	if err := RecoverJournal(plan.ProjectRoot); err != nil {
		return fmt.Errorf("recover interrupted commit: %w", err)
	}

	// Sound no-op fast exit: only skip Commit if no journal existed, state.json exists on disk,
	// no file actions, prune operations, or sidecar writes exist, and prevState == plan.NextState (including ExcludeFingerprint).
	if !journalExists && len(plan.Actions) == 0 && len(plan.PruneOps) == 0 && len(plan.SidecarWrites) == 0 {
		if _, serr := os.Stat(StatePath(plan.ProjectRoot)); serr == nil {
			if prevState, lerr := LoadState(plan.ProjectRoot); lerr == nil && statesEqual(prevState, plan.NextState) {
				return nil
			}
		}
	}

	sidecars := make([]SidecarWrite, 0, len(plan.SidecarWrites))
	for _, sw := range plan.SidecarWrites {
		cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(sw.RelPath)))
		if cleanRel == "" || cleanRel == "." || strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
			return spec.NewPolicyError("sidecar path %q escapes project root", sw.RelPath)
		}
		mode := sw.Mode
		if mode == 0 {
			mode = 0o644
		}
		sidecars = append(sidecars, SidecarWrite{
			RelPath: cleanRel,
			Content: sw.Content,
			Mode:    mode,
		})
	}

	allOps := make([]PlannedAction, 0, len(plan.Actions)+len(plan.PruneOps))
	allOps = append(allOps, plan.Actions...)
	allOps = append(allOps, plan.PruneOps...)

	ts := time.Now().UTC().Format("20060102T150405Z")
	backupRelRoot := filepath.ToSlash(filepath.Join(".agentfile", "backups", ts))
	journal := &CommitJournal{
		BackupDir: backupRelRoot,
	}

	// Stage backups for all modified/deleted existing targets and sidecars before mutating workspace.
	staged := make(map[string]bool, len(allOps)+len(sidecars))
	stageBackup := func(relPath string) error {
		if staged[relPath] {
			return nil
		}
		staged[relPath] = true

		abs := filepath.Join(plan.ProjectRoot, filepath.FromSlash(relPath))
		info, lerr := os.Lstat(abs)
		if errors.Is(lerr, os.ErrNotExist) {
			journal.CreatedPaths = append(journal.CreatedPaths, relPath)
			return nil
		}
		if lerr != nil {
			return lerr
		}

		// Record backup for rollback (and retain permanently when NeedsBackup is true).
		bakRel := filepath.ToSlash(filepath.Join(backupRelRoot, filepath.FromSlash(relPath)))
		bakAbs := filepath.Join(plan.ProjectRoot, filepath.FromSlash(bakRel))
		if info.IsDir() {
			if err := copyDir(abs, bakAbs); err != nil {
				return fmt.Errorf("backup directory %s: %w", relPath, err)
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			// For symlinks, don't duplicate target; record symlink not backed up as file.
			return nil
		} else {
			if err := copyFile(abs, bakAbs, 0o644); err != nil {
				return fmt.Errorf("backup file %s: %w", relPath, err)
			}
		}
		journal.Backups = append(journal.Backups, BackupEntry{
			OrigRelPath:   relPath,
			BackupRelPath: bakRel,
			IsDir:         info.IsDir(),
		})
		return nil
	}

	for _, op := range allOps {
		if err := stageBackup(op.RelPath); err != nil {
			return err
		}
	}
	for _, sw := range sidecars {
		if err := stageBackup(sw.RelPath); err != nil {
			return err
		}
	}

	jBytes, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomicWithDeps(JournalPath(plan.ProjectRoot), jBytes, 0o644, deps); err != nil {
		return err
	}

	applyErr := func() error {
		for _, op := range plan.Actions {
			abs := filepath.Join(plan.ProjectRoot, filepath.FromSlash(op.RelPath))
			switch op.Type {
			case ActionWriteFile:
				if err := removeIfTypeMismatch(abs, false); err != nil {
					return err
				}
				if err := writeFileAtomicWithDeps(abs, op.Content, op.Mode, deps); err != nil {
					return err
				}
			case ActionCopyTree:
				if err := removeIfTypeMismatch(abs, true); err != nil {
					return err
				}
				if err := replaceDirAtomic(abs, op.SourcePath, deps); err != nil {
					return err
				}
			case ActionSymlink:
				if err := removeIfTypeMismatch(abs, false); err != nil {
					return err
				}
				if err := replaceSymlinkAtomic(abs, op.SourcePath, deps); err != nil {
					return err
				}
			}
		}

		for _, op := range plan.PruneOps {
			abs := filepath.Join(plan.ProjectRoot, filepath.FromSlash(op.RelPath))
			switch op.Type {
			case ActionWriteFile:
				if err := writeFileAtomicWithDeps(abs, op.Content, op.Mode, deps); err != nil {
					return err
				}
			case ActionDeletePath:
				if err := os.RemoveAll(abs); err != nil {
					return err
				}
			}
		}

		// Prune empty owned parent directories for active targets.
		for _, target := range plan.ActiveTargets {
			ts := plan.NextState.Targets[target]
			if ts == nil {
				continue
			}
			ts.OwnedDirs = pruneEmptyOwnedDirs(plan.ProjectRoot, ts.OwnedDirs)
		}
		plan.NextState.SharedOwnedDirs = pruneEmptyOwnedDirs(plan.ProjectRoot, plan.NextState.SharedOwnedDirs)

		for _, sw := range sidecars {
			abs := filepath.Join(plan.ProjectRoot, filepath.FromSlash(sw.RelPath))
			if err := removeIfTypeMismatch(abs, false); err != nil {
				return err
			}
			if err := writeFileAtomicWithDeps(abs, sw.Content, sw.Mode, deps); err != nil {
				return err
			}
		}

		if err := SaveState(plan.ProjectRoot, plan.NextState); err != nil {
			return err
		}
		return nil
	}()

	if applyErr != nil {
		_ = rollbackJournal(plan.ProjectRoot, journal, deps)
		_ = os.Remove(JournalPath(plan.ProjectRoot))
		return applyErr
	}

	// Clean transient backup directory unless any action explicitly requested persistent backup.
	keepPersistentBackups := false
	for _, op := range plan.Actions {
		if op.NeedsBackup {
			keepPersistentBackups = true
			break
		}
	}
	if !keepPersistentBackups {
		_ = os.RemoveAll(filepath.Join(plan.ProjectRoot, filepath.FromSlash(journal.BackupDir)))
	}
	_ = os.Remove(JournalPath(plan.ProjectRoot))

	w, err := vcs.SyncExcludes(plan.ProjectRoot, plan.AllExcludePath, plan.GitExclude)
	if err != nil {
		return err
	}
	plan.Summary.Warnings = append(plan.Summary.Warnings, w...)
	return nil
}

// Clean removes tracked workspace artifacts for the selected targets (or all targets if empty).
func Clean(projectRoot string, targets []string, dryRun bool) (*PlanSummary, error) {
	l, err := lock.Acquire(SyncLockPath(projectRoot))
	if err != nil {
		return nil, err
	}
	defer func() { _ = l.Close() }()

	if !dryRun {
		if err := RecoverJournal(projectRoot); err != nil {
			return nil, err
		}
	}

	st, err := LoadState(projectRoot)
	if err != nil {
		return nil, err
	}

	targetSet := make(map[string]bool)
	if len(targets) == 0 {
		for t := range st.Targets {
			targetSet[t] = true
		}
		for _, sar := range st.Shared {
			for _, owner := range sar.Owners {
				targetSet[owner] = true
			}
		}
	} else {
		for _, t := range targets {
			targetSet[t] = true
		}
	}

	summary := &PlanSummary{}
	targetNames := make([]string, 0, len(targetSet))
	for t := range targetSet {
		targetNames = append(targetNames, t)
	}
	sort.Strings(targetNames)

	for _, t := range targetNames {
		ts := st.Targets[t]
		if ts == nil {
			continue
		}
		paths := make([]string, 0, len(ts.Artifacts))
		for rel := range ts.Artifacts {
			paths = append(paths, rel)
		}
		sort.Strings(paths)

		for _, rel := range paths {
			if err := cleanTrackedArtifact(projectRoot, rel, ts.Artifacts[rel], dryRun, summary); err != nil {
				return nil, err
			}
		}

		if !dryRun {
			for _, d := range ts.OwnedDirs {
				_ = removeIfEffectivelyEmpty(filepath.Join(projectRoot, filepath.FromSlash(d)))
			}
			delete(st.Targets, t)
		}
	}

	sharedPaths := make([]string, 0, len(st.Shared))
	for rel := range st.Shared {
		sharedPaths = append(sharedPaths, rel)
	}
	sort.Strings(sharedPaths)

	prunedAnyShared := false
	for _, rel := range sharedPaths {
		sar := st.Shared[rel]
		remainingOwners := make([]string, 0, len(sar.Owners))
		removedAny := false
		for _, owner := range sar.Owners {
			if targetSet[owner] {
				removedAny = true
				continue
			}
			remainingOwners = append(remainingOwners, owner)
		}
		if !removedAny {
			continue
		}
		if len(remainingOwners) > 0 {
			if !dryRun {
				sar.Owners = remainingOwners
				st.Shared[rel] = sar
			}
			continue
		}
		prunedAnyShared = true
		if err := cleanTrackedArtifact(projectRoot, rel, sar.Record, dryRun, summary); err != nil {
			return nil, err
		}
		if !dryRun {
			delete(st.Shared, rel)
		}
	}

	if !dryRun {
		if prunedAnyShared {
			st.SharedOwnedDirs = pruneEmptyOwnedDirs(projectRoot, st.SharedOwnedDirs)
		}
		if len(st.Targets) == 0 && len(st.Shared) == 0 {
			_ = os.Remove(StatePath(projectRoot))
			w, err := vcs.ClearExcludes(projectRoot)
			if err != nil {
				return nil, err
			}
			summary.Warnings = append(summary.Warnings, w...)
		} else {
			remainingSet := make(map[string]bool)
			for _, ts := range st.Targets {
				for rel := range ts.Artifacts {
					remainingSet[rel] = true
				}
			}
			for rel := range st.Shared {
				remainingSet[rel] = true
			}
			remaining := make([]string, 0, len(remainingSet))
			for rel := range remainingSet {
				remaining = append(remaining, rel)
			}
			sort.Strings(remaining)
			st.ExcludeFingerprint = hashBytes([]byte(fmt.Sprintf(
				"git_exclude=%t\n%s", true, strings.Join(remaining, "\n"),
			)))
			if err := SaveState(projectRoot, st); err != nil {
				return nil, err
			}
			w, err := vcs.SyncExcludes(projectRoot, remaining, true)
			if err != nil {
				return nil, err
			}
			summary.Warnings = append(summary.Warnings, w...)
		}
	}

	return summary, nil
}

func cleanTrackedArtifact(projectRoot, rel string, rec ArtifactRecord, dryRun bool, summary *PlanSummary) error {
	abs := filepath.Join(projectRoot, filepath.FromSlash(rel))
	if _, err := os.Lstat(abs); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if rec.ArtifactType == TypeJSONCPatch {
		curBytes, err := os.ReadFile(abs)
		if err == nil {
			updatedBytes, deleteFile, rerr := RemoveJSONC(curBytes, rec)
			if rerr != nil {
				return rerr
			}
			if !deleteFile {
				summary.Updated = append(summary.Updated, rel)
				if !dryRun {
					if err := writeFileAtomic(abs, updatedBytes, 0o644); err != nil {
						return err
					}
				}
				return nil
			}
		}
	}

	if rec.ArtifactType == TypeTOMLPatch {
		curBytes, err := os.ReadFile(abs)
		if err == nil {
			updatedBytes, deleteFile, rerr := RemoveTOML(curBytes, rec)
			if rerr != nil {
				return rerr
			}
			if !deleteFile {
				summary.Updated = append(summary.Updated, rel)
				if !dryRun {
					if err := writeFileAtomic(abs, updatedBytes, 0o644); err != nil {
						return err
					}
				}
				return nil
			}
		}
	}

	if rec.ArtifactType == TypeMarkdownBlock {
		curBytes, err := os.ReadFile(abs)
		if err == nil {
			updatedBytes, deleteFile, rerr := RemoveMarkdownBlock(curBytes, rec)
			if rerr != nil {
				return rerr
			}
			if !deleteFile {
				summary.Updated = append(summary.Updated, rel)
				if !dryRun {
					if err := writeFileAtomic(abs, updatedBytes, 0o644); err != nil {
						return err
					}
				}
				return nil
			}
		}
	}

	summary.Pruned = append(summary.Pruned, rel)
	if !dryRun {
		if err := os.RemoveAll(abs); err != nil {
			return err
		}
	}
	return nil
}

func pruneEmptyOwnedDirs(projectRoot string, dirs []string) []string {
	keptDirs := make([]string, 0, len(dirs))
	for _, d := range dirs {
		absDir := filepath.Join(projectRoot, filepath.FromSlash(d))
		if removed := removeIfEffectivelyEmpty(absDir); !removed {
			if _, err := os.Stat(absDir); err == nil {
				keptDirs = append(keptDirs, d)
			}
		}
	}
	return keptDirs
}

func rollbackJournal(projectRoot string, j *CommitJournal, deps FSDeps) error {
	for _, rel := range j.CreatedPaths {
		_ = os.RemoveAll(filepath.Join(projectRoot, filepath.FromSlash(rel)))
	}
	for _, b := range j.Backups {
		origAbs := filepath.Join(projectRoot, filepath.FromSlash(b.OrigRelPath))
		bakAbs := filepath.Join(projectRoot, filepath.FromSlash(b.BackupRelPath))
		if _, err := os.Lstat(bakAbs); err != nil {
			continue
		}
		_ = os.RemoveAll(origAbs)
		if b.IsDir {
			_ = copyDir(bakAbs, origAbs)
		} else {
			if data, err := os.ReadFile(bakAbs); err == nil {
				_ = writeFileAtomicWithDeps(origAbs, data, 0o644, deps)
			}
		}
	}
	return nil
}

func removeIfTypeMismatch(abs string, wantDir bool) error {
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	isSym := info.Mode()&os.ModeSymlink != 0
	if wantDir && (!info.IsDir() || isSym) {
		return os.RemoveAll(abs)
	}
	if !wantDir && (info.IsDir() || isSym) {
		return os.RemoveAll(abs)
	}
	return nil
}

func replaceDirAtomic(destDir, srcDir string, deps FSDeps) error {
	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(parent, ".agentfile-dir-*")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	if err := copyDir(srcDir, tmpDir); err != nil {
		return err
	}
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	if err := renameWithRetry(tmpDir, destDir, deps); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func replaceSymlinkAtomic(destPath, target string, deps FSDeps) error {
	parent := filepath.Dir(destPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmpLink := filepath.Join(parent, fmt.Sprintf(".agentfile-link-%d", time.Now().UnixNano()))
	if err := os.Symlink(target, tmpLink); err != nil {
		if os.IsPermission(err) || strings.Contains(strings.ToLower(err.Error()), "privilege") {
			return spec.NewPolicyError("failed to create symlink %s (enable Developer Mode on Windows or switch to link_mode: copy): %v", destPath, err)
		}
		return err
	}
	if err := renameWithRetry(tmpLink, destPath, deps); err != nil {
		_ = os.Remove(tmpLink)
		return err
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return copyFile(p, target, mode)
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomicWithDeps(path, data, mode, defaultFSDeps())
}

func writeFileAtomicWithDeps(path string, data []byte, mode os.FileMode, deps FSDeps) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agentfile-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := renameWithRetry(tmpPath, path, deps); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func renameWithRetry(oldpath, newpath string, deps FSDeps) error {
	backoff := 10 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := deps.Rename(oldpath, newpath); err == nil {
			return nil
		} else {
			lastErr = err
			if !isTransientRenameError(err) || attempt == 4 {
				return err
			}
		}
		deps.Sleep(backoff)
		backoff *= 2
	}
	return lastErr
}

func isTransientRenameError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access is denied") || strings.Contains(msg, "sharing violation")
}

func removeIfEffectivelyEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if name != ".DS_Store" && name != "Thumbs.db" {
			return false
		}
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return os.Remove(dir) == nil
}
