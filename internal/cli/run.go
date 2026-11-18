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
	"github.com/spf13/cobra"
)

// RunDryRunReport is the structured output of `agentfile run --dry-run --json`.
type RunDryRunReport struct {
	Target           string            `json:"target"`
	Binary           string            `json:"binary"`
	Argv             []string          `json:"argv"`
	EnvAddedRedacted map[string]string `json:"env_added_redacted"`
	LoadGlobal       bool              `json:"load_global"`
	LoadGlobalSource string            `json:"load_global_source"`
	IsolationMode    string            `json:"isolation_mode"`
	SyncAction       string            `json:"sync_action"`
}

func newRunCmd(ec *execContext) *cobra.Command {
	var loadGlobal bool
	var noGlobal bool
	var syncMode string
	var noSync bool
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

			manifestPath := filepath.Join(ec.workDir, "Agentfile.yaml")
			ps, err := spec.ParseFile(manifestPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return spec.NewPolicyError("Agentfile.yaml not found in %s (run 'agentfile init' first)", ec.workDir)
				}
				return err
			}

			if !slices.Contains(ps.Targets, target) {
				return spec.NewPolicyError("target %q is not configured in Agentfile.yaml (configured targets: %v)", target, ps.Targets)
			}

			reg := registry.Default()
			adp, err := registry.Lookup(reg, target)
			if err != nil {
				return spec.NewPolicyError("target %q is not supported: %v", target, err)
			}

			effCfg, err := config.Resolve(ec.workDir, config.CLIOverrides{}, ec.isTTY)
			if err != nil {
				return err
			}

			loadGlobalChanged := cmd.Flags().Changed("load-global")
			lg, lgSource, overrideWarn, err := config.ResolveTargetLoadGlobal(
				ps.TargetsConfig[target],
				effCfg,
				loadGlobal,
				loadGlobalChanged,
				noGlobal,
			)
			if err != nil {
				return err
			}
			if overrideWarn != "" {
				fmt.Fprintf(ec.errOut, "warning: %s\n", diff.RedactSecrets(overrideWarn))
			}
			if syncMode != "auto" {
				for _, w := range effCfg.Warnings {
					fmt.Fprintf(ec.errOut, "warning: %s\n", diff.RedactSecrets(w))
				}
			}

			if os.Getenv("AGENTFILE_ACTIVE") == "1" {
				fmt.Fprintln(ec.errOut, "warning: nested agentfile run detected (AGENTFILE_ACTIVE=1)")
			}

			syncAction := "skipped"
			switch syncMode {
			case "auto":
				if dryRun {
					syncAction = "dry_run"
				} else {
					syncAction = "synced"
				}
				if _, err := executeSync(cmd.Context(), ec, syncOptions{
					Targets:     []string{target},
					DryRun:      dryRun,
					QuietStdout: true,
				}); err != nil {
					return err
				}
			case "check":
				syncAction = "checked"
				if _, err := executeSync(cmd.Context(), ec, syncOptions{
					Targets:     []string{target},
					CheckOnly:   true,
					QuietStdout: true,
				}); err != nil {
					return err
				}
			}

			bundle := ir.BuildForRun(ps)
			probes, err := engine.CollectProbes(ec.workDir, adp.RequiredProbes())
			if err != nil {
				return err
			}

			facts, err := runner.GatherFacts(cmd.Context(), ec.workDir, adp, probes, lg, userArgs)
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
					"AGENTFILE_TARGET":         target,
					"AGENTFILE_LOAD_GLOBAL":    fmt.Sprintf("%t", lg),
					"AGENTFILE_ISOLATION_MODE": string(plan.Mode),
				}
				for k, v := range plan.EnvOverrides {
					envAdded[k] = redactEnvValue(k, v)
				}
				rep := RunDryRunReport{
					Target:           target,
					Binary:           binPath,
					Argv:             argv,
					EnvAddedRedacted: envAdded,
					LoadGlobal:       lg,
					LoadGlobalSource: lgSource,
					IsolationMode:    string(plan.Mode),
					SyncAction:       syncAction,
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

			return runner.Execute(
				cmd.Context(),
				ec.procRunner,
				ec.workDir,
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
