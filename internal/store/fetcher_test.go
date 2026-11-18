package store

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bettertux007/agentfile/internal/spec"
)

func runGitTest(t *testing.T, dir string, args ...string) string {
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

func TestFetcherBareRepoAnnotatedTagAndCacheReuse(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	cacheDir := filepath.Join(tmp, "cache")
	projDir := filepath.Join(tmp, "project")

	if err := os.MkdirAll(filepath.Join(workRepo, "skills", "k8s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projDir, "local-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "local-skill", "SKILL.md"), []byte("# Local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workRepo, "skills", "k8s", "SKILL.md"), []byte("# K8s v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runGitTest(t, workRepo, "init", "-b", "main")
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "init")
	v1SHA := runGitTest(t, workRepo, "rev-parse", "HEAD")
	runGitTest(t, workRepo, "tag", "-a", "v1.0.0", "-m", "release v1")
	runGitTest(t, tmp, "clone", "--bare", workRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	ps := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "loc", Source: spec.SourceSpec{Raw: "./local-skill"}},
			{Name: "k8s", Source: spec.SourceSpec{Raw: repoURL + "#v1.0.0//skills/k8s"}},
		},
	}

	st := &Store{RootDir: cacheDir, Git: &ExecGitRunner{}}
	opts := ParseOptions{AllowFileScheme: true}

	resolved, lf, err := st.ResolveAll(context.Background(), projDir, ps, nil, "", opts)
	if err != nil {
		t.Fatalf("ResolveAll initial failed: %v", err)
	}
	entry, ok := lf.Dependencies["skill:k8s"]
	if !ok {
		t.Fatalf("missing lock entry for skill:k8s")
	}
	if entry.CommitSHA != v1SHA {
		t.Fatalf("expected peeled commit SHA %s, got %s", v1SHA, entry.CommitSHA)
	}
	if _, ok := lf.Dependencies["skill:loc"]; ok {
		t.Fatalf("local dependency must not appear in Agentfile.lock")
	}
	if _, err := os.Stat(filepath.Join(resolved["skill:k8s"].SourcePath, "SKILL.md")); err != nil {
		t.Fatalf("extracted SKILL.md missing: %v", err)
	}

	// Remove remote bare repo to prove locked cache reuse works offline.
	if err := os.RemoveAll(bareRepo); err != nil {
		t.Fatal(err)
	}
	resolved2, _, err := st.ResolveAll(context.Background(), projDir, ps, lf, "", opts)
	if err != nil {
		t.Fatalf("offline ResolveAll failed: %v", err)
	}
	if resolved2["skill:k8s"].SourcePath != resolved["skill:k8s"].SourcePath {
		t.Fatalf("expected identical extracted cache path")
	}

	// Verify PruneCache evicts expired entries.
	luFile := resolved["skill:k8s"].SourcePath + ".last_used"
	oldTime := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(luFile, []byte(oldTime+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pruned, err := st.PruneCache(24*time.Hour, false)
	if err != nil {
		t.Fatalf("PruneCache failed: %v", err)
	}
	if len(pruned) != 1 {
		t.Fatalf("expected 1 pruned cache entry, got %d", len(pruned))
	}
}

func TestFetcherRejectsSubmoduleGitlink(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	subRepo := filepath.Join(tmp, "sub")
	mainRepo := filepath.Join(tmp, "main")
	bareRepo := filepath.Join(tmp, "main.git")
	cacheDir := filepath.Join(tmp, "cache")
	projDir := filepath.Join(tmp, "project")
	_ = os.MkdirAll(subRepo, 0o755)
	_ = os.MkdirAll(filepath.Join(mainRepo, "skills", "bad"), 0o755)
	_ = os.MkdirAll(projDir, 0o755)

	_ = os.WriteFile(filepath.Join(subRepo, "x.txt"), []byte("x"), 0o644)
	runGitTest(t, subRepo, "init", "-b", "main")
	runGitTest(t, subRepo, "add", ".")
	runGitTest(t, subRepo, "commit", "-m", "sub")
	subSHA := runGitTest(t, subRepo, "rev-parse", "HEAD")

	_ = os.WriteFile(filepath.Join(mainRepo, "skills", "bad", "SKILL.md"), []byte("# Bad\n"), 0o644)
	runGitTest(t, mainRepo, "init", "-b", "main")
	runGitTest(t, mainRepo, "add", ".")
	runGitTest(t, mainRepo, "update-index", "--add", "--cacheinfo", "160000,"+subSHA+",skills/bad/nested")
	runGitTest(t, mainRepo, "commit", "-m", "with gitlink")
	runGitTest(t, tmp, "clone", "--bare", mainRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	ps := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "bad", Source: spec.SourceSpec{Raw: repoURL + "#main//skills/bad"}},
		},
	}

	st := &Store{RootDir: cacheDir, Git: &ExecGitRunner{}}
	_, _, err := st.ResolveAll(context.Background(), projDir, ps, nil, "", ParseOptions{AllowFileScheme: true})
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError rejecting submodule gitlink, got %v", err)
	}
}

