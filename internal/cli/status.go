package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/adapters/registry"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/diff"
	"github.com/bettertux007/agentfile/internal/engine"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/spf13/cobra"
)

// StatusReport is the structured output of `agentfile status --json`.
type StatusReport struct {
	ManifestValid        bool                 `json:"manifest_valid"`
	SchemaErrors         []string             `json:"schema_errors,omitempty"`
	LockStatus           string               `json:"lock_status"`
	EffectiveConfig      *DoctorConfigSummary `json:"effective_config,omitempty"`
	UnhydratedDeps       []string             `json:"unhydrated_deps,omitempty"`
	MissingLocalDeps     []string             `json:"missing_local_deps,omitempty"`
	CommitJournalPending bool                 `json:"commit_journal_pending"`
	Drifted              bool                 `json:"drifted"`
	DriftReason          string               `json:"drift_reason,omitempty"`
	PerTarget            []StatusTargetReport `json:"per_target"`
	Warnings             []string             `json:"warnings,omitempty"`
}

// StatusTargetReport describes sync, conflict, and static isolation state for one target.
type StatusTargetReport struct {
	Target              string                `json:"target"`
	Status              string                `json:"status"` // in_sync | drifted | unhydrated | conflict | not_synced
	LoadGlobal          bool                  `json:"load_global"`
	LoadGlobalSource    string                `json:"load_global_source"`
	IsolationCapability string                `json:"isolation_capability"`
	MinIsolationVersion string                `json:"min_isolation_version,omitempty"`
	DriftedPaths        []string              `json:"drifted_paths,omitempty"`
	Conflicts           []engine.PlanConflict `json:"conflicts,omitempty"`
}

