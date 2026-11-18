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
	Version            int                     `json:"version"`
	ExcludeFingerprint string                  `json:"exclude_fingerprint,omitempty"`
	Targets            map[string]*TargetState `json:"targets"`
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

func cloneState(st *State) *State {
	out := &State{
		Version:            StateVersion,
		ExcludeFingerprint: st.ExcludeFingerprint,
		Targets:            make(map[string]*TargetState, len(st.Targets)),
	}
	for t, ts := range st.Targets {
		nts := &TargetState{
			Artifacts: make(map[string]ArtifactRecord, len(ts.Artifacts)),
			OwnedDirs: append([]string(nil), ts.OwnedDirs...),
		}
		for k, rec := range ts.Artifacts {
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
			nts.Artifacts[k] = nrec
		}
		out.Targets[t] = nts
	}
	return out
}
