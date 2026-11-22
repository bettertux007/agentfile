package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHashTreeDeterministicAndIgnoresGit(t *testing.T) {
	t.Parallel()

	dirA := t.TempDir()
	dirB := t.TempDir()

	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(d, "empty"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("# Skill\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sub", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Extra .git metadata in dirA must be ignored.
	if err := os.MkdirAll(filepath.Join(dirA, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hA, err := HashTree(dirA)
	if err != nil {
		t.Fatalf("HashTree dirA failed: %v", err)
	}
	hB, err := HashTree(dirB)
	if err != nil {
		t.Fatalf("HashTree dirB failed: %v", err)
	}
	if hA != hB {
		t.Fatalf("expected identical hashes ignoring .git, got %s vs %s", hA, hB)
	}

	// Executable bit change must alter hash (POSIX only; Windows NTFS does not persist 0111).
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(dirB, "sub", "run.sh"), 0o644); err != nil {
			t.Fatal(err)
		}
		hB2, err := HashTree(dirB)
		if err != nil {
			t.Fatal(err)
		}
		if hA == hB2 {
			t.Fatalf("expected hash change when executable bit changed")
		}
	}
}

func TestHashTreeRejectsSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}
	_, err := HashTree(dir)
	if !errors.Is(err, ErrSymlinkNotAllowed) {
		t.Fatalf("expected ErrSymlinkNotAllowed, got %v", err)
	}
}

func TestHashTreeSingleFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := filepath.Join(dir, "rule.md")
	_ = os.WriteFile(f, []byte("# Rule\n"), 0o644)
	h, err := HashTree(f)
	if err != nil || h == "" {
		t.Fatalf("HashTree single file failed: h=%q err=%v", h, err)
	}
	if _, err := HashTree(filepath.Join(dir, "missing.md")); err == nil {
		t.Fatalf("expected error for missing file")
	}
}
