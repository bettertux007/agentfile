package windsurf

import (
	"bytes"
	"fmt"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/ir"
	"gopkg.in/yaml.v3"
)

// Adapter compiles IR bundles for Windsurf.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string       { return "windsurf" }
func (a *Adapter) BinaryName() string { return "windsurf" }

func (a *Adapter) SchemaContract() adapters.SchemaContract {
	return adapters.SchemaContract{
		MinVersion:     "1.0.0",
		MaxTested:      "1.x",
		BinaryOptional: true,
	}
}

func (a *Adapter) RequiredProbes() []adapters.ProbeRequest {
	return nil
}

func (a *Adapter) GlobalConfigSpecs() []adapters.GlobalConfigSpec {
	return []adapters.GlobalConfigSpec{
		{RelPath: ".codeium/windsurf/mcp_config.json", Kind: "mcp", JSONMCPKey: "mcpServers"},
		{RelPath: ".codeium/windsurf/memories/global_rules.md", Kind: "rules"},
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
			Message: "windsurf is a single-instance GUI IDE and cannot isolate global settings via CLI launch; set targets_config.windsurf.on_isolation_unsupported: warn to launch unisolated",
		})
		return plan, nil
	}

	plan.Mode = adapters.IsolationPartialWarn
	plan.Diagnostics = append(plan.Diagnostics, adapters.Diagnostic{
		Level:   adapters.DiagWarning,
		Target:  a.Name(),
		Code:    "ISOLATION_PARTIAL_WARN",
		Message: "windsurf cannot enforce runtime global isolation; launching unisolated due to on_isolation_unsupported: warn",
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

	// 1. Rules: .windsurf/rules/agentfile-<name>.md (dropin, default) or AGENTS.md (agents_md, Shared: true)
	switch tcfg.RulesMode {
	case "", "dropin":
		for _, rl := range bundle.Rules {
			res.Files = append(res.Files, adapters.ArtifactOp{
				Target:  a.Name(),
				RelPath: ".windsurf/rules/agentfile-" + rl.Name + ".md",
				Kind:    adapters.OpWriteDropIn,
				Shared:  false,
				Content: formatWindsurfRule(rl),
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
			Message: fmt.Sprintf("targets_config.windsurf.rules_mode %q is unsupported by windsurf (expected \"dropin\" or \"agents_md\")", tcfg.RulesMode),
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

	// 3. MCP: Windsurf reads MCP user-globally (~/.codeium/windsurf/mcp_config.json); never mutate globals.
	if len(bundle.MCP) > 0 {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "PROJECT_MCP_UNSUPPORTED",
			Message: "windsurf has no project-scoped MCP configuration file (reads ~/.codeium/windsurf/mcp_config.json globally); project MCP servers were omitted",
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
			Message: fmt.Sprintf("deny_commands %q is unsupported natively by windsurf; set targets_config.windsurf.on_unsupported: warn to omit with warning", dc.Pattern),
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
			Message: fmt.Sprintf("hook (%s: %s) is unsupported by windsurf and was omitted", h.Event, h.Command.Raw),
		})
	}
	if len(bundle.AllowCommands) > 0 {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_ALLOW_COMMANDS",
			Message: "settings.permissions.allow_commands is unsupported by windsurf and was omitted",
		})
	}
	for _, ag := range bundle.Agents {
		res.Diagnostics = append(res.Diagnostics, adapters.Diagnostic{
			Level:   adapters.DiagWarning,
			Target:  a.Name(),
			Code:    "UNSUPPORTED_AGENT",
			Message: fmt.Sprintf("subagent %q is unsupported natively by windsurf and was omitted", ag.Name),
		})
	}

	return res, nil
}

func (a *Adapter) CheckPolicy(_ *ir.Bundle, _ map[string][]byte) []adapters.Diagnostic {
	return nil
}

func formatWindsurfRule(rl ir.IRRule) []byte {
	type windsurfFrontmatter struct {
		Trigger     string   `yaml:"trigger"`
		Description string   `yaml:"description,omitempty"`
		Globs       []string `yaml:"globs,omitempty"`
	}
	fm := windsurfFrontmatter{
		Description: rl.Description,
	}
	if rl.AlwaysApply || len(rl.Globs) == 0 {
		fm.Trigger = "always_on"
	} else {
		fm.Trigger = "glob"
		fm.Globs = rl.Globs
	}
	fmBytes, _ := yaml.Marshal(fm)

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(fmBytes)
	buf.WriteString("---\n\n")
	buf.Write(rl.Content)
	if len(rl.Content) > 0 && rl.Content[len(rl.Content)-1] != '\n' {
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}
