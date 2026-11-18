package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/tailscale/hujson"
	"gopkg.in/yaml.v3"
)

const (
	probeAgentsMD      = "claude_agents_md_exists"
	probeProjectMCPMap = "claude_project_mcp_json_exists"
)

// Adapter compiles IR bundles for Claude Code.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string       { return "claude" }
func (a *Adapter) BinaryName() string { return "claude" }

func (a *Adapter) SchemaContract() adapters.SchemaContract {
	return adapters.SchemaContract{
		MinVersion:          "1.0.0",
		MinIsolationVersion: "2.1.0",
		MaxTested:           "2.x",
		BinaryOptional:      false,
	}
}

func (a *Adapter) RequiredProbes() []adapters.ProbeRequest {
	return []adapters.ProbeRequest{
		{Key: probeAgentsMD, RelPath: "AGENTS.md"},
		{Key: probeProjectMCPMap, RelPath: ".mcp.json"},
	}
}

func (a *Adapter) GlobalConfigSpecs() []adapters.GlobalConfigSpec {
	return []adapters.GlobalConfigSpec{
		{RelPath: ".claude.json", Kind: "mcp", JSONMCPKey: "mcpServers"},
		{RelPath: ".claude/skills", Kind: "skills"},
		{RelPath: ".claude/agents", Kind: "agents"},
		{RelPath: ".claude/rules", Kind: "rules"},
		{RelPath: ".claude/CLAUDE.md", Kind: "rules"},
	}
}

func (a *Adapter) IsSessionArgs(userArgs []string) bool {
	nonSessionSubcmds := map[string]bool{
		"update": true, "doctor": true, "auth": true,
		"mcp": true, "plugin": true, "install": true,
	}
	valueFlags := map[string]bool{
		"-p": true, "--print": true, "--prompt": true, "--model": true,
		"--output-format": true, "--input-format": true, "--permission-mode": true,
		"--system-prompt": true, "--append-system-prompt": true,
		"--allowedTools": true, "--disallowedTools": true,
		"--setting-sources": true, "--settings": true, "--mcp-config": true,
	}
	skipNext := false
	seenPositional := false
	for _, tok := range userArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if tok == "--" {
			break
		}
		if tok == "--help" || tok == "-h" || tok == "--version" || tok == "-v" {
			return false
		}
		if !seenPositional && !strings.HasPrefix(tok, "-") {
			seenPositional = true
			if nonSessionSubcmds[tok] {
				return false
			}
		}
		if valueFlags[tok] {
			skipNext = true
		}
	}
	return true
}

