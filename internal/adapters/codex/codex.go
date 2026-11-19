package codex

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/pelletier/go-toml/v2"
)

// Adapter compiles IR bundles for OpenAI Codex CLI.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string       { return "codex" }
func (a *Adapter) BinaryName() string { return "codex" }

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
		{RelPath: ".codex/config.toml", Kind: "mcp"},
		{RelPath: ".codex/AGENTS.md", Kind: "rules"},
		{RelPath: ".agents/skills", Kind: "skills"},
	}
}

func (a *Adapter) IsSessionArgs(userArgs []string) bool {
	nonSessionSubcmds := map[string]bool{
		"login": true, "logout": true, "mcp": true, "mcp-server": true,
		"app-server": true, "completion": true, "features": true,
		"debug": true, "sandbox": true, "apply": true,
	}
	valueFlags := map[string]bool{
		"-m": true, "--model": true, "-c": true, "--config": true,
		"-p": true, "--profile": true, "-s": true, "--sandbox": true,
		"-a": true, "--ask-for-approval": true, "-C": true, "--cd": true,
		"-i": true, "--image": true,
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
		if tok == "--help" || tok == "-h" || tok == "--version" || tok == "-v" || tok == "-V" {
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
			Message: "codex cannot isolate global ~/.codex/config.toml, ~/.codex/AGENTS.md, or ~/.agents/skills without overriding CODEX_HOME/HOME and breaking auth credentials; set targets_config.codex.on_isolation_unsupported: warn to allow unisolated launch",
		})
		return plan, nil
	}

	plan.Mode = adapters.IsolationPartialWarn
	plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
		Level:   adapters.DiagWarning,
		Target:  a.Name(),
		Code:    "ISOLATION_PARTIAL_WARN",
		Message: "codex cannot enforce runtime global isolation; launching unisolated due to on_isolation_unsupported: warn",
	})
	return plan, nil
}

func (a *Adapter) Compile(bundle *ir.Bundle, _ adapters.ProbeResults) (*adapters.CompileResult, error) {
	res := &adapters.CompileResult{Target: a.Name()}
	tcfg := bundle.TargetsConfig[a.Name()]
	warnMode := tcfg.OnUnsupported == "warn"

	if tcfg.RulesMode != "" && tcfg.RulesMode != "agents_md" {
		lvl := adapters.DiagError
		if warnMode {
			lvl = adapters.DiagWarning
		}
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   lvl,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_RULES_MODE",
			Message: fmt.Sprintf("targets_config.codex.rules_mode %q is unsupported by codex (expected \"agents_md\")", tcfg.RulesMode),
		})
	}

	// 1. .codex/config.toml (mcp_servers + overrides)
	if len(bundle.MCP) > 0 || len(tcfg.Overrides) > 0 {
		tableEntries := make(map[string]any)
		for _, srv := range bundle.MCP {
			if srv.Transport == "sse" {
				lvl := adapters.DiagError
				if warnMode {
					lvl = adapters.DiagWarning
				}
				res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
					Level:   lvl,
					Target:  a.Name(),
					Code:    "UNSUPPORTED_MCP_TRANSPORT",
					Message: fmt.Sprintf("mcp.%s uses sse transport which is unsupported by codex .codex/config.toml; set targets_config.codex.on_unsupported: warn to omit", srv.Name),
				})
				continue
			}

			entry := make(map[string]any)
			switch srv.Transport {
			case "stdio":
				cmdStr, cmdDiags := a.renderLiteralOnly(srv.Command, fmt.Sprintf("mcp.%s.command", srv.Name), warnMode)
				res.Diagnostics = append(res.Diagnostics, cmdDiags...)
				entry["command"] = cmdStr

				if len(srv.Args) > 0 {
					args := make([]string, 0, len(srv.Args))
					for i, arg := range srv.Args {
						s, ad := a.renderLiteralOnly(arg, fmt.Sprintf("mcp.%s.args[%d]", srv.Name, i), warnMode)
						res.Diagnostics = append(res.Diagnostics, ad...)
						args = append(args, s)
					}
					entry["args"] = args
				}

				if len(srv.Env) > 0 {
					envMap, envVars, envDiags := a.compileStdioEnv(srv.Name, srv.Env, warnMode)
					res.Diagnostics = append(res.Diagnostics, envDiags...)
					if len(envMap) > 0 {
						entry["env"] = envMap
					}
					if len(envVars) > 0 {
						entry["env_vars"] = envVars
					}
				}

			case "http":
				urlStr, urlDiags := a.renderLiteralOnly(srv.URL, fmt.Sprintf("mcp.%s.url", srv.Name), warnMode)
				res.Diagnostics = append(res.Diagnostics, urlDiags...)
				entry["url"] = urlStr

				if len(srv.Headers) > 0 {
					bearerVar, envHdrs, staticHdrs, hdrDiags := a.compileHTTPHeaders(srv.Name, srv.Headers, warnMode)
					res.Diagnostics = append(res.Diagnostics, hdrDiags...)
					if bearerVar != "" {
						entry["bearer_token_env_var"] = bearerVar
					}
					if len(envHdrs) > 0 {
						entry["env_http_headers"] = envHdrs
					}
					if len(staticHdrs) > 0 {
						entry["http_headers"] = staticHdrs
					}
				}
			}

			// Note: engine/toml.go splits TableEntries keys on '.'; standard slugs without '.' map cleanly here.
			tableEntries["mcp_servers."+srv.Name] = entry
		}

		if len(tcfg.Overrides) > 0 {
			keys := make([]string, 0, len(tcfg.Overrides))
			for k := range tcfg.Overrides {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				tableEntries[k] = tcfg.Overrides[k]
			}
		}

		if len(tableEntries) > 0 {
			res.Files = append(res.Files, adapters.ArtifactOp{
				Target:  a.Name(),
				RelPath: ".codex/config.toml",
				Kind:    adapters.OpMergeTOML,
				TOMLMerge: &adapters.TOMLMergeSpec{
					TableEntries: tableEntries,
				},
			})
		}
	}

	// 2. .agents/skills/<name> (shared with copilot)
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

	// 3. AGENTS.md managed block (shared with copilot)
	if len(bundle.Rules) > 0 {
		res.Files = append(res.Files, adapters.ArtifactOp{
			Target:  a.Name(),
			RelPath: "AGENTS.md",
			Kind:    adapters.OpManagedBlock,
			Shared:  true,
			Content: adapters.FormatManagedRulesBlock(bundle.Rules),
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
			Message: fmt.Sprintf("deny_commands %q is unsupported natively in .codex/config.toml; set targets_config.codex.on_unsupported: warn to omit with warning", dc.Pattern),
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
			Message: fmt.Sprintf("hook (%s: %s) is unsupported by codex and was omitted", h.Event, h.Command.Raw),
		})
	}
	if len(bundle.AllowCommands) > 0 {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_ALLOW_COMMANDS",
			Message: "settings.permissions.allow_commands is unsupported by codex and was omitted",
		})
	}
	for _, ag := range bundle.Agents {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_AGENT",
			Message: fmt.Sprintf("subagent %q is unsupported natively by codex and was omitted", ag.Name),
		})
	}

	return res, nil
}

