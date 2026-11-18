package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LockKey formats the canonical dependency key (<kind>:<name>) in Agentfile.lock.
func LockKey(kind, name string) string {
	return kind + ":" + name
}

// LoadLock reads and validates Agentfile.lock (version 1).
func LoadLock(path string) (*Lockfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var lf Lockfile
	if err := dec.Decode(&lf); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, NewPolicyError("Agentfile.lock is empty")
		}
		return nil, NewPolicyError("invalid Agentfile.lock: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, NewPolicyError("Agentfile.lock must contain a single YAML document")
	}
	if lf.Version != 1 {
		return nil, NewPolicyError("unsupported Agentfile.lock version %d (expected 1)", lf.Version)
	}
	if lf.Dependencies == nil {
		lf.Dependencies = make(map[string]LockEntry)
	}
	return &lf, nil
}

// SaveLock atomically serializes Agentfile.lock as deterministic sorted-key YAML.
func SaveLock(path string, lf *Lockfile) error {
	if lf == nil {
		return fmt.Errorf("nil lockfile")
	}
	out := *lf
	out.Version = 1
	if out.Dependencies == nil {
		out.Dependencies = map[string]LockEntry{}
	}

	data, err := yaml.Marshal(&out)
	if err != nil {
		return fmt.Errorf("marshal Agentfile.lock: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create lockfile directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".Agentfile.lock-*")
	if err != nil {
		return fmt.Errorf("create temp lockfile: %w", err)
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
		return fmt.Errorf("write temp lockfile: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp lockfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp lockfile: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename lockfile: %w", err)
	}
	cleanup = false
	return nil
}

// SourceInspector resolves canonicalized source metadata for lock verification.
type SourceInspector func(src SourceSpec) (canonicalSource string, ref string, isRemote bool, err error)

// VerifyMatches checks that an existing lockfile matches every remote dependency in spec.
func VerifyMatches(lf *Lockfile, s *ProjectSpec, inspect SourceInspector) error {
	if lf == nil {
		return NewPolicyError("Agentfile.lock is missing")
	}
	if s == nil {
		return NewPolicyError("ProjectSpec is nil")
	}

	expected := make(map[string]struct {
		source string
		ref    string
	})

	collect := func(kind, name string, src SourceSpec) error {
		canon, ref, isRemote, err := inspect(src)
		if err != nil {
			return err
		}
		if !isRemote {
			return nil
		}
		expected[LockKey(kind, name)] = struct {
			source string
			ref    string
		}{source: canon, ref: ref}
		return nil
	}

	for _, sk := range s.Skills {
		if err := collect("skill", sk.Name, sk.Source); err != nil {
			return err
		}
	}
	for _, ag := range s.Agents {
		if err := collect("agent", ag.Name, ag.Source); err != nil {
			return err
		}
	}
	for _, rl := range s.Rules {
		if err := collect("rule", rl.Name, rl.Source); err != nil {
			return err
		}
	}

	for key, exp := range expected {
		got, ok := lf.Dependencies[key]
		if !ok {
			return NewPolicyError("dependency %s is missing from Agentfile.lock (run 'agentfile sync --update')", key)
		}
		if got.Source != exp.source || got.Ref != exp.ref {
			return NewPolicyError("dependency %s changed in Agentfile.yaml relative to Agentfile.lock (run 'agentfile sync --update=%s')", key, key)
		}
		if got.CommitSHA == "" || got.ContentHash == "" {
			return NewPolicyError("lock entry for %s is incomplete (run 'agentfile sync --update=%s')", key, key)
		}
	}

	for key := range lf.Dependencies {
		if _, ok := expected[key]; !ok {
			return NewPolicyError("stale dependency %s in Agentfile.lock not present in Agentfile.yaml (run 'agentfile sync --update')", key)
		}
	}
	return nil
}