func (a *Adapter) PlanLaunch(bundle *ir.Bundle, opts adapters.LaunchOptions, facts adapters.LaunchFacts) (*adapters.LaunchPlan, error) {
	plan := &adapters.LaunchPlan{
		Target: a.Name(),
		Mode:   adapters.IsolationNone,
	}
	if opts.LoadGlobal {
		return plan, nil
	}
	if !a.IsSessionArgs(opts.UserArgs) {
		for _, tok := range opts.UserArgs {
			if tok == "--" || strings.HasPrefix(tok, "-") {
				continue
			}
			if tok == "mcp" || tok == "plugin" || tok == "auth" {
				plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
					Level:   adapters.DiagWarning,
					Target:  a.Name(),
					Code:    "NON_SESSION_SUBCOMMAND",
					Message: fmt.Sprintf("claude subcommand %q runs without isolation flags and may inspect user-global configuration", tok),
				})
			}
			break
		}
		return plan, nil
	}

	warnMode := false
	if bundle != nil {
		warnMode = bundle.TargetsConfig[a.Name()].OnIsolationUnsupported == "warn"
	}

	// Check forbidden flags in userArgs first.
	forbidden := []string{"--setting-sources", "--strict-mcp-config", "--mcp-config", "--bare"}
	valueFlags := []string{
		"-p", "--prompt", "--model", "--output-format", "--input-format",
		"--permission-mode", "--system-prompt", "--append-system-prompt",
		"--allowedTools", "--disallowedTools", "--settings",
	}
	if fDiags := adapters.ValidateForbiddenFlags(a.Name(), opts.UserArgs, forbidden, valueFlags); len(fDiags) > 0 {
		plan.Mode = adapters.IsolationUnsupported
		plan.Diagnostics = append(plan.Diagnostics, fDiags...)
		return plan, nil
	}

	// Version check against MinIsolationVersion.
	minVer := a.SchemaContract().MinIsolationVersion
	verOK, verErr := adapters.CompareSemver(facts.BinaryVersion, minVer)
	if verErr != nil || !verOK {
		msg := fmt.Sprintf("claude binary version %q does not satisfy minimum isolation version %s", facts.BinaryVersion, minVer)
		if verErr != nil {
			msg = fmt.Sprintf("could not verify claude version against %s: %v", minVer, verErr)
		}
		if !warnMode {
			plan.Mode = adapters.IsolationUnsupported
			plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
				Level:   adapters.DiagError,
				Target:  a.Name(),
				Code:    "ISOLATION_VERSION_UNSUPPORTED",
				Message: msg,
			})
			return plan, nil
		}
		plan.Mode = adapters.IsolationPartialWarn
		plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "ISOLATION_VERSION_UNSUPPORTED",
			Message: msg + "; launching without isolation flags due to on_isolation_unsupported: warn",
		})
		return plan, nil
	}

	plan.Mode = adapters.IsolationNativeFlags

	if facts.GlobalHits["skills"] {
		lvl := adapters.DiagError
		if warnMode {
			lvl = adapters.DiagWarning
			plan.Mode = adapters.IsolationPartialWarn
		}
		plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
			Level:   lvl,
			Target:  a.Name(),
			Code:    "GLOBAL_SKILLS_PRESENT",
			Message: "non-empty ~/.claude/skills detected and is not guaranteed suppressed by --setting-sources; remove ~/.claude/skills or set targets_config.claude.on_isolation_unsupported: warn",
		})
		if lvl == adapters.DiagError {
			plan.Mode = adapters.IsolationUnsupported
			return plan, nil
		}
	}

	mcpPath := filepath.Join(facts.CacheRoot, "empty-mcp.json")
	if facts.Probes[probeProjectMCPMap] {
		mcpPath = filepath.Join(facts.ProjectRoot, ".mcp.json")
	} else {
		plan.NeedEmptyMCP = true
	}

	plan.PrependedArgs = []string{
		"--setting-sources", "project,local",
		"--strict-mcp-config",
		"--mcp-config", mcpPath,
	}
	return plan, nil
}

