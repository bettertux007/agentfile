package ir

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
)

// Build lowers a validated ProjectSpec and resolved dependencies into a normalized IR Bundle.
func Build(s *spec.ProjectSpec, deps map[string]store.ResolvedAsset) (*Bundle, error) {
	if s == nil {
		return nil, spec.NewPolicyError("nil ProjectSpec")
	}

	b := &Bundle{
		Targets:       append([]string(nil), s.Targets...),
		AllowCommands: append([]string(nil), s.Settings.Permissions.AllowCommands...),
		TargetsConfig: s.TargetsConfig,
	}

	// Deterministic sorted MCP list.
	mcpNames := make([]string, 0, len(s.MCP))
	for k := range s.MCP {
		mcpNames = append(mcpNames, k)
	}
	sort.Strings(mcpNames)
	for _, name := range mcpNames {
		srv := s.MCP[name]
		b.MCP = append(b.MCP, IRMCPServer{
			Name:      name,
			Transport: srv.Transport,
			Command:   srv.Command,
			Args:      srv.Args,
			Env:       srv.Env,
			URL:       srv.URL,
			Headers:   srv.Headers,
		})
	}

	for _, sk := range s.Skills {
		key := spec.LockKey("skill", sk.Name)
		dep, ok := deps[key]
		if !ok {
			return nil, fmt.Errorf("missing resolved dependency for %s", key)
		}
		info, err := os.Stat(dep.SourcePath)
		if err != nil || !info.IsDir() {
			return nil, spec.NewPolicyError("skill %q source %s must be a directory", sk.Name, dep.SourcePath)
		}
		skillMD, _ := os.ReadFile(filepath.Join(dep.SourcePath, "SKILL.md"))
		b.Skills = append(b.Skills, IRAsset{
			Name:       sk.Name,
			SourcePath: dep.SourcePath,
			IsRemote:   dep.IsRemote,
			SkillMD:    skillMD,
		})
	}

	for _, ag := range s.Agents {
		key := spec.LockKey("agent", ag.Name)
		dep, ok := deps[key]
		if !ok {
			return nil, fmt.Errorf("missing resolved dependency for %s", key)
		}
		content, err := readSingleMarkdown(dep.SourcePath, "agent", ag.Name)
		if err != nil {
			return nil, err
		}
		b.Agents = append(b.Agents, IRAgent{
			Name:        ag.Name,
			Description: ag.Description,
			Model:       ag.Model,
			SourcePath:  dep.SourcePath,
			IsRemote:    dep.IsRemote,
			Content:     content,
		})
	}

	for _, rl := range s.Rules {
		key := spec.LockKey("rule", rl.Name)
		dep, ok := deps[key]
		if !ok {
			return nil, fmt.Errorf("missing resolved dependency for %s", key)
		}
		content, err := readSingleMarkdown(dep.SourcePath, "rule", rl.Name)
		if err != nil {
			return nil, err
		}
		b.Rules = append(b.Rules, IRRule{
			Name:        rl.Name,
			Description: rl.Description,
			Globs:       append([]string(nil), rl.Globs...),
			AlwaysApply: rl.AlwaysApply,
			SourcePath:  dep.SourcePath,
			IsRemote:    dep.IsRemote,
			Content:     content,
		})
	}

	for _, pat := range s.Settings.Permissions.DenyCommands {
		b.DenyCommands = append(b.DenyCommands, SecurityControl{
			Kind:    "deny_command",
			Pattern: pat,
		})
	}

	hookEvents := make([]string, 0, len(s.Settings.Hooks))
	for ev := range s.Settings.Hooks {
		hookEvents = append(hookEvents, ev)
	}
	sort.Strings(hookEvents)
	for _, ev := range hookEvents {
		for _, h := range s.Settings.Hooks[ev] {
			b.Hooks = append(b.Hooks, IRHook{
				Event:    ev,
				Matcher:  h.Matcher,
				Command:  h.Command,
				Enforced: ev == "PreToolUse" || h.Enforce,
			})
		}
	}

	return b, nil
}

// BuildForRun builds a lightweight Bundle from ProjectSpec without reading asset trees on disk.
// Used by `agentfile run` under --sync=never or --sync=check where full hydration may not have run.
func BuildForRun(s *spec.ProjectSpec) *Bundle {
	if s == nil {
		return &Bundle{}
	}
	b := &Bundle{
		Targets:       append([]string(nil), s.Targets...),
		AllowCommands: append([]string(nil), s.Settings.Permissions.AllowCommands...),
		TargetsConfig: s.TargetsConfig,
	}

	mcpNames := make([]string, 0, len(s.MCP))
	for k := range s.MCP {
		mcpNames = append(mcpNames, k)
	}
	sort.Strings(mcpNames)
	for _, name := range mcpNames {
		srv := s.MCP[name]
		b.MCP = append(b.MCP, IRMCPServer{
			Name:      name,
			Transport: srv.Transport,
			Command:   srv.Command,
			Args:      srv.Args,
			Env:       srv.Env,
			URL:       srv.URL,
			Headers:   srv.Headers,
		})
	}
	for _, pat := range s.Settings.Permissions.DenyCommands {
		b.DenyCommands = append(b.DenyCommands, SecurityControl{
			Kind:    "deny_command",
			Pattern: pat,
		})
	}
	hookEvents := make([]string, 0, len(s.Settings.Hooks))
	for ev := range s.Settings.Hooks {
		hookEvents = append(hookEvents, ev)
	}
	sort.Strings(hookEvents)
	for _, ev := range hookEvents {
		for _, h := range s.Settings.Hooks[ev] {
			b.Hooks = append(b.Hooks, IRHook{
				Event:    ev,
				Matcher:  h.Matcher,
				Command:  h.Command,
				Enforced: ev == "PreToolUse" || h.Enforce,
			})
		}
	}
	return b
}

func readSingleMarkdown(path, kind, name string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, spec.NewPolicyError("%s %q source %s not found: %v", kind, name, path, err)
	}
	if info.IsDir() {
		return nil, spec.NewPolicyError("%s %q source %s must be a file, not a directory", kind, name, path)
	}
	return os.ReadFile(path)
}
