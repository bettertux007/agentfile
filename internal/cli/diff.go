package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters/registry"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/diff"
	"github.com/bettertux007/agentfile/internal/engine"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/bettertux007/agentfile/schemas"
	"github.com/spf13/cobra"
)

var diffHunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

type diffFileStat struct {
	relPath   string
	additions int
	deletions int
}

func newDiffCmd(ec *execContext) *cobra.Command {
	var targets []string
	var stat bool
	var nameOnly bool
	var check bool
	var fetch bool

	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Preview unified diffs between Agentfile.yaml and workspace files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if stat && nameOnly {
				return spec.NewPolicyError("conflicting flags: --stat and --name-only cannot be combined")
			}

			manifestPath := filepath.Join(ec.workDir, "Agentfile.yaml")
			rawManifest, readErr := os.ReadFile(manifestPath)
			if readErr != nil {
				if errors.Is(readErr, os.ErrNotExist) {
					return spec.NewPolicyError("Agentfile.yaml not found in %s (run 'agentfile init' first)", ec.workDir)
				}
				return readErr
			}
			if err := schemas.ValidateAgentfileV1(rawManifest); err != nil {
				return spec.NewPolicyError("invalid Agentfile.yaml: %v", err)
			}
			ps, err := spec.ParseFile(manifestPath)
			if err != nil {
				return err
			}

			effCfg, err := config.Resolve(ec.workDir, config.CLIOverrides{}, ec.isTTY)
			if err != nil {
				return err
			}
			for _, w := range effCfg.Warnings {
				fmt.Fprintf(ec.errOut, "warning: %s\n", diff.RedactSecrets(w))
			}

			reg := registry.Default()
			activeTargets := ps.Targets
			if len(targets) > 0 {
				activeTargets = targets
			}
			for _, t := range activeTargets {
				if _, lerr := registry.Lookup(reg, t); lerr != nil {
					return spec.NewPolicyError("target %q is not supported (available: %v)", t, registry.Names(reg))
				}
			}

			lockPath := filepath.Join(ec.workDir, "Agentfile.lock")
			lf, lerr := spec.LoadLock(lockPath)
			if lerr != nil {
				if errors.Is(lerr, os.ErrNotExist) {
					lf = nil
				} else {
					return lerr
				}
			}

			st, err := store.NewDefaultStore()
			if err != nil {
				return err
			}
			parseOpts := store.ParseOptions{
				AllowFileScheme: os.Getenv("AGENTFILE_ALLOW_FILE_GIT") == "1",
			}

			var resolvedDeps map[string]store.ResolvedAsset
			var lockStatus string

			if !fetch {
				insDep, derr := st.InspectDependencies(ec.workDir, ps, lf, parseOpts)
				if derr != nil {
					return derr
				}
				if len(insDep.MissingLocal) > 0 {
					return spec.NewPolicyError("missing local dependencies: %s", strings.Join(insDep.MissingLocal, ", "))
				}
				if len(insDep.Unhydrated) > 0 {
					return spec.NewPolicyError("unhydrated remote dependencies (%s); run 'agentfile diff --fetch' or 'agentfile sync'", strings.Join(insDep.Unhydrated, ", "))
				}
				resolvedDeps = insDep.Assets
				lockStatus = insDep.LockStatus
			} else {
				res, rerr := st.ResolveWithOptions(cmd.Context(), ec.workDir, ps, lf, store.ResolveOptions{
					ParseOpts: parseOpts,
				})
				if rerr != nil {
					return rerr
				}
				resolvedDeps = res.Assets
				switch {
				case lf == nil && len(res.NextLock.Dependencies) == 0:
					lockStatus = "not_required"
				case lf == nil:
					lockStatus = "missing"
				default:
					lockStatus = "ok"
				}
			}

			plan, err := buildInspectionPlan(ec.workDir, ps, resolvedDeps, effCfg, reg, activeTargets)
			if err != nil {
				return err
			}

			for _, w := range plan.Summary.Warnings {
				fmt.Fprintf(ec.errOut, "warning: %s\n", diff.RedactSecrets(w))
			}
			for _, c := range plan.Conflicts {
				fmt.Fprintf(ec.errOut, "conflict: [%s] %s (%s)\n", c.Target, c.RelPath, diff.RedactSecrets(c.Reason))
			}

			allOps := make([]engine.PlannedAction, 0, len(plan.Actions)+len(plan.PruneOps))
			allOps = append(allOps, plan.Actions...)
			allOps = append(allOps, plan.PruneOps...)
			sort.SliceStable(allOps, func(i, j int) bool {
				return allOps[i].RelPath < allOps[j].RelPath
			})

			var (
				diffOut   strings.Builder
				truncated bool
				fileStats []diffFileStat
				hasDiff   bool
				totalAdds int
				totalDels int
			)

			appendRendered := func(chunk string, fallbackPath string, parseTree bool) {
				if chunk == "" {
					return
				}
				hasDiff = true
				redacted := diff.RedactUnifiedDiff(chunk)

				if parseTree {
					parsed := parseUnifiedDiffStats(redacted, fallbackPath)
					for _, fs := range parsed {
						fileStats = append(fileStats, fs)
						totalAdds += fs.additions
						totalDels += fs.deletions
					}
				} else {
					adds, dels := countUnifiedChunkLines(redacted)
					fileStats = append(fileStats, diffFileStat{
						relPath:   fallbackPath,
						additions: adds,
						deletions: dels,
					})
					totalAdds += adds
					totalDels += dels
				}

				if truncated {
					return
				}
				if diffOut.Len()+len(redacted) > diff.MaxTotalDiffBytes {
					truncated = true
					fmt.Fprintf(&diffOut, "... diff truncated (exceeded %d MiB total diff limit)\n", diff.MaxTotalDiffBytes/(1<<20))
					return
				}
				diffOut.WriteString(redacted)
				if !strings.HasSuffix(redacted, "\n") {
					diffOut.WriteByte('\n')
				}
			}

			for _, op := range allOps {
				switch op.Type {
				case engine.ActionWriteFile:
					if op.OldKind == "dir" || op.OldKind == "symlink" {
						appendRendered(diff.FormatModeChangeDiff(op.RelPath, op.OldKind, "file", op.OldLinkTarget, ""), op.RelPath, false)
						continue
					}
					if op.OldKind == "file" && op.OldContent == nil {
						chunk := formatOversizedFileDiff(ec.workDir, op.RelPath, false, len(op.Content))
						appendRendered(chunk, op.RelPath, false)
						continue
					}
					chunk, ch, derr := diff.UnifiedDiff(op.RelPath, op.OldContent, op.Content, 3)
					if derr != nil {
						return derr
					}
					if ch {
						appendRendered(chunk, op.RelPath, false)
					}

				case engine.ActionDeletePath:
					switch op.OldKind {
					case "symlink":
						appendRendered(diff.FormatSymlinkDiff(op.RelPath, op.OldLinkTarget, ""), op.RelPath, false)
					case "dir":
						absOld := filepath.Join(ec.workDir, filepath.FromSlash(op.RelPath))
						chunk, ch, _, _, _, derr := diff.DiffTrees(op.RelPath, absOld, "", true, false, 3)
						if derr != nil {
							return derr
						}
						if ch {
							appendRendered(chunk, op.RelPath, true)
						}
					case "file":
						if op.OldContent == nil {
							chunk := formatOversizedFileDiff(ec.workDir, op.RelPath, true, 0)
							appendRendered(chunk, op.RelPath, false)
							continue
						}
						chunk, ch, derr := diff.UnifiedDiff(op.RelPath, op.OldContent, nil, 3)
						if derr != nil {
							return derr
						}
						if ch {
							appendRendered(chunk, op.RelPath, false)
						}
					}

				case engine.ActionCopyTree:
					absOld := filepath.Join(ec.workDir, filepath.FromSlash(op.RelPath))
					if op.OldKind == "file" || op.OldKind == "symlink" {
						appendRendered(diff.FormatModeChangeDiff(op.RelPath, op.OldKind, "dir", op.OldLinkTarget, ""), op.RelPath, false)
						chunk, ch, _, _, _, derr := diff.DiffTrees(op.RelPath, "", op.SourcePath, false, true, 3)
						if derr != nil {
							return derr
						}
						if ch {
							appendRendered(chunk, op.RelPath, true)
						}
						continue
					}
					oldExists := op.OldKind == "dir"
					chunk, ch, _, _, _, derr := diff.DiffTrees(op.RelPath, absOld, op.SourcePath, oldExists, true, 3)
					if derr != nil {
						return derr
					}
					if ch {
						appendRendered(chunk, op.RelPath, true)
					}

				case engine.ActionSymlink:
					if op.OldKind == "file" || op.OldKind == "dir" {
						appendRendered(diff.FormatModeChangeDiff(op.RelPath, op.OldKind, "symlink", op.OldLinkTarget, op.SourcePath), op.RelPath, false)
						continue
					}
					appendRendered(diff.FormatSymlinkDiff(op.RelPath, op.OldLinkTarget, op.SourcePath), op.RelPath, false)
				}
			}

			switch {
			case nameOnly:
				seen := make(map[string]bool, len(fileStats))
				for _, fs := range fileStats {
					if !seen[fs.relPath] {
						seen[fs.relPath] = true
						fmt.Fprintln(ec.out, fs.relPath)
					}
				}
			case stat:
				printDiffStat(ec.out, fileStats, totalAdds, totalDels)
			default:
				if diffOut.Len() > 0 {
					fmt.Fprint(ec.out, diffOut.String())
				}
			}

			if check {
				if hasDiff || len(plan.Actions) > 0 || len(plan.PruneOps) > 0 {
					return spec.NewPolicyError("workspace has pending diff (run 'agentfile sync')")
				}
				if lockStatus == "missing" || lockStatus == "out_of_sync" || lockStatus == "invalid" {
					return spec.NewPolicyError("Agentfile.lock is %s (run 'agentfile sync')", lockStatus)
				}
				if len(plan.Conflicts) > 0 {
					return spec.NewPolicyError("%d conflict(s) detected", len(plan.Conflicts))
				}
				if drifted, reason := engine.CheckWorkspaceDrift(ec.workDir, plan); drifted {
					return spec.NewPolicyError("workspace is out of sync: %s", reason)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&targets, "targets", nil, "Comma-separated subset of targets to diff")
	cmd.Flags().BoolVar(&stat, "stat", false, "Show diffstat summary instead of full hunks")
	cmd.Flags().BoolVar(&nameOnly, "name-only", false, "Show only changed file paths")
	cmd.Flags().BoolVar(&check, "check", false, "Exit 2 if diff is non-empty, lockfile is out of sync, or conflicts/journal exist")
	cmd.Flags().BoolVar(&fetch, "fetch", false, "Hydrate cold remote dependencies in cache without writing Agentfile.lock")
	return cmd
}

func formatOversizedFileDiff(projectRoot, relPath string, deleting bool, newSize int) string {
	rel := filepath.ToSlash(relPath)
	abs := filepath.Join(projectRoot, filepath.FromSlash(relPath))
	var oldSize int64
	if info, err := os.Lstat(abs); err == nil {
		oldSize = info.Size()
	}
	newHdr := "+++ b/" + rel
	if deleting {
		newHdr = "+++ /dev/null"
	}
	return fmt.Sprintf("--- a/%s\n%s\nFiles %s differ (exceeds 1 MiB diff limit: old=%d bytes, new=%d bytes)\n",
		rel, newHdr, rel, oldSize, newSize)
}

func countUnifiedChunkLines(chunk string) (additions, deletions int) {
	lines := strings.Split(chunk, "\n")
	oldRem, newRem := 0, 0
	for _, line := range lines {
		if oldRem > 0 || newRem > 0 {
			switch {
			case line == `\ No newline at end of file`:
				continue
			case strings.HasPrefix(line, "-"):
				oldRem--
				deletions++
				continue
			case strings.HasPrefix(line, "+"):
				newRem--
				additions++
				continue
			case strings.HasPrefix(line, " "):
				oldRem--
				newRem--
				continue
			default:
				oldRem, newRem = 0, 0
			}
		}
		if sub := diffHunkRe.FindStringSubmatch(line); len(sub) == 5 {
			oldRem = parseDiffHunkCount(sub[2])
			newRem = parseDiffHunkCount(sub[4])
		}
	}
	return additions, deletions
}

func parseUnifiedDiffStats(chunk, fallbackPath string) []diffFileStat {
	lines := strings.Split(chunk, "\n")
	var out []diffFileStat
	curIdx := -1
	oldRem, newRem := 0, 0

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if oldRem > 0 || newRem > 0 {
			switch {
			case line == `\ No newline at end of file`:
				continue
			case strings.HasPrefix(line, "-"):
				oldRem--
				if curIdx >= 0 {
					out[curIdx].deletions++
				}
				continue
			case strings.HasPrefix(line, "+"):
				newRem--
				if curIdx >= 0 {
					out[curIdx].additions++
				}
				continue
			case strings.HasPrefix(line, " "):
				oldRem--
				newRem--
				continue
			default:
				oldRem, newRem = 0, 0
			}
		}

		if sub := diffHunkRe.FindStringSubmatch(line); len(sub) == 5 {
			oldRem = parseDiffHunkCount(sub[2])
			newRem = parseDiffHunkCount(sub[4])
			continue
		}

		if strings.HasPrefix(line, "--- a/") || line == "--- /dev/null" {
			var rel string
			if strings.HasPrefix(line, "--- a/") {
				rel = strings.TrimPrefix(line, "--- a/")
			}
			if i+1 < len(lines) && (strings.HasPrefix(lines[i+1], "+++ b/") || lines[i+1] == "+++ /dev/null") {
				if strings.HasPrefix(lines[i+1], "+++ b/") {
					rel = strings.TrimPrefix(lines[i+1], "+++ b/")
				}
				i++
			}
			if rel == "" {
				rel = fallbackPath
			}
			out = append(out, diffFileStat{relPath: rel})
			curIdx = len(out) - 1
		}
	}

	if len(out) == 0 {
		adds, dels := countUnifiedChunkLines(chunk)
		return []diffFileStat{{relPath: fallbackPath, additions: adds, deletions: dels}}
	}
	return out
}

func parseDiffHunkCount(s string) int {
	if s == "" {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func printDiffStat(w io.Writer, stats []diffFileStat, totalAdds, totalDels int) {
	if len(stats) == 0 {
		return
	}
	maxLen := 0
	for _, s := range stats {
		if len(s.relPath) > maxLen {
			maxLen = len(s.relPath)
		}
	}
	for _, s := range stats {
		changes := s.additions + s.deletions
		plusCount := s.additions
		minusCount := s.deletions
		const maxBar = 30
		if changes > maxBar && changes > 0 {
			plusCount = (s.additions * maxBar) / changes
			minusCount = maxBar - plusCount
		}
		bar := strings.Repeat("+", plusCount) + strings.Repeat("-", minusCount)
		fmt.Fprintf(w, " %-*s | %4d %s\n", maxLen, s.relPath, changes, bar)
	}
	fmt.Fprintf(w, " %d file(s) changed, %d insertion(s)(+), %d deletion(s)(-)\n", len(stats), totalAdds, totalDels)
}