func (a *Adapter) Compile(bundle *ir.Bundle, facts adapters.ProbeResults) (*adapters.CompileResult, error) {
	res := &adapters.CompileResult{Target: a.Name()}
	tcfg := bundle.TargetsConfig[a.Name()]

	if facts[probeAgentsMD] && len(bundle.Rules) > 0 {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "AGENTS_MD_DOUBLE_LOAD",
			Message: "AGENTS.md exists in workspace alongside .claude/rules/agentfile-*.md; verify instructions are not duplicated across both",
		})
	}

	// 1. .mcp.json
	if len(bundle.MCP) > 0 {
		mapEntries := make(map[string]any, len(bundle.MCP))
		for _, srv := range bundle.MCP {
			entry := make(map[string]any)
			switch srv.Transport {
			case "stdio":
				entry["command"] = renderEnv(srv.Command)
				if len(srv.Args) > 0 {
					args := make([]string, 0, len(srv.Args))
					for _, arg := range srv.Args {
						args = append(args, renderEnv(arg))
					}
					entry["args"] = args
				}
				if len(srv.Env) > 0 {
					envMap := make(map[string]string, len(srv.Env))
					for k, v := range srv.Env {
						envMap[k] = renderEnv(v)
					}
					entry["env"] = envMap
				}
			case "http", "sse":
				entry["type"] = srv.Transport
				entry["url"] = renderEnv(srv.URL)
				if len(srv.Headers) > 0 {
					hdrs := make(map[string]string, len(srv.Headers))
					for k, v := range srv.Headers {
						hdrs[k] = renderEnv(v)
					}
					entry["headers"] = hdrs
				}
			}
			mapEntries["/mcpServers/"+escapeJSONPointer(srv.Name)] = entry
		}
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:  a.Name(),
			RelPath: ".mcp.json",
			Kind:    adapters.OpMergeJSONC,
			JSONMerge: &adapters.JSONMergeSpec{
				MapEntries: mapEntries,
			},
		})
	}

	// 2. .claude/settings.json (permissions & hooks & overrides)
	hasSettings := len(bundle.AllowCommands) > 0 || len(bundle.DenyCommands) > 0 || len(bundle.Hooks) > 0 || len(tcfg.Overrides) > 0
	if hasSettings {
		mergeSpec := &adapters.JSONMergeSpec{
			MapEntries:   make(map[string]any),
			ArrayEntries: make(map[string][]string),
		}
		if len(bundle.AllowCommands) > 0 {
			allows := make([]string, 0, len(bundle.AllowCommands))
			for _, cmd := range bundle.AllowCommands {
				allows = append(allows, toClaudeBashRule(cmd))
			}
			mergeSpec.ArrayEntries["/permissions/allow"] = allows
		}
		if len(bundle.DenyCommands) > 0 {
			denies := make([]string, 0, len(bundle.DenyCommands))
			for _, dc := range bundle.DenyCommands {
				denies = append(denies, toClaudeBashRule(dc.Pattern))
			}
			mergeSpec.ArrayEntries["/permissions/deny"] = denies
		}
		if len(bundle.Hooks) > 0 {
			byEvent := make(map[string][]map[string]any)
			for _, h := range bundle.Hooks {
				hookObj := map[string]any{
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": renderEnv(h.Command),
						},
					},
				}
				if h.Matcher != "" {
					hookObj["matcher"] = h.Matcher
				}
				byEvent[h.Event] = append(byEvent[h.Event], hookObj)
			}
			events := make([]string, 0, len(byEvent))
			for ev := range byEvent {
				events = append(events, ev)
			}
			sort.Strings(events)
			for _, ev := range events {
				mergeSpec.MapEntries["/hooks/"+escapeJSONPointer(ev)] = byEvent[ev]
			}
		}
		applyOverrides(mergeSpec, tcfg.Overrides)
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:    a.Name(),
			RelPath:   ".claude/settings.json",
			Kind:      adapters.OpMergeJSONC,
			JSONMerge: mergeSpec,
		})
	}

	// 3. .claude/skills/<name>
	for _, sk := range bundle.Skills {
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:     a.Name(),
			RelPath:    ".claude/skills/" + sk.Name,
			Kind:       adapters.OpMaterializeTree,
			SourcePath: sk.SourcePath,
			IsRemote:   sk.IsRemote,
		})
	}

	// 4. .claude/agents/<name>.md
	for _, ag := range bundle.Agents {
		content := formatAgentMarkdown(ag)
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:  a.Name(),
			RelPath: ".claude/agents/" + ag.Name + ".md",
			Kind:    adapters.OpWriteOwned,
			Content: content,
		})
	}

	// 5. .claude/rules/agentfile-<name>.md
	for _, rl := range bundle.Rules {
		content := formatClaudeRuleMarkdown(rl)
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:  a.Name(),
			RelPath: ".claude/rules/agentfile-" + rl.Name + ".md",
			Kind:    adapters.OpWriteDropIn,
			Content: content,
		})
	}

	return res, nil
}

