package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bettertux007/agentfile/internal/spec"
)

const StateVersion = 1

// ArtifactType classifies materialized artifacts in .agentfile/state.json.
type ArtifactType string

const (
	TypeFile          ArtifactType = "file"
	TypeDir           ArtifactType = "dir"
	TypeSymlink       ArtifactType = "symlink"
	TypeJSONCPatch    ArtifactType = "jsonc_patch"
	TypeMarkdownBlock ArtifactType = "markdown_block"
)

// State represents .agentfile/state.json (version 1).
type State struct {
	Version            int                             `json:"version"`
	ExcludeFingerprint string                          `json:"exclude_fingerprint,omitempty"`
	Targets            map[string]*TargetState         `json:"targets"`
	Shared             map[string]SharedArtifactRecord `json:"shared,omitempty"`
	SharedOwnedDirs    []string                        `json:"shared_owned_dirs,omitempty"`
}

// SharedArtifactRecord tracks a workspace artifact co-owned by multiple targets.
type SharedArtifactRecord struct {
	Record ArtifactRecord `json:"record"`
	Owners []string       `json:"owners"`
}

// TargetState tracks artifacts and created parent directories for one adapter target.
type TargetState struct {
	Artifacts map[string]ArtifactRecord `json:"artifacts"`
	OwnedDirs []string                  `json:"owned_dirs,omitempty"`
}

// ArtifactRecord tracks ownership and merge baselines for one relative path.
type ArtifactRecord struct {
	ArtifactType       ArtifactType        `json:"artifact_type"`
	SHA256             string              `json:"sha256,omitempty"`
	JSONPointers       []string            `json:"json_pointers,omitempty"`
	JSONPointerHashes  map[string]string   `json:"json_pointer_hashes,omitempty"`
	ArrayEntries       map[string][]string `json:"array_entries,omitempty"`
	BlockHash          string              `json:"block_hash,omitempty"`
	CreatedByAgentfile bool                `json:"created_by_agentfile"`
}

// StatePath returns the absolute path to .agentfile/state.json.
func StatePath(projectRoot string) string {
	return filepath.Join(projectRoot, ".agentfile", "state.json")
}

// LoadState reads .agentfile/state.json, returning an empty initialized State if absent.
func LoadState(projectRoot string) (*State, error) {
	p := StatePath(projectRoot)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &State{
				Version: StateVersion,
				Targets: make(map[string]*TargetState),
				Shared:  make(map[string]SharedArtifactRecord),
			}, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, spec.NewPolicyError("invalid %s: %v", p, err)
	}
	if st.Version != StateVersion {
		return nil, spec.NewPolicyError("unsupported %s version %d (expected %d)", p, st.Version, StateVersion)
	}
	if st.Targets == nil {
		st.Targets = make(map[string]*TargetState)
	}
	for _, ts := range st.Targets {
		if ts.Artifacts == nil {
			ts.Artifacts = make(map[string]ArtifactRecord)
		}
	}
	if st.Shared == nil {
		st.Shared = make(map[string]SharedArtifactRecord)
	}
	return &st, nil
}

// SaveState atomically writes .agentfile/state.json.
func SaveState(projectRoot string, st *State) error {
	if st == nil {
		return fmt.Errorf("nil state")
	}
	st.Version = StateVersion
	if st.Targets == nil {
		st.Targets = make(map[string]*TargetState)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(StatePath(projectRoot), data, 0o644)
}

func cloneArtifactRecord(rec ArtifactRecord) ArtifactRecord {
	nrec := rec
	nrec.JSONPointers = append([]string(nil), rec.JSONPointers...)
	if rec.JSONPointerHashes != nil {
		nrec.JSONPointerHashes = make(map[string]string, len(rec.JSONPointerHashes))
		for pk, pv := range rec.JSONPointerHashes {
			nrec.JSONPointerHashes[pk] = pv
		}
	}
	if rec.ArrayEntries != nil {
		nrec.ArrayEntries = make(map[string][]string, len(rec.ArrayEntries))
		for ak, av := range rec.ArrayEntries {
			nrec.ArrayEntries[ak] = append([]string(nil), av...)
		}
	}
	return nrec
}

func cloneState(st *State) *State {
	out := &State{
		Version:            StateVersion,
		ExcludeFingerprint: st.ExcludeFingerprint,
		Targets:            make(map[string]*TargetState, len(st.Targets)),
		Shared:             make(map[string]SharedArtifactRecord, len(st.Shared)),
		SharedOwnedDirs:    append([]string(nil), st.SharedOwnedDirs...),
	}
	for t, ts := range st.Targets {
		nts := &TargetState{
			Artifacts: make(map[string]ArtifactRecord, len(ts.Artifacts)),
			OwnedDirs: append([]string(nil), ts.OwnedDirs...),
		}
		for k, rec := range ts.Artifacts {
			nts.Artifacts[k] = cloneArtifactRecord(rec)
		}
		out.Targets[t] = nts
	}
	for rel, sar := range st.Shared {
		out.Shared[rel] = SharedArtifactRecord{
			Record: cloneArtifactRecord(sar.Record),
			Owners: append([]string(nil), sar.Owners...),
		}
	}
	return out
}

// StatesEqual compares two State values while treating nil and empty slices/maps identically.
func StatesEqual(a, b *State) bool {
	return statesEqual(a, b)
}

func statesEqual(a, b *State) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Version != b.Version || a.ExcludeFingerprint != b.ExcludeFingerprint {
		return false
	}
	if len(a.Targets) != len(b.Targets) {
		return false
	}
	for k, tsA := range a.Targets {
		tsB, ok := b.Targets[k]
		if !ok || tsA == nil || tsB == nil {
			if tsA != tsB {
				return false
			}
			continue
		}
		if !stringSlicesEqual(tsA.OwnedDirs, tsB.OwnedDirs) {
			return false
		}
		if len(tsA.Artifacts) != len(tsB.Artifacts) {
			return false
		}
		for ak, recA := range tsA.Artifacts {
			recB, ok := tsB.Artifacts[ak]
			if !ok || !recordsEqual(recA, recB) {
				return false
			}
		}
	}
	if !stringSlicesEqual(a.SharedOwnedDirs, b.SharedOwnedDirs) {
		return false
	}
	if len(a.Shared) != len(b.Shared) {
		return false
	}
	for k, sarA := range a.Shared {
		sarB, ok := b.Shared[k]
		if !ok || !recordsEqual(sarA.Record, sarB.Record) || !stringSlicesEqual(sarA.Owners, sarB.Owners) {
			return false
		}
	}
	return true
}

func recordsEqual(a, b ArtifactRecord) bool {
	if a.ArtifactType != b.ArtifactType ||
		a.SHA256 != b.SHA256 ||
		a.BlockHash != b.BlockHash ||
		a.CreatedByAgentfile != b.CreatedByAgentfile {
		return false
	}
	if !stringSlicesEqual(a.JSONPointers, b.JSONPointers) {
		return false
	}
	if len(a.JSONPointerHashes) != len(b.JSONPointerHashes) {
		return false
	}
	for k, v := range a.JSONPointerHashes {
		if b.JSONPointerHashes[k] != v {
			return false
		}
	}
	if len(a.ArrayEntries) != len(b.ArrayEntries) {
		return false
	}
	for k, v := range a.ArrayEntries {
		if !stringSlicesEqual(v, b.ArrayEntries[k]) {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
