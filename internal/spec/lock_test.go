package spec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockSaveLoadRoundTripDeterministic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "Agentfile.lock")

	lf := &Lockfile{
		Version: 1,
		Dependencies: map[string]LockEntry{
			"skill:zeta": {
				Source:      "https://github.com/org/repo.git",
				Ref:         "v2.0.0",
				CommitSHA:   strings.Repeat("b", 40),
				ContentHash: "sha256:" + strings.Repeat("2", 64),
			},
			"agent:alpha": {
				Source:      "https://github.com/org/repo.git",
				Ref:         "v1.0.0",
				CommitSHA:   strings.Repeat("a", 40),
				ContentHash: "sha256:" + strings.Repeat("1", 64),
			},
		},
	}

	if err := SaveLock(lockPath, lf); err != nil {
		t.Fatalf("SaveLock failed: %v", err)
	}
	firstBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	loaded, err := LoadLock(lockPath)
	if err != nil {
		t.Fatalf("LoadLock failed: %v", err)
	}
	if err := SaveLock(lockPath, loaded); err != nil {
		t.Fatalf("second SaveLock failed: %v", err)
	}
	secondBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("lockfile serialization not deterministic:\n%s\nvs\n%s", firstBytes, secondBytes)
	}
	if strings.Index(string(firstBytes), "agent:alpha") > strings.Index(string(firstBytes), "skill:zeta") {
		t.Fatalf("expected sorted dependency keys in lockfile:\n%s", firstBytes)
	}
}

func TestVerifyMatchesSkipsLocalAndDetectsDrift(t *testing.T) {
	t.Parallel()
	s := &ProjectSpec{
		Version: 1,
		Targets: []string{"claude"},
		Skills: []AssetSpec{
			{Name: "local-skill", Source: SourceSpec{Raw: "./local/skill"}},
			{Name: "remote-skill", Source: SourceSpec{Raw: "https://github.com/org/skills.git#v1.0.0"}},
		},
	}
	inspect := func(src SourceSpec) (string, string, bool, error) {
		if strings.HasPrefix(src.Raw, "./") {
			return src.Raw, "", false, nil
		}
		parts := strings.SplitN(src.Raw, "#", 2)
		return parts[0], parts[1], true, nil
	}

	lf := &Lockfile{
		Version: 1,
		Dependencies: map[string]LockEntry{
			"skill:remote-skill": {
				Source:      "https://github.com/org/skills.git",
				Ref:         "v1.0.0",
				CommitSHA:   strings.Repeat("a", 40),
				ContentHash: "sha256:" + strings.Repeat("1", 64),
			},
		},
	}

	if err := VerifyMatches(lf, s, inspect); err != nil {
		t.Fatalf("expected match when local skill is absent from lockfile: %v", err)
	}

	// Ref edit in Agentfile.yaml must fail VerifyMatches with PolicyError.
	s.Skills[1].Source.Raw = "https://github.com/org/skills.git#v2.0.0"
	err := VerifyMatches(lf, s, inspect)
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on ref mismatch, got %v", err)
	}

	// Nil lockfile, stale lock entry, and missing entry in lockfile.
	if err := VerifyMatches(nil, s, inspect); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for nil lockfile")
	}
	lfStale := &Lockfile{
		Version: 1,
		Dependencies: map[string]LockEntry{
			"skill:remote-skill": lf.Dependencies["skill:remote-skill"],
			"skill:extra":        {Source: "https://x", CommitSHA: "a", ContentHash: "b"},
		},
	}
	s.Skills[1].Source.Raw = "https://github.com/org/skills.git#v1.0.0"
	if err := VerifyMatches(lfStale, s, inspect); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for stale lock entry")
	}
	delete(lf.Dependencies, "skill:remote-skill")
	if err := VerifyMatches(lf, s, inspect); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError for missing lock entry")
	}

	// Bad lockfile versions & empty lockfile fail LoadLock.
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.lock")
	_ = os.WriteFile(badPath, []byte("version: 2\n"), 0o644)
	if _, err := LoadLock(badPath); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on lockfile version 2")
	}
	_ = os.WriteFile(badPath, []byte(""), 0o644)
	if _, err := LoadLock(badPath); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on empty lockfile")
	}
	_ = os.WriteFile(badPath, []byte("version: 1\n---\nversion: 1\n"), 0o644)
	if _, err := LoadLock(badPath); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on multi-doc lockfile")
	}
	if err := SaveLock(badPath, nil); err == nil {
		t.Fatalf("expected error saving nil lockfile")
	}
}
