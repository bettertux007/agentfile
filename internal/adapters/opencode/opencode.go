package opencode

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

const opencodeSchemaURL = "https://opencode.ai/config.json"

// Adapter compiles IR bundles for OpenCode V1.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string       { return "opencode" }
func (a *Adapter) BinaryName() string { return "opencode" }

func (a *Adapter) SchemaContract() adapters.SchemaContract {
	return adapters.SchemaContract{
		MinVersion:          "1.0.0",
		MinIsolationVersion: "1.18.0",
		MaxTested:           "2.x",
		BinaryOptional:      false,
	}
}

func (a *Adapter) RequiredProbes() []adapters.ProbeRequest {
	return nil
}

func (a *Adapter) GlobalConfigSpecs() []adapters.GlobalConfigSpec {
	return []adapters.GlobalConfigSpec{
		{RelPath: "opencode/opencode.json", Kind: "mcp", JSONMCPKey: "mcp", UseXDGConfig: true},
		{RelPath: "opencode/skills", Kind: "skills", UseXDGConfig: true},
		{RelPath: "opencode/agents", Kind: "agents", UseXDGConfig: true},
		{RelPath: ".claude/skills", Kind: "skills"},
		{RelPath: ".agents/skills", Kind: "agents_skills"},
	}
}

func (a *Adapter) IsSessionArgs(userArgs []string) bool {
	nonSessionSubcmds := map[string]bool{
		"auth": true, "mcp": true, "service": true,
		"upgrade": true, "update": true, "uninstall": true,
	}
	for _, tok := range userArgs {
		if tok == "--" {
			break
		}
		if tok == "--help" || tok == "-h" || tok == "--version" || tok == "-v" {
			return false
		}
		if !strings.HasPrefix(tok, "-") {
			return !nonSessionSubcmds[tok]
		}
	}
	return true
}

func (a *Adapter) PlanLaunch(bundle *ir.Bundle, opts adapters.LaunchOptions, facts adapters.LaunchFacts) (*adapters.LaunchPlan, error) {
	plan := &adapters.LaunchPlan{
		Target: a.Name(),
		Mode:   adapters.IsolationNone,
	}
	if opts.LoadGlobal || !a.IsSessionArgs(opts.UserArgs) {
		return plan, nil
	}

	warnMode := false
	if bundle != nil {
		warnMode = bundle.TargetsConfig[a.Name()].OnIsolationUnsupported == "warn"
	}

	if fDiags := adapters.ValidateForbiddenFlags(a.Name(), opts.UserArgs, []string{"--standalone"}, []string{"--prompt", "-p", "--session", "-s", "--server"}); len(fDiags) > 0 {
		plan.Mode = adapters.IsolationUnsupported
		plan.Diagnostics = append(plan.Diagnostics, fDiags...)
		return plan, nil
	}

	minVer := a.SchemaContract().MinIsolationVersion
	verOK, verErr := adapters.CompareSemver(facts.BinaryVersion, minVer)
	if verErr != nil || !verOK {
		msg := fmt.Sprintf("opencode binary version %q does not satisfy minimum isolation version %s", facts.BinaryVersion, minVer)
		if verErr != nil {
			msg = fmt.Sprintf("could not verify opencode version against %s: %v", minVer, verErr)
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
			Message: msg + "; launching without --standalone due to on_isolation_unsupported: warn",
		})
		return plan, nil
	}

	plan.Mode = adapters.IsolationNativeFlags
	emptyCfgDir := filepath.Join(facts.CacheRoot, "empty-opencode-config")
	plan.EnsureDirs = []string{emptyCfgDir}
	plan.EnvOverrides = map[string]string{
		"OPENCODE_CONFIG_DIR":                 emptyCfgDir,
		"OPENCODE_DISABLE_CLAUDE_CODE_SKILLS": "1",
	}
	plan.PrependedArgs = []string{"--standalone"}

	if facts.GlobalHits["agents_skills"] {
		lvl := adapters.DiagError
		if warnMode {
			lvl = adapters.DiagWarning
			plan.Mode = adapters.IsolationPartialWarn
		}
		plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
			Level:   lvl,
			Target:  a.Name(),
			Code:    "GLOBAL_AGENTS_SKILLS_PRESENT",
			Message: "non-empty ~/.agents/skills detected and may still be scanned by opencode; remove ~/.agents/skills or set targets_config.opencode.on_isolation_unsupported: warn",
		})
		if lvl == adapters.DiagError {
			plan.Mode = adapters.IsolationUnsupported
			plan.PrependedArgs = nil
			plan.EnvOverrides = nil
			plan.EnsureDirs = nil
			return plan, nil
		}
	}

	return plan, nil
}