func newStatusCmd(ec *execContext) *cobra.Command {
	var targets []string
	var check bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Inspect manifest, lockfile, hydration, workspace drift, and isolation status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			parseOpts := store.ParseOptions{
				AllowFileScheme: os.Getenv("AGENTFILE_ALLOW_FILE_GIT") == "1",
			}
			ins, err := inspectWorkspace(ec.workDir, ec.isTTY, parseOpts)
			if err != nil {
				return err
			}

			rep := StatusReport{
				ManifestValid:   ins.ManifestValid,
				SchemaErrors:    append([]string(nil), ins.SchemaErrors...),
				LockStatus:      ins.LockStatus,
				EffectiveConfig: ins.ConfigSummary,
				PerTarget:       []StatusTargetReport{},
				Warnings:        append([]string(nil), ins.Warnings...),
			}
			if ins.Deps != nil {
				rep.UnhydratedDeps = append([]string(nil), ins.Deps.Unhydrated...)
				rep.MissingLocalDeps = append([]string(nil), ins.Deps.MissingLocal...)
			}
			if ins.LockStatus == "missing" {
				rep.Warnings = appendUniqueString(rep.Warnings, "Agentfile.lock does not exist yet; run 'agentfile sync' to pin remote dependencies")
			} else if ins.LockStatus == "out_of_sync" && ins.LockError != nil {
				rep.Warnings = appendUniqueString(rep.Warnings, ins.LockError.Error())
			}

			if _, jerr := os.Stat(engine.JournalPath(ec.workDir)); jerr == nil {
				rep.CommitJournalPending = true
			} else if !errors.Is(jerr, os.ErrNotExist) {
				rep.SchemaErrors = append(rep.SchemaErrors, fmt.Sprintf("stat commit journal: %v", jerr))
			}

			_, stateStatErr := os.Stat(engine.StatePath(ec.workDir))
			stateExists := stateStatErr == nil
			if stateStatErr != nil && !errors.Is(stateStatErr, os.ErrNotExist) {
				rep.SchemaErrors = append(rep.SchemaErrors, fmt.Sprintf("stat state: %v", stateStatErr))
			}
			prevState, stateLoadErr := engine.LoadState(ec.workDir)
			if stateLoadErr != nil {
				rep.SchemaErrors = append(rep.SchemaErrors, stateLoadErr.Error())
			}

			reg := registry.Default()
			var activeTargets []string
			if len(targets) > 0 {
				activeTargets = append(activeTargets, targets...)
			} else if ins.ProjectSpec != nil {
				activeTargets = append(activeTargets, ins.ProjectSpec.Targets...)
			}

			validTargets := make([]string, 0, len(activeTargets))
			for _, t := range activeTargets {
				if _, lerr := registry.Lookup(reg, t); lerr != nil {
					rep.SchemaErrors = append(rep.SchemaErrors, fmt.Sprintf("target %q is not supported (available: %v)", t, registry.Names(reg)))
					continue
				}
				validTargets = append(validTargets, t)
			}

			targetReports := make([]StatusTargetReport, 0, len(validTargets))
			for _, t := range validTargets {
				adp, _ := registry.Lookup(reg, t)
				contract := adp.SchemaContract()
				isoCap := "unsupported"
				if contract.MinIsolationVersion != "" {
					isoCap = "supported"
				}

				lg := true
				lgSource := config.SourceDefault
				if ins.EffectiveCfg != nil {
					var specCfg spec.TargetConfigSpec
					if ins.ProjectSpec != nil {
						if c, ok := ins.ProjectSpec.TargetsConfig[t]; ok {
							specCfg = c
						}
					}
					var ow string
					var rerr error
					lg, lgSource, ow, rerr = config.ResolveTargetLoadGlobal(specCfg, ins.EffectiveCfg, false, false, false)
					if rerr != nil {
						rep.SchemaErrors = append(rep.SchemaErrors, rerr.Error())
					}
					if ow != "" {
						rep.Warnings = appendUniqueString(rep.Warnings, ow)
					}
					if !lg && ins.ProjectSpec != nil {
						rep.Warnings = appendUniqueString(rep.Warnings,
							fmt.Sprintf("RUNTIME_ONLY_ISOLATION: target %s resolves load_global=false (%s); bare '%s' invocations still read user globals unless launched via 'agentfile run %s'", t, lgSource, adp.BinaryName(), t))
					}
				}

				targetReports = append(targetReports, StatusTargetReport{
					Target:              t,
					Status:              "not_synced",
					LoadGlobal:          lg,
					LoadGlobalSource:    lgSource,
					IsolationCapability: isoCap,
					MinIsolationVersion: contract.MinIsolationVersion,
				})
			}

			if ins.ProjectSpec != nil && ins.EffectiveCfg != nil && len(rep.SchemaErrors) == 0 && len(validTargets) > 0 {
				if len(rep.UnhydratedDeps) == 0 && len(rep.MissingLocalDeps) == 0 {
					plan, perr := buildInspectionPlan(ec.workDir, ins.ProjectSpec, ins.Deps.Assets, ins.EffectiveCfg, reg, validTargets)
					if perr != nil {
						rep.SchemaErrors = append(rep.SchemaErrors, perr.Error())
					} else {
						for _, w := range plan.Summary.Warnings {
							rep.Warnings = appendUniqueString(rep.Warnings, w)
						}
						drifted, driftReason := engine.CheckWorkspaceDrift(ec.workDir, plan)
						rep.Drifted = drifted
						rep.DriftReason = driftReason

						driftedByTarget := make(map[string]map[string]bool, len(validTargets))
						for _, t := range validTargets {
							driftedByTarget[t] = make(map[string]bool)
						}
						for _, a := range plan.Actions {
							if driftedByTarget[a.Target] != nil {
								driftedByTarget[a.Target][a.RelPath] = true
							}
						}
						for _, p := range plan.PruneOps {
							if driftedByTarget[p.Target] != nil {
								driftedByTarget[p.Target][p.RelPath] = true
							}
						}
						conflictsByTarget := make(map[string][]engine.PlanConflict, len(validTargets))
						for _, c := range plan.Conflicts {
							conflictsByTarget[c.Target] = append(conflictsByTarget[c.Target], c)
						}

						for i := range targetReports {
							t := targetReports[i].Target
							var dpaths []string
							for p := range driftedByTarget[t] {
								dpaths = append(dpaths, p)
							}
							sort.Strings(dpaths)
							targetReports[i].DriftedPaths = dpaths
							targetReports[i].Conflicts = conflictsByTarget[t]

							targetSynced := stateExists && prevState != nil && prevState.Targets[t] != nil
							switch {
							case len(conflictsByTarget[t]) > 0:
								targetReports[i].Status = "conflict"
							case !targetSynced:
								targetReports[i].Status = "not_synced"
							case len(dpaths) > 0 || rep.CommitJournalPending || drifted:
								targetReports[i].Status = "drifted"
							default:
								targetReports[i].Status = "in_sync"
							}
						}
					}
				} else {
					// Never invoke ir.Build or engine.BuildPlan when dependencies are unhydrated or missing.
					anyTargetDrifted := false
					for i := range targetReports {
						t := targetReports[i].Target
						dpaths := verifyTargetStateOnDisk(ec.workDir, prevState, t)
						targetReports[i].DriftedPaths = dpaths
						if len(dpaths) > 0 {
							anyTargetDrifted = true
						}
						targetSynced := stateExists && prevState != nil && prevState.Targets[t] != nil
						switch {
						case len(rep.UnhydratedDeps) > 0:
							targetReports[i].Status = "unhydrated"
						case !targetSynced:
							targetReports[i].Status = "not_synced"
						case len(dpaths) > 0 || rep.CommitJournalPending:
							targetReports[i].Status = "drifted"
						default:
							targetReports[i].Status = "in_sync"
						}
					}
					if rep.CommitJournalPending {
						rep.Drifted = true
						rep.DriftReason = "pending commit journal (.agentfile/commit-journal.json)"
					} else if !stateExists {
						rep.Drifted = true
						rep.DriftReason = "missing .agentfile/state.json"
					} else if anyTargetDrifted {
						rep.Drifted = true
						rep.DriftReason = "tracked workspace artifacts modified or missing on disk"
					}
				}
			}

			rep.PerTarget = targetReports

			for i, w := range rep.Warnings {
				rep.Warnings[i] = diff.RedactSecrets(w)
			}
			for i, e := range rep.SchemaErrors {
				rep.SchemaErrors[i] = diff.RedactSecrets(e)
			}

			if jsonOutput {
				enc := json.NewEncoder(ec.out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				printStatusHuman(ec.out, &rep)
			}

			if !rep.ManifestValid || len(rep.SchemaErrors) > 0 {
				return spec.NewPolicyError("status failed with %d error(s)", len(rep.SchemaErrors))
			}

			if check {
				if rep.LockStatus == "missing" || rep.LockStatus == "out_of_sync" || rep.LockStatus == "invalid" {
					return spec.NewPolicyError("Agentfile.lock is %s (run 'agentfile sync')", rep.LockStatus)
				}
				if rep.CommitJournalPending {
					return spec.NewPolicyError("pending commit journal detected (.agentfile/commit-journal.json)")
				}
				if len(rep.UnhydratedDeps) > 0 {
					return spec.NewPolicyError("unhydrated remote dependencies: %s", strings.Join(rep.UnhydratedDeps, ", "))
				}
				if len(rep.MissingLocalDeps) > 0 {
					return spec.NewPolicyError("missing local dependencies: %s", strings.Join(rep.MissingLocalDeps, ", "))
				}
				if rep.Drifted {
					return spec.NewPolicyError("workspace drift detected: %s", rep.DriftReason)
				}
				for _, pt := range rep.PerTarget {
					if len(pt.Conflicts) > 0 {
						return spec.NewPolicyError("unresolved conflict(s) for target %s", pt.Target)
					}
					if pt.Status != "in_sync" {
						return spec.NewPolicyError("target %s status is %s", pt.Target, pt.Status)
					}
				}
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&targets, "targets", nil, "Comma-separated subset of targets to inspect")
	cmd.Flags().BoolVar(&check, "check", false, "Exit 2 if workspace is drifted, unhydrated, conflicted, or lockfile is out of sync")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit status report as JSON")
	return cmd
}

func buildInspectionPlan(
	projectRoot string,
	ps *spec.ProjectSpec,
	assets map[string]store.ResolvedAsset,
	effCfg *config.EffectiveConfig,
	reg map[string]adapters.Adapter,
	activeTargets []string,
) (*engine.Plan, error) {
	bundle, err := ir.Build(ps, assets)
	if err != nil {
		return nil, err
	}
	var probeReqs []adapters.ProbeRequest
	for _, t := range activeTargets {
		adp, _ := registry.Lookup(reg, t)
		probeReqs = append(probeReqs, adp.RequiredProbes()...)
	}
	probeFacts, err := engine.CollectProbes(projectRoot, probeReqs)
	if err != nil {
		return nil, err
	}
	compiled := make([]*adapters.CompileResult, 0, len(activeTargets))
	for _, t := range activeTargets {
		adp, _ := registry.Lookup(reg, t)
		res, cerr := adp.Compile(bundle, probeFacts)
		if cerr != nil {
			return nil, cerr
		}
		compiled = append(compiled, res)
	}
	return engine.BuildPlan(engine.PlanInput{
		ProjectRoot:      projectRoot,
		ActiveTargets:    activeTargets,
		Config:           effCfg,
		Bundle:           bundle,
		CompiledResults:  compiled,
		InspectConflicts: true,
		ValidatePolicy: func(target string, mergedFiles map[string][]byte) []adapters.Diagnostic {
			adp, lerr := registry.Lookup(reg, target)
			if lerr != nil {
				return nil
			}
			return adp.CheckPolicy(bundle, mergedFiles)
		},
	})
}

func verifyTargetStateOnDisk(projectRoot string, st *engine.State, target string) []string {
	if st == nil {
		return nil
	}
	ts := st.Targets[target]
	if ts == nil || len(ts.Artifacts) == 0 {
		return nil
	}
	paths := make([]string, 0, len(ts.Artifacts))
	for p := range ts.Artifacts {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var drifted []string
	for _, rel := range paths {
		rec := ts.Artifacts[rel]
		abs := filepath.Join(projectRoot, filepath.FromSlash(rel))
		info, err := os.Lstat(abs)
		if err != nil {
			drifted = append(drifted, rel)
			continue
		}
		switch rec.ArtifactType {
		case engine.TypeFile, engine.TypeJSONCPatch, engine.TypeMarkdownBlock:
			if !info.Mode().IsRegular() {
				drifted = append(drifted, rel)
				continue
			}
			if rec.SHA256 != "" {
				if h, herr := sha256File(abs); herr != nil || h != rec.SHA256 {
					drifted = append(drifted, rel)
				}
			}
		case engine.TypeDir:
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				drifted = append(drifted, rel)
				continue
			}
			if rec.SHA256 != "" {
				if h, herr := store.HashTree(abs); herr != nil || h != rec.SHA256 {
					drifted = append(drifted, rel)
				}
			}
		case engine.TypeSymlink:
			if info.Mode()&os.ModeSymlink == 0 {
				drifted = append(drifted, rel)
				continue
			}
			resolved, rerr := filepath.EvalSymlinks(abs)
			if rerr != nil {
				drifted = append(drifted, rel)
				continue
			}
			if rec.SHA256 != "" {
				if h, herr := store.HashTree(resolved); herr != nil || h != rec.SHA256 {
					drifted = append(drifted, rel)
				}
			}
		}
	}
	return drifted
}

func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func printStatusHuman(w io.Writer, rep *StatusReport) {
	fmt.Fprintf(w, "Manifest valid: %v (lock_status=%s)\n", rep.ManifestValid, rep.LockStatus)
	if rep.EffectiveConfig != nil {
		linkSrc := rep.EffectiveConfig.Sources["link_mode"]
		confSrc := rep.EffectiveConfig.Sources["on_conflict"]
		exclSrc := rep.EffectiveConfig.Sources["git_exclude"]
		fmt.Fprintf(w, "Config: link_mode=%s (%s) on_conflict=%s (%s) git_exclude=%v (%s)\n",
			rep.EffectiveConfig.LinkMode, linkSrc,
			rep.EffectiveConfig.OnConflict, confSrc,
			rep.EffectiveConfig.GitExclude, exclSrc)
	}
	if len(rep.UnhydratedDeps) > 0 {
		fmt.Fprintf(w, "Unhydrated dependencies: %s (run 'agentfile sync' or 'agentfile diff --fetch')\n", strings.Join(rep.UnhydratedDeps, ", "))
	}
	if len(rep.MissingLocalDeps) > 0 {
		fmt.Fprintf(w, "Missing local dependencies: %s\n", strings.Join(rep.MissingLocalDeps, ", "))
	}
	if rep.CommitJournalPending {
		fmt.Fprintln(w, "Commit journal: PENDING (.agentfile/commit-journal.json)")
	}
	for _, pt := range rep.PerTarget {
		fmt.Fprintf(w, "Target %-8s [%s]: load_global=%t (%s) isolation=%s\n",
			pt.Target, pt.Status, pt.LoadGlobal, pt.LoadGlobalSource, pt.IsolationCapability)
		for _, p := range pt.DriftedPaths {
			fmt.Fprintf(w, "  drift: %s\n", p)
		}
		for _, c := range pt.Conflicts {
			fmt.Fprintf(w, "  conflict: %s (%s)\n", c.RelPath, c.Reason)
		}
	}
	for _, e := range rep.SchemaErrors {
		fmt.Fprintf(w, "error: %s\n", e)
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
}