func TestFetcherRawSHADefaultHeadAndMissingRef(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	cacheDir := filepath.Join(tmp, "cache")
	projDir := filepath.Join(tmp, "project")
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "s"), 0o755)
	_ = os.MkdirAll(projDir, 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "s", "SKILL.md"), []byte("# S\n"), 0o644)

	runGitTest(t, workRepo, "init", "-b", "main")
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "init")
	sha := runGitTest(t, workRepo, "rev-parse", "HEAD")
	runGitTest(t, tmp, "clone", "--bare", workRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	st := &Store{RootDir: cacheDir, Git: &ExecGitRunner{}}
	opts := ParseOptions{AllowFileScheme: true}

	// 1. Default HEAD (omitted #ref)
	psHead := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills:  []spec.AssetSpec{{Name: "s", Source: spec.SourceSpec{Raw: repoURL + "//skills/s"}}},
	}
	_, lfHead, err := st.ResolveAll(context.Background(), projDir, psHead, nil, "", opts)
	if err != nil || lfHead.Dependencies["skill:s"].CommitSHA != sha {
		t.Fatalf("HEAD resolve failed: %v lf=%+v", err, lfHead)
	}

	// 2. Raw 40-char SHA ref
	psSHA := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills:  []spec.AssetSpec{{Name: "s", Source: spec.SourceSpec{Raw: repoURL + "#" + sha + "//skills/s"}}},
	}
	_, lfSHA, err := st.ResolveAll(context.Background(), projDir, psSHA, nil, "*", opts)
	if err != nil || lfSHA.Dependencies["skill:s"].CommitSHA != sha {
		t.Fatalf("raw SHA resolve failed: %v", err)
	}

	// 3. Missing ref returns PolicyError
	psBadRef := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills:  []spec.AssetSpec{{Name: "s", Source: spec.SourceSpec{Raw: repoURL + "#nonexistent//skills/s"}}},
	}
	if _, _, err := st.ResolveAll(context.Background(), projDir, psBadRef, nil, "*", opts); err == nil {
		t.Fatalf("expected error for missing ref")
	}

	// 4. Missing subpath returns PolicyError
	psBadSub := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills:  []spec.AssetSpec{{Name: "s", Source: spec.SourceSpec{Raw: repoURL + "#main//skills/nope"}}},
	}
	if _, _, err := st.ResolveAll(context.Background(), projDir, psBadSub, nil, "*", opts); err == nil {
		t.Fatalf("expected error for missing subpath")
	}
}

func TestDefaultStoreTimeoutAndTarSafety(t *testing.T) {
	t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())
	t.Setenv("AGENTFILE_GIT_TIMEOUT", "15s")
	st, err := NewDefaultStore()
	if err != nil {
		t.Fatalf("NewDefaultStore failed: %v", err)
	}
	if d := st.Git.(*ExecGitRunner).timeout(); d != 15*time.Second {
		t.Fatalf("expected 15s timeout, got %v", d)
	}
	// Invalid timeout falls back to 60s.
	t.Setenv("AGENTFILE_GIT_TIMEOUT", "bad")
	if d := (&ExecGitRunner{}).timeout(); d != 60*time.Second {
		t.Fatalf("expected 60s fallback timeout, got %v", d)
	}

	// Verify ParseOptions AllowExternalLocal allows ../ path.
	root := t.TempDir()
	ext, err := ParseSource(spec.SourceSpec{Raw: "../sibling"}, root, ParseOptions{AllowExternalLocal: true})
	if err != nil || !ext.IsLocal {
		t.Fatalf("expected AllowExternalLocal to permit ../sibling, got %+v err=%v", ext, err)
	}

	// Verify extractTarSafe rejects symlinks inside archive and traversal paths.
	if err := extractTarSafe([]byte("not-a-tar"), "", t.TempDir()); err == nil {
		t.Fatalf("expected error on corrupt tar stream")
	}
	// Verify empty archive for subpath fails.
	if err := extractTarSafe(nil, "sub/missing", t.TempDir()); err == nil {
		t.Fatalf("expected error when subpath yields no regular files")
	}

	// Verify tar symlink & traversal rejection.
	var symBuf bytes.Buffer
	tw := tar.NewWriter(&symBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777})
	_ = tw.Close()
	if err := extractTarSafe(symBuf.Bytes(), "", t.TempDir()); !errors.Is(err, ErrSymlinkNotAllowed) {
		t.Fatalf("expected ErrSymlinkNotAllowed on tar symlink, got %v", err)
	}

	var travBuf bytes.Buffer
	tw2 := tar.NewWriter(&travBuf)
	_ = tw2.WriteHeader(&tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644})
	_, _ = tw2.Write([]byte("x"))
	_ = tw2.Close()
	if err := extractTarSafe(travBuf.Bytes(), "", t.TempDir()); err == nil {
		t.Fatalf("expected error on ../evil.txt tar path")
	}
}

