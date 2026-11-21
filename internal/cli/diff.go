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
			rawSpec, err := spec.ParseRawFile(manifestPath)
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

			var ps *spec.ProjectSpec
			var resolvedDeps map[string]store.ResolvedAsset
			var lockStatus string

			if !fetch {
				composed, insDep, derr := st.InspectComposedManifest(ec.workDir, rawSpec, lf, parseOpts)
				if derr != nil {
					return derr
				}
				if len(insDep.MissingLocal) > 0 {
					return spec.NewPolicyError("missing local dependencies: %s", strings.Join(insDep.MissingLocal, ", "))
				}
				if len(insDep.Unhydrated) > 0 {
					return spec.NewPolicyError("unhydrated remote dependencies (%s); run 'agentfile diff --fetch' or 'agentfile sync'", strings.Join(insDep.Unhydrated, ", "))
				}
				ps = composed.Effective
				resolvedDeps = insDep.Assets
				lockStatus = insDep.LockStatus
			} else {
				compRes, cerr := st.ComposeManifest(cmd.Context(), ec.workDir, rawSpec, lf, store.ResolveOptions{
					ParseOpts: parseOpts,
				})
				if cerr != nil {
					return cerr
				}
				ps = compRes.Composed.Effective
				res, rerr := st.ResolveWithOptions(cmd.Context(), ec.workDir, ps, compRes.NextLock, store.ResolveOptions{
					ParseOpts: parseOpts,
				})
				if rerr != nil {
					return rerr
				}
				resolvedDeps = res.Assets
				switch {
				case lf == nil && !res.NextLock.HasRemoteEntries():
					lockStatus = "not_required"
				case lf == nil:
					lockStatus = "missing"
				default:
					lockStatus = "ok"
				}
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

			var dr diffRenderer
			appendRendered := dr.appendRendered

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
				seen := make(map[string]bool, len(dr.fileStats))
				for _, fs := range dr.fileStats {
					if !seen[fs.relPath] {
						seen[fs.relPath] = true
						fmt.Fprintln(ec.out, fs.relPath)
					}
				}
			case stat:
				printDiffStat(ec.out, dr.fileStats, dr.totalAdds, dr.totalDels)
			default:
				if dr.diffOut.Len() > 0 {
					fmt.Fprint(ec.out, dr.diffOut.String())
				}
			}

			if check {
				if dr.hasDiff || len(plan.Actions) > 0 || len(plan.PruneOps) > 0 {
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

type diffRenderer struct {
	diffOut   strings.Builder
	truncated bool
	fileStats []diffFileStat
	hasDiff   bool
	totalAdds int
	totalDels int
	maxBytes  int
}

func (r *diffRenderer) maxTotalBytes() int {
	if r.maxBytes > 0 {
		return r.maxBytes
	}
	return diff.MaxTotalDiffBytes
}

func (r *diffRenderer) appendRendered(chunk string, fallbackPath string, parseTree bool) {
	if chunk == "" {
		return
	}
	r.hasDiff = true
	redacted := diff.RedactUnifiedDiff(chunk)

	if parseTree {
		parsed := parseUnifiedDiffStats(redacted, fallbackPath)
		for _, fs := range parsed {
			r.fileStats = append(r.fileStats, fs)
			r.totalAdds += fs.additions
			r.totalDels += fs.deletions
		}
	} else {
		adds, dels := countUnifiedChunkLines(redacted)
		r.fileStats = append(r.fileStats, diffFileStat{
			relPath:   fallbackPath,
			additions: adds,
			deletions: dels,
		})
		r.totalAdds += adds
		r.totalDels += dels
	}

	if r.truncated {
		return
	}
	maxBytes := r.maxTotalBytes()
	truncNotice := fmt.Sprintf("... diff truncated (exceeded %d MiB total diff limit)\n", diff.MaxTotalDiffBytes/(1<<20))
	if r.diffOut.Len()+len(redacted) > maxBytes {
		r.truncated = true
		if r.diffOut.Len() == 0 {
			if strings.HasSuffix(redacted, truncNotice) {
				r.diffOut.WriteString(redacted)
				return
			}
			limit := maxBytes
			if limit > len(redacted) {
				limit = len(redacted)
			}
			prefix := redacted[:limit]
			if nl := strings.LastIndexByte(prefix, '\n'); nl >= 0 {
				prefix = prefix[:nl+1]
			}
			r.diffOut.WriteString(prefix)
			if prefix != "" && !strings.HasSuffix(prefix, "\n") {
				r.diffOut.WriteByte('\n')
			}
		}
		r.diffOut.WriteString(truncNotice)
		return
	}
	r.diffOut.WriteString(redacted)
	if !strings.HasSuffix(redacted, "\n") {
		r.diffOut.WriteByte('\n')
	}
	if strings.HasSuffix(redacted, truncNotice) {
		r.truncated = true
	}
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
