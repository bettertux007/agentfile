package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters/registry"
	"github.com/bettertux007/agentfile/internal/diff"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/bettertux007/agentfile/schemas"
	"github.com/spf13/cobra"
)

var (
	auditHex40Re  = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	auditSemverRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+(?:\.[0-9]+)?(?:[-+][0-9A-Za-z.-]+)?$`)
)

// AuditReport is the deterministic output of `agentfile audit --json`.
type AuditReport struct {
	Version     int                 `json:"version"`
	LockStatus  string              `json:"lock_status"`
	StoreStatus string              `json:"store_status"` // "checked" | "partial_unhydrated" | "skipped"
	Extends     []AuditLayerSummary `json:"extends,omitempty"`
	Inventory   AuditInventory      `json:"inventory"`
	Findings    []AuditFinding      `json:"findings,omitempty"`
	Summary     AuditSummary        `json:"summary"`
}

// AuditLayerSummary describes one inherited extends base layer.
type AuditLayerSummary struct {
	Source    string `json:"source"`
	Ref       string `json:"ref,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Subpath   string `json:"subpath,omitempty"`
	IsRemote  bool   `json:"is_remote"`
}

// AuditInventory lists executable surfaces present in the composed manifest and hydrated skills.
type AuditInventory struct {
	Hooks        []AuditHookEntry        `json:"hooks,omitempty"`
	StdioMCPs    []AuditStdioMCPEntry    `json:"stdio_mcps,omitempty"`
	SkillScripts []AuditSkillScriptEntry `json:"skill_scripts,omitempty"`
}

// AuditHookEntry describes a configured lifecycle hook.
type AuditHookEntry struct {
	Event   string `json:"event"`
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
	Enforce bool   `json:"enforce"`
	Layer   string `json:"layer"`
}

// AuditStdioMCPEntry describes a stdio MCP server command.
type AuditStdioMCPEntry struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Layer   string   `json:"layer"`
}

// AuditSkillScriptEntry describes an executable or script file inside a hydrated skill tree.
type AuditSkillScriptEntry struct {
	Skill      string `json:"skill"`
	RelPath    string `json:"rel_path"`
	Executable bool   `json:"executable"`
}

// AuditFinding represents one deterministic security or supply-chain finding.
type AuditFinding struct {
	Severity string `json:"severity"` // "error" | "warning"
	RuleID   string `json:"rule_id"`
	Location string `json:"location"`
	Layer    string `json:"layer,omitempty"`
	Message  string `json:"message"`
	Evidence string `json:"evidence,omitempty"`
}

// AuditSummary aggregates finding counts and pass/fail status.
type AuditSummary struct {
	Errors   int  `json:"errors"`
	Warnings int  `json:"warnings"`
	Passed   bool `json:"passed"`
}

