package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
)

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

// PlannedAction is a concrete filesystem mutation computed by Plan.
type PlannedAction struct {
	Target      string
	RelPath     string
	Type        OpActionType
	Content     []byte
	SourcePath  string
	Mode        os.FileMode
	NeedsBackup bool
	Record      ArtifactRecord
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
	ProjectRoot     string
	ActiveTargets   []string
	Config          *config.EffectiveConfig
	Bundle          *ir.Bundle
	CompiledResults []*adapters.CompileResult
	ValidatePolicy  PolicyValidator
	Prompter        Prompter
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
	for _, cr := range in.CompiledResults {
		for _, op := range cr.Files {
			cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(op.RelPath)))
			if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
				return nil, spec.NewPolicyError("artifact path %q escapes project root", op.RelPath)
			}
			op.RelPath = cleanRel
			lowerKey := strings.ToLower(cleanRel)
			if prevOp, exists := seenLower[lowerKey]; exists {
				return nil, spec.NewPolicyError(
					"path collision between target %q (%s) and target %q (%s)",
					prevOp.Target, prevOp.RelPath, op.Target, op.RelPath,
				)
			}
			seenLower[lowerKey] = op
			opsByTarget[op.Target] = append(opsByTarget[op.Target], op)
		}
	}

	// 3. Plan operations per active target.
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
			absPath := filepath.Join(in.ProjectRoot, filepath.FromSlash(op.RelPath))
			collectCandidateDirs(in.ProjectRoot, op.RelPath, ownedDirsSet)

			prevRec, wasOwned := prevTargetState.Artifacts[op.RelPath]

			switch op.Kind {
			case adapters.OpMergeJSONC:
				var existing []byte
				exists := false
				if b, err := os.ReadFile(absPath); err == nil {
					existing = b
					exists = true
				} else if !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}

				var prevPtr *ArtifactRecord
				if wasOwned {
					cp := prevRec
					prevPtr = &cp
				}
				mergedBytes, nextRec, needsBackup, err := MergeJSONC(existing, exists, op.JSONMerge, prevPtr, func(detail string) (bool, bool, error) {
					return resolveConflictChoice(op.RelPath, detail, in.Config.OnConflict, in.Prompter)
				})
				if err != nil {
					return nil, err
				}
				mergedFiles[op.RelPath] = mergedBytes
				nextState.Targets[target].Artifacts[op.RelPath] = nextRec

				if !exists {
					plan.Summary.Created = append(plan.Summary.Created, op.RelPath)
				} else if bytes.Equal(existing, mergedBytes) {
					plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
					continue
				} else {
					plan.Summary.Updated = append(plan.Summary.Updated, op.RelPath)
				}

				plan.Actions = append(plan.Actions, PlannedAction{
					Target:      target,
					RelPath:     op.RelPath,
					Type:        ActionWriteFile,
					Content:     mergedBytes,
					Mode:        0o644,
					NeedsBackup: needsBackup && exists,
					Record:      nextRec,
				})

			case adapters.OpManagedBlock:
				var existing []byte
				exists := false
				if b, err := os.ReadFile(absPath); err == nil {
					existing = b
					exists = true
				} else if !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}

				var prevPtr *ArtifactRecord
				if wasOwned {
					cp := prevRec
					prevPtr = &cp
				}
				mergedBytes, nextRec, needsBackup, err := MergeMarkdownBlock(existing, exists, op.Content, prevPtr, func(detail string) (bool, bool, error) {
					return resolveConflictChoice(op.RelPath, detail, in.Config.OnConflict, in.Prompter)
				})
				if err != nil {
					return nil, err
				}
				nextState.Targets[target].Artifacts[op.RelPath] = nextRec

				if !exists {
					plan.Summary.Created = append(plan.Summary.Created, op.RelPath)
				} else if bytes.Equal(existing, mergedBytes) {
					plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
					continue
				} else {
					plan.Summary.Updated = append(plan.Summary.Updated, op.RelPath)
				}

				plan.Actions = append(plan.Actions, PlannedAction{
					Target:      target,
					RelPath:     op.RelPath,
					Type:        ActionWriteFile,
					Content:     mergedBytes,
					Mode:        0o644,
					NeedsBackup: needsBackup && exists,
					Record:      nextRec,
				})

			case adapters.OpWriteOwned, adapters.OpWriteDropIn:
				_, lerr := os.Lstat(absPath)
				exists := lerr == nil
				needsBackup := false

				if exists && !wasOwned {
					overwrite, backup, err := resolveConflictChoice(op.RelPath, "unmanaged file already exists on disk", in.Config.OnConflict, in.Prompter)
					if err != nil {
						return nil, err
					}
					if !overwrite {
						plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
						continue
					}
					needsBackup = backup
				}

				nextHash := hashBytes(op.Content)
				rec := ArtifactRecord{
					ArtifactType:       TypeFile,
					SHA256:             nextHash,
					CreatedByAgentfile: true,
				}
				nextState.Targets[target].Artifacts[op.RelPath] = rec

				if exists {
					if curBytes, rerr := os.ReadFile(absPath); rerr == nil && bytes.Equal(curBytes, op.Content) && wasOwned {
						plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
						continue
					}
					plan.Summary.Updated = append(plan.Summary.Updated, op.RelPath)
					if !wasOwned {
						needsBackup = needsBackup || in.Config.OnConflict == "backup"
					}
				} else {
					plan.Summary.Created = append(plan.Summary.Created, op.RelPath)
				}

				plan.Actions = append(plan.Actions, PlannedAction{
					Target:      target,
					RelPath:     op.RelPath,
					Type:        ActionWriteFile,
					Content:     op.Content,
					Mode:        0o644,
					NeedsBackup: needsBackup && exists,
					Record:      rec,
				})

			case adapters.OpMaterializeTree:
				_, lerr := os.Lstat(absPath)
				exists := lerr == nil
				needsBackup := false

				if exists && !wasOwned {
					overwrite, backup, err := resolveConflictChoice(op.RelPath, "unmanaged path already exists on disk", in.Config.OnConflict, in.Prompter)
					if err != nil {
						return nil, err
					}
					if !overwrite {
						plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
						continue
					}
					needsBackup = backup
				}

				useSymlink := in.Config.LinkMode == "symlink" && !op.IsRemote
				if in.Config.LinkMode == "symlink" && op.IsRemote {
					plan.Summary.Warnings = append(plan.Summary.Warnings, fmt.Sprintf("[%s] %s is a remote dependency; materializing as copy instead of symlink", target, op.RelPath))
				}

				treeHash, herr := store.HashTree(op.SourcePath)
				if herr != nil {
					return nil, herr
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
				nextState.Targets[target].Artifacts[op.RelPath] = rec

				if exists && wasOwned && prevRec.ArtifactType == artType {
					if artType == TypeSymlink {
						if dest, rerr := os.Readlink(absPath); rerr == nil && dest == op.SourcePath {
							plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
							continue
						}
					} else if curHash, cherr := store.HashTree(absPath); cherr == nil && curHash == treeHash {
						plan.Summary.Unchanged = append(plan.Summary.Unchanged, op.RelPath)
						continue
					}
				}

				if exists {
					plan.Summary.Updated = append(plan.Summary.Updated, op.RelPath)
				} else {
					plan.Summary.Created = append(plan.Summary.Created, op.RelPath)
				}

				plan.Actions = append(plan.Actions, PlannedAction{
					Target:      target,
					RelPath:     op.RelPath,
					Type:        actionType,
					SourcePath:  op.SourcePath,
					NeedsBackup: needsBackup && exists,
					Record:      rec,
				})
			}
		}

		// Preserve previously recorded owned_dirs alongside newly needed dirs so pruning knows what to clean.
		for _, od := range prevTargetState.OwnedDirs {
			ownedDirsSet[od] = true
		}
		ownedDirs := make([]string, 0, len(ownedDirsSet))
		for d := range ownedDirsSet {
			ownedDirs = append(ownedDirs, d)
		}
		sort.Slice(ownedDirs, func(i, j int) bool {
			// Deepest directories first for bottom-up pruning.
			if len(ownedDirs[i]) != len(ownedDirs[j]) {
				return len(ownedDirs[i]) > len(ownedDirs[j])
			}
			return ownedDirs[i] < ownedDirs[j]
		})
		nextState.Targets[target].OwnedDirs = ownedDirs

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
		for oldRel, oldRec := range prevTargetState.Artifacts {
			if desiredPaths[oldRel] {
				continue
			}
			delete(nextState.Targets[target].Artifacts, oldRel)
			absOld := filepath.Join(in.ProjectRoot, filepath.FromSlash(oldRel))
			if _, err := os.Lstat(absOld); errors.Is(err, os.ErrNotExist) {
				continue
			}

			if oldRec.ArtifactType == TypeJSONCPatch {
				curBytes, err := os.ReadFile(absOld)
				if err == nil {
					updatedBytes, deleteFile, rerr := RemoveJSONC(curBytes, oldRec)
					if rerr != nil {
						return nil, rerr
					}
					if !deleteFile {
						plan.Summary.Updated = append(plan.Summary.Updated, oldRel)
						plan.PruneOps = append(plan.PruneOps, PlannedAction{
							Target:  target,
							RelPath: oldRel,
							Type:    ActionWriteFile,
							Content: updatedBytes,
							Mode:    0o644,
						})
						continue
					}
				}
			}

			if oldRec.ArtifactType == TypeMarkdownBlock {
				curBytes, err := os.ReadFile(absOld)
				if err == nil {
					updatedBytes, deleteFile, rerr := RemoveMarkdownBlock(curBytes, oldRec)
					if rerr != nil {
						return nil, rerr
					}
					if !deleteFile {
						plan.Summary.Updated = append(plan.Summary.Updated, oldRel)
						plan.PruneOps = append(plan.PruneOps, PlannedAction{
							Target:  target,
							RelPath: oldRel,
							Type:    ActionWriteFile,
							Content: updatedBytes,
							Mode:    0o644,
						})
						continue
					}
				}
			}

			plan.Summary.Pruned = append(plan.Summary.Pruned, oldRel)
			plan.PruneOps = append(plan.PruneOps, PlannedAction{
				Target:  target,
				RelPath: oldRel,
				Type:    ActionDeletePath,
			})
		}
	}

	// Build complete exclude list across all targets retained in NextState.
	excludeSet := make(map[string]bool)
	for _, ts := range nextState.Targets {
		for rel := range ts.Artifacts {
			excludeSet[rel] = true
		}
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
