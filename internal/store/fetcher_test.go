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
