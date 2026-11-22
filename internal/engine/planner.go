package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
)

const maxOldContentBytes = 1 << 20

// ConflictChoice represents the user or policy resolution for a collision.
type ConflictChoice string

const (
	ChoiceOverwrite ConflictChoice = "overwrite"
	ChoiceBackup    ConflictChoice = "backup"
	ChoiceSkip      ConflictChoice = "skip"
	ChoiceAbort     ConflictChoice = "abort"
)

// Prompter resolves interactive TTY collisions when on_conflict == "ask".
type Prompter interface {
	AskConflict(relPath, reason string) (ConflictChoice, error)
}

// OpActionType defines a planned filesystem operation.
type OpActionType string

const (
	ActionWriteFile  OpActionType = "write_file"
	ActionCopyTree   OpActionType = "copy_tree"
	ActionSymlink    OpActionType = "symlink"
	ActionDeletePath OpActionType = "delete_path"
)

// PlanConflict records a workspace conflict discovered during inspection mode.
type PlanConflict struct {
	Target  string   `json:"target"`
	RelPath string   `json:"rel_path"`
	Reason  string   `json:"reason"`
	Owners  []string `json:"owners,omitempty"`
}

// SidecarWrite represents a manifest or lockfile update committed alongside workspace changes.
type SidecarWrite struct {
	RelPath string
	Content []byte
	Mode    os.FileMode
}

// PlannedAction is a concrete filesystem mutation computed by Plan.
type PlannedAction struct {
	Target        string
	RelPath       string
	Type          OpActionType
	Content       []byte
	OldContent    []byte
	OldKind       string
	OldLinkTarget string
	SourcePath    string
	Mode          os.FileMode
	NeedsBackup   bool
	Record        ArtifactRecord
}

// SecuritySummary highlights security-sensitive entries introduced or present in the sync.
type SecuritySummary struct {
	Hooks         []string `json:"hooks,omitempty"`
	AllowCommands []string `json:"allow_commands,omitempty"`
	DenyCommands  []string `json:"deny_commands,omitempty"`
	StdioMCPs     []string `json:"stdio_mcps,omitempty"`
}

// PlanSummary summarizes planned changes for human and --json output.
type PlanSummary struct {
	Created            []string        `json:"created,omitempty"`
	Updated            []string        `json:"updated,omitempty"`
	Pruned             []string        `json:"pruned,omitempty"`
	Unchanged          []string        `json:"unchanged,omitempty"`
	Conflicts          []PlanConflict  `json:"conflicts,omitempty"`
	Warnings           []string        `json:"warnings,omitempty"`
	SecurityHighlights SecuritySummary `json:"security_highlights,omitempty"`
}

// Plan holds all pre-validated mutations and updated state ready for Commit.
type Plan struct {
	ProjectRoot    string
	ActiveTargets  []string
	GitExclude     bool
	Actions        []PlannedAction
	PruneOps       []PlannedAction
	Conflicts      []PlanConflict
	SidecarWrites  []SidecarWrite
	NextState      *State
	AllExcludePath []string
	Summary        PlanSummary
}

// CollectProbes evaluates adapter workspace existence probes read-only.
func CollectProbes(projectRoot string, reqs []adapters.ProbeRequest) (adapters.ProbeResults, error) {
	out := make(adapters.ProbeResults, len(reqs))
	for _, r := range reqs {
		target := filepath.Join(projectRoot, filepath.FromSlash(r.RelPath))
		_, err := os.Stat(target)
		out[r.Key] = err == nil
	}
	return out, nil
}

// PolicyValidator validates merged target files for post-merge security contradictions.
type PolicyValidator func(target string, mergedFiles map[string][]byte) []adapters.Diagnostic

// PlanInput bundles inputs for Plan without coupling engine to concrete adapters.
type PlanInput struct {
	ProjectRoot      string
	ActiveTargets    []string
	Config           *config.EffectiveConfig
	Bundle           *ir.Bundle
	CompiledResults  []*adapters.CompileResult
	ValidatePolicy   PolicyValidator
	Prompter         Prompter
	InspectConflicts bool
}