func newAuditCmd(ec *execContext) *cobra.Command {
	var jsonOutput bool
	var strict bool
	var noStore bool

	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Run a deterministic offline security and supply-chain audit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := evaluateAudit(ec.workDir, noStore, strict)
			if err != nil {
				return err
			}

			if jsonOutput {
				enc := json.NewEncoder(ec.out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				printAuditHuman(ec.out, rep, strict)
			}

			if !rep.Summary.Passed {
				return spec.NewPolicyError("audit failed (%d error(s), %d warning(s))", rep.Summary.Errors, rep.Summary.Warnings)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit audit report as JSON")
	cmd.Flags().BoolVar(&strict, "strict", false, "Fail with exit code 2 when any warning finding is present")
	cmd.Flags().BoolVar(&noStore, "no-store", false, "Skip local cache store tree hash and script checks")
	return cmd
}

func evaluateAudit(workDir string, noStore, strict bool) (*AuditReport, error) {
	rep := &AuditReport{
		Version:     1,
		LockStatus:  "not_checked",
		StoreStatus: "checked",
	}
	if noStore {
		rep.StoreStatus = "skipped"
	}

	addFinding := func(sev, ruleID, loc, layer, msg, evidence string) {
		rep.Findings = append(rep.Findings, AuditFinding{
			Severity: sev,
			RuleID:   ruleID,
			Location: loc,
			Layer:    diff.RedactSecrets(layer),
			Message:  diff.RedactSecrets(msg),
			Evidence: diff.RedactSecrets(evidence),
		})
	}

	manifestPath := filepath.Join(workDir, "Agentfile.yaml")
	rawManifest, readErr := os.ReadFile(manifestPath)
	var rawRoot *spec.ProjectSpec
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			addFinding("error", "MANIFEST_INVALID", "Agentfile.yaml", "Agentfile.yaml", "Agentfile.yaml not found", "")
		} else {
			addFinding("error", "MANIFEST_INVALID", "Agentfile.yaml", "Agentfile.yaml", readErr.Error(), "")
		}
	} else {
		if err := schemas.ValidateAgentfileV1(rawManifest); err != nil {
			addFinding("error", "MANIFEST_INVALID", "Agentfile.yaml", "Agentfile.yaml", fmt.Sprintf("JSON schema: %v", err), "")
		}
		parsed, perr := spec.ParseRawFile(manifestPath)
		if perr != nil {
			addFinding("error", "MANIFEST_INVALID", "Agentfile.yaml", "Agentfile.yaml", perr.Error(), "")
		} else {
			rawRoot = parsed
		}
	}

	lockPath := filepath.Join(workDir, "Agentfile.lock")
	var lf *spec.Lockfile
	rawLock, lreadErr := os.ReadFile(lockPath)
	if lreadErr == nil {
		if err := schemas.ValidateLockfileV1(rawLock); err != nil {
			rep.LockStatus = "invalid"
			addFinding("error", "LOCK_INVALID", "Agentfile.lock", "Agentfile.lock", fmt.Sprintf("JSON schema: %v", err), "")
		}
		loaded, lerr := spec.LoadLock(lockPath)
		if lerr != nil {
			rep.LockStatus = "invalid"
			addFinding("error", "LOCK_INVALID", "Agentfile.lock", "Agentfile.lock", lerr.Error(), "")
		} else {
			lf = loaded
		}
	} else if !errors.Is(lreadErr, os.ErrNotExist) {
		rep.LockStatus = "invalid"
		addFinding("error", "LOCK_INVALID", "Agentfile.lock", "Agentfile.lock", lreadErr.Error(), "")
	}

	st, err := store.NewDefaultStore()
	if err != nil {
		return nil, err
	}
	parseOpts := store.ParseOptions{
		AllowExternalLocal: true,
		AllowFileScheme:    os.Getenv("AGENTFILE_ALLOW_FILE_GIT") == "1",
	}

	var cm *spec.ComposedManifest
	var depRep *store.InspectReport
	if rawRoot != nil {
		composed, insDep, cerr := st.InspectComposedManifest(workDir, rawRoot, lf, parseOpts)
		if cerr != nil {
			ruleID := "MANIFEST_INVALID"
			msg := cerr.Error()
			if strings.Contains(msg, "cyclic extends") || strings.Contains(msg, "extends depth") || strings.Contains(msg, "conflicting refs") {
				ruleID = "EXTENDS_CYCLE"
			}
			addFinding("error", ruleID, "Agentfile.yaml", "Agentfile.yaml", msg, "")
		} else {
			cm = composed
			depRep = insDep
			if rep.LockStatus != "invalid" {
				rep.LockStatus = depRep.LockStatus
			}
		}
	}

	if cm != nil {
		for _, layer := range cm.Layers {
			if !layer.Origin.IsRoot {
				rep.Extends = append(rep.Extends, AuditLayerSummary{
					Source:    diff.RedactSecrets(layer.Origin.Source),
					Ref:       layer.Origin.Ref,
					CommitSHA: layer.Origin.CommitSHA,
					Subpath:   layer.Origin.Subpath,
					IsRemote:  layer.Origin.IsRemote,
				})
			}
			auditLayerSpec(workDir, layer, parseOpts, addFinding)
		}

		// Inventory: Hooks and Stdio MCPs from composed manifest.
		for _, ho := range cm.HookOrigins {
			rep.Inventory.Hooks = append(rep.Inventory.Hooks, AuditHookEntry{
				Event:   ho.Event,
				Matcher: ho.Matcher,
				Command: diff.RedactSecrets(ho.Command),
				Enforce: ho.Enforce,
				Layer:   diff.RedactSecrets(ho.Origin.DisplayName()),
			})
		}
		mcpNames := make([]string, 0, len(cm.Effective.MCP))
		for k := range cm.Effective.MCP {
			mcpNames = append(mcpNames, k)
		}
		sort.Strings(mcpNames)
		for _, name := range mcpNames {
			srv := cm.Effective.MCP[name]
			if srv.Transport == "stdio" {
				redArgs := make([]string, 0, len(srv.Args))
				for _, a := range srv.Args {
					redArgs = append(redArgs, diff.RedactSecrets(a.Raw))
				}
				orig := cm.Origins[spec.LockKey("mcp", name)]
				rep.Inventory.StdioMCPs = append(rep.Inventory.StdioMCPs, AuditStdioMCPEntry{
					Name:    name,
					Command: diff.RedactSecrets(srv.Command.Raw),
					Args:    redArgs,
					Layer:   diff.RedactSecrets(orig.DisplayName()),
				})
			}
		}

		// Check INHERITED_ENTRY_OVERRIDDEN across overridden keys.
		overriddenKeys := make([]string, 0, len(cm.Overridden))
		for k := range cm.Overridden {
			overriddenKeys = append(overriddenKeys, k)
		}
		sort.Strings(overriddenKeys)
		for _, k := range overriddenKeys {
			prevs := cm.Overridden[k]
			if len(prevs) == 0 {
				continue
			}
			curOrig := cm.Origins[k]
			prevOrig := prevs[len(prevs)-1]
			addFinding(
				"warning",
				"INHERITED_ENTRY_OVERRIDDEN",
				k,
				curOrig.DisplayName(),
				fmt.Sprintf("%s overrides inherited definition from %s", k, prevOrig.DisplayName()),
				"",
			)
		}

		// Check GLOBAL_ISOLATION_OPEN strictly from composed manifest targets_config.
		reg := registry.Default()
		for _, t := range cm.Effective.Targets {
			tc := cm.Effective.TargetsConfig[t]
			adp, _ := registry.Lookup(reg, t)
			canIsolate := adp != nil && adp.SchemaContract().MinIsolationVersion != ""
			orig := cm.Origins["target_config:"+t]
			layerName := orig.DisplayName()
			if tc.LoadGlobal == nil || *tc.LoadGlobal {
				addFinding(
					"warning",
					"GLOBAL_ISOLATION_OPEN",
					fmt.Sprintf("targets_config.%s.load_global", t),
					layerName,
					fmt.Sprintf("target %s allows loading user-global configs (set targets_config.%s.load_global: false to isolate)", t, t),
					"load_global: true",
				)
			} else if !canIsolate {
				addFinding(
					"warning",
					"GLOBAL_ISOLATION_OPEN",
					fmt.Sprintf("targets.%s", t),
					layerName,
					fmt.Sprintf("target %s does not support native runtime global isolation", t),
					"",
				)
			}
		}
	}

	if depRep != nil {
		if rep.LockStatus == "missing" || rep.LockStatus == "out_of_sync" {
			msg := "Agentfile.lock is missing or out of sync with Agentfile.yaml"
			if depRep.LockError != nil {
				msg = depRep.LockError.Error()
			}
			addFinding("warning", "LOCK_MISSING_OR_DRIFT", "Agentfile.lock", "Agentfile.lock", msg, "")
		}

		for _, k := range depRep.ExternalLocal {
			addFinding("warning", "EXTERNAL_LOCAL_PATH", k, "Agentfile.yaml", fmt.Sprintf("%s references a local path outside the repository root", k), "")
		}
		for _, k := range depRep.SymlinkBlocked {
			addFinding("error", "SYMLINK_IN_TREE", k, "Agentfile.yaml", fmt.Sprintf("dependency %s contains a disallowed symlink", k), "")
		}
		for _, k := range depRep.UnhydratedExtends {
			if containsStr(depRep.Corrupt, k) || containsStr(depRep.SymlinkBlocked, k) {
				continue
			}
			addFinding("warning", "EXTENDS_BASE_UNHYDRATED", k, "Agentfile.yaml", fmt.Sprintf("remote extends base %s is not hydrated in local cache (run 'agentfile sync')", k), "")
		}

		if !noStore {
			for _, k := range depRep.Corrupt {
				addFinding("error", "CACHE_INTEGRITY_MISMATCH", k, "Agentfile.lock", fmt.Sprintf("cached store entry for %s failed content hash verification against Agentfile.lock", k), "")
			}
			unhydratedAssets := 0
			for _, k := range depRep.Unhydrated {
				if strings.HasPrefix(k, "extends:") || containsStr(depRep.Corrupt, k) || containsStr(depRep.SymlinkBlocked, k) {
					continue
				}
				unhydratedAssets++
				addFinding("warning", "CACHE_ENTRY_MISSING", k, "Agentfile.lock", fmt.Sprintf("remote dependency %s is not hydrated in local cache (run 'agentfile sync')", k), "")
			}
			if unhydratedAssets > 0 || len(depRep.UnhydratedExtends) > 0 {
				rep.StoreStatus = "partial_unhydrated"
			}

			// Scan hydrated skills for executable or script files.
			skillKeys := make([]string, 0, len(depRep.Assets))
			for k, asset := range depRep.Assets {
				if asset.Kind == "skill" {
					skillKeys = append(skillKeys, k)
				}
			}
			sort.Strings(skillKeys)
			for _, k := range skillKeys {
				asset := depRep.Assets[k]
				scripts := collectSkillScripts(asset.Name, asset.SourcePath)
				rep.Inventory.SkillScripts = append(rep.Inventory.SkillScripts, scripts...)
			}
		}
	}

	sort.Slice(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		ra, rb := auditSeverityRank(a.Severity), auditSeverityRank(b.Severity)
		if ra != rb {
			return ra < rb
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if a.Location != b.Location {
			return a.Location < b.Location
		}
		return a.Layer < b.Layer
	})

	for _, f := range rep.Findings {
		switch f.Severity {
		case "error":
			rep.Summary.Errors++
		case "warning":
			rep.Summary.Warnings++
		}
	}
	rep.Summary.Passed = rep.Summary.Errors == 0 && (!strict || rep.Summary.Warnings == 0)
	return rep, nil
}

