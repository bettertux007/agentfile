package cursor

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
	"gopkg.in/yaml.v3"
)

// Adapter compiles IR bundles for Cursor.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string       { return "cursor" }
func (a *Adapter) BinaryName() string { return "cursor" }

func (a *Adapter) SchemaContract() adapters.SchemaContract {
	return adapters.SchemaContract{
		MinVersion:     "0.40.0",
		MaxTested:      "1.x",
		BinaryOptional: true,
	}
}

func (a *Adapter) RequiredProbes() []adapters.ProbeRequest {
	return nil
}

func (a *Adapter) GlobalConfigSpecs() []adapters.GlobalConfigSpec {
	return []adapters.GlobalConfigSpec{
		{RelPath: ".cursor/mcp.json", Kind: "mcp", JSONMCPKey: "mcpServers"},
		{RelPath: ".cursor/rules", Kind: "rules"},
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
			Message: "cursor is a single-instance GUI IDE and cannot isolate global settings via CLI launch; set targets_config.cursor.on_isolation_unsupported: warn to launch unisolated",
		})
		return plan, nil
	}

	plan.Mode = adapters.IsolationPartialWarn
	plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
		Level:   adapters.DiagWarning,
		Target:  a.Name(),
		Code:    "ISOLATION_PARTIAL_WARN",
		Message: "cursor cannot enforce runtime global isolation; launching unisolated due to on_isolation_unsupported: warn",
	})
	return plan, nil
}

func (a *Adapter) Compile(bundle *ir.Bundle, _ adapters.ProbeResults) (*adapters.CompileResult, error) {
	res := &adapters.CompileResult{Target: a.Name()}
	tcfg := bundle.TargetsConfig[a.Name()]
	warnMode := tcfg.OnUnsupported == "warn"

	// 1. .cursor/mcp.json
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
				entry["url"] = urlStr
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
			RelPath:   ".cursor/mcp.json",
			Kind:      adapters.OpMergeJSONC,
			JSONMerge: mergeSpec,
		})
	}

	// 2. .cursor/rules/agentfile-<name>.mdc
	for _, rl := range bundle.Rules {
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:  a.Name(),
			RelPath: ".cursor/rules/agentfile-" + rl.Name + ".mdc",
			Kind:    adapters.OpWriteDropIn,
			Content: formatCursorMDC(rl.Description, rl.Globs, rl.AlwaysApply, rl.Content),
		})
	}

	// 3. .cursor/rules/agentfile-skill-<name>.mdc
	for _, sk := range bundle.Skills {
		body := sk.SkillMD
		if len(bytes.TrimSpace(body)) == 0 {
			body = []byte(fmt.Sprintf("Skill `%s` materialized from `%s`.\n", sk.Name, sk.SourcePath))
		}
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:  a.Name(),
			RelPath: ".cursor/rules/agentfile-skill-" + sk.Name + ".mdc",
			Kind:    adapters.OpWriteDropIn,
			Content: formatCursorMDC("Skill: "+sk.Name, nil, false, body),
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
			Message: fmt.Sprintf("deny_commands %q is unsupported natively by cursor; set targets_config.cursor.on_unsupported: warn to omit with warning", dc.Pattern),
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
			Message: fmt.Sprintf("hook (%s: %s) is unsupported by cursor and was omitted", h.Event, h.Command.Raw),
		})
	}
	if len(bundle.AllowCommands) > 0 {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_ALLOW_COMMANDS",
			Message: "settings.permissions.allow_commands is unsupported by cursor and was omitted",
		})
	}
	for _, ag := range bundle.Agents {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_AGENT",
			Message: fmt.Sprintf("subagent %q is unsupported natively by cursor and was omitted", ag.Name),
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
			msg := fmt.Sprintf("env placeholder ${%s:-%s} with default value is unsupported in .cursor/mcp.json ${env:VAR} syntax", seg.Var.Name, seg.Var.Default)
			if warnMode {
				lvl = adapters.DiagWarning
				msg = fmt.Sprintf("env placeholder ${%s:-%s} default stripped to ${env:%s} for .cursor/mcp.json", seg.Var.Name, seg.Var.Default, seg.Var.Name)
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

func formatCursorMDC(description string, globs []string, alwaysApply bool, content []byte) []byte {
	type mdcFrontmatter struct {
		Description string `yaml:"description,omitempty"`
		Globs       string `yaml:"globs,omitempty"`
		AlwaysApply bool   `yaml:"alwaysApply"`
	}
	fm := mdcFrontmatter{
		Description: description,
		Globs:       strings.Join(globs, ", "),
		AlwaysApply: alwaysApply,
	}
	fmBytes, _ := yaml.Marshal(fm)

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(fmBytes)
	buf.WriteString("---\n\n")
	buf.Write(content)
	if len(content) > 0 && content[len(content)-1] != '\n' {
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}
