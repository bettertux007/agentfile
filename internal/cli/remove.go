package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/bettertux007/agentfile/schemas"
	"github.com/spf13/cobra"
)

func newRemoveCmd(ec *execContext) *cobra.Command {
	var noSync bool
	var onConflict string
	var allowExternalLocal bool

	cmd := &cobra.Command{
		Use:   "remove <skill|agent|rule|mcp> <name>",
		Short: "Remove a skill, agent, rule, or MCP server from Agentfile.yaml",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name := args[0], args[1]

			manifestPath, origBytes, err := readExistingManifest(ec.workDir)
			if err != nil {
				return err
			}

			var revertOrigin *spec.LayerOrigin
			if rawSpec, perr := spec.ParseRaw(bytes.NewReader(origBytes)); perr == nil && len(rawSpec.Extends) > 0 {
				if st, serr := store.NewDefaultStore(); serr == nil {
					lf, _ := spec.LoadLock(filepath.Join(ec.workDir, "Agentfile.lock"))
					cm, _, cerr := st.InspectComposedManifest(ec.workDir, rawSpec, lf, store.ParseOptions{
						AllowExternalLocal: allowExternalLocal,
						AllowFileScheme:    os.Getenv("AGENTFILE_ALLOW_FILE_GIT") == "1",
					})
					if cerr == nil && cm != nil {
						state, orig := cm.EntryProvenance(kind, name)
						switch state {
						case spec.ProvenanceInheritedOnly:
							return spec.NewPolicyError("cannot remove %s %q: inherited from %s (override it in Agentfile.yaml instead)", kind, name, orig.DisplayName())
						case spec.ProvenanceOverriddenLocally:
							cp := orig
							revertOrigin = &cp
						}
					}
				}
			}

			candidateBytes, err := spec.RemoveEntry(origBytes, kind, name)
			if err != nil {
				return err
			}

			if err := schemas.ValidateAgentfileV1(candidateBytes); err != nil {
				return spec.NewPolicyError("invalid Agentfile.yaml: %v", err)
			}
			candidateSpec, err := spec.ParseRaw(bytes.NewReader(candidateBytes))
			if err != nil {
				return err
			}

			if revertOrigin != nil {
				fmt.Fprintf(ec.errOut, "note: removed local override for %s %q; reverting to inherited definition from %s\n", kind, name, revertOrigin.DisplayName())
			}

			if noSync {
				if err := writeFileAtomic(manifestPath, candidateBytes, 0o644); err != nil {
					return err
				}
				fmt.Fprintf(ec.out, "Removed %s %s from %s (--no-sync)\n", kind, name, manifestPath)
				return nil
			}

			_, err = executeSyncWithSpec(cmd.Context(), ec, syncOptions{
				OnConflict:         onConflict,
				AllowExternalLocal: allowExternalLocal,
				ManifestBytes:      candidateBytes,
			}, candidateSpec, store.ResolveOptions{
				ReconcileKey: spec.LockKey(kind, name),
				ParseOpts: store.ParseOptions{
					AllowExternalLocal: allowExternalLocal,
					AllowFileScheme:    os.Getenv("AGENTFILE_ALLOW_FILE_GIT") == "1",
				},
			})
			return err
		},
	}

	cmd.Flags().BoolVar(&noSync, "no-sync", false, "Update Agentfile.yaml only without syncing workspace or lockfile")
	cmd.Flags().StringVar(&onConflict, "on-conflict", "", "Conflict policy: ask, fail, backup, or overwrite")
	cmd.Flags().BoolVar(&allowExternalLocal, "allow-external-local", false, "Allow local dependency paths outside repository root")
	return cmd
}