func TestCorruptCacheRehydrateAndIntegrityMismatch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	cacheDir := filepath.Join(tmp, "cache")
	projDir := filepath.Join(tmp, "project")
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "s"), 0o755)
	_ = os.MkdirAll(projDir, 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "s", "SKILL.md"), []byte("# S\n"), 0o644)
	runGitTest(t, workRepo, "init", "-b", "main")
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "init")
	runGitTest(t, tmp, "clone", "--bare", workRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	ps := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills:  []spec.AssetSpec{{Name: "s", Source: spec.SourceSpec{Raw: repoURL + "#main//skills/s"}}},
	}
	st := &Store{RootDir: cacheDir, Git: &ExecGitRunner{}}
	opts := ParseOptions{AllowFileScheme: true}

	res1, lf, err := st.ResolveAll(context.Background(), projDir, ps, nil, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper extracted cache file -> ResolveAll must detect hash mismatch, delete corrupt entry, and re-extract cleanly.
	_ = os.WriteFile(filepath.Join(res1["skill:s"].SourcePath, "SKILL.md"), []byte("corrupted"), 0o644)
	res2, _, err := st.ResolveAll(context.Background(), projDir, ps, lf, "", opts)
	if err != nil {
		t.Fatalf("expected corrupt cache recovery, got %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(res2["skill:s"].SourcePath, "SKILL.md"))
	if string(b) != "# S\n" {
		t.Fatalf("expected clean re-extracted content, got %q", b)
	}

	// Remove cache and tamper lock ContentHash -> must fail with integrity mismatch PolicyError.
	_ = os.RemoveAll(res2["skill:s"].SourcePath)
	badLock := &spec.Lockfile{
		Version: 1,
		Dependencies: map[string]spec.LockEntry{
			"skill:s": {
				Source:      lf.Dependencies["skill:s"].Source,
				Ref:         "main",
				CommitSHA:   lf.Dependencies["skill:s"].CommitSHA,
				ContentHash: "sha256:" + strings.Repeat("0", 64),
			},
		},
	}
	if _, _, err := st.ResolveAll(context.Background(), projDir, ps, badLock, "", opts); err == nil {
		t.Fatalf("expected integrity mismatch error")
	}

	// PruneCache on empty/nonexistent store returns nil without error; dryRun does not remove entry.
	emptySt := &Store{RootDir: t.TempDir(), Git: &ExecGitRunner{}}
	if removed, err := emptySt.PruneCache(time.Hour, false); err != nil || len(removed) != 0 {
		t.Fatalf("expected empty PruneCache no-op: removed=%v err=%v", removed, err)
	}
	luPath := res1["skill:s"].SourcePath + ".last_used"
	_ = os.WriteFile(luPath, []byte(time.Now().Add(-10*time.Hour).UTC().Format(time.RFC3339)+"\n"), 0o644)
	dryRemoved, err := st.PruneCache(time.Hour, true)
	if err != nil || len(dryRemoved) != 1 {
		t.Fatalf("expected 1 dryRemoved entry, got %v err=%v", dryRemoved, err)
	}
	if _, err := os.Stat(res1["skill:s"].SourcePath); err != nil {
		t.Fatalf("dry-run PruneCache must not delete directory: %v", err)
	}
}

type failingGitRunner struct {
	t *testing.T
}

func (f *failingGitRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.t.Fatalf("unexpected git invocation during read-only inspection: git %s", strings.Join(args, " "))
	return nil, errors.New("git invoked")
}

func TestInspectDependenciesZeroSideEffects(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	cacheDir := filepath.Join(tmp, "cache")
	projDir := filepath.Join(tmp, "project")

	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "k8s"), 0o755)
	_ = os.MkdirAll(filepath.Join(projDir, "local-skill"), 0o755)
	_ = os.WriteFile(filepath.Join(projDir, "local-skill", "SKILL.md"), []byte("# Local\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "k8s", "SKILL.md"), []byte("# K8s\n"), 0o644)

	runGitTest(t, workRepo, "init", "-b", "main")
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "init")
	runGitTest(t, workRepo, "tag", "-a", "v1.0.0", "-m", "v1")
	runGitTest(t, tmp, "clone", "--bare", workRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	ps := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "loc", Source: spec.SourceSpec{Raw: "./local-skill"}},
			{Name: "k8s", Source: spec.SourceSpec{Raw: repoURL + "#v1.0.0//skills/k8s"}},
		},
	}

	opts := ParseOptions{AllowFileScheme: true}
	stFetch := &Store{RootDir: cacheDir, Git: &ExecGitRunner{}}
	resolved, lf, err := stFetch.ResolveAll(context.Background(), projDir, ps, nil, "", opts)
	if err != nil {
		t.Fatalf("initial ResolveAll failed: %v", err)
	}

	// Set .last_used to a known old timestamp and verify InspectDependencies never modifies it.
	luPath := resolved["skill:k8s"].SourcePath + ".last_used"
	sentinelTS := "2020-01-01T00:00:00Z\n"
	if err := os.WriteFile(luPath, []byte(sentinelTS), 0o644); err != nil {
		t.Fatal(err)
	}
	sentinelMod := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(luPath, sentinelMod, sentinelMod); err != nil {
		t.Fatal(err)
	}

	// 1. Happy path with failing GitRunner: Assets populated, LockStatus == "ok", .last_used untouched.
	roStore := &Store{RootDir: cacheDir, Git: &failingGitRunner{t: t}}
	rep, err := roStore.InspectDependencies(projDir, ps, lf, opts)
	if err != nil {
		t.Fatalf("InspectDependencies failed: %v", err)
	}
	if rep.LockStatus != "ok" || rep.LockError != nil {
		t.Fatalf("expected LockStatus=ok LockError=nil, got %q %v", rep.LockStatus, rep.LockError)
	}
	if len(rep.Unhydrated) != 0 || len(rep.MissingLocal) != 0 {
		t.Fatalf("expected no unhydrated/missing deps, got unhydrated=%v missing=%v", rep.Unhydrated, rep.MissingLocal)
	}
	if rep.Assets["skill:loc"].SourcePath != filepath.Join(projDir, "local-skill") {
		t.Fatalf("unexpected local asset path: %+v", rep.Assets["skill:loc"])
	}
	if rep.Assets["skill:k8s"].SourcePath != resolved["skill:k8s"].SourcePath {
		t.Fatalf("unexpected remote asset path: %+v", rep.Assets["skill:k8s"])
	}
	gotLU, err := os.ReadFile(luPath)
	if err != nil || string(gotLU) != sentinelTS {
		t.Fatalf(".last_used content mutated: got %q err=%v", string(gotLU), err)
	}
	luInfo, err := os.Stat(luPath)
	if err != nil || !luInfo.ModTime().Equal(sentinelMod) {
		t.Fatalf(".last_used mtime mutated: got %v want %v err=%v", luInfo.ModTime(), sentinelMod, err)
	}

	// 2. Corrupt cache dir -> Unhydrated, cache dir NOT deleted.
	skillFile := filepath.Join(resolved["skill:k8s"].SourcePath, "SKILL.md")
	_ = os.WriteFile(skillFile, []byte("corrupted"), 0o644)
	repCorrupt, err := roStore.InspectDependencies(projDir, ps, lf, opts)
	if err != nil {
		t.Fatalf("InspectDependencies corrupt failed: %v", err)
	}
	if len(repCorrupt.Unhydrated) != 1 || repCorrupt.Unhydrated[0] != "skill:k8s" {
		t.Fatalf("expected Unhydrated=[skill:k8s] on corrupt cache, got %v", repCorrupt.Unhydrated)
	}
	if _, ok := repCorrupt.Assets["skill:k8s"]; ok {
		t.Fatalf("corrupt remote asset must not be in Assets")
	}
	if _, err := os.Stat(skillFile); err != nil {
		t.Fatalf("InspectDependencies must not delete corrupt cache: %v", err)
	}

	// 3. Missing cache dir -> Unhydrated.
	_ = os.RemoveAll(resolved["skill:k8s"].SourcePath)
	repMissingCache, err := roStore.InspectDependencies(projDir, ps, lf, opts)
	if err != nil {
		t.Fatalf("InspectDependencies missing cache failed: %v", err)
	}
	if len(repMissingCache.Unhydrated) != 1 || repMissingCache.Unhydrated[0] != "skill:k8s" {
		t.Fatalf("expected Unhydrated=[skill:k8s] on missing cache, got %v", repMissingCache.Unhydrated)
	}

	// 4. Lock entry mismatches manifest ref -> Unhydrated and LockStatus=out_of_sync.
	psMismatch := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "loc", Source: spec.SourceSpec{Raw: "./local-skill"}},
			{Name: "k8s", Source: spec.SourceSpec{Raw: repoURL + "#v2.0.0//skills/k8s"}},
		},
	}
	repMismatch, err := roStore.InspectDependencies(projDir, psMismatch, lf, opts)
	if err != nil {
		t.Fatalf("InspectDependencies mismatch failed: %v", err)
	}
	if repMismatch.LockStatus != "out_of_sync" || repMismatch.LockError == nil {
		t.Fatalf("expected LockStatus=out_of_sync, got %q %v", repMismatch.LockStatus, repMismatch.LockError)
	}
	if len(repMismatch.Unhydrated) != 1 || repMismatch.Unhydrated[0] != "skill:k8s" {
		t.Fatalf("expected Unhydrated=[skill:k8s] on lock mismatch, got %v", repMismatch.Unhydrated)
	}

	// 5. Missing local path -> MissingLocal.
	_ = os.RemoveAll(filepath.Join(projDir, "local-skill"))
	repMissingLocal, err := roStore.InspectDependencies(projDir, ps, lf, opts)
	if err != nil {
		t.Fatalf("InspectDependencies missing local failed: %v", err)
	}
	if len(repMissingLocal.MissingLocal) != 1 || repMissingLocal.MissingLocal[0] != "skill:loc" {
		t.Fatalf("expected MissingLocal=[skill:loc], got %v", repMissingLocal.MissingLocal)
	}

	// 6. Local-only spec with lf == nil -> LockStatus == "not_required".
	_ = os.MkdirAll(filepath.Join(projDir, "local-skill"), 0o755)
	psLocalOnly := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "loc", Source: spec.SourceSpec{Raw: "./local-skill"}},
		},
	}
	repLocalOnly, err := roStore.InspectDependencies(projDir, psLocalOnly, nil, opts)
	if err != nil {
		t.Fatalf("InspectDependencies local-only failed: %v", err)
	}
	if repLocalOnly.LockStatus != "not_required" || repLocalOnly.LockError != nil {
		t.Fatalf("expected LockStatus=not_required LockError=nil, got %q %v", repLocalOnly.LockStatus, repLocalOnly.LockError)
	}

	// 7. Remote spec with lf == nil -> LockStatus == "missing".
	repNoLock, err := roStore.InspectDependencies(projDir, ps, nil, opts)
	if err != nil {
		t.Fatalf("InspectDependencies nil lock failed: %v", err)
	}
	if repNoLock.LockStatus != "missing" || repNoLock.LockError == nil {
		t.Fatalf("expected LockStatus=missing, got %q %v", repNoLock.LockStatus, repNoLock.LockError)
	}
}

