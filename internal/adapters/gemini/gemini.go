package gemini

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
)

// Adapter compiles IR bundles for Gemini CLI.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string       { return "gemini" }
func (a *Adapter) BinaryName() string { return "gemini" }

func (a *Adapter) SchemaContract() adapters.SchemaContract {
	return adapters.SchemaContract{
		MinVersion:     "0.1.0",
		MaxTested:      "0.x",
		BinaryOptional: false,
	}
}

func (a *Adapter) RequiredProbes() []adapters.ProbeRequest {
	return nil
}

func (a *Adapter) GlobalConfigSpecs() []adapters.GlobalConfigSpec {
	return []adapters.GlobalConfigSpec{
		{RelPath: ".gemini/settings.json", Kind: "mcp", JSONMCPKey: "mcpServers"},
		{RelPath: ".gemini/skills", Kind: "skills"},
		{RelPath: ".gemini/GEMINI.md", Kind: "rules"},
	}
}

func (a *Adapter) IsSessionArgs(userArgs []string) bool {
	nonSessionSubcmds := map[string]bool{
		"mcp": true, "skills": true, "hooks": true, "extensions": true,
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

func (a *Adapter) PlanLaunch(bundle *ir.Bundle, opts adapters.LaunchOptions, _ adapters.LaunchFacts) (*adapters.LaunchPlan, error) {
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
	if !warnMode {
		plan.Mode = adapters.IsolationUnsupported
		plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagError,
			Target:  a.Name(),
			Code:    "ISOLATION_UNSUPPORTED",
			Message: "gemini cannot isolate global ~/.gemini/skills or GEMINI.md without overriding HOME and breaking OAuth/child toolchains; set targets_config.gemini.on_isolation_unsupported: warn to allow partial isolation",
		})
		return plan, nil
	}

	plan.Mode = adapters.IsolationPartialWarn
	if bundle != nil && len(bundle.MCP) > 0 {
		if fDiags := adapters.ValidateForbiddenFlags(a.Name(), opts.UserArgs, []string{"--allowed-mcp-server-names"}, []string{"-p", "--prompt", "-m", "--model"}); len(fDiags) > 0 {
			plan.Mode = adapters.IsolationUnsupported
			plan.Diagnostics = append(plan.Diagnostics, fDiags...)
			return plan, nil
		}
		names := make([]string, 0, len(bundle.MCP))
		for _, srv := range bundle.MCP {
			names = append(names, srv.Name)
		}
		sort.Strings(names)
		plan.PrependedArgs = []string{"--allowed-mcp-server-names", strings.Join(names, ",")}
		plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "ISOLATION_PARTIAL_WARN",
			Message: "gemini global isolation is partial: MCPs are scoped to project servers, while ~/.gemini/skills and ~/.gemini/GEMINI.md remain active",
		})
	} else {
		plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "ISOLATION_PARTIAL_WARN",
			Message: "gemini global isolation is partial: no project MCPs declared, so global MCPs, ~/.gemini/skills, and ~/.gemini/GEMINI.md remain active",
		})
	}
	return plan, nil
}

