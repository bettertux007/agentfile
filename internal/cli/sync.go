package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/adapters/registry"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/engine"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/spf13/cobra"
)

type syncOptions struct {
	Targets            []string
	LinkMode           string
	Copy               bool
	OnConflict         string
	GitExclude         bool
	GitExcludeSet      bool
	Update             string
	DryRun             bool
	Force              bool
	AllowExternalLocal bool
	JSONOutput         bool
	QuietStdout        bool
	CheckOnly          bool
}

func newSyncCmd(ec *execContext) *cobra.Command {
	var opts syncOptions

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Resolve dependencies and compile project-scoped agent configurations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.GitExcludeSet = cmd.Flags().Changed("git-exclude")
			_, err := executeSync(cmd.Context(), ec, opts)
			return err
		},
	}

	cmd.Flags().StringSliceVar(&opts.Targets, "targets", nil, "Comma-separated subset of targets to sync")
	cmd.Flags().StringVar(&opts.LinkMode, "link-mode", "", "Materialization mode: copy or symlink")
	cmd.Flags().BoolVar(&opts.Copy, "copy", false, "Shorthand for --link-mode=copy")
	cmd.Flags().StringVar(&opts.OnConflict, "on-conflict", "", "Conflict policy: ask, fail, backup, or overwrite")
	cmd.Flags().BoolVar(&opts.GitExclude, "git-exclude", true, "Manage .git/info/exclude entries")
	cmd.Flags().StringVar(&opts.Update, "update", "", "Update locked Git dependencies (all or <kind>:<name>)")
	cmd.Flags().Lookup("update").NoOptDefVal = "*"
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "Show planned changes without modifying files")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "Alias for --on-conflict=overwrite")
	cmd.Flags().BoolVar(&opts.AllowExternalLocal, "allow-external-local", false, "Allow local dependency paths outside repository root")
	cmd.Flags().BoolVar(&opts.JSONOutput, "json", false, "Emit summary as JSON (for --dry-run)")

	return cmd
}

func executeSync(ctx context.Context, ec *execContext, opts syncOptions) (*engine.PlanSummary, error) {
	manifestPath := filepath.Join(ec.workDir, "Agentfile.yaml")
	ps, err := spec.ParseFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, spec.NewPolicyError("Agentfile.yaml not found in %s (run 'agentfile init' first)", ec.workDir)
		}
		return nil, err
	}

	if opts.Force {
		fmt.Fprintln(ec.errOut, "note: --force sets --on-conflict=overwrite (lockfile and security checks remain enforced)")
	}

	cliOv := config.CLIOverrides{
		LinkMode:      opts.LinkMode,
		Copy:          opts.Copy,
		OnConflict:    opts.OnConflict,
		Force:         opts.Force,
		GitExclude:    opts.GitExclude,
		GitExcludeSet: opts.GitExcludeSet,
	}
	effCfg, err := config.Resolve(ec.workDir, cliOv, ec.isTTY)
	if err != nil {
		return nil, err
	}
	if opts.CheckOnly && effCfg.OnConflict == "ask" {
		effCfg.OnConflict = "fail"
	}

	reg := registry.Default()
	activeTargets := ps.Targets
	if len(opts.Targets) > 0 {
		activeTargets = opts.Targets
	}
	for _, t := range activeTargets {
		if _, err := registry.Lookup(reg, t); err != nil {
			return nil, spec.NewPolicyError("target %q is not supported in this milestone (available: %v)", t, registry.Names(reg))
		}
	}

	lockPath := filepath.Join(ec.workDir, "Agentfile.lock")
	lf, err := spec.LoadLock(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			lf = nil
		} else {
			return nil, err
		}
	}

	st, err := store.NewDefaultStore()
	if err != nil {
		return nil, err
	}
	parseOpts := store.ParseOptions{
		AllowExternalLocal: opts.AllowExternalLocal,
		AllowFileScheme:    os.Getenv("AGENTFILE_ALLOW_FILE_GIT") == "1",
	}
	resolvedDeps, nextLock, err := st.ResolveAll(ctx, ec.workDir, ps, lf, opts.Update, parseOpts)
	if err != nil {
		return nil, err
	}

	if opts.CheckOnly {
		if (lf == nil && len(nextLock.Dependencies) > 0) || (lf != nil && !locksEqual(lf, nextLock)) {
			return nil, spec.NewPolicyError("Agentfile.lock is out of sync (run 'agentfile sync')")
		}
	}

	bundle, err := ir.Build(ps, resolvedDeps)
	if err != nil {
		return nil, err
	}

	var probeReqs []adapters.ProbeRequest
	for _, t := range activeTargets {
		adp, _ := registry.Lookup(reg, t)
		probeReqs = append(probeReqs, adp.RequiredProbes()...)
	}
	probeFacts, err := engine.CollectProbes(ec.workDir, probeReqs)
	if err != nil {
		return nil, err
	}

	compiled := make([]*adapters.CompileResult, 0, len(activeTargets))
	for _, t := range activeTargets {
		adp, _ := registry.Lookup(reg, t)
		res, err := adp.Compile(bundle, probeFacts)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, res)
	}

	var prompter engine.Prompter
	if effCfg.OnConflict == "ask" && !opts.CheckOnly {
		prompter = newTTYPrompter(ec.in, ec.errOut)
	}

	plan, err := engine.BuildPlan(engine.PlanInput{
		ProjectRoot:     ec.workDir,
		ActiveTargets:   activeTargets,
		Config:          effCfg,
		Bundle:          bundle,
		CompiledResults: compiled,
		ValidatePolicy: func(target string, mergedFiles map[string][]byte) []adapters.Diagnostic {
			adp, err := registry.Lookup(reg, target)
			if err != nil {
				return nil
			}
			return adp.CheckPolicy(bundle, mergedFiles)
		},
		Prompter: prompter,
	})
	if err != nil {
		return nil, err
	}

	// Surface RUNTIME_ONLY_ISOLATION notice on normal sync/dry-run (suppressed during `agentfile run` QuietStdout).
	if !opts.QuietStdout {
		for _, t := range activeTargets {
			if cfg, ok := ps.TargetsConfig[t]; ok && cfg.LoadGlobal != nil && !*cfg.LoadGlobal {
				plan.Summary.Warnings = append(plan.Summary.Warnings,
					fmt.Sprintf("RUNTIME_ONLY_ISOLATION: target %s sets load_global=false; use 'agentfile run %s' to enforce runtime isolation (bare '%s' still reads user globals)", t, t, t))
			}
		}
	}

	if opts.CheckOnly {
		_, jErr := os.Stat(engine.JournalPath(ec.workDir))
		_, sErr := os.Stat(engine.StatePath(ec.workDir))
		prevState, _ := engine.LoadState(ec.workDir)
		if len(plan.Actions) > 0 || len(plan.PruneOps) > 0 || jErr == nil || os.IsNotExist(sErr) || !engine.StatesEqual(prevState, plan.NextState) {
			return nil, spec.NewPolicyError("workspace is out of sync with Agentfile.yaml (run 'agentfile sync')")
		}
		return &plan.Summary, nil
	}

	if opts.DryRun {
		if !opts.QuietStdout {
			if err := printSummary(ec.out, &plan.Summary, true, opts.JSONOutput); err != nil {
				return nil, err
			}
		}
		return &plan.Summary, nil
	}

	if err := engine.Commit(plan); err != nil {
		return nil, err
	}

	if (len(nextLock.Dependencies) > 0 && lf == nil) || (lf != nil && !locksEqual(lf, nextLock)) {
		if err := spec.SaveLock(lockPath, nextLock); err != nil {
			return nil, err
		}
	}

	if opts.QuietStdout {
		for _, warn := range plan.Summary.Warnings {
			fmt.Fprintf(ec.errOut, "warning: %s\n", RedactSecrets(warn))
		}
		return &plan.Summary, nil
	}

	if err := printSummary(ec.out, &plan.Summary, false, opts.JSONOutput); err != nil {
		return nil, err
	}
	return &plan.Summary, nil
}