func TestResolveWithOptionsPrunesRemovedAndVerifiesUntouched(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	cacheDir := filepath.Join(tmp, "cache")
	projDir := filepath.Join(tmp, "project")

	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "other"), 0o755)
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "removed"), 0o755)
	_ = os.MkdirAll(projDir, 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "other", "SKILL.md"), []byte("# Other\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "removed", "SKILL.md"), []byte("# Removed\n"), 0o644)

	runGitTest(t, workRepo, "init", "-b", "main")
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "init")
	runGitTest(t, tmp, "clone", "--bare", workRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	psInitial := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "other", Source: spec.SourceSpec{Raw: repoURL + "#main//skills/other"}},
			{Name: "removed", Source: spec.SourceSpec{Raw: repoURL + "#main//skills/removed"}},
		},
	}

	st := &Store{RootDir: cacheDir, Git: &ExecGitRunner{}}
	opts := ParseOptions{AllowFileScheme: true}
	_, lf, err := st.ResolveAll(context.Background(), projDir, psInitial, nil, "", opts)
	if err != nil {
		t.Fatalf("initial ResolveAll failed: %v", err)
	}

	// Remove skill:removed from spec and reconcile with ReconcileKey: "skill:removed".
	psAfterRemove := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "other", Source: spec.SourceSpec{Raw: repoURL + "#main//skills/other"}},
		},
	}
	res, err := st.ResolveWithOptions(context.Background(), projDir, psAfterRemove, lf, ResolveOptions{
		ReconcileKey: "skill:removed",
		ParseOpts:    opts,
	})
	if err != nil {
		t.Fatalf("ResolveWithOptions reconcile remove failed: %v", err)
	}
	if _, ok := res.NextLock.Dependencies["skill:removed"]; ok {
		t.Fatalf("expected skill:removed to be pruned from NextLock")
	}
	if _, ok := res.NextLock.Dependencies["skill:other"]; !ok {
		t.Fatalf("expected skill:other to remain in NextLock")
	}
	if len(res.Deltas) != 1 || res.Deltas[0].Key != "skill:removed" || res.Deltas[0].Action != "removed" {
		t.Fatalf("expected single removed delta for skill:removed, got %+v", res.Deltas)
	}

	// If unrelated skill:other drifted from lf, ReconcileKey: "skill:removed" must still fail with PolicyError.
	psDrifted := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "other", Source: spec.SourceSpec{Raw: repoURL + "#v999//skills/other"}},
		},
	}
	_, err = st.ResolveWithOptions(context.Background(), projDir, psDrifted, lf, ResolveOptions{
		ReconcileKey: "skill:removed",
		ParseOpts:    opts,
	})
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError when unrelated skill:other drifted, got %v", err)
	}
}