// BuildPlan computes all filesystem operations, detects collisions, runs 3-way merges,
// validates post-merge security policies, and resolves conflicts before touching disk.
func BuildPlan(in PlanInput) (*Plan, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("nil effective config")
	}
	prevState, err := LoadState(in.ProjectRoot)
	if err != nil {
		return nil, err
	}
	nextState := cloneState(prevState)

	plan := &Plan{
		ProjectRoot:   in.ProjectRoot,
		ActiveTargets: append([]string(nil), in.ActiveTargets...),
		GitExclude:    in.Config.GitExclude,
		NextState:     nextState,
	}

	// Populate security highlights for dry-run / sync transparency.
	if in.Bundle != nil {
		for _, dc := range in.Bundle.DenyCommands {
			plan.Summary.SecurityHighlights.DenyCommands = append(plan.Summary.SecurityHighlights.DenyCommands, dc.Pattern)
		}
		plan.Summary.SecurityHighlights.AllowCommands = append([]string(nil), in.Bundle.AllowCommands...)
		for _, h := range in.Bundle.Hooks {
			plan.Summary.SecurityHighlights.Hooks = append(plan.Summary.SecurityHighlights.Hooks, h.Event+": "+h.Command.Raw)
		}
		for _, srv := range in.Bundle.MCP {
			if srv.Transport == "stdio" {
				plan.Summary.SecurityHighlights.StdioMCPs = append(plan.Summary.SecurityHighlights.StdioMCPs, srv.Name+": "+srv.Command.Raw)
			}
		}
	}

	// 1. Check compile diagnostics for fail-closed errors and collect warnings.
	for _, cr := range in.CompiledResults {
		for _, d := range cr.Diagnostics {
			switch d.Level {
			case adapters.DiagError:
				return nil, spec.NewPolicyError("[%s] %s", d.Target, d.Message)
			case adapters.DiagWarning:
				plan.Summary.Warnings = append(plan.Summary.Warnings, fmt.Sprintf("[%s] %s", d.Target, d.Message))
			}
		}
	}

	// 2. Check inter-adapter and case-insensitive path collisions across all compiled ops.
	seenLower := make(map[string]adapters.ArtifactOp)
	opsByTarget := make(map[string][]adapters.ArtifactOp)
	sharedOps := make(map[string]adapters.ArtifactOp)
	emittersSetByPath := make(map[string]map[string]bool)

	for _, cr := range in.CompiledResults {
		for _, op := range cr.Files {
			cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(op.RelPath)))
			if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
				return nil, spec.NewPolicyError("artifact path %q escapes project root", op.RelPath)
			}
			op.RelPath = cleanRel
			lowerKey := strings.ToLower(cleanRel)
			if prevOp, exists := seenLower[lowerKey]; exists {
				if !prevOp.Shared || !op.Shared {
					return nil, spec.NewPolicyError(
						"path collision between target %q (%s) and target %q (%s)",
						prevOp.Target, prevOp.RelPath, op.Target, op.RelPath,
					)
				}
				if !sharedOpsEqual(prevOp, op) {
					return nil, spec.NewPolicyError(
						"shared artifact mismatch between target %q (%s) and target %q (%s)",
						prevOp.Target, prevOp.RelPath, op.Target, op.RelPath,
					)
				}
			} else {
				seenLower[lowerKey] = op
			}

			if op.Shared {
				if _, ok := sharedOps[cleanRel]; !ok {
					sharedOps[cleanRel] = op
				}
				if emittersSetByPath[cleanRel] == nil {
					emittersSetByPath[cleanRel] = make(map[string]bool)
				}
				emittersSetByPath[cleanRel][op.Target] = true
			} else {
				opsByTarget[op.Target] = append(opsByTarget[op.Target], op)
			}
		}
	}

	emittersByPath := make(map[string][]string, len(emittersSetByPath))
	for rel, set := range emittersSetByPath {
		owners := make([]string, 0, len(set))
		for t := range set {
			owners = append(owners, t)
		}
		sort.Strings(owners)
		emittersByPath[rel] = owners
	}

	// 3. Plan operations per active target and for shared artifacts.
	type conflictKey struct {
		target  string
		relPath string
	}
	seenConflicts := make(map[conflictKey]bool)
	recordConflict := func(target, relPath, reason string, owners []string) (bool, bool, error) {
		if in.InspectConflicts && (in.Config.OnConflict == "ask" || in.Config.OnConflict == "fail") {
			key := conflictKey{target: target, relPath: relPath}
			if !seenConflicts[key] {
				seenConflicts[key] = true
				plan.Conflicts = append(plan.Conflicts, PlanConflict{
					Target:  target,
					RelPath: relPath,
					Reason:  reason,
					Owners:  append([]string(nil), owners...),
				})
			}
			return true, false, nil
		}
		return resolveConflictChoice(relPath, reason, in.Config.OnConflict, in.Prompter)
	}
	sourceHashes := make(map[string]string)

	for _, target := range in.ActiveTargets {
		if nextState.Targets[target] == nil {
			nextState.Targets[target] = &TargetState{Artifacts: make(map[string]ArtifactRecord)}
		}
		prevTargetState := prevState.Targets[target]
		if prevTargetState == nil {
			prevTargetState = &TargetState{Artifacts: make(map[string]ArtifactRecord)}
		}

		desiredPaths := make(map[string]bool)
		mergedFiles := make(map[string][]byte)
		ownedDirsSet := make(map[string]bool)

		for _, op := range opsByTarget[target] {
			desiredPaths[op.RelPath] = true
			collectCandidateDirs(in.ProjectRoot, op.RelPath, ownedDirsSet)

			prevRec, wasOwned := prevTargetState.Artifacts[op.RelPath]
			nextRec, recorded, mergedBytes, err := planSingleArtifactOp(
				in.ProjectRoot,
				target,
				op,
				prevRec,
				wasOwned,
				in.Config,
				sourceHashes,
				func(detail string) (bool, bool, error) {
					return recordConflict(target, op.RelPath, detail, nil)
				},
				plan,
			)
			if err != nil {
				return nil, err
			}
			if recorded {
				nextState.Targets[target].Artifacts[op.RelPath] = nextRec
			}
			if mergedBytes != nil {
				mergedFiles[op.RelPath] = mergedBytes
			}
		}

		// Preserve previously recorded owned_dirs alongside newly needed dirs so pruning knows what to clean.
		for _, od := range prevTargetState.OwnedDirs {
			ownedDirsSet[od] = true
		}
		nextState.Targets[target].OwnedDirs = sortOwnedDirsDesc(ownedDirsSet)

		// Post-merge security policy check.
		if in.ValidatePolicy != nil && len(mergedFiles) > 0 {
			for _, d := range in.ValidatePolicy(target, mergedFiles) {
				if d.Level == adapters.DiagError {
					return nil, spec.NewPolicyError("[%s] %s", d.Target, d.Message)
				}
				if d.Level == adapters.DiagWarning {
					plan.Summary.Warnings = append(plan.Summary.Warnings, fmt.Sprintf("[%s] %s", d.Target, d.Message))
				}
			}
		}

		// Plan pruning for artifacts previously owned by this target that are no longer desired.
		oldRels := make([]string, 0, len(prevTargetState.Artifacts))
		for oldRel := range prevTargetState.Artifacts {
			if !desiredPaths[oldRel] {
				oldRels = append(oldRels, oldRel)
			}
		}
		sort.Strings(oldRels)

		for _, oldRel := range oldRels {
			oldRec := prevTargetState.Artifacts[oldRel]
			delete(nextState.Targets[target].Artifacts, oldRel)
			if err := planPruneArtifact(in.ProjectRoot, target, oldRel, oldRec, plan); err != nil {
				return nil, err
			}
		}
	}

	// Plan shared operations once across all sharing targets.
	activeSet := make(map[string]bool, len(in.ActiveTargets))
	for _, t := range in.ActiveTargets {
		activeSet[t] = true
	}

	sharedRels := make([]string, 0, len(sharedOps))
	for rel := range sharedOps {
		sharedRels = append(sharedRels, rel)
	}
	sort.Strings(sharedRels)

	sharedOwnedDirsSet := make(map[string]bool)
	desiredSharedPaths := make(map[string]bool, len(sharedRels))
	for _, rel := range sharedRels {
		op := sharedOps[rel]
		desiredSharedPaths[rel] = true
		collectCandidateDirs(in.ProjectRoot, op.RelPath, sharedOwnedDirsSet)

		prevShared, wasOwned := prevState.Shared[rel]
		nextRec, recorded, mergedBytes, err := planSingleArtifactOp(
			in.ProjectRoot,
			"shared",
			op,
			prevShared.Record,
			wasOwned,
			in.Config,
			sourceHashes,
			func(detail string) (bool, bool, error) {
				return recordConflict("shared", op.RelPath, detail, emittersByPath[rel])
			},
			plan,
		)
		if err != nil {
			return nil, err
		}
		if recorded {
			ownerSet := make(map[string]bool)
			for _, prevOwner := range prevShared.Owners {
				if !activeSet[prevOwner] {
					ownerSet[prevOwner] = true
				}
			}
			for _, emitter := range emittersByPath[rel] {
				ownerSet[emitter] = true
			}
			owners := make([]string, 0, len(ownerSet))
			for o := range ownerSet {
				owners = append(owners, o)
			}
			sort.Strings(owners)
			nextState.Shared[rel] = SharedArtifactRecord{
				Record: nextRec,
				Owners: owners,
			}
		}
		if mergedBytes != nil && in.ValidatePolicy != nil {
			singleMerged := map[string][]byte{op.RelPath: mergedBytes}
			for _, emitter := range emittersByPath[rel] {
				for _, d := range in.ValidatePolicy(emitter, singleMerged) {
					if d.Level == adapters.DiagError {
						return nil, spec.NewPolicyError("[%s] %s", d.Target, d.Message)
					}
					if d.Level == adapters.DiagWarning {
						plan.Summary.Warnings = append(plan.Summary.Warnings, fmt.Sprintf("[%s] %s", d.Target, d.Message))
					}
				}
			}
		}
	}

	for _, od := range prevState.SharedOwnedDirs {
		sharedOwnedDirsSet[od] = true
	}
	nextState.SharedOwnedDirs = sortOwnedDirsDesc(sharedOwnedDirsSet)

	// Prune shared artifacts only when no remaining owners exist after stripping ActiveTargets.
	prevSharedRels := make([]string, 0, len(prevState.Shared))
	for rel := range prevState.Shared {
		if !desiredSharedPaths[rel] {
			prevSharedRels = append(prevSharedRels, rel)
		}
	}
	sort.Strings(prevSharedRels)

	for _, oldRel := range prevSharedRels {
		prevShared := prevState.Shared[oldRel]
		ownerSet := make(map[string]bool)
		for _, o := range prevShared.Owners {
			if !activeSet[o] {
				ownerSet[o] = true
			}
		}
		if len(ownerSet) > 0 {
			remaining := make([]string, 0, len(ownerSet))
			for o := range ownerSet {
				remaining = append(remaining, o)
			}
			sort.Strings(remaining)
			nextState.Shared[oldRel] = SharedArtifactRecord{
				Record: cloneArtifactRecord(prevShared.Record),
				Owners: remaining,
			}
			continue
		}
		delete(nextState.Shared, oldRel)
		if err := planPruneArtifact(in.ProjectRoot, "shared", oldRel, prevShared.Record, plan); err != nil {
			return nil, err
		}
	}

	if len(plan.Conflicts) > 0 {
		plan.Summary.Conflicts = append([]PlanConflict(nil), plan.Conflicts...)
	}

	// Build complete exclude list across all targets and shared artifacts retained in NextState.
	excludeSet := make(map[string]bool)
	for _, ts := range nextState.Targets {
		for rel := range ts.Artifacts {
			excludeSet[rel] = true
		}
	}
	for rel := range nextState.Shared {
		excludeSet[rel] = true
	}
	for rel := range excludeSet {
		plan.AllExcludePath = append(plan.AllExcludePath, rel)
	}
	sort.Strings(plan.AllExcludePath)

	nextState.ExcludeFingerprint = hashBytes([]byte(fmt.Sprintf(
		"git_exclude=%t\n%s", plan.GitExclude, strings.Join(plan.AllExcludePath, "\n"),
	)))

	return plan, nil
}