func (a *Adapter) Compile(bundle *ir.Bundle, _ adapters.ProbeResults) (*adapters.CompileResult, error) {
	res := &adapters.CompileResult{Target: a.Name()}
	tcfg := bundle.TargetsConfig[a.Name()]
	warnMode := tcfg.OnUnsupported == "warn"

	mergeSpec := &adapters.JSONMergeSpec{
		SchemaURL:    opencodeSchemaURL,
		MapEntries:   make(map[string]any),
		ArrayEntries: make(map[string][]string),
	}
	hasConfigEntries := false

	// 1. MCP servers in opencode.json
	for _, srv := range bundle.MCP {
		hasConfigEntries = true
		entry := make(map[string]any)
		switch srv.Transport {
		case "stdio":
			entry["type"] = "local"
			cmdStr, diags := a.renderEnv(srv.Command, warnMode)
			res.Diagnostics = append(res.Diagnostics, diags...)
			cmdSlice := []string{cmdStr}
			for _, arg := range srv.Args {
				s, d := a.renderEnv(arg, warnMode)
				res.Diagnostics = append(res.Diagnostics, d...)
				cmdSlice = append(cmdSlice, s)
			}
			entry["command"] = cmdSlice
			if len(srv.Env) > 0 {
				envMap := make(map[string]string, len(srv.Env))
				for k, v := range srv.Env {
					s, d := a.renderEnv(v, warnMode)
					res.Diagnostics = append(res.Diagnostics, d...)
					envMap[k] = s
				}
				entry["environment"] = envMap
			}
		case "http", "sse":
			entry["type"] = "remote"
			urlStr, diags := a.renderEnv(srv.URL, warnMode)
			res.Diagnostics = append(res.Diagnostics, diags...)
			entry["url"] = urlStr
			if len(srv.Headers) > 0 {
				hdrs := make(map[string]string, len(srv.Headers))
				for k, v := range srv.Headers {
					s, d := a.renderEnv(v, warnMode)
					res.Diagnostics = append(res.Diagnostics, d...)
					hdrs[k] = s
				}
				entry["headers"] = hdrs
			}
		}
		mergeSpec.MapEntries["/mcp/"+escapeJSONPointer(srv.Name)] = entry
	}

	// 2. Permissions (V1 permission.bash map)
	for _, allow := range bundle.AllowCommands {
		pat, ok := toOpenCodeBashPattern(allow)
		if !ok {
			res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
				Level:   adapters.DiagWarning,
				Target:  a.Name(),
				Code:    "UNTRANSLATABLE_ALLOW_COMMAND",
				Message: fmt.Sprintf("allow_commands entry %q contains unsupported shell operators for opencode V1 permission.bash and was omitted", allow),
			})
			continue
		}
		hasConfigEntries = true
		mergeSpec.MapEntries["/permission/bash/"+escapeJSONPointer(pat)] = "allow"
	}

	for _, dc := range bundle.DenyCommands {
		pat, ok := toOpenCodeBashPattern(dc.Pattern)
		if !ok {
			lvl := adapters.DiagError
			if warnMode {
				lvl = adapters.DiagWarning
			}
			res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
				Level:   lvl,
				Target:  a.Name(),
				Code:    "UNTRANSLATABLE_DENY_COMMAND",
				Message: fmt.Sprintf("deny_commands entry %q cannot be represented in opencode V1 permission.bash", dc.Pattern),
			})
			continue
		}
		hasConfigEntries = true
		mergeSpec.MapEntries["/permission/bash/"+escapeJSONPointer(pat)] = "deny"
	}

	// 3. Hooks (unsupported natively in opencode.json)
	for _, h := range bundle.Hooks {
		if h.Enforced && !warnMode {
			res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
				Level:   adapters.DiagError,
				Target:  a.Name(),
				Code:    "UNSUPPORTED_HOOK",
				Message: fmt.Sprintf("enforced hook (%s: %s) is unsupported by opencode; set targets_config.opencode.on_unsupported: warn to omit with warning", h.Event, h.Command.Raw),
			})
		} else {
			res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
				Level:   adapters.DiagWarning,
				Target:  a.Name(),
				Code:    "UNSUPPORTED_HOOK",
				Message: fmt.Sprintf("hook (%s: %s) is unsupported by opencode and was omitted", h.Event, h.Command.Raw),
			})
		}
	}

	// 4. Rules -> .opencode/agentfile-rules.md + opencode.json instructions array
	if len(bundle.Rules) > 0 {
		hasConfigEntries = true
		mergeSpec.ArrayEntries["/instructions"] = []string{".opencode/agentfile-rules.md"}
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:  a.Name(),
			RelPath: ".opencode/agentfile-rules.md",
			Kind:    adapters.OpWriteDropIn,
			Content: compileRulesMarkdown(bundle.Rules),
		})
	}

	// 5. Overrides
	if len(tcfg.Overrides) > 0 {
		hasConfigEntries = true
		keys := make([]string, 0, len(tcfg.Overrides))
		for k := range tcfg.Overrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			mergeSpec.MapEntries["/"+escapeJSONPointer(k)] = tcfg.Overrides[k]
		}
	}

	if hasConfigEntries {
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:    a.Name(),
			RelPath:   "opencode.json",
			Kind:      adapters.OpMergeJSONC,
			JSONMerge: mergeSpec,
		})
	}

	// 6. Skills -> .opencode/skills/<name>
	for _, sk := range bundle.Skills {
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:     a.Name(),
			RelPath:    ".opencode/skills/" + sk.Name,
			Kind:       adapters.OpMaterializeTree,
			SourcePath: sk.SourcePath,
			IsRemote:   sk.IsRemote,
		})
	}

	// 7. Agents -> .opencode/agents/<name>.md
	for _, ag := range bundle.Agents {
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:  a.Name(),
			RelPath: ".opencode/agents/" + ag.Name + ".md",
			Kind:    adapters.OpWriteOwned,
			Content: formatAgentMarkdown(ag),
		})
	}

	return res, nil
}