func TestResolveWithOptionsAddReplace(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	cacheDir := filepath.Join(tmp, "cache")
	projDir := filepath.Join(tmp, "project")

	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "k8s"), 0o755)
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "other"), 0o755)
	_ = os.MkdirAll(projDir, 0o755)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "k8s", "SKILL.md"), []byte("# K8s v1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "other", "SKILL.md"), []byte("# Other\n"), 0o644)

	runGitTest(t, workRepo, "init", "-b", "main")
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "v1")
	v1SHA := runGitTest(t, workRepo, "rev-parse", "HEAD")
	runGitTest(t, workRepo, "tag", "-a", "v1.0.0", "-m", "v1")

	_ = os.WriteFile(filepath.Join(workRepo, "skills", "k8s", "SKILL.md"), []byte("# K8s v2\n"), 0o644)
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "v2")
	v2SHA := runGitTest(t, workRepo, "rev-parse", "HEAD")
	runGitTest(t, workRepo, "tag", "-a", "v2.0.0", "-m", "v2")

	runGitTest(t, tmp, "clone", "--bare", workRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	psV1 := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "k8s", Source: spec.SourceSpec{Raw: repoURL + "#v1.0.0//skills/k8s"}},
			{Name: "other", Source: spec.SourceSpec{Raw: repoURL + "#v1.0.0//skills/other"}},
		},
	}

	st := &Store{RootDir: cacheDir, Git: &ExecGitRunner{}}
	opts := ParseOptions{AllowFileScheme: true}
	_, lf, err := st.ResolveAll(context.Background(), projDir, psV1, nil, "", opts)
	if err != nil {
		t.Fatalf("initial ResolveAll failed: %v", err)
	}

	// Simulate `add --replace` changing skill:k8s to #v2.0.0.
	psV2 := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "k8s", Source: spec.SourceSpec{Raw: repoURL + "#v2.0.0//skills/k8s"}},
			{Name: "other", Source: spec.SourceSpec{Raw: repoURL + "#v1.0.0//skills/other"}},
		},
	}
	res, err := st.ResolveWithOptions(context.Background(), projDir, psV2, lf, ResolveOptions{
		ReconcileKey: "skill:k8s",
		ParseOpts:    opts,
	})
	if err != nil {
		t.Fatalf("ResolveWithOptions add --replace failed: %v", err)
	}
	if res.NextLock.Dependencies["skill:k8s"].CommitSHA != v2SHA {
		t.Fatalf("expected updated CommitSHA %s, got %s", v2SHA, res.NextLock.Dependencies["skill:k8s"].CommitSHA)
	}
	if res.NextLock.Dependencies["skill:other"].CommitSHA != v1SHA {
		t.Fatalf("expected untouched skill:other SHA %s, got %s", v1SHA, res.NextLock.Dependencies["skill:other"].CommitSHA)
	}
	if len(res.Deltas) != 1 || res.Deltas[0].Key != "skill:k8s" || res.Deltas[0].Action != "updated" {
		t.Fatalf("expected single updated delta for skill:k8s, got %+v", res.Deltas)
	}
	if res.Deltas[0].OldSHA != v1SHA || res.Deltas[0].NewSHA != v2SHA {
		t.Fatalf("expected OldSHA=%s NewSHA=%s, got %+v", v1SHA, v2SHA, res.Deltas[0])
	}
}

