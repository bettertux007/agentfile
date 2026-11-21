package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/adapters/registry"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/diff"
	"github.com/bettertux007/agentfile/internal/engine"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/runner"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/spf13/cobra"
)

// RunDryRunReport is the structured output of `agentfile run --dry-run --json`.
type RunDryRunReport struct {
	Target             string            `json:"target"`
	Binary             string            `json:"binary"`
	Argv               []string          `json:"argv"`
	EnvAddedRedacted   map[string]string `json:"env_added_redacted"`
	LoadGlobal         bool              `json:"load_global"`
	LoadGlobalSource   string            `json:"load_global_source"`
	IsolationMode      string            `json:"isolation_mode"`
	SyncAction         string            `json:"sync_action"`
	FastPathHit        bool              `json:"fast_path_hit"`
	FastPathMissReason string            `json:"fast_path_miss_reason,omitempty"`
}

func newRunCmd(ec *execContext) *cobra.Command {
	var loadGlobal bool
	var noGlobal bool
	var syncMode string
	var noSync bool
	var noCache bool
	var projectRootFlag string
	var ifConfigured bool
	var onlyIsolated bool
	var dryRun bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "run [flags] <target> [target-args...]",
		Short: "Sync target configuration and launch an AI coding agent with optional global isolation",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonOutput && !dryRun {
				return spec.NewPolicyError("--json on 'agentfile run' requires --dry-run")
			}
			syncChanged := cmd.Flags().Changed("sync")
			if syncChanged && noSync {
				return spec.NewPolicyError("conflicting flags: --sync and --no-sync cannot be combined")
			}
			if noSync {
				syncMode = "never"
			}
			switch syncMode {
			case "auto", "check", "never":
			default:
				return spec.NewPolicyError("invalid --sync mode %q (expected auto, check, or never)", syncMode)
			}

			target := args[0]
			userArgs := args[1:]

			launchDir := ec.workDir
			projectRoot := ec.workDir
			if strings.TrimSpace(projectRootFlag) != "" {
				absRoot, err := filepath.Abs(projectRootFlag)
				if err != nil {
					return err
				}
				projectRoot = absRoot
			}

			reg := registry.Default()
			adp, err := registry.Lookup(reg, target)
			if err != nil {
				return spec.NewPolicyError("target %q is not supported: %v", target, err)
			}

			passthroughExec := func() error {
				if dryRun {
					binPath := adp.BinaryName()
					if resolved, lerr := exec.LookPath(adp.BinaryName()); lerr == nil && resolved != "" {
						binPath = resolved
					}
					rep := RunDryRunReport{
						Target:           target,
						Binary:           binPath,
						Argv:             append([]string(nil), userArgs...),
						EnvAddedRedacted: map[string]string{},
						LoadGlobal:       true,
						LoadGlobalSource: config.SourceDefault,
						IsolationMode:    string(adapters.IsolationNone),
						SyncAction:       "passthrough",
					}
					if jsonOutput {
						enc := json.NewEncoder(ec.out)
						enc.SetIndent("", "  ")
						return enc.Encode(rep)
					}
					fmt.Fprintf(ec.out, "Run dry-run: target=%s sync=passthrough\n", rep.Target)
					fmt.Fprintf(ec.out, "Command: %s %s\n", rep.Binary, strings.Join(rep.Argv, " "))
					return nil
				}
				absBin, lerr := exec.LookPath(adp.BinaryName())
				if lerr != nil {
					return spec.NewPolicyError("target binary %q not found on $PATH: %v", adp.BinaryName(), lerr)
				}
				return ec.procRunner.Run(absBin, userArgs, os.Environ(), launchDir)
			}

			// Under --if-configured (used by shell-hook), non-session invocations (--version, --help, auth)
			// pass through directly without syncing or reading Agentfile.lock.
			if ifConfigured && !adp.IsSessionArgs(userArgs) {
				return passthroughExec()
			}

			manifestPath := filepath.Join(projectRoot, "Agentfile.yaml")
			rawChild, err := spec.ParseRawFile(manifestPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if ifConfigured {
						return passthroughExec()
					}
					return spec.NewPolicyError("Agentfile.yaml not found in %s (run 'agentfile init' first)", projectRoot)
				}
				return err
			}

			// Pre-composition short-circuit: when rawChild.Targets is non-empty, it deterministically
			// replaces inherited targets. If --if-configured is set and target is not listed, pass through immediately.
			if ifConfigured && len(rawChild.Targets) > 0 && !slices.Contains(rawChild.Targets, target) {
				return passthroughExec()
			}
			if !ifConfigured && len(rawChild.Extends) == 0 && !slices.Contains(rawChild.Targets, target) {
				return spec.NewPolicyError("target %q is not configured in Agentfile.yaml (configured targets: %v)", target, rawChild.Targets)
			}

			effCfg, err := config.Resolve(projectRoot, config.CLIOverrides{}, ec.isTTY)
			if err != nil {
				return err
			}

			probes, err := engine.CollectProbes(projectRoot, adp.RequiredProbes())
			if err != nil {
				return err
			}

			cacheRoot, err := store.ResolveCacheRoot()
			if err != nil {
				return err
			}

			loadGlobalChanged := cmd.Flags().Changed("load-global")
			var (
				lg                 bool
				lgSource           string
				overrideWarn       string
				bundle             *ir.Bundle
				fastPathHit        bool
				fastPathMissReason string
			)

			// Check per-target input-digest fast path BEFORE composing/hydrating manifests.
			if (syncMode == "auto" || syncMode == "check") && !noCache {
				rec, hit, missReason := engine.CheckRunFastPath(engine.RunFastPathCheckInput{
					ProjectRoot:      projectRoot,
					AgentfileVersion: Version,
					Target:           target,
					EffectiveConfig:  effCfg,
					CacheRoot:        cacheRoot,
					Probes:           probes,
				})
				if hit && rec != nil {
					if !slices.Contains(rec.Targets, target) {
						if ifConfigured {
							return passthroughExec()
						}
						return spec.NewPolicyError("target %q is not configured in Agentfile.yaml (configured targets: %v)", target, rec.Targets)
					}
					lg, lgSource, overrideWarn, err = config.ResolveTargetLoadGlobal(
						rec.TargetConfig,
						effCfg,
						loadGlobal,
						loadGlobalChanged,
						noGlobal,
					)
					if err != nil {
						return err
					}
					if onlyIsolated && lg {
						return passthroughExec()
					}
					fastPathHit = true
					bundle = rec.BuildBundleForRun()
					if ec.verbose {
						fmt.Fprintf(ec.errOut, "info: [%s] fast path hit (.agentfile/run-cache/%s.json)\n", target, target)
					}
				} else {
					fastPathMissReason = missReason
					if ec.verbose && missReason != "" {
						fmt.Fprintf(ec.errOut, "info: [%s] fast path miss: %s\n", target, missReason)
					}
				}
			}

			syncAction := "skipped"
			if fastPathHit {
				syncAction = "fast_path"
				if overrideWarn != "" {
					fmt.Fprintf(ec.errOut, "warning: %s\n", diff.RedactSecrets(overrideWarn))
				}
			} else {
				st := &store.Store{RootDir: cacheRoot, Git: &store.ExecGitRunner{}}
				lf, lerr := spec.LoadLock(filepath.Join(projectRoot, "Agentfile.lock"))
				if lerr != nil && !errors.Is(lerr, os.ErrNotExist) {
					return lerr
				}
				parseOpts := store.ParseOptions{
					AllowFileScheme: os.Getenv("AGENTFILE_ALLOW_FILE_GIT") == "1",
				}

				var ps *spec.ProjectSpec
				if len(rawChild.Extends) == 0 {
					if verr := spec.ValidateEffective(rawChild); verr != nil {
						return verr
					}
					ps = rawChild
				} else if syncMode == "never" {
					cm, _, ierr := st.InspectComposedManifest(projectRoot, rawChild, lf, parseOpts)
					if ierr != nil {
						return ierr
					}
					ps = cm.Effective
				} else {
					compRes, cerr := st.ComposeManifest(cmd.Context(), projectRoot, rawChild, lf, store.ResolveOptions{
						ParseOpts: parseOpts,
					})
					if cerr != nil {
						return cerr
					}
					ps = compRes.Composed.Effective
				}

				if !slices.Contains(ps.Targets, target) {
					if ifConfigured {
						return passthroughExec()
					}
					return spec.NewPolicyError("target %q is not configured in Agentfile.yaml (configured targets: %v)", target, ps.Targets)
				}

				lg, lgSource, overrideWarn, err = config.ResolveTargetLoadGlobal(
					ps.TargetsConfig[target],
					effCfg,
					loadGlobal,
					loadGlobalChanged,
					noGlobal,
				)
				if err != nil {
					return err
				}
				if onlyIsolated && lg {
					return passthroughExec()
				}
				if overrideWarn != "" {
					fmt.Fprintf(ec.errOut, "warning: %s\n", diff.RedactSecrets(overrideWarn))
				}
				if syncMode != "auto" {
					for _, w := range effCfg.Warnings {
						fmt.Fprintf(ec.errOut, "warning: %s\n", diff.RedactSecrets(w))
					}
				}

				syncEC := *ec
				syncEC.workDir = projectRoot

				switch syncMode {
				case "auto":
					if dryRun {
						syncAction = "dry_run"
					} else {
						syncAction = "synced"
					}
					if _, err := executeSyncWithSpec(cmd.Context(), &syncEC, syncOptions{
						Targets:     []string{target},
						DryRun:      dryRun,
						QuietStdout: true,
					}, rawChild, store.ResolveOptions{
						ParseOpts: parseOpts,
					}); err != nil {
						return err
					}
				case "check":
					syncAction = "checked"
					if _, err := executeSyncWithSpec(cmd.Context(), &syncEC, syncOptions{
						Targets:     []string{target},
						CheckOnly:   true,
						QuietStdout: true,
					}, rawChild, store.ResolveOptions{
						ParseOpts: parseOpts,
					}); err != nil {
						return err
					}
				}

				bundle = ir.BuildForRun(ps)
				probes, err = engine.CollectProbes(projectRoot, adp.RequiredProbes())
				if err != nil {
					return err
				}
			}

			if os.Getenv("AGENTFILE_ACTIVE") == "1" {
				fmt.Fprintln(ec.errOut, "warning: nested agentfile run detected (AGENTFILE_ACTIVE=1)")
			}

			facts, err := runner.GatherFactsWithOptions(cmd.Context(), projectRoot, adp, probes, lg, userArgs, !noCache)
			if err != nil {
				return err
			}

			plan, err := adp.PlanLaunch(bundle, adapters.LaunchOptions{
				LoadGlobal: lg,
				UserArgs:   userArgs,
			}, facts)
			if err != nil {
				return err
			}

			for _, d := range plan.Diagnostics {
				switch d.Level {
				case adapters.DiagError:
					return spec.NewPolicyError("[%s] %s", d.Target, diff.RedactSecrets(d.Message))
				case adapters.DiagWarning:
					fmt.Fprintf(ec.errOut, "warning: [%s] %s\n", d.Target, diff.RedactSecrets(d.Message))
				case adapters.DiagInfo:
					if ec.verbose {
						fmt.Fprintf(ec.errOut, "info: [%s] %s\n", d.Target, diff.RedactSecrets(d.Message))
					}
				}
			}

			if dryRun {
				binPath := adp.BinaryName()
				if resolved, lerr := exec.LookPath(adp.BinaryName()); lerr == nil && resolved != "" {
					binPath = resolved
				}
				argv := make([]string, 0, len(plan.PrependedArgs)+len(userArgs))
				argv = append(argv, plan.PrependedArgs...)
				argv = append(argv, userArgs...)

				envAdded := map[string]string{
					"AGENTFILE_ACTIVE":         "1",
					"AGENTFILE_ROOT":           projectRoot,
					"AGENTFILE_TARGET":         target,
					"AGENTFILE_LOAD_GLOBAL":    fmt.Sprintf("%t", lg),
					"AGENTFILE_ISOLATION_MODE": string(plan.Mode),
				}
				for k, v := range plan.EnvOverrides {
					envAdded[k] = redactEnvValue(k, v)
				}
				rep := RunDryRunReport{
					Target:             target,
					Binary:             binPath,
					Argv:               argv,
					EnvAddedRedacted:   envAdded,
					LoadGlobal:         lg,
					LoadGlobalSource:   lgSource,
					IsolationMode:      string(plan.Mode),
					SyncAction:         syncAction,
					FastPathHit:        fastPathHit,
					FastPathMissReason: fastPathMissReason,
				}
				if jsonOutput {
					enc := json.NewEncoder(ec.out)
					enc.SetIndent("", "  ")
					return enc.Encode(rep)
				}
				fmt.Fprintf(ec.out, "Run dry-run: target=%s load_global=%t (%s) mode=%s sync=%s\n",
					rep.Target, rep.LoadGlobal, rep.LoadGlobalSource, rep.IsolationMode, rep.SyncAction)
				fmt.Fprintf(ec.out, "Command: %s %s\n", rep.Binary, strings.Join(rep.Argv, " "))
				return nil
			}

			return runner.ExecuteWithOptions(
				cmd.Context(),
				ec.procRunner,
				runner.ExecuteOptions{
					ProjectRoot: projectRoot,
					LaunchDir:   launchDir,
					QuietBanner: !adp.IsSessionArgs(userArgs),
				},
				adp,
				plan,
				userArgs,
				lg,
				facts.CacheRoot,
				ec.errOut,
				ec.verbose,
			)
		},
	}

	// Stop parsing agentfile flags at the first positional argument (<target>).
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().BoolVar(&loadGlobal, "load-global", true, "Load native user-global agent configs")
	cmd.Flags().BoolVar(&noGlobal, "no-global", false, "Disable native user-global agent configs (shorthand for --load-global=false)")
	cmd.Flags().StringVar(&syncMode, "sync", "auto", "Pre-launch sync mode: auto, check, or never")
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "Shorthand for --sync=never")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "Bypass fast-path run cache and binary version probe cache")
	cmd.Flags().StringVar(&projectRootFlag, "project-root", "", "Project root directory containing Agentfile.yaml (preserves current working directory for launch)")
	cmd.Flags().BoolVar(&ifConfigured, "if-configured", false, "Pass through directly to the native binary if target is not configured in Agentfile.yaml")
	cmd.Flags().BoolVar(&onlyIsolated, "only-isolated", false, "Pass through directly to the native binary when resolved load_global is true")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print resolved command and isolation state without launching")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit --dry-run report as JSON")
	return cmd
}

func redactEnvValue(k, v string) string {
	if v == "" {
		return ""
	}
	kv := k + "=" + strconv.Quote(v)
	redKV := diff.RedactSecrets(kv)
	if rest, ok := strings.CutPrefix(redKV, k+"="); ok {
		if unq, err := strconv.Unquote(rest); err == nil {
			return unq
		}
	}
	return diff.RedactSecrets(v)
}
