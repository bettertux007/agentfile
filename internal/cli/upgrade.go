package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bettertux007/agentfile/internal/engine"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/spf13/cobra"
)

// UpgradeReport is the structured JSON output of `agentfile upgrade --json`.
type UpgradeReport struct {
	Deltas  []store.LockDelta  `json:"deltas"`
	Summary engine.PlanSummary `json:"summary"`
}

func newUpgradeCmd(ec *execContext) *cobra.Command {
	var targets []string
	var dryRun bool
	var onConflict string
	var force bool
	var allowExternalLocal bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "upgrade [<kind>:<name> | <kind> <name>]",
		Short: "Refresh locked remote Git dependencies and sync workspace configurations",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			updateTarget, err := normalizeUpgradeTarget(args)
			if err != nil {
				return err
			}

			res, err := executeSyncWithSpec(cmd.Context(), ec, syncOptions{
				Targets:            targets,
				OnConflict:         onConflict,
				Update:             updateTarget,
				DryRun:             dryRun,
				Force:              force,
				AllowExternalLocal: allowExternalLocal,
				QuietStdout:        jsonOutput,
			}, nil, store.ResolveOptions{
				UpdateTarget: updateTarget,
				ParseOpts: store.ParseOptions{
					AllowExternalLocal: allowExternalLocal,
					AllowFileScheme:    os.Getenv("AGENTFILE_ALLOW_FILE_GIT") == "1",
				},
			})
			if err != nil {
				return err
			}

			deltas := res.Deltas
			if deltas == nil {
				deltas = []store.LockDelta{}
			}

			if jsonOutput {
				enc := json.NewEncoder(ec.out)
				enc.SetIndent("", "  ")
				return enc.Encode(UpgradeReport{
					Deltas:  deltas,
					Summary: res.Summary,
				})
			}

			if len(deltas) == 0 {
				fmt.Fprintln(ec.out, "No remote dependencies to upgrade.")
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&targets, "targets", nil, "Comma-separated subset of targets to sync")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview lockfile and workspace updates without modifying files")
	cmd.Flags().StringVar(&onConflict, "on-conflict", "", "Conflict policy: ask, fail, backup, or overwrite")
	cmd.Flags().BoolVar(&force, "force", false, "Alias for --on-conflict=overwrite")
	cmd.Flags().BoolVar(&allowExternalLocal, "allow-external-local", false, "Allow local dependency paths outside repository root")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit upgrade deltas and summary as JSON")
	return cmd
}

func normalizeUpgradeTarget(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "*", nil
	case 1:
		arg := strings.TrimSpace(args[0])
		if arg == "extends" {
			return "extends", nil
		}
		kind, name, ok := strings.Cut(arg, ":")
		if !ok || strings.TrimSpace(kind) == "" || strings.TrimSpace(name) == "" {
			return "", spec.NewPolicyError("invalid upgrade target %q (expected <kind>:<name>, <kind> <name>, or extends[:<url>])", args[0])
		}
		return strings.TrimSpace(kind) + ":" + strings.TrimSpace(name), nil
	case 2:
		kind := strings.TrimSpace(args[0])
		name := strings.TrimSpace(args[1])
		if kind == "" || name == "" {
			return "", spec.NewPolicyError("invalid upgrade target %q %q (expected <kind> <name>)", args[0], args[1])
		}
		return spec.LockKey(kind, name), nil
	default:
		return "", spec.NewPolicyError("accepts at most 2 arg(s), received %d", len(args))
	}
}