func TestResolveWithOptionsUpgradeValidationAndDeltas(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	cacheDir := filepath.Join(tmp, "cache")
	projDir := filepath.Join(tmp, "project")

	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "pinned-tag"), 0o755)
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "pinned-sha"), 0o755)
	_ = os.MkdirAll(filepath.Join(workRepo, "skills", "floating"), 0o755)
	_ = os.MkdirAll(filepath.Join(projDir, "local-skill"), 0o755)
	_ = os.WriteFile(filepath.Join(projDir, "local-skill", "SKILL.md"), []byte("# Local\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "pinned-tag", "SKILL.md"), []byte("# Tag\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "pinned-sha", "SKILL.md"), []byte("# SHA\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "floating", "SKILL.md"), []byte("# Float v1\n"), 0o644)

	runGitTest(t, workRepo, "init", "-b", "main")
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "v1")
	initialSHA := runGitTest(t, workRepo, "rev-parse", "HEAD")
	runGitTest(t, workRepo, "tag", "-a", "v1.0.0", "-m", "v1")
	runGitTest(t, tmp, "clone", "--bare", workRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	ps := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		MCP: map[string]spec.MCPServerSpec{
			"github": {Transport: "http", URL: spec.EnvString{Raw: "https://api.example.com/mcp"}},
		},
		Skills: []spec.AssetSpec{
			{Name: "local", Source: spec.SourceSpec{Raw: "./local-skill"}},
			{Name: "pinned-tag", Source: spec.SourceSpec{Raw: repoURL + "#v1.0.0//skills/pinned-tag"}},
			{Name: "pinned-sha", Source: spec.SourceSpec{Raw: repoURL + "#" + initialSHA + "//skills/pinned-sha"}},
			{Name: "floating", Source: spec.SourceSpec{Raw: repoURL + "#main//skills/floating"}},
		},
	}

	st := &Store{RootDir: cacheDir, Git: &ExecGitRunner{}}
	opts := ParseOptions{AllowFileScheme: true}
	_, lf, err := st.ResolveAll(context.Background(), projDir, ps, nil, "", opts)
	if err != nil {
		t.Fatalf("initial ResolveAll failed: %v", err)
	}
	oldFloatHash := lf.Dependencies["skill:floating"].ContentHash

	// Validation errors for invalid UpdateTarget values.
	for _, badTarget := range []string{"mcp:github", "skill:local", "skill:missing"} {
		_, err := st.ResolveWithOptions(context.Background(), projDir, ps, lf, ResolveOptions{
			UpdateTarget: badTarget,
			ParseOpts:    opts,
		})
		var pe *spec.PolicyError
		if !errors.As(err, &pe) {
			t.Fatalf("expected PolicyError for UpdateTarget %q, got %v", badTarget, err)
		}
	}

	// Push a new commit to main in bareRepo.
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "floating", "SKILL.md"), []byte("# Float v2\n"), 0o644)
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "v2")
	newMainSHA := runGitTest(t, workRepo, "rev-parse", "HEAD")
	runGitTest(t, workRepo, "push", bareRepo, "main")

	// Upgrade all ("*") and verify deltas.
	res, err := st.ResolveWithOptions(context.Background(), projDir, ps, lf, ResolveOptions{
		UpdateTarget: "*",
		ParseOpts:    opts,
	})
	if err != nil {
		t.Fatalf("upgrade * failed: %v", err)
	}
	deltaByKey := make(map[string]LockDelta, len(res.Deltas))
	for _, d := range res.Deltas {
		deltaByKey[d.Key] = d
	}

	dTag, ok := deltaByKey["skill:pinned-tag"]
	if !ok || !dTag.Pinned || dTag.Action != "unchanged" {
		t.Fatalf("expected pinned tag delta Pinned=true Action=unchanged, got %+v", dTag)
	}
	dSHA, ok := deltaByKey["skill:pinned-sha"]
	if !ok || !dSHA.Pinned || dSHA.Action != "unchanged" {
		t.Fatalf("expected pinned SHA delta Pinned=true Action=unchanged, got %+v", dSHA)
	}
	dFloat, ok := deltaByKey["skill:floating"]
	if !ok || dFloat.Pinned || dFloat.Action != "updated" {
		t.Fatalf("expected floating branch delta Pinned=false Action=updated, got %+v", dFloat)
	}
	if dFloat.OldSHA != initialSHA || dFloat.NewSHA != newMainSHA {
		t.Fatalf("expected OldSHA=%s NewSHA=%s, got %+v", initialSHA, newMainSHA, dFloat)
	}
	if dFloat.OldHash != oldFloatHash || dFloat.NewHash == "" || dFloat.NewHash == oldFloatHash {
		t.Fatalf("expected OldHash=%s and distinct NewHash, got %+v", oldFloatHash, dFloat)
	}

	// Single-target upgrade ("skill:floating") after another commit, with pinned-tag untouched.
	_ = os.WriteFile(filepath.Join(workRepo, "skills", "floating", "SKILL.md"), []byte("# Float v3\n"), 0o644)
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "v3")
	v3SHA := runGitTest(t, workRepo, "rev-parse", "HEAD")
	runGitTest(t, workRepo, "push", bareRepo, "main")

	resSingle, err := st.ResolveWithOptions(context.Background(), projDir, ps, res.NextLock, ResolveOptions{
		UpdateTarget: "skill:floating",
		ParseOpts:    opts,
	})
	if err != nil {
		t.Fatalf("single-target upgrade failed: %v", err)
	}
	if len(resSingle.Deltas) != 1 || resSingle.Deltas[0].Key != "skill:floating" || resSingle.Deltas[0].NewSHA != v3SHA {
		t.Fatalf("expected single delta for skill:floating with NewSHA=%s, got %+v", v3SHA, resSingle.Deltas)
	}

	// Single-target upgrade fails with PolicyError if an unrelated dependency drifted from lf.
	psOtherDrift := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []spec.AssetSpec{
			{Name: "local", Source: spec.SourceSpec{Raw: "./local-skill"}},
			{Name: "pinned-tag", Source: spec.SourceSpec{Raw: repoURL + "#v9.9.9//skills/pinned-tag"}},
			{Name: "pinned-sha", Source: spec.SourceSpec{Raw: repoURL + "#" + initialSHA + "//skills/pinned-sha"}},
			{Name: "floating", Source: spec.SourceSpec{Raw: repoURL + "#main//skills/floating"}},
		},
	}
	_, err = st.ResolveWithOptions(context.Background(), projDir, psOtherDrift, resSingle.NextLock, ResolveOptions{
		UpdateTarget: "skill:floating",
		ParseOpts:    opts,
	})
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError when unrelated dependency drifted during targeted upgrade, got %v", err)
	}
}