func sharedOpsEqual(a, b adapters.ArtifactOp) bool {
	if a.RelPath != b.RelPath || a.Kind != b.Kind || a.SourcePath != b.SourcePath || a.IsRemote != b.IsRemote {
		return false
	}
	if !bytes.Equal(a.Content, b.Content) {
		return false
	}
	if !reflect.DeepEqual(a.JSONMerge, b.JSONMerge) || !reflect.DeepEqual(a.TOMLMerge, b.TOMLMerge) {
		return false
	}
	return true
}

func sortOwnedDirsDesc(dirsSet map[string]bool) []string {
	dirs := make([]string, 0, len(dirsSet))
	for d := range dirsSet {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if len(dirs[i]) != len(dirs[j]) {
			return len(dirs[i]) > len(dirs[j])
		}
		return dirs[i] < dirs[j]
	})
	return dirs
}

func planSingleArtifactOp(
	projectRoot string,
	targetLabel string,
	op adapters.ArtifactOp,
	prevRec ArtifactRecord,
	wasOwned bool,
	cfg *config.EffectiveConfig,
	sourceHashes map[string]string,
	onConflict func(detail string) (bool, bool, error),
	plan *Plan,
) (ArtifactRecord, bool, []byte, error) {
	absPath := filepath.Join(projectRoot, filepath.FromSlash(op.RelPath))

	switch op.Kind {
	case adapters.OpMergeJSONC:
		var existing []byte
		exists := false
		if b, err := os.ReadFile(absPath); err == nil {
			existing = b
			exists = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return ArtifactRecord{}, false, nil, err
		}
		oldKind, oldLinkTarget, oldContent, _, ierr := inspectExistingPath(absPath, existing)
		if ierr != nil {
			return ArtifactRecord{}, false, nil, ierr
		}

		var prevPtr *ArtifactRecord
		if wasOwned {
			cp := prevRec
			prevPtr = &cp
		}
		mergedBytes, nextRec, needsBackup, err := MergeJSONC(existing, exists, op.JSONMerge, prevPtr, onConflict)
		if err != nil {
			return ArtifactRecord{}, false, nil, err
		}

		if !exists {
			plan.Summary.Created = append(plan.Summary.Created, op.RelPath)
		} else if bytes.Equal(existing, mergedBytes) {
			plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
			return nextRec, true, mergedBytes, nil
		} else {
			plan.Summary.Updated = append(plan.Summary.Updated, op.RelPath)
		}

		plan.Actions = append(plan.Actions, PlannedAction{
			Target:        targetLabel,
			RelPath:       op.RelPath,
			Type:          ActionWriteFile,
			Content:       mergedBytes,
			OldContent:    oldContent,
			OldKind:       oldKind,
			OldLinkTarget: oldLinkTarget,
			Mode:          0o644,
			NeedsBackup:   needsBackup && exists,
			Record:        nextRec,
		})
		return nextRec, true, mergedBytes, nil

	case adapters.OpMergeTOML:
		var existing []byte
		exists := false
		if b, err := os.ReadFile(absPath); err == nil {
			existing = b
			exists = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return ArtifactRecord{}, false, nil, err
		}
		oldKind, oldLinkTarget, oldContent, _, ierr := inspectExistingPath(absPath, existing)
		if ierr != nil {
			return ArtifactRecord{}, false, nil, ierr
		}

		var prevPtr *ArtifactRecord
		if wasOwned {
			cp := prevRec
			prevPtr = &cp
		}
		mergedBytes, nextRec, needsBackup, err := MergeTOML(existing, exists, op.TOMLMerge, prevPtr, onConflict)
		if err != nil {
			return ArtifactRecord{}, false, nil, err
		}

		if !exists {
			plan.Summary.Created = append(plan.Summary.Created, op.RelPath)
		} else if bytes.Equal(existing, mergedBytes) {
			plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
			return nextRec, true, mergedBytes, nil
		} else {
			plan.Summary.Updated = append(plan.Summary.Updated, op.RelPath)
		}

		plan.Actions = append(plan.Actions, PlannedAction{
			Target:        targetLabel,
			RelPath:       op.RelPath,
			Type:          ActionWriteFile,
			Content:       mergedBytes,
			OldContent:    oldContent,
			OldKind:       oldKind,
			OldLinkTarget: oldLinkTarget,
			Mode:          0o644,
			NeedsBackup:   needsBackup && exists,
			Record:        nextRec,
		})
		return nextRec, true, mergedBytes, nil

	case adapters.OpManagedBlock:
		var existing []byte
		exists := false
		if b, err := os.ReadFile(absPath); err == nil {
			existing = b
			exists = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return ArtifactRecord{}, false, nil, err
		}
		oldKind, oldLinkTarget, oldContent, _, ierr := inspectExistingPath(absPath, existing)
		if ierr != nil {
			return ArtifactRecord{}, false, nil, ierr
		}

		var prevPtr *ArtifactRecord
		if wasOwned {
			cp := prevRec
			prevPtr = &cp
		}
		mergedBytes, nextRec, needsBackup, err := MergeMarkdownBlock(existing, exists, op.Content, prevPtr, onConflict)
		if err != nil {
			return ArtifactRecord{}, false, nil, err
		}

		if !exists {
			plan.Summary.Created = append(plan.Summary.Created, op.RelPath)
		} else if bytes.Equal(existing, mergedBytes) {
			plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
			return nextRec, true, nil, nil
		} else {
			plan.Summary.Updated = append(plan.Summary.Updated, op.RelPath)
		}

		plan.Actions = append(plan.Actions, PlannedAction{
			Target:        targetLabel,
			RelPath:       op.RelPath,
			Type:          ActionWriteFile,
			Content:       mergedBytes,
			OldContent:    oldContent,
			OldKind:       oldKind,
			OldLinkTarget: oldLinkTarget,
			Mode:          0o644,
			NeedsBackup:   needsBackup && exists,
			Record:        nextRec,
		})
		return nextRec, true, nil, nil

	case adapters.OpWriteOwned, adapters.OpWriteDropIn:
		oldKind, oldLinkTarget, oldContent, exists, lerr := inspectExistingPath(absPath, nil)
		if lerr != nil {
			return ArtifactRecord{}, false, nil, lerr
		}
		needsBackup := false

		if exists && !wasOwned {
			overwrite, backup, err := onConflict("unmanaged file already exists on disk")
			if err != nil {
				return ArtifactRecord{}, false, nil, err
			}
			if !overwrite {
				plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
				return ArtifactRecord{}, false, nil, nil
			}
			needsBackup = backup
		}

		nextHash := hashBytes(op.Content)
		rec := ArtifactRecord{
			ArtifactType:       TypeFile,
			SHA256:             nextHash,
			CreatedByAgentfile: true,
		}

		if exists {
			if wasOwned && oldKind == "file" {
				if oldContent != nil && bytes.Equal(oldContent, op.Content) {
					plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
					return rec, true, nil, nil
				}
				if oldContent == nil {
					if curBytes, rerr := os.ReadFile(absPath); rerr == nil && bytes.Equal(curBytes, op.Content) {
						plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
						return rec, true, nil, nil
					}
				}
			}
			plan.Summary.Updated = append(plan.Summary.Updated, op.RelPath)
			if !wasOwned {
				needsBackup = needsBackup || cfg.OnConflict == "backup"
			}
		} else {
			plan.Summary.Created = append(plan.Summary.Created, op.RelPath)
		}

		plan.Actions = append(plan.Actions, PlannedAction{
			Target:        targetLabel,
			RelPath:       op.RelPath,
			Type:          ActionWriteFile,
			Content:       op.Content,
			OldContent:    oldContent,
			OldKind:       oldKind,
			OldLinkTarget: oldLinkTarget,
			Mode:          0o644,
			NeedsBackup:   needsBackup && exists,
			Record:        rec,
		})
		return rec, true, nil, nil

	case adapters.OpMaterializeTree:
		oldKind, oldLinkTarget, oldContent, exists, lerr := inspectExistingPath(absPath, nil)
		if lerr != nil {
			return ArtifactRecord{}, false, nil, lerr
		}
		needsBackup := false

		if exists && !wasOwned {
			overwrite, backup, err := onConflict("unmanaged path already exists on disk")
			if err != nil {
				return ArtifactRecord{}, false, nil, err
			}
			if !overwrite {
				plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
				return ArtifactRecord{}, false, nil, nil
			}
			needsBackup = backup
		}

		useSymlink := cfg.LinkMode == "symlink" && !op.IsRemote
		if cfg.LinkMode == "symlink" && op.IsRemote {
			plan.Summary.Warnings = append(plan.Summary.Warnings, fmt.Sprintf("[%s] %s is a remote dependency; materializing as copy instead of symlink", targetLabel, op.RelPath))
		}

		treeHash, ok := sourceHashes[op.SourcePath]
		if !ok {
			var herr error
			treeHash, herr = store.HashTree(op.SourcePath)
			if herr != nil {
				return ArtifactRecord{}, false, nil, herr
			}
			sourceHashes[op.SourcePath] = treeHash
		}

		actionType := ActionCopyTree
		artType := TypeDir
		if useSymlink {
			actionType = ActionSymlink
			artType = TypeSymlink
		}
		rec := ArtifactRecord{
			ArtifactType:       artType,
			SHA256:             treeHash,
			CreatedByAgentfile: true,
		}

		if exists && wasOwned && prevRec.ArtifactType == artType {
			if artType == TypeSymlink {
				if oldKind == "symlink" && oldLinkTarget == op.SourcePath {
					plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
					return rec, true, nil, nil
				}
			} else if curHash, cherr := store.HashTree(absPath); cherr == nil && curHash == treeHash {
				plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
				return rec, true, nil, nil
			}
		}

		if exists {
			plan.Summary.Updated = append(plan.Summary.Updated, op.RelPath)
		} else {
			plan.Summary.Created = append(plan.Summary.Created, op.RelPath)
		}

		plan.Actions = append(plan.Actions, PlannedAction{
			Target:        targetLabel,
			RelPath:       op.RelPath,
			Type:          actionType,
			OldContent:    oldContent,
			OldKind:       oldKind,
			OldLinkTarget: oldLinkTarget,
			SourcePath:    op.SourcePath,
			NeedsBackup:   needsBackup && exists,
			Record:        rec,
		})
		return rec, true, nil, nil
	}

	return ArtifactRecord{}, false, nil, nil
}