func (a *Adapter) CheckPolicy(bundle *ir.Bundle, mergedFiles map[string][]byte) []adapters.Diagnostic {
	if len(bundle.DenyCommands) == 0 {
		return nil
	}
	raw, ok := mergedFiles[".codex/config.toml"]
	if !ok {
		return nil
	}
	var doc struct {
		ApprovalPolicy string `toml:"approval_policy"`
		SandboxMode    string `toml:"sandbox_mode"`
	}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return []adapters.Diagnostic{{
			Level:   adapters.DiagError,
			Target:  a.Name(),
			Code:    "INVALID_TOML",
			Message: fmt.Sprintf(".codex/config.toml invalid after merge: %v", err),
		}}
	}

	var diags []adapters.Diagnostic
	if doc.ApprovalPolicy == "never" {
		diags = append(diags, adapters.Diagnostic{
			Level:   adapters.DiagError,
			Target:  a.Name(),
			Code:    "POLICY_CONTRADICTION",
			Message: ".codex/config.toml sets approval_policy = \"never\", contradicting declared deny_commands",
		})
	}
	if doc.SandboxMode == "danger-full-access" {
		diags = append(diags, adapters.Diagnostic{
			Level:   adapters.DiagError,
			Target:  a.Name(),
			Code:    "POLICY_CONTRADICTION",
			Message: ".codex/config.toml sets sandbox_mode = \"danger-full-access\", contradicting declared deny_commands",
		})
	}
	return diags
}

func (a *Adapter) renderLiteralOnly(es spec.EnvString, field string, warnMode bool) (string, []adapters.Diagnostic) {
	if lit, ok := extractLiteral(es); ok {
		return lit, nil
	}
	lvl := adapters.DiagError
	if warnMode {
		lvl = adapters.DiagWarning
	}
	return es.Raw, []adapters.Diagnostic{{
		Level:   lvl,
		Target:  a.Name(),
		Code:    "UNSUPPORTED_ENV_INTERPOLATION",
		Message: fmt.Sprintf("%s %q uses ${VAR} interpolation which is unsupported in codex .codex/config.toml", field, es.Raw),
	}}
}

