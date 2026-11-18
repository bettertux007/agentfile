package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/adapters/registry"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/runner"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/bettertux007/agentfile/schemas"
	"github.com/spf13/cobra"
	"github.com/tailscale/hujson"
)

// DoctorReport is the structured output of `agentfile doctor --json`.
type DoctorReport struct {
	ManifestValid   bool                  `json:"manifest_valid"`
	SchemaErrors    []string              `json:"schema_errors,omitempty"`
	LockStatus      string                `json:"lock_status"`
	EffectiveConfig *DoctorConfigSummary  `json:"effective_config,omitempty"`
	Targets         []DoctorTargetStatus  `json:"targets"`
	GlobalConflicts []GlobalConflictEntry `json:"global_conflicts,omitempty"`
	GlobalAssets    []GlobalAssetEntry    `json:"global_assets,omitempty"`
	Warnings        []string              `json:"warnings,omitempty"`
}

// DoctorConfigSummary displays the resolved machine-local configuration.
type DoctorConfigSummary struct {
	LinkMode   string `json:"link_mode"`
	OnConflict string `json:"on_conflict"`
	GitExclude bool   `json:"git_exclude"`
	LoadGlobal *bool  `json:"load_global,omitempty"`
	ConfigPath string `json:"config_path"`
}

// DoctorTargetStatus describes binary presence, SchemaContract, and isolation status for one target.
type DoctorTargetStatus struct {
	Target           string `json:"target"`
	Binary           string `json:"binary"`
	BinaryFound      bool   `json:"binary_found"`
	BinaryOptional   bool   `json:"binary_optional"`
	Version          string `json:"version,omitempty"`
	SchemaRange      string `json:"schema_range"`
	LoadGlobal       bool   `json:"load_global"`
	LoadGlobalSource string `json:"load_global_source"`
	IsolationMode    string `json:"isolation_mode"`
	Status           string `json:"status"` // ok | warning
}

// GlobalConflictEntry records a global MCP server shadowing a project-scoped server.
type GlobalConflictEntry struct {
	GlobalFile string `json:"global_file"`
	ServerName string `json:"server_name"`
}

// GlobalAssetEntry records a non-empty user-global skills, rules, or agents path.
type GlobalAssetEntry struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
}