func (a *Adapter) CheckPolicy(bundle *ir.Bundle, mergedFiles map[string][]byte) []adapters.Diagnostic {
	if len(bundle.DenyCommands) == 0 {
		return nil
	}
	raw, ok := mergedFiles[".claude/settings.json"]
	if !ok {
		return nil
	}
	std, err := hujson.Standardize(raw)
	if err != nil {
		return []adapters.Diagnostic{{
			Level:   adapters.DiagError,
			Target:  a.Name(),
			Code:    "INVALID_JSONC",
			Message: fmt.Sprintf(".claude/settings.json invalid after merge: %v", err),
		}}
	}
	var doc struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(std, &doc); err != nil {
		return nil
	}

	var diags []adapters.Diagnostic
	for _, allow := range doc.Permissions.Allow {
		for _, deny := range bundle.DenyCommands {
			denyRule := toClaudeBashRule(deny.Pattern)
			if allow == denyRule || strings.Contains(allow, strings.TrimSpace(deny.Pattern)) {
				diags = append(diags, adapters.Diagnostic{
					Level:   adapters.DiagError,
					Target:  a.Name(),
					Code:    "POLICY_CONTRADICTION",
					Message: fmt.Sprintf(".claude/settings.json permissions.allow entry %q contradicts deny_commands %q", allow, deny.Pattern),
				})
			}
		}
	}
	return diags
}

func renderEnv(es spec.EnvString) string {
	if len(es.Segments) == 0 {
		return es.Raw
	}
	var b strings.Builder
	for _, seg := range es.Segments {
		if seg.Var == nil {
			b.WriteString(seg.Literal)
			continue
		}
		if seg.Var.HasDefault {
			b.WriteString("${")
			b.WriteString(seg.Var.Name)
			b.WriteString(":-")
			b.WriteString(seg.Var.Default)
			b.WriteString("}")
		} else {
			b.WriteString("${")
			b.WriteString(seg.Var.Name)
			b.WriteString("}")
		}
	}
	return b.String()
}

func toClaudeBashRule(cmd string) string {
	trimmed := strings.TrimSpace(cmd)
	if strings.HasPrefix(trimmed, "Bash(") && strings.HasSuffix(trimmed, ")") {
		return trimmed
	}
	if strings.HasSuffix(trimmed, "*") {
		return "Bash(" + trimmed + ")"
	}
	return "Bash(" + trimmed + ":*)"
}

func formatAgentMarkdown(ag ir.IRAgent) []byte {
	trimmed := bytes.TrimSpace(ag.Content)
	if bytes.HasPrefix(trimmed, []byte("---")) {
		return append(bytes.Clone(ag.Content), ensureTrailingNewline(ag.Content)...)
	}
	if ag.Description == "" && ag.Model == "" {
		return append(bytes.Clone(ag.Content), ensureTrailingNewline(ag.Content)...)
	}
	fm := make(map[string]any)
	if ag.Description != "" {
		fm["description"] = ag.Description
	}
	if ag.Model != "" {
		fm["model"] = ag.Model
	}
	fmBytes, _ := yaml.Marshal(fm)
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(fmBytes)
	buf.WriteString("---\n\n")
	buf.Write(ag.Content)
	buf.Write(ensureTrailingNewline(ag.Content))
	return buf.Bytes()
}

func formatClaudeRuleMarkdown(rl ir.IRRule) []byte {
	trimmed := bytes.TrimSpace(rl.Content)
	if bytes.HasPrefix(trimmed, []byte("---")) || len(rl.Globs) == 0 {
		return append(bytes.Clone(rl.Content), ensureTrailingNewline(rl.Content)...)
	}
	fm := map[string]any{
		"paths": rl.Globs,
	}
	fmBytes, _ := yaml.Marshal(fm)
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(fmBytes)
	buf.WriteString("---\n\n")
	buf.Write(rl.Content)
	buf.Write(ensureTrailingNewline(rl.Content))
	return buf.Bytes()
}

func ensureTrailingNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return nil
	}
	return []byte("\n")
}

func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

func applyOverrides(spec *adapters.JSONMergeSpec, overrides map[string]any) {
	if len(overrides) == 0 {
		return
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec.MapEntries["/"+escapeJSONPointer(k)] = overrides[k]
	}
}
