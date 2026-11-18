package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/bettertux007/agentfile/internal/engine"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/spf13/cobra"
)

func newCleanCmd(ec *execContext) *cobra.Command {
	var dryRun bool
	var targets []string
	var cache bool
	var maxAgeStr string
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove agentfile-managed workspace artifacts or prune stale store entries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cache {
				maxAge, err := time.ParseDuration(maxAgeStr)
				if err != nil || maxAge < 0 {
					return spec.NewPolicyError("invalid --max-age duration %q (example: 720h)", maxAgeStr)
				}
				st, err := store.NewDefaultStore()
				if err != nil {
					return err
				}
				removed, err := st.PruneCache(maxAge, dryRun)
				if err != nil {
					return err
				}
				if jsonOutput {
					enc := json.NewEncoder(ec.out)
					enc.SetIndent("", "  ")
					return enc.Encode(map[string]any{"pruned_cache": removed, "dry_run": dryRun})
				}
				label := "Removed"
				if dryRun {
					label = "Would remove"
				}
				fmt.Fprintf(ec.out, "%s %d stale store entries\n", label, len(removed))
				for _, p := range removed {
					fmt.Fprintf(ec.out, "  - %s\n", p)
				}
				return nil
			}

			sum, err := engine.Clean(ec.workDir, targets, dryRun)
			if err != nil {
				return err
			}
			if jsonOutput {
				enc := json.NewEncoder(ec.out)
				enc.SetIndent("", "  ")
				return enc.Encode(sum)
			}
			prefix := "Clean"
			if dryRun {
				prefix = "Clean dry-run"
			}
			fmt.Fprintf(ec.out, "%s: %d updated, %d removed\n", prefix, len(sum.Updated), len(sum.Pruned))
			for _, p := range sum.Updated {
				fmt.Fprintf(ec.out, "  ~ %s\n", p)
			}
			for _, p := range sum.Pruned {
				fmt.Fprintf(ec.out, "  - %s\n", p)
			}
			for _, w := range sum.Warnings {
				fmt.Fprintf(ec.out, "warning: %s\n", w)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show files that would be cleaned without modifying disk")
	cmd.Flags().StringSliceVar(&targets, "targets", nil, "Clean only the specified target namespaces")
	cmd.Flags().BoolVar(&cache, "cache", false, "Prune stale extracted store entries instead of workspace files")
	cmd.Flags().StringVar(&maxAgeStr, "max-age", "720h", "Age threshold for --cache pruning (Go duration, e.g. 720h)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit clean summary as JSON")
	return cmd
}