func planPruneArtifact(projectRoot, targetLabel, oldRel string, oldRec ArtifactRecord, plan *Plan) error {
	absOld := filepath.Join(projectRoot, filepath.FromSlash(oldRel))
	oldKind, oldLinkTarget, oldContent, oldExists, ierr := inspectExistingPath(absOld, nil)
	if ierr != nil {
		return ierr
	}
	if !oldExists {
		return nil
	}

	if oldRec.ArtifactType == TypeJSONCPatch {
		curBytes := oldContent
		var err error
		if curBytes == nil {
			curBytes, err = os.ReadFile(absOld)
		}
		if err == nil {
			updatedBytes, deleteFile, rerr := RemoveJSONC(curBytes, oldRec)
			if rerr != nil {
				return rerr
			}
			if !deleteFile {
				plan.Summary.Updated = append(plan.Summary.Updated, oldRel)
				plan.PruneOps = append(plan.PruneOps, PlannedAction{
					Target:        targetLabel,
					RelPath:       oldRel,
					Type:          ActionWriteFile,
					Content:       updatedBytes,
					OldContent:    oldContent,
					OldKind:       oldKind,
					OldLinkTarget: oldLinkTarget,
					Mode:          0o644,
				})
				return nil
			}
		}
	}

	if oldRec.ArtifactType == TypeTOMLPatch {
		curBytes := oldContent
		var err error
		if curBytes == nil {
			curBytes, err = os.ReadFile(absOld)
		}
		if err == nil {
			updatedBytes, deleteFile, rerr := RemoveTOML(curBytes, oldRec)
			if rerr != nil {
				return rerr
			}
			if !deleteFile {
				plan.Summary.Updated = append(plan.Summary.Updated, oldRel)
				plan.PruneOps = append(plan.PruneOps, PlannedAction{
					Target:        targetLabel,
					RelPath:       oldRel,
					Type:          ActionWriteFile,
					Content:       updatedBytes,
					OldContent:    oldContent,
					OldKind:       oldKind,
					OldLinkTarget: oldLinkTarget,
					Mode:          0o644,
				})
				return nil
			}
		}
	}

	if oldRec.ArtifactType == TypeMarkdownBlock {
		curBytes := oldContent
		var err error
		if curBytes == nil {
			curBytes, err = os.ReadFile(absOld)
		}
		if err == nil {
			updatedBytes, deleteFile, rerr := RemoveMarkdownBlock(curBytes, oldRec)
			if rerr != nil {
				return rerr
			}
			if !deleteFile {
				plan.Summary.Updated = append(plan.Summary.Updated, oldRel)
				plan.PruneOps = append(plan.PruneOps, PlannedAction{
					Target:        targetLabel,
					RelPath:       oldRel,
					Type:          ActionWriteFile,
					Content:       updatedBytes,
					OldContent:    oldContent,
					OldKind:       oldKind,
					OldLinkTarget: oldLinkTarget,
					Mode:          0o644,
				})
				return nil
			}
		}
	}

	plan.Summary.Pruned = append(plan.Summary.Pruned, oldRel)
	plan.PruneOps = append(plan.PruneOps, PlannedAction{
		Target:        targetLabel,
		RelPath:       oldRel,
		Type:          ActionDeletePath,
		OldContent:    oldContent,
		OldKind:       oldKind,
		OldLinkTarget: oldLinkTarget,
	})
	return nil
}