func auditLayerSpec(
	workDir string,
	layer spec.ManifestLayer,
	parseOpts store.ParseOptions,
	addFinding func(sev, ruleID, loc, layerName, msg, evidence string),
) {
	ps := layer.Spec
	if ps == nil {
		return
	}
	layerName := layer.Origin.DisplayName()

	checkSource := func(loc string, src spec.SourceSpec) {
		raw := src.Raw
		if src.IsStructured {
			raw = src.Git
		}
		if diff.ContainsHighConfidenceSecret(raw) {
			addFinding("error", "LITERAL_SECRET", loc, layerName, fmt.Sprintf("literal credential detected in %s", loc), raw)
		}
		rebased, err := spec.RebaseSourceSpec(src, layer.Origin, workDir)
		if err != nil {
			return
		}
		parsed, err := store.ParseSource(rebased, workDir, parseOpts)
		if err != nil || parsed.IsLocal {
			return
		}
		// Inspect the originally authored ref on this layer (before remote base SHA pinning).
		authoredRef := parsed.Ref
		if !src.IsStructured {
			if pOrig, perr := store.ParseSource(src, workDir, parseOpts); perr == nil && !pOrig.IsLocal {
				authoredRef = pOrig.Ref
			}
		} else {
			authoredRef = src.Ref
		}
		classifyGitRef(loc, layerName, parsed.CanonicalURL, authoredRef, addFinding)
	}

	for i, ext := range ps.Extends {
		checkSource(fmt.Sprintf("extends[%d]", i), ext)
	}
	for _, sk := range ps.Skills {
		checkSource(fmt.Sprintf("skills.%s.source", sk.Name), sk.Source)
	}
	for _, ag := range ps.Agents {
		checkSource(fmt.Sprintf("agents.%s.source", ag.Name), ag.Source)
		if diff.ContainsHighConfidenceSecret(ag.Description) {
			addFinding("error", "LITERAL_SECRET", fmt.Sprintf("agents.%s.description", ag.Name), layerName, "literal credential detected in agent description", ag.Description)
		}
	}
	for _, rl := range ps.Rules {
		checkSource(fmt.Sprintf("rules.%s.source", rl.Name), rl.Source)
		if diff.ContainsHighConfidenceSecret(rl.Description) {
			addFinding("error", "LITERAL_SECRET", fmt.Sprintf("rules.%s.description", rl.Name), layerName, "literal credential detected in rule description", rl.Description)
		}
	}

	mcpNames := make([]string, 0, len(ps.MCP))
	for k := range ps.MCP {
		mcpNames = append(mcpNames, k)
	}
	sort.Strings(mcpNames)
	for _, name := range mcpNames {
		srv := ps.MCP[name]
		locPrefix := fmt.Sprintf("mcp.%s", name)
		if srv.Transport == "http" || srv.Transport == "sse" {
			if diff.ContainsLiteralSecret(srv.URL.Raw) || diff.ContainsHighConfidenceSecret(srv.URL.Raw) {
				addFinding("error", "LITERAL_SECRET", locPrefix+".url", layerName, fmt.Sprintf("literal credential detected in %s.url", locPrefix), srv.URL.Raw)
			}
			if isCleartextNonLoopbackHTTP(srv.URL.Raw) {
				addFinding("warning", "CLEARTEXT_HTTP_MCP", locPrefix+".url", layerName, fmt.Sprintf("%s uses cleartext http:// to a non-loopback host", locPrefix), srv.URL.Raw)
			}
			hKeys := make([]string, 0, len(srv.Headers))
			for hk := range srv.Headers {
				hKeys = append(hKeys, hk)
			}
			sort.Strings(hKeys)
			for _, hk := range hKeys {
				hv := srv.Headers[hk]
				if diff.ContainsLiteralSecret(hk+": "+hv.Raw) || diff.ContainsHighConfidenceSecret(hv.Raw) {
					addFinding("error", "LITERAL_SECRET", fmt.Sprintf("%s.headers.%s", locPrefix, hk), layerName, fmt.Sprintf("literal secret detected in %s.headers.%s (use ${ENV_VAR})", locPrefix, hk), hk+": "+hv.Raw)
				}
			}
		}
		if srv.Transport == "stdio" {
			if diff.ContainsLiteralSecret(srv.Command.Raw) || diff.ContainsHighConfidenceSecret(srv.Command.Raw) {
				addFinding("error", "LITERAL_SECRET", locPrefix+".command", layerName, fmt.Sprintf("literal credential detected in %s.command", locPrefix), srv.Command.Raw)
			}
			rawArgs := make([]string, 0, len(srv.Args))
			for idx, a := range srv.Args {
				rawArgs = append(rawArgs, a.Raw)
				if diff.ContainsLiteralSecret(a.Raw) || diff.ContainsHighConfidenceSecret(a.Raw) {
					addFinding("error", "LITERAL_SECRET", fmt.Sprintf("%s.args[%d]", locPrefix, idx), layerName, fmt.Sprintf("literal credential detected in %s.args[%d]", locPrefix, idx), a.Raw)
				}
			}
			if pkg, unpinned := detectUnpinnedPackageRunner(srv.Command.Raw, rawArgs); unpinned {
				addFinding("warning", "UNPINNED_PACKAGE_RUNNER", locPrefix+".command", layerName, fmt.Sprintf("%s executes unpinned package %q via %s (pin an explicit version)", locPrefix, pkg, srv.Command.Raw), srv.Command.Raw+" "+strings.Join(rawArgs, " "))
			}
			eKeys := make([]string, 0, len(srv.Env))
			for ek := range srv.Env {
				eKeys = append(eKeys, ek)
			}
			sort.Strings(eKeys)
			for _, ek := range eKeys {
				ev := srv.Env[ek]
				if diff.ContainsLiteralSecret(ek+"="+ev.Raw) || diff.ContainsHighConfidenceSecret(ev.Raw) {
					addFinding("error", "LITERAL_SECRET", fmt.Sprintf("%s.env.%s", locPrefix, ek), layerName, fmt.Sprintf("literal secret detected in %s.env.%s (use ${ENV_VAR})", locPrefix, ek), ek+"="+ev.Raw)
				}
			}
		}
	}

	hookEvents := make([]string, 0, len(ps.Settings.Hooks))
	for ev := range ps.Settings.Hooks {
		hookEvents = append(hookEvents, ev)
	}
	sort.Strings(hookEvents)
	for _, ev := range hookEvents {
		for idx, h := range ps.Settings.Hooks[ev] {
			loc := fmt.Sprintf("settings.hooks.%s[%d]", ev, idx)
			if diff.ContainsLiteralSecret(h.Command.Raw) || diff.ContainsHighConfidenceSecret(h.Command.Raw) {
				addFinding("error", "LITERAL_SECRET", loc, layerName, fmt.Sprintf("literal credential detected in %s", loc), h.Command.Raw)
			}
			fields := strings.Fields(h.Command.Raw)
			if len(fields) > 1 {
				if pkg, unpinned := detectUnpinnedPackageRunner(fields[0], fields[1:]); unpinned {
					addFinding("warning", "UNPINNED_PACKAGE_RUNNER", loc, layerName, fmt.Sprintf("%s executes unpinned package %q via %s", loc, pkg, fields[0]), h.Command.Raw)
				}
			}
		}
	}

	tNames := make([]string, 0, len(ps.TargetsConfig))
	for t := range ps.TargetsConfig {
		tNames = append(tNames, t)
	}
	sort.Strings(tNames)
	for _, t := range tNames {
		tc := ps.TargetsConfig[t]
		if tc.OnUnsupported == "warn" {
			addFinding("warning", "POLICY_DOWNGRADE_WARN", fmt.Sprintf("targets_config.%s.on_unsupported", t), layerName, fmt.Sprintf("targets_config.%s.on_unsupported: warn downgrades unsupported security controls to warnings", t), "on_unsupported: warn")
		}
		if tc.OnIsolationUnsupported == "warn" {
			addFinding("warning", "POLICY_DOWNGRADE_WARN", fmt.Sprintf("targets_config.%s.on_isolation_unsupported", t), layerName, fmt.Sprintf("targets_config.%s.on_isolation_unsupported: warn downgrades global isolation failures to warnings", t), "on_isolation_unsupported: warn")
		}
		auditOverridesValue(fmt.Sprintf("targets_config.%s.overrides", t), layerName, "", tc.Overrides, addFinding)
	}
}