func newDoctorCmd(ec *execContext) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate Agentfile.yaml, lockfile, installed CLIs, and global config conflicts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rep := DoctorReport{
				LockStatus: "not_checked",
			}

			// 1. Validate Agentfile.yaml against JSON Schema v1 & spec.Parse.
			manifestPath := filepath.Join(ec.workDir, "Agentfile.yaml")
			var ps *spec.ProjectSpec
			rawManifest, readErr := os.ReadFile(manifestPath)
			if readErr != nil {
				if errors.Is(readErr, os.ErrNotExist) {
					rep.SchemaErrors = append(rep.SchemaErrors, "Agentfile.yaml not found")
				} else {
					rep.SchemaErrors = append(rep.SchemaErrors, readErr.Error())
				}
			} else {
				if err := schemas.ValidateAgentfileV1(rawManifest); err != nil {
					rep.SchemaErrors = append(rep.SchemaErrors, fmt.Sprintf("JSON schema: %v", err))
				}
				parsed, perr := spec.ParseFile(manifestPath)
				if perr != nil {
					rep.SchemaErrors = append(rep.SchemaErrors, fmt.Sprintf("spec parser: %v", perr))
				} else {
					ps = parsed
				}
				if len(rep.SchemaErrors) == 0 {
					rep.ManifestValid = true
				}
			}

			// 2. Verify Agentfile.lock if manifest parsed.
			if ps != nil {
				lockPath := filepath.Join(ec.workDir, "Agentfile.lock")
				lf, lerr := spec.LoadLock(lockPath)
				if errors.Is(lerr, os.ErrNotExist) {
					rep.LockStatus = "missing"
					if hasRemoteDeps(ps, ec.workDir) {
						rep.Warnings = append(rep.Warnings, "Agentfile.lock does not exist yet; run 'agentfile sync' to pin remote dependencies")
					}
				} else if lerr != nil {
					rep.LockStatus = "invalid"
					rep.SchemaErrors = append(rep.SchemaErrors, lerr.Error())
				} else {
					verr := spec.VerifyMatches(lf, ps, func(src spec.SourceSpec) (string, string, bool, error) {
						p, err := store.ParseSource(src, ec.workDir, store.ParseOptions{AllowFileScheme: true})
						if err != nil {
							return "", "", false, err
						}
						if p.IsLocal {
							return p.LocalPath, "", false, nil
						}
						return p.CanonicalURL, p.Ref, true, nil
					})
					if verr != nil {
						rep.LockStatus = "out_of_sync"
						rep.SchemaErrors = append(rep.SchemaErrors, verr.Error())
					} else {
						rep.LockStatus = "ok"
					}
				}
			}

			// 3. Resolve effective .agentfile/config.yaml.
			effCfg, cerr := config.Resolve(ec.workDir, config.CLIOverrides{}, ec.isTTY)
			if cerr != nil {
				rep.SchemaErrors = append(rep.SchemaErrors, fmt.Sprintf("config: %v", cerr))
			} else {
				rep.EffectiveConfig = &DoctorConfigSummary{
					LinkMode:   effCfg.LinkMode,
					OnConflict: effCfg.OnConflict,
					GitExclude: effCfg.GitExclude,
					LoadGlobal: effCfg.LoadGlobal,
					ConfigPath: effCfg.ConfigPath,
				}
			}

			// 4. Check installed target CLIs, SchemaContract versions, and global isolation status.
			reg := registry.Default()
			targetsToCheck := registry.Names(reg)
			if ps != nil {
				targetsToCheck = ps.Targets
			}
			var runBundle *ir.Bundle
			if ps != nil {
				runBundle = ir.BuildForRun(ps)
			}

			homeDir, _ := os.UserHomeDir()

			for _, t := range targetsToCheck {
				adp, err := registry.Lookup(reg, t)
				if err != nil {
					continue
				}
				contract := adp.SchemaContract()
				lg := true
				lgSource := config.SourceDefault
				if ps != nil {
					var specCfg spec.TargetConfigSpec
					if c, ok := ps.TargetsConfig[t]; ok {
						specCfg = c
					}
					var ow string
					var rerr error
					lg, lgSource, ow, rerr = config.ResolveTargetLoadGlobal(specCfg, effCfg, false, false, false)
					if rerr != nil {
						rep.SchemaErrors = append(rep.SchemaErrors, rerr.Error())
					}
					if ow != "" {
						rep.Warnings = append(rep.Warnings, ow)
					}
					if specCfg.LoadGlobal != nil && !*specCfg.LoadGlobal {
						rep.Warnings = append(rep.Warnings,
							fmt.Sprintf("RUNTIME_ONLY_ISOLATION: target %s sets load_global=false; bare '%s' invocations still read user globals unless launched via 'agentfile run %s'", t, adp.BinaryName(), t))
					}
				}

				ts := DoctorTargetStatus{
					Target:           t,
					Binary:           adp.BinaryName(),
					BinaryOptional:   contract.BinaryOptional,
					SchemaRange:      fmt.Sprintf("%s..%s", contract.MinVersion, contract.MaxTested),
					LoadGlobal:       lg,
					LoadGlobalSource: lgSource,
					IsolationMode:    string(adapters.IsolationNone),
					Status:           "ok",
				}

				if binPath, lerr := exec.LookPath(adp.BinaryName()); lerr == nil && binPath != "" {
					ts.BinaryFound = true
					ts.Version = runner.ProbeBinaryVersion(cmd.Context(), binPath)
					if contract.MinVersion != "" {
						if ok, verr := adapters.CompareSemver(ts.Version, contract.MinVersion); verr != nil || !ok {
							ts.Status = "warning"
							rep.Warnings = append(rep.Warnings, fmt.Sprintf("target %s CLI version %q may not satisfy SchemaContract MinVersion %s", t, ts.Version, contract.MinVersion))
						}
					}
					if !lg && contract.MinIsolationVersion != "" {
						if ok, verr := adapters.CompareSemver(ts.Version, contract.MinIsolationVersion); verr != nil || !ok {
							ts.Status = "warning"
							rep.Warnings = append(rep.Warnings, fmt.Sprintf("target %s CLI version %q does not satisfy MinIsolationVersion %s", t, ts.Version, contract.MinIsolationVersion))
						}
					}
				} else {
					ts.Status = "warning"
					if contract.BinaryOptional {
						rep.Warnings = append(rep.Warnings, fmt.Sprintf("optional/GUI binary %q not found on $PATH (%s)", adp.BinaryName(), t))
					} else {
						rep.Warnings = append(rep.Warnings, fmt.Sprintf("target CLI %q not found on $PATH (%s)", adp.BinaryName(), t))
					}
				}

				if !lg && ps != nil {
					facts, ferr := runner.GatherFacts(cmd.Context(), ec.workDir, adp, nil, false, nil)
					if ferr == nil {
						facts.BinaryVersion = ts.Version
						if lp, perr := adp.PlanLaunch(runBundle, adapters.LaunchOptions{LoadGlobal: false}, facts); perr == nil {
							ts.IsolationMode = string(lp.Mode)
							for _, d := range lp.Diagnostics {
								if d.Level == adapters.DiagError || d.Level == adapters.DiagWarning {
									ts.Status = "warning"
									rep.Warnings = append(rep.Warnings, fmt.Sprintf("[%s] %s", d.Target, d.Message))
								}
							}
						}
					}
				}

				// 5. Inspect global MCPs and non-empty global skills/rules/agents directories.
				if homeDir != "" {
					for _, gs := range adp.GlobalConfigSpecs() {
						full := runner.ResolveGlobalPath(homeDir, gs)
						if gs.Kind == "mcp" && ps != nil && len(ps.MCP) > 0 {
							conflicts, cw := checkGlobalMCPFile(full, gs.JSONMCPKey, ps)
							rep.GlobalConflicts = append(rep.GlobalConflicts, conflicts...)
							rep.Warnings = append(rep.Warnings, cw...)
							continue
						}
						if gs.Kind != "mcp" && runner.IsNonEmptyPath(full) {
							rep.GlobalAssets = append(rep.GlobalAssets, GlobalAssetEntry{
								Target: t,
								Kind:   gs.Kind,
								Path:   full,
							})
							rep.Warnings = append(rep.Warnings, fmt.Sprintf("global %s detected at %s for target %s (load_global=%t)", gs.Kind, full, t, lg))
						}
					}
				}

				rep.Targets = append(rep.Targets, ts)
			}

			for i, w := range rep.Warnings {
				rep.Warnings[i] = RedactSecrets(w)
			}
			for i, e := range rep.SchemaErrors {
				rep.SchemaErrors[i] = RedactSecrets(e)
			}

			if jsonOutput {
				enc := json.NewEncoder(ec.out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				printDoctorHuman(ec.out, &rep)
			}

			if len(rep.SchemaErrors) > 0 {
				return spec.NewPolicyError("doctor found %d error(s)", len(rep.SchemaErrors))
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit doctor report as JSON")
	return cmd
}

func hasRemoteDeps(ps *spec.ProjectSpec, root string) bool {
	check := func(s spec.SourceSpec) bool {
		p, err := store.ParseSource(s, root, store.ParseOptions{AllowFileScheme: true})
		return err == nil && !p.IsLocal
	}
	for _, sk := range ps.Skills {
		if check(sk.Source) {
			return true
		}
	}
	for _, ag := range ps.Agents {
		if check(ag.Source) {
			return true
		}
	}
	for _, rl := range ps.Rules {
		if check(rl.Source) {
			return true
		}
	}
	return false
}

func checkGlobalMCPFile(full, jsonKey string, ps *spec.ProjectSpec) ([]GlobalConflictEntry, []string) {
	raw, err := os.ReadFile(full)
	if err != nil {
		return nil, nil
	}
	std, err := hujson.Standardize(raw)
	if err != nil {
		return nil, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(std, &doc); err != nil {
		return nil, nil
	}
	sub, ok := doc[jsonKey].(map[string]any)
	if !ok {
		return nil, nil
	}
	names := make([]string, 0, len(sub))
	for k := range sub {
		names = append(names, k)
	}
	sort.Strings(names)
	var conflicts []GlobalConflictEntry
	var warnings []string
	for _, srv := range names {
		if _, exists := ps.MCP[srv]; exists {
			conflicts = append(conflicts, GlobalConflictEntry{
				GlobalFile: full,
				ServerName: srv,
			})
			warnings = append(warnings, fmt.Sprintf("global config %s defines MCP server %q also declared in Agentfile.yaml", full, srv))
		}
	}
	return conflicts, warnings
}

// RedactSecrets masks credential-like tokens inside diagnostic text.
func RedactSecrets(s string) string {
	lower := strings.ToLower(s)
	for _, kw := range []string{"bearer ", "token=", "secret=", "password=", "api_key="} {
		if idx := strings.Index(lower, kw); idx >= 0 {
			prefix := s[:idx+len(kw)]
			rest := s[idx+len(kw):]
			end := strings.IndexAny(rest, " \t\n\"'")
			if end < 0 {
				return prefix + "[REDACTED]"
			}
			return prefix + "[REDACTED]" + rest[end:]
		}
	}
	return s
}

func printDoctorHuman(w interface{ Write([]byte) (int, error) }, rep *DoctorReport) {
	fmt.Fprintf(w, "Manifest valid: %v (lock_status=%s)\n", rep.ManifestValid, rep.LockStatus)
	if rep.EffectiveConfig != nil {
		fmt.Fprintf(w, "Config: link_mode=%s on_conflict=%s git_exclude=%v\n",
			rep.EffectiveConfig.LinkMode, rep.EffectiveConfig.OnConflict, rep.EffectiveConfig.GitExclude)
	}
	for _, ts := range rep.Targets {
		ver := ts.Version
		if ver == "" {
			ver = "not installed"
		}
		fmt.Fprintf(w, "Target %-8s [%s]: binary=%s (%s) contract=%s load_global=%t (%s, mode=%s)\n",
			ts.Target, ts.Status, ts.Binary, ver, ts.SchemaRange, ts.LoadGlobal, ts.LoadGlobalSource, ts.IsolationMode)
	}
	for _, e := range rep.SchemaErrors {
		fmt.Fprintf(w, "error: %s\n", e)
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
}