// CheckWorkspaceDrift reports whether the workspace has drifted from plan or needs sync/recovery.
func CheckWorkspaceDrift(projectRoot string, plan *Plan) (bool, string) {
	if plan == nil {
		return true, "missing plan"
	}
	if projectRoot == "" {
		projectRoot = plan.ProjectRoot
	}
	if len(plan.Conflicts) > 0 {
		return true, fmt.Sprintf("%d conflict(s) detected", len(plan.Conflicts))
	}
	if len(plan.Actions) > 0 {
		return true, fmt.Sprintf("%d pending action(s)", len(plan.Actions))
	}
	if len(plan.PruneOps) > 0 {
		return true, fmt.Sprintf("%d pending prune operation(s)", len(plan.PruneOps))
	}
	if _, err := os.Stat(JournalPath(projectRoot)); err == nil {
		return true, "pending commit journal (.agentfile/commit-journal.json)"
	} else if !errors.Is(err, os.ErrNotExist) {
		return true, fmt.Sprintf("stat commit journal: %v", err)
	}
	if _, err := os.Stat(StatePath(projectRoot)); errors.Is(err, os.ErrNotExist) {
		return true, "missing .agentfile/state.json"
	} else if err != nil {
		return true, fmt.Sprintf("stat state: %v", err)
	}
	prevState, err := LoadState(projectRoot)
	if err != nil {
		return true, fmt.Sprintf("load state: %v", err)
	}
	if plan.NextState != nil && prevState.ExcludeFingerprint != plan.NextState.ExcludeFingerprint {
		return true, "git exclude fingerprint mismatch"
	}
	if !StatesEqual(prevState, plan.NextState) {
		return true, "workspace state differs from plan"
	}
	return false, ""
}