func (a *Adapter) Compile(bundle *ir.Bundle, _ adapters.ProbeResults) (*adapters.CompileResult, error) {
	res := &adapters.CompileResult{Target: a.Name()}
	tcfg := bundle.TargetsConfig[a.Name()]
	warnMode := tcfg.OnUnsupported == "warn"

	// 1. .gemini/settings.json (mcpServers + overrides)
	if len(bundle.MCP) > 0 || len(tcfg.Overrides) > 0 {
		mergeSpec := &adapters.JSONMergeSpec{
			MapEntries: make(map[string]any),
		}
		for _, srv := range bundle.MCP {
			entry := make(map[string]any)
			switch srv.Transport {
			case "stdio":
				cmdStr, d := a.renderEnv(srv.Command, warnMode)
				res.Diagnostics = append(res.Diagnostics, d...)
				entry["command"] = cmdStr
				if len(srv.Args) > 0 {
					args := make([]string, 0, len(srv.Args))
					for _, arg := range srv.Args {
						s, ad := a.renderEnv(arg, warnMode)
						res.Diagnostics = append(res.Diagnostics, ad...)
						args = append(args, s)
					}
					entry["args"] = args
				}
				if len(srv.Env) > 0 {
					envMap := make(map[string]string, len(srv.Env))
					for k, v := range srv.Env {
						s, ed := a.renderEnv(v, warnMode)
						res.Diagnostics = append(res.Diagnostics, ed...)
						envMap[k] = s
					}
					entry["env"] = envMap
				}
			case "http", "sse":
				urlStr, d := a.renderEnv(srv.URL, warnMode)
				res.Diagnostics = append(res.Diagnostics, d...)
				entry["httpUrl"] = urlStr
				if len(srv.Headers) > 0 {
					hdrs := make(map[string]string, len(srv.Headers))
					for k, v := range srv.Headers {
						s, hd := a.renderEnv(v, warnMode)
						res.Diagnostics = append(res.Diagnostics, hd...)
						hdrs[k] = s
					}
					entry["headers"] = hdrs
				}
			}
			mergeSpec.MapEntries["/mcpServers/"+escapeJSONPointer(srv.Name)] = entry
		}
		if len(tcfg.Overrides) > 0 {
			keys := make([]string, 0, len(tcfg.Overrides))
			for k := range tcfg.Overrides {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				mergeSpec.MapEntries["/"+escapeJSONPointer(k)] = tcfg.Overrides[k]
			}
		}
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:    a.Name(),
			RelPath:   ".gemini/settings.json",
			Kind:      adapters.OpMergeJSONC,
			JSONMerge: mergeSpec,
		})
	}

	// 2. .gemini/skills/<name>
	for _, sk := range bundle.Skills {
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:     a.Name(),
			RelPath:    ".gemini/skills/" + sk.Name,
			Kind:       adapters.OpMaterializeTree,
			SourcePath: sk.SourcePath,
			IsRemote:   sk.IsRemote,
		})
	}

	// 3. GEMINI.md managed block
	if len(bundle.Rules) > 0 {
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:  a.Name(),
			RelPath: "GEMINI.md",
			Kind:    adapters.OpManagedBlock,
			Content: compileRulesMarkdown(bundle.Rules),
		})
	}

	// 4. Unsupported security & advisory primitives
	for _, dc := range bundle.DenyCommands {
		lvl := adapters.DiagError
		if warnMode {
			lvl = adapters.DiagWarning
		}
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   lvl,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_DENY_COMMAND",
			Message: fmt.Sprintf("deny_commands %q is unsupported natively in .gemini/settings.json; set targets_config.gemini.on_unsupported: warn to omit with warning", dc.Pattern),
		})
	}
	for _, h := range bundle.Hooks {
		lvl := adapters.DiagWarning
		if h.Enforced && !warnMode {
			lvl = adapters.DiagError
		}
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   lvl,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_HOOK",
			Message: fmt.Sprintf("hook (%s: %s) is unsupported by gemini and was omitted", h.Event, h.Command.Raw),
		})
	}
	if len(bundle.AllowCommands) > 0 {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_ALLOW_COMMANDS",
			Message: "settings.permissions.allow_commands is unsupported by gemini and was omitted",
		})
	}
	for _, ag := range bundle.Agents {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_AGENT",
			Message: fmt.Sprintf("subagent %q is unsupported natively by gemini and was omitted", ag.Name),
		})
	}

	return res, nil
}

func (a *Adapter) CheckPolicy(_ *ir.Bundle, _ map[string][]byte) []adapters.Diagnostic {
	return nil
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
			msg := fmt.Sprintf("env placeholder ${%s:-%s} with default value is unsupported in .gemini/settings.json", seg.Var.Name, seg.Var.Default)
			if warnMode {
				lvl = adapters.DiagWarning
				msg = fmt.Sprintf("env placeholder ${%s:-%s} default stripped to ${%s} for .gemini/settings.json", seg.Var.Name, seg.Var.Default, seg.Var.Name)
			}
			diags = append(diags, adapters.Diagnostic{
				Level:   lvl,
				Target:  a.Name(),
				Code:    "UNSUPPORTED_ENV_DEFAULT",
				Message: msg,
			})
		}
		b.WriteString("${")
		b.WriteString(seg.Var.Name)
		b.WriteString("}")
	}
	return b.String(), diags
}

func compileRulesMarkdown(rules []ir.IRRule) []byte {
	var buf bytes.Buffer
	buf.WriteString("# Project Agent Instructions\n\n")
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

func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}