func (a *Adapter) CheckPolicy(bundle *ir.Bundle, mergedFiles map[string][]byte) []adapters.Diagnostic {
	if len(bundle.DenyCommands) == 0 {
		return nil
	}
	raw, ok := mergedFiles["opencode.json"]
	if !ok {
		return nil
	}
	std, err := hujson.Standardize(raw)
	if err != nil {
		return []adapters.Diagnostic{{
			Level:   adapters.DiagError,
			Target:  a.Name(),
			Code:    "INVALID_JSONC",
			Message: fmt.Sprintf("opencode.json invalid after merge: %v", err),
		}}
	}
	var doc struct {
		Permission struct {
			Bash map[string]string `json:"bash"`
		} `json:"permission"`
	}
	if err := json.Unmarshal(std, &doc); err != nil {
		return nil
	}

	var diags []adapters.Diagnostic
	for _, dc := range bundle.DenyCommands {
		pat, ok := toOpenCodeBashPattern(dc.Pattern)
		if !ok {
			continue
		}
		if action, exists := doc.Permission.Bash[pat]; exists && action == "allow" {
			diags = append(diags, adapters.Diagnostic{
				Level:   adapters.DiagError,
				Target:  a.Name(),
				Code:    "POLICY_CONTRADICTION",
				Message: fmt.Sprintf("opencode.json permission.bash[%q] is 'allow', contradicting deny_commands %q", pat, dc.Pattern),
			})
		}
	}
	return diags
}

func (a *Adapter) renderEnv(es spec.EnvString, warnMode bool) (string, []adapters.Diagnostic) {
	if len(es.Segments) == 0 {
		return es.Raw, nil
	}
	var b strings.Builder
	var diags []adapters.Diagnostic
	for _, seg := range es.Segments {
		if seg.Var == nil {
			b.WriteString(seg.Literal)
			continue
		}
		if seg.Var.HasDefault {
			lvl := adapters.DiagError
			msg := fmt.Sprintf("env placeholder ${%s:-%s} with default value is unsupported in opencode.json {env:VAR} syntax", seg.Var.Name, seg.Var.Default)
			if warnMode {
				lvl = adapters.DiagWarning
				msg = fmt.Sprintf("env placeholder ${%s:-%s} default stripped to {env:%s} for opencode.json", seg.Var.Name, seg.Var.Default, seg.Var.Name)
			}
			diags = append(diags, adapters.Diagnostic{
				Level:   lvl,
				Target:  a.Name(),
				Code:    "UNSUPPORTED_ENV_DEFAULT",
				Message: msg,
			})
		}
		b.WriteString("{env:")
		b.WriteString(seg.Var.Name)
		b.WriteString("}")
	}
	return b.String(), diags
}

func toOpenCodeBashPattern(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	for _, bad := range []string{";", "&&", "||", "|", "`", "$(", "\n", "\r"} {
		if strings.Contains(trimmed, bad) {
			return "", false
		}
	}
	if strings.HasSuffix(trimmed, "*") {
		return trimmed, true
	}
	return trimmed + " *", true
}

func compileRulesMarkdown(rules []ir.IRRule) []byte {
	var buf bytes.Buffer
	buf.WriteString("# Project Agent Instructions (Managed by agentfile)\n\n")
	for i, rl := range rules {
		if i > 0 {
			buf.WriteString("\n---\n\n")
		}
		buf.WriteString("## ")
		buf.WriteString(rl.Name)
		buf.WriteString("\n\n")
		if rl.Description != "" {
			buf.WriteString("*Scope:* ")
			buf.WriteString(rl.Description)
			buf.WriteString("\n")
		}
		if len(rl.Globs) > 0 {
			buf.WriteString("*Applies to:* `")
			buf.WriteString(strings.Join(rl.Globs, "`, `"))
			buf.WriteString("`\n")
		}
		if rl.Description != "" || len(rl.Globs) > 0 {
			buf.WriteString("\n")
		}
		buf.Write(rl.Content)
		if len(rl.Content) > 0 && rl.Content[len(rl.Content)-1] != '\n' {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

func formatAgentMarkdown(ag ir.IRAgent) []byte {
	trimmed := bytes.TrimSpace(ag.Content)
	if bytes.HasPrefix(trimmed, []byte("---")) || (ag.Description == "" && ag.Model == "") {
		out := bytes.Clone(ag.Content)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		return out
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
	if len(ag.Content) > 0 && ag.Content[len(ag.Content)-1] != '\n' {
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}