func inspectExistingPath(absPath string, preloaded []byte) (oldKind, oldLinkTarget string, oldContent []byte, exists bool, err error) {
	info, lerr := os.Lstat(absPath)
	if lerr != nil {
		if errors.Is(lerr, os.ErrNotExist) {
			return "", "", nil, false, nil
		}
		return "", "", nil, false, lerr
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(absPath)
		if rerr != nil {
			return "symlink", "", nil, true, rerr
		}
		return "symlink", target, nil, true, nil
	}
	if info.IsDir() {
		return "dir", "", nil, true, nil
	}
	if info.Mode().IsRegular() {
		if info.Size() <= maxOldContentBytes {
			if preloaded != nil {
				return "file", "", bytes.Clone(preloaded), true, nil
			}
			b, rerr := os.ReadFile(absPath)
			if rerr != nil {
				return "file", "", nil, true, rerr
			}
			return "file", "", b, true, nil
		}
		return "file", "", nil, true, nil
	}
	return "", "", nil, true, nil
}

func resolveConflictChoice(relPath, reason, policy string, prompter Prompter) (overwrite bool, backup bool, err error) {
	switch policy {
	case "overwrite":
		return true, false, nil
	case "backup":
		return true, true, nil
	case "fail":
		return false, false, spec.NewPolicyError("conflict at %s: %s (re-run with --on-conflict=overwrite or --on-conflict=backup)", relPath, reason)
	case "ask":
		if prompter == nil {
			return false, false, spec.NewPolicyError("conflict at %s: %s (non-interactive; pass --on-conflict)", relPath, reason)
		}
		choice, err := prompter.AskConflict(relPath, reason)
		if err != nil {
			return false, false, err
		}
		switch choice {
		case ChoiceOverwrite:
			return true, false, nil
		case ChoiceBackup:
			return true, true, nil
		case ChoiceSkip:
			return false, false, nil
		default:
			return false, false, spec.NewPolicyError("aborted on conflict at %s", relPath)
		}
	default:
		return false, false, spec.NewPolicyError("unsupported on_conflict policy %q", policy)
	}
}

func collectCandidateDirs(projectRoot, relPath string, out map[string]bool) {
	dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relPath)))
	for dir != "." && dir != "" && dir != "/" {
		abs := filepath.Join(projectRoot, filepath.FromSlash(dir))
		if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
			out[dir] = true
		}
		dir = filepath.ToSlash(filepath.Dir(filepath.FromSlash(dir)))
	}
}
