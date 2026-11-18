package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/bettertux007/agentfile/internal/spec"
)

func TestParseSourceVariants(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Local workspace path
	loc, err := ParseSource(spec.SourceSpec{Raw: "./local/skills/tf"}, root, ParseOptions{})
	if err != nil {
		t.Fatalf("local parse failed: %v", err)
	}
	if !loc.IsLocal || loc.LocalPath != filepath.Join(root, "local", "skills", "tf") {
		t.Fatalf("unexpected local result: %+v", loc)
	}

	// HTTPS with #ref and //subpath + embedded credentials stripped
	httpsSrc, err := ParseSource(spec.SourceSpec{Raw: "https://user:secret@GitHub.com/org/skills.git/#v1.0.0//skills/k8s"}, root, ParseOptions{})
	if err != nil {
		t.Fatalf("https parse failed: %v", err)
	}
	if httpsSrc.IsLocal || httpsSrc.CanonicalURL != "https://github.com/org/skills.git" || httpsSrc.Ref != "v1.0.0" || httpsSrc.Subpath != "skills/k8s" {
		t.Fatalf("unexpected https parse: %+v", httpsSrc)
	}

	// SCP-style git@host:org/repo.git with #ref and //subpath
	scpSrc, err := ParseSource(spec.SourceSpec{Raw: "git@GitHub.com:org/skills.git#feat/a//skills/foo"}, root, ParseOptions{})
	if err != nil {
		t.Fatalf("scp parse failed: %v", err)
	}
	if scpSrc.CanonicalURL != "git@github.com:org/skills.git" || scpSrc.Ref != "feat/a" || scpSrc.Subpath != "skills/foo" {
		t.Fatalf("unexpected scp parse: %+v", scpSrc)
	}

	// SSH URL with explicit port and //subpath without #ref
	portSrc, err := ParseSource(spec.SourceSpec{Raw: "ssh://git.example.com:2222/org/skills.git//sub/dir"}, root, ParseOptions{})
	if err != nil {
		t.Fatalf("ssh port parse failed: %v", err)
	}
	if portSrc.CanonicalURL != "ssh://git.example.com:2222/org/skills.git" || portSrc.Ref != "" || portSrc.Subpath != "sub/dir" {
		t.Fatalf("unexpected ssh port parse: %+v", portSrc)
	}

	// Structured YAML source
	stSrc, err := ParseSource(spec.SourceSpec{IsStructured: true, Git: "https://github.com/org/skills.git", Ref: "main", Path: "skills/a"}, root, ParseOptions{})
	if err != nil {
		t.Fatalf("structured parse failed: %v", err)
	}
	if stSrc.CanonicalURL != "https://github.com/org/skills.git" || stSrc.Ref != "main" || stSrc.Subpath != "skills/a" {
		t.Fatalf("unexpected structured parse: %+v", stSrc)
	}
}

func TestParseSourceRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	bad := []string{
		"ext::sh -c evil",
		"file:///tmp/repo",
		"../outside",
		"git@github.com:org/repo.git:skills/foo",
		"https://github.com/org/repo.git:skills/foo",
		"https://github.com/org/repo.git//../escape",
		"https:///missing-host/repo.git",
		"ftp://github.com/org/repo.git",
		"git@github.com:",
		"not-a-valid-source",
		"https://github.com/org/repo.git//\\backslash",
		"https://github.com/org/repo.git///abs",
	}
	for _, raw := range bad {
		_, err := ParseSource(spec.SourceSpec{Raw: raw}, root, ParseOptions{})
		if err == nil {
			t.Fatalf("expected error for %q", raw)
		}
		var pe *spec.PolicyError
		if !errors.As(err, &pe) {
			t.Fatalf("expected PolicyError for %q, got %T: %v", raw, err, err)
		}
	}
}