func auditOverridesValue(
	loc, layerName, keyHint string,
	val any,
	addFinding func(sev, ruleID, loc, layerName, msg, evidence string),
) {
	switch v := val.(type) {
	case string:
		kv := v
		if keyHint != "" {
			kv = keyHint + ": " + v
		}
		if diff.ContainsLiteralSecret(kv) || diff.ContainsHighConfidenceSecret(v) {
			addFinding("error", "LITERAL_SECRET", loc, layerName, fmt.Sprintf("literal secret detected in %s", loc), kv)
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			auditOverridesValue(loc+"."+k, layerName, k, v[k], addFinding)
		}
	case []any:
		for i, item := range v {
			auditOverridesValue(fmt.Sprintf("%s[%d]", loc, i), layerName, keyHint, item, addFinding)
		}
	}
}

func classifyGitRef(
	loc, layerName, canonicalURL, ref string,
	addFinding func(sev, ruleID, loc, layerName, msg, evidence string),
) {
	trimmed := strings.TrimSpace(ref)
	if auditHex40Re.MatchString(trimmed) {
		return
	}
	if strings.HasPrefix(trimmed, "refs/tags/") || auditSemverRe.MatchString(trimmed) {
		addFinding(
			"warning",
			"MUTABLE_TAG",
			loc,
			layerName,
			fmt.Sprintf("%s uses mutable Git tag %q on %s (pin a 40-character commit SHA for immutable supply-chain pinning)", loc, trimmed, canonicalURL),
			trimmed,
		)
		return
	}
	label := trimmed
	if label == "" {
		label = "HEAD (omitted ref)"
	}
	addFinding(
		"warning",
		"FLOATING_REF",
		loc,
		layerName,
		fmt.Sprintf("%s uses floating Git ref %q on %s (pin a release tag or 40-character commit SHA)", loc, label, canonicalURL),
		label,
	)
}