func TestResolveWithOptionsReconcileAddAndReplaceToLocal(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workRepo := filepath.Join(tmp, "work")
	bareRepo := filepath.Join(tmp, "remote.git")
	cacheDir := filepath.Join(tmp, "cache")
	projDir := filepath.Join(tmp, "project")

	_ = os.MkdirAll(filepath.Join(workRepo, "agents", "helper"), 0o755)
	_ = os.MkdirAll(filepath.Join(workRepo, "rules", "style"), 0o755)
	_ = os.MkdirAll(filepath.Join(projDir, "local-agent"), 0o755)
	_ = os.WriteFile(filepath.Join(projDir, "local-agent", "AGENT.md"), []byte("# Local Agent\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workRepo, "agents", "helper", "AGENT.md"), []byte("# Helper\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workRepo, "rules", "style", "RULE.md"), []byte("# Style\n"), 0o644)

	runGitTest(t, workRepo, "init", "-b", "main")
	runGitTest(t, workRepo, "add", ".")
	runGitTest(t, workRepo, "commit", "-m", "init")
	runGitTest(t, tmp, "clone", "--bare", workRepo, bareRepo)

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	opts := ParseOptions{AllowFileScheme: true}
	st := &Store{RootDir: cacheDir, Git: &ExecGitRunner{}}

	ps1 := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Agents:  []spec.AgentSpec{{Name: "helper", Source: spec.SourceSpec{Raw: repoURL + "#main//agents/helper"}}},
	}
	_, lf1, err := st.ResolveAll(context.Background(), projDir, ps1, nil, "", opts)
	if err != nil {
		t.Fatal(err)
	}

	// Reconcile adding rule:style while agent:helper remains locked.
	ps2 := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Agents:  []spec.AgentSpec{{Name: "helper", Source: spec.SourceSpec{Raw: repoURL + "#main//agents/helper"}}},
		Rules:   []spec.RuleSpec{{Name: "style", Source: spec.SourceSpec{Raw: repoURL + "#main//rules/style"}}},
	}
	resAdd, err := st.ResolveWithOptions(context.Background(), projDir, ps2, lf1, ResolveOptions{
		ReconcileKey: "rule:style",
		ParseOpts:    opts,
	})
	if err != nil {
		t.Fatalf("reconcile add failed: %v", err)
	}
	if len(resAdd.Deltas) != 1 || resAdd.Deltas[0].Key != "rule:style" || resAdd.Deltas[0].Action != "added" {
		t.Fatalf("expected added delta for rule:style, got %+v", resAdd.Deltas)
	}

	// Reconcile replacing agent:helper with a local path -> agent:helper pruned from lockfile.
	ps3 := &spec.ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Agents:  []spec.AgentSpec{{Name: "helper", Source: spec.SourceSpec{Raw: "./local-agent"}}},
		Rules:   []spec.RuleSpec{{Name: "style", Source: spec.SourceSpec{Raw: repoURL + "#main//rules/style"}}},
	}
	resLocal, err := st.ResolveWithOptions(context.Background(), projDir, ps3, resAdd.NextLock, ResolveOptions{
		ReconcileKey: "agent:helper",
		ParseOpts:    opts,
	})
	if err != nil {
		t.Fatalf("reconcile replace-to-local failed: %v", err)
	}
	if _, ok := resLocal.NextLock.Dependencies["agent:helper"]; ok {
		t.Fatalf("expected agent:helper to be pruned from NextLock when replaced with local path")
	}
	if len(resLocal.Deltas) != 1 || resLocal.Deltas[0].Key != "agent:helper" || resLocal.Deltas[0].Action != "removed" {
		t.Fatalf("expected removed delta for agent:helper, got %+v", resLocal.Deltas)
	}

	// InspectDependencies across agents and rules + nil ProjectSpec guard.
	roStore := &Store{RootDir: cacheDir, Git: &failingGitRunner{t: t}}
	rep, err := roStore.InspectDependencies(projDir, ps3, resLocal.NextLock, opts)
	if err != nil || rep.LockStatus != "ok" {
		t.Fatalf("InspectDependencies failed: rep=%+v err=%v", rep, err)
	}
	if _, err := roStore.InspectDependencies(projDir, nil, nil, opts); err == nil {
		t.Fatalf("expected error on nil ProjectSpec in InspectDependencies")
	}
	if _, err := roStore.ResolveWithOptions(context.Background(), projDir, nil, nil, ResolveOptions{}); err == nil {
		t.Fatalf("expected error on nil ProjectSpec in ResolveWithOptions")
	}
}
