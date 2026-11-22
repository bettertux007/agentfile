package copilot

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
)

// Adapter compiles IR bundles for GitHub Copilot (VS Code).
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string       { return "copilot" }
func (a *Adapter) BinaryName() string { return "code" }

func (a *Adapter) SchemaContract() adapters.SchemaContract {
	return adapters.SchemaContract{
		MinVersion:     "1.99.0",
		MaxTested:      "1.x",
		BinaryOptional: true,
	}
}

func (a *Adapter) RequiredProbes() []adapters.ProbeRequest {
	return nil
}

func (a *Adapter) GlobalConfigSpecs() []adapters.GlobalConfigSpec {
	return []adapters.GlobalConfigSpec{
		{RelPath: ".copilot/skills", Kind: "skills"},
		{RelPath: ".copilot/mcp-config.json", Kind: "mcp", JSONMCPKey: "mcpServers"},
	}
}

func (a *Adapter) IsSessionArgs(userArgs []string) bool {
	for _, tok := range userArgs {
		if tok == "--" {
			break
		}
		if tok == "--help" || tok == "-h" || tok == "--version" || tok == "-v" {
			return false
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
			Message: "copilot (VS Code) is a single-instance GUI IDE and cannot isolate global settings via CLI launch; set targets_config.copilot.on_isolation_unsupported: warn to launch unisolated",
		})
		return plan, nil
	}

	plan.Mode = adapters.IsolationPartialWarn
	plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
		Level:   adapters.DiagWarning,
		Target:  a.Name(),
		Code:    "ISOLATION_PARTIAL_WARN",
		Message: "copilot cannot enforce runtime global isolation; launching unisolated due to on_isolation_unsupported: warn",
	})
	return plan, nil
}

func (a *Adapter) Compile(bundle *ir.Bundle, _ adapters.ProbeResults) (*adapters.CompileResult, error) {
	if bundle == nil {
		bundle = &ir.Bundle{}
	}
	res := &adapters.CompileResult{Target: a.Name()}
	tcfg := bundle.TargetsConfig[a.Name()]
	warnMode := tcfg.OnUnsupported == "warn"

	// 1. .vscode/mcp.json (servers + overrides)
	if len(bundle.MCP) > 0 || len(tcfg.Overrides) > 0 {
		mergeSpec := &adapters.JSONMergeSpec{
			MapEntries: make(map[string]any),
		}
		for _, srv := range bundle.MCP {
			entry := make(map[string]any)
			switch srv.Transport {
			case "stdio":
				entry["type"] = "stdio"
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
					envKeys := make([]string, 0, len(srv.Env))
					for k := range srv.Env {
						envKeys = append(envKeys, k)
					}
					sort.Strings(envKeys)
					for _, k := range envKeys {
						s, ed := a.renderEnv(srv.Env[k], warnMode)
						res.Diagnostics = append(res.Diagnostics, ed...)
						envMap[k] = s
					}
					entry["env"] = envMap
				}
			case "http", "sse":
				entry["type"] = srv.Transport
				urlStr, d := a.renderEnv(srv.URL, warnMode)
				res.Diagnostics = append(res.Diagnostics, d...)
				entry["url"] = urlStr
				if len(srv.Headers) > 0 {
					hdrs := make(map[string]string, len(srv.Headers))
					hdrKeys := make([]string, 0, len(srv.Headers))
					for k := range srv.Headers {
						hdrKeys = append(hdrKeys, k)
					}
					sort.Strings(hdrKeys)
					for _, k := range hdrKeys {
						s, hd := a.renderEnv(srv.Headers[k], warnMode)
						res.Diagnostics = append(res.Diagnostics, hd...)
						hdrs[k] = s
					}
					entry["headers"] = hdrs
				}
			}
			mergeSpec.MapEntries["/servers/"+escapeJSONPointer(srv.Name)] = entry
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
			RelPath:   ".vscode/mcp.json",
			Kind:      adapters.OpMergeJSONC,
			JSONMerge: mergeSpec,
		})
	}

	// 2. .agents/skills/<name> (shared)
	for _, sk := range bundle.Skills {
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:     a.Name(),
			RelPath:    ".agents/skills/" + sk.Name,
			Kind:       adapters.OpMaterializeTree,
			Shared:     true,
			SourcePath: sk.SourcePath,
			IsRemote:   sk.IsRemote,
		})
	}

	// 3. Rules: .github/copilot-instructions.md (default / copilot) or AGENTS.md (agents_md, Shared: true)
	switch tcfg.RulesMode {
	case "", "copilot":
		if len(bundle.Rules) > 0 {
			res.Files = append(res.Files, adapters.ArtifactOp{
				Target:  a.Name(),
				RelPath: ".github/copilot-instructions.md",
				Kind:    adapters.OpManagedBlock,
				Shared:  false,
				Content: adapters.FormatManagedRulesBlock(bundle.Rules),
			})
		}
	case "agents_md":
		if len(bundle.Rules) > 0 {
			res.Files = append(res.Files, adapters.ArtifactOp{
				Target:  a.Name(),
				RelPath: "AGENTS.md",
				Kind:    adapters.OpManagedBlock,
				Shared:  true,
				Content: adapters.FormatManagedRulesBlock(bundle.Rules),
			})
		}
	default:
		lvl := adapters.DiagError
		if warnMode {
			lvl = adapters.DiagWarning
		}
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   lvl,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_RULES_MODE",
			Message: fmt.Sprintf("targets_config.copilot.rules_mode %q is unsupported by copilot (expected \"copilot\" or \"agents_md\")", tcfg.RulesMode),
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
			Message: fmt.Sprintf("deny_commands %q is unsupported natively by copilot; set targets_config.copilot.on_unsupported: warn to omit with warning", dc.Pattern),
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
			Message: fmt.Sprintf("hook (%s: %s) is unsupported by copilot and was omitted", h.Event, h.Command.Raw),
		})
	}
	if len(bundle.AllowCommands) > 0 {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_ALLOW_COMMANDS",
			Message: "settings.permissions.allow_commands is unsupported by copilot and was omitted",
		})
	}
	for _, ag := range bundle.Agents {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_AGENT",
			Message: fmt.Sprintf("subagent %q is unsupported natively by copilot and was omitted", ag.Name),
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
			msg := fmt.Sprintf("env placeholder ${%s:-%s} with default value is unsupported in .vscode/mcp.json ${env:VAR} syntax", seg.Var.Name, seg.Var.Default)
			if warnMode {
				lvl = adapters.DiagWarning
				msg = fmt.Sprintf("env placeholder ${%s:-%s} default stripped to ${env:%s} for .vscode/mcp.json", seg.Var.Name, seg.Var.Default, seg.Var.Name)
			}
			diags = append(diags, adapters.Diagnostic{
				Level:   lvl,
				Target:  a.Name(),
				Code:    "UNSUPPORTED_ENV_DEFAULT",
				Message: msg,
			})
		}
		b.WriteString("${env:")
		b.WriteString(seg.Var.Name)
		b.WriteString("}")
	}
	return b.String(), diags
}

func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}