func locksEqual(a, b *spec.Lockfile) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Version != b.Version || len(a.Dependencies) != len(b.Dependencies) {
		return false
	}
	for k, va := range a.Dependencies {
		vb, ok := b.Dependencies[k]
		if !ok || va != vb {
			return false
		}
	}
	return true
}

type ttyPrompter struct {
	reader *bufio.Reader
	out    io.Writer
}

func newTTYPrompter(in io.Reader, out io.Writer) *ttyPrompter {
	return &ttyPrompter{reader: bufio.NewReader(in), out: out}
}

func (p *ttyPrompter) AskConflict(relPath, reason string) (engine.ConflictChoice, error) {
	for {
		fmt.Fprintf(p.out, "Conflict at %s (%s). [o]verwrite / [b]ackup / [s]kip / [a]bort: ", relPath, reason)
		line, err := p.reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return engine.ChoiceAbort, err
		}
		ans := strings.ToLower(strings.TrimSpace(line))
		switch ans {
		case "o", "overwrite":
			return engine.ChoiceOverwrite, nil
		case "b", "backup":
			return engine.ChoiceBackup, nil
		case "s", "skip":
			return engine.ChoiceSkip, nil
		case "a", "abort", "":
			return engine.ChoiceAbort, nil
		}
		if errors.Is(err, io.EOF) {
			return engine.ChoiceAbort, nil
		}
	}
}

func printSummary(w io.Writer, s *engine.PlanSummary, dryRun bool, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	prefix := "Sync"
	if dryRun {
		prefix = "Dry-run"
	}
	fmt.Fprintf(w, "%s summary: %d created, %d updated, %d pruned, %d unchanged\n",
		prefix, len(s.Created), len(s.Updated), len(s.Pruned), len(s.Unchanged))
	for _, p := range s.Created {
		fmt.Fprintf(w, "  + %s\n", p)
	}
	for _, p := range s.Updated {
		fmt.Fprintf(w, "  ~ %s\n", p)
	}
	for _, p := range s.Pruned {
		fmt.Fprintf(w, "  - %s\n", p)
	}
	if len(s.SecurityHighlights.Hooks) > 0 || len(s.SecurityHighlights.StdioMCPs) > 0 || len(s.SecurityHighlights.AllowCommands) > 0 || len(s.SecurityHighlights.DenyCommands) > 0 {
		fmt.Fprintln(w, "Security / executable items:")
		for _, h := range s.SecurityHighlights.Hooks {
			fmt.Fprintf(w, "  hook: %s\n", h)
		}
		for _, m := range s.SecurityHighlights.StdioMCPs {
			fmt.Fprintf(w, "  stdio mcp: %s\n", m)
		}
		for _, a := range s.SecurityHighlights.AllowCommands {
			fmt.Fprintf(w, "  allow: %s\n", a)
		}
		for _, d := range s.SecurityHighlights.DenyCommands {
			fmt.Fprintf(w, "  deny: %s\n", d)
		}
	}
	for _, warn := range s.Warnings {
		fmt.Fprintf(w, "warning: %s\n", RedactSecrets(warn))
	}
	return nil
}