func (a *Adapter) compileStdioEnv(srvName string, env map[string]spec.EnvString, warnMode bool) (map[string]string, []string, []adapters.Diagnostic) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	staticEnv := make(map[string]string)
	envVarsSet := make(map[string]bool)
	var diags []adapters.Diagnostic

	for _, k := range keys {
		es := env[k]
		if lit, ok := extractLiteral(es); ok {
			staticEnv[k] = lit
			continue
		}
		if ref, ok := extractPureEnvRef(es); ok {
			if ref.HasDefault {
				lvl := adapters.DiagError
				msg := fmt.Sprintf("mcp.%s.env.%s placeholder ${%s:-%s} with default value is unsupported in codex env_vars", srvName, k, ref.Name, ref.Default)
				if warnMode {
					lvl = adapters.DiagWarning
					msg = fmt.Sprintf("mcp.%s.env.%s placeholder ${%s:-%s} default dropped for codex env_vars", srvName, k, ref.Name, ref.Default)
				}
				diags = append(diags, adapters.Diagnostic{
					Level:   lvl,
					Target:  a.Name(),
					Code:    "UNSUPPORTED_ENV_DEFAULT",
					Message: msg,
				})
			}
			envVarsSet[ref.Name] = true
			continue
		}
		lvl := adapters.DiagError
		if warnMode {
			lvl = adapters.DiagWarning
		}
		diags = append(diags, adapters.Diagnostic{
			Level:   lvl,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_ENV_INTERPOLATION",
			Message: fmt.Sprintf("mcp.%s.env.%s %q uses mixed env interpolation which is unsupported in codex .codex/config.toml and was omitted", srvName, k, es.Raw),
		})
	}

	var envVars []string
	if len(envVarsSet) > 0 {
		envVars = make([]string, 0, len(envVarsSet))
		for v := range envVarsSet {
			envVars = append(envVars, v)
		}
		sort.Strings(envVars)
	}
	return staticEnv, envVars, diags
}

func (a *Adapter) compileHTTPHeaders(srvName string, headers map[string]spec.EnvString, warnMode bool) (string, map[string]string, map[string]string, []adapters.Diagnostic) {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var bearerVar string
	envHeaders := make(map[string]string)
	staticHeaders := make(map[string]string)
	var diags []adapters.Diagnostic

	for _, k := range keys {
		es := headers[k]
		if lit, ok := extractLiteral(es); ok {
			staticHeaders[k] = lit
			continue
		}
		if strings.EqualFold(k, "Authorization") {
			if ref, ok := extractBearerEnvRef(es); ok {
				if ref.HasDefault {
					lvl := adapters.DiagError
					msg := fmt.Sprintf("mcp.%s.headers.%s placeholder ${%s:-%s} with default value is unsupported in codex bearer_token_env_var", srvName, k, ref.Name, ref.Default)
					if warnMode {
						lvl = adapters.DiagWarning
						msg = fmt.Sprintf("mcp.%s.headers.%s placeholder ${%s:-%s} default dropped for codex bearer_token_env_var", srvName, k, ref.Name, ref.Default)
					}
					diags = append(diags, adapters.Diagnostic{
						Level:   lvl,
						Target:  a.Name(),
						Code:    "UNSUPPORTED_ENV_DEFAULT",
						Message: msg,
					})
				}
				bearerVar = ref.Name
				continue
			}
		}
		if ref, ok := extractPureEnvRef(es); ok {
			if ref.HasDefault {
				lvl := adapters.DiagError
				msg := fmt.Sprintf("mcp.%s.headers.%s placeholder ${%s:-%s} with default value is unsupported in codex env_http_headers", srvName, k, ref.Name, ref.Default)
				if warnMode {
					lvl = adapters.DiagWarning
					msg = fmt.Sprintf("mcp.%s.headers.%s placeholder ${%s:-%s} default dropped for codex env_http_headers", srvName, k, ref.Name, ref.Default)
				}
				diags = append(diags, adapters.Diagnostic{
					Level:   lvl,
					Target:  a.Name(),
					Code:    "UNSUPPORTED_ENV_DEFAULT",
					Message: msg,
				})
			}
			envHeaders[k] = ref.Name
			continue
		}
		lvl := adapters.DiagError
		if warnMode {
			lvl = adapters.DiagWarning
		}
		diags = append(diags, adapters.Diagnostic{
			Level:   lvl,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_ENV_INTERPOLATION",
			Message: fmt.Sprintf("mcp.%s.headers.%s %q uses mixed env interpolation which is unsupported in codex .codex/config.toml and was omitted", srvName, k, es.Raw),
		})
	}

	return bearerVar, envHeaders, staticHeaders, diags
}

func extractLiteral(es spec.EnvString) (string, bool) {
	if len(es.Segments) == 0 {
		return es.Raw, true
	}
	var b strings.Builder
	for _, seg := range es.Segments {
		if seg.Var != nil {
			return "", false
		}
		b.WriteString(seg.Literal)
	}
	return b.String(), true
}

func extractPureEnvRef(es spec.EnvString) (*spec.EnvRef, bool) {
	if len(es.Segments) == 1 && es.Segments[0].Var != nil {
		return es.Segments[0].Var, true
	}
	return nil, false
}

func extractBearerEnvRef(es spec.EnvString) (*spec.EnvRef, bool) {
	if len(es.Segments) == 2 && es.Segments[0].Var == nil && es.Segments[0].Literal == "Bearer " && es.Segments[1].Var != nil {
		return es.Segments[1].Var, true
	}
	return nil, false
}