func isCleartextNonLoopbackHTTP(rawURL string) bool {
	trimmed := strings.TrimSpace(rawURL)
	if !strings.HasPrefix(strings.ToLower(trimmed), "http://") {
		return false
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "localhost", "127.0.0.1", "::1", "":
		return false
	default:
		return !strings.HasPrefix(host, "${")
	}
}

func detectUnpinnedPackageRunner(cmd string, args []string) (string, bool) {
	base := filepath.Base(strings.TrimSpace(cmd))
	switch base {
	case "npx", "bunx":
		for _, a := range args {
			a = strings.TrimSpace(a)
			if a == "" || strings.HasPrefix(a, "-") {
				continue
			}
			if strings.HasPrefix(a, "${") || strings.HasPrefix(a, ".") || strings.HasPrefix(a, "/") {
				return "", false
			}
			if !hasNPMPackageVersionPin(a) {
				return a, true
			}
			return "", false
		}
	case "uvx":
		for i := 0; i < len(args); i++ {
			a := strings.TrimSpace(args[i])
			if a == "" {
				continue
			}
			if a == "--from" && i+1 < len(args) {
				pkg := strings.TrimSpace(args[i+1])
				if !strings.Contains(pkg, "==") && !strings.Contains(pkg, "@") && !strings.HasPrefix(pkg, "${") {
					return pkg, true
				}
				return "", false
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			if strings.HasPrefix(a, "${") || strings.HasPrefix(a, ".") || strings.HasPrefix(a, "/") {
				return "", false
			}
			if !strings.Contains(a, "==") && !strings.Contains(a, "@") {
				return a, true
			}
			return "", false
		}
	}
	return "", false
}

func hasNPMPackageVersionPin(pkg string) bool {
	if strings.HasPrefix(pkg, "@") {
		slash := strings.IndexByte(pkg, '/')
		if slash < 0 {
			return false
		}
		rest := pkg[slash+1:]
		at := strings.IndexByte(rest, '@')
		return at > 0 && rest[at+1:] != "" && rest[at+1:] != "latest"
	}
	at := strings.IndexByte(pkg, '@')
	return at > 0 && pkg[at+1:] != "" && pkg[at+1:] != "latest"
}

func collectSkillScripts(skillName, rootDir string) []AuditSkillScriptEntry {
	info, err := os.Stat(rootDir)
	if err != nil || !info.IsDir() {
		return nil
	}
	var out []AuditSkillScriptEntry
	_ = filepath.WalkDir(rootDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		finfo, serr := d.Info()
		if serr != nil {
			return nil
		}
		isExec := finfo.Mode()&0o111 != 0
		ext := strings.ToLower(filepath.Ext(p))
		isScriptExt := ext == ".sh" || ext == ".py" || ext == ".js" || ext == ".ts" || ext == ".ps1" || ext == ".rb"
		if !isExec && !isScriptExt {
			return nil
		}
		rel, rerr := filepath.Rel(rootDir, p)
		if rerr != nil {
			rel = filepath.Base(p)
		}
		out = append(out, AuditSkillScriptEntry{
			Skill:      skillName,
			RelPath:    filepath.ToSlash(rel),
			Executable: isExec,
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].RelPath < out[j].RelPath
	})
	return out
}

func auditSeverityRank(sev string) int {
	switch sev {
	case "error":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

func containsStr(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func printAuditHuman(w interface{ Write([]byte) (int, error) }, rep *AuditReport, strict bool) {
	status := "PASS"
	if !rep.Summary.Passed {
		status = "FAIL"
	}
	fmt.Fprintf(w, "Audit status: %s (errors=%d warnings=%d strict=%t lock_status=%s store_status=%s)\n",
		status, rep.Summary.Errors, rep.Summary.Warnings, strict, rep.LockStatus, rep.StoreStatus)

	if len(rep.Extends) > 0 {
		fmt.Fprintln(w, "Extends layers:")
		for _, ext := range rep.Extends {
			fmt.Fprintf(w, "  - %s (ref=%s sha=%s remote=%t)\n", ext.Source, ext.Ref, ext.CommitSHA, ext.IsRemote)
		}
	}
	if len(rep.Inventory.Hooks) > 0 || len(rep.Inventory.StdioMCPs) > 0 || len(rep.Inventory.SkillScripts) > 0 {
		fmt.Fprintln(w, "Executable inventory:")
		for _, h := range rep.Inventory.Hooks {
			fmt.Fprintf(w, "  hook: %s (%s): %s [enforce=%t layer=%s]\n", h.Event, h.Matcher, h.Command, h.Enforce, h.Layer)
		}
		for _, m := range rep.Inventory.StdioMCPs {
			fmt.Fprintf(w, "  stdio mcp: %s: %s %s [layer=%s]\n", m.Name, m.Command, strings.Join(m.Args, " "), m.Layer)
		}
		for _, sc := range rep.Inventory.SkillScripts {
			fmt.Fprintf(w, "  skill script: %s/%s [exec=%t]\n", sc.Skill, sc.RelPath, sc.Executable)
		}
	}
	for _, f := range rep.Findings {
		fmt.Fprintf(w, "%s [%s] %s (%s): %s\n", f.Severity, f.RuleID, f.Location, f.Layer, f.Message)
	}
}
