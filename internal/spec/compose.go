package spec

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// LayerOrigin identifies the manifest file or Git commit that declared a layer.
type LayerOrigin struct {
	Source    string `json:"source"`
	Ref       string `json:"ref,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Subpath   string `json:"subpath,omitempty"`
	BaseDir   string `json:"base_dir,omitempty"`
	IsRemote  bool   `json:"is_remote"`
	IsRoot    bool   `json:"is_root,omitempty"`
}

// DisplayName formats a human-readable identifier for diagnostics.
func (o LayerOrigin) DisplayName() string {
	if o.IsRemote {
		s := o.Source
		if o.Ref != "" {
			s += "#" + o.Ref
		}
		if o.Subpath != "" {
			s += "//" + o.Subpath
		}
		return s
	}
	if o.Source != "" {
		return o.Source
	}
	return "Agentfile.yaml"
}

// ManifestLayer pairs a parsed single-file ProjectSpec with its LayerOrigin.
type ManifestLayer struct {
	Origin LayerOrigin
	Spec   *ProjectSpec
}

// HookOrigin pairs an effective hook entry with its declaring LayerOrigin.
type HookOrigin struct {
	Event   string
	Matcher string
	Command string
	Enforce bool
	Origin  LayerOrigin
}

// ComposedManifest holds the merged effective ProjectSpec and per-entry layer provenance.
type ComposedManifest struct {
	Effective   *ProjectSpec
	RawRoot     *ProjectSpec
	Layers      []ManifestLayer
	Origins     map[string]LayerOrigin
	Overridden  map[string][]LayerOrigin
	HookOrigins []HookOrigin
}

// Provenance states for add/remove/audit inspection.
const (
	ProvenanceNone              = "none"
	ProvenanceLocalOnly         = "local_only"
	ProvenanceInheritedOnly     = "inherited_only"
	ProvenanceOverriddenLocally = "overridden_locally"
)

// EntryProvenance classifies whether a skill, agent, rule, or mcp entry is
// defined locally, inherited only from extends, or overridden locally.
func (cm *ComposedManifest) EntryProvenance(kind, name string) (state string, inheritedFrom LayerOrigin) {
	if cm == nil {
		return ProvenanceNone, LayerOrigin{}
	}
	key := LockKey(kind, name)
	inRoot := hasEntryInSpec(cm.RawRoot, kind, name)

	var baseOrigin LayerOrigin
	inBase := false
	for _, layer := range cm.Layers {
		if layer.Origin.IsRoot {
			continue
		}
		if hasEntryInSpec(layer.Spec, kind, name) {
			inBase = true
			baseOrigin = layer.Origin
		}
	}

	switch {
	case inRoot && inBase:
		return ProvenanceOverriddenLocally, baseOrigin
	case !inRoot && inBase:
		if orig, ok := cm.Origins[key]; ok {
			baseOrigin = orig
		}
		return ProvenanceInheritedOnly, baseOrigin
	case inRoot && !inBase:
		return ProvenanceLocalOnly, LayerOrigin{}
	default:
		return ProvenanceNone, LayerOrigin{}
	}
}

func hasEntryInSpec(s *ProjectSpec, kind, name string) bool {
	if s == nil {
		return false
	}
	switch kind {
	case "skill":
		for _, sk := range s.Skills {
			if sk.Name == name {
				return true
			}
		}
	case "agent":
		for _, ag := range s.Agents {
			if ag.Name == name {
				return true
			}
		}
	case "rule":
		for _, rl := range s.Rules {
			if rl.Name == name {
				return true
			}
		}
	case "mcp":
		_, ok := s.MCP[name]
		return ok
	}
	return false
}

// ComposeLayers merges ordered manifest layers (bases first, root child last)
// with origin-anchored SourceSpec rebasing and monotonic security non-downgrade rules.
func ComposeLayers(layers []ManifestLayer, projectRoot string) (*ComposedManifest, error) {
	return composeLayersInternal(layers, projectRoot, false)
}

// ComposeLayersPartial merges layers when one or more remote bases are unhydrated
// offline, skipping the final non-empty targets check.
func ComposeLayersPartial(layers []ManifestLayer, projectRoot string) (*ComposedManifest, error) {
	return composeLayersInternal(layers, projectRoot, true)
}

func composeLayersInternal(layers []ManifestLayer, projectRoot string, allowEmptyTargets bool) (*ComposedManifest, error) {
	if len(layers) == 0 {
		return nil, NewPolicyError("no manifest layers to compose")
	}

	eff := &ProjectSpec{
		Version: 1,
	}
	origins := make(map[string]LayerOrigin)
	overridden := make(map[string][]LayerOrigin)
	var hookOrigins []HookOrigin

	skillIndex := make(map[string]int)
	agentIndex := make(map[string]int)
	ruleIndex := make(map[string]int)
	allowSet := make(map[string]bool)
	denySet := make(map[string]bool)

	var rawRoot *ProjectSpec

	for i, layer := range layers {
		if layer.Spec == nil {
			return nil, NewPolicyError("manifest layer %d (%s) is nil", i, layer.Origin.DisplayName())
		}
		isRoot := layer.Origin.IsRoot || i == len(layers)-1
		if err := ValidateRaw(layer.Spec, !isRoot || allowEmptyTargets); err != nil {
			return nil, err
		}
		if isRoot {
			rawRoot = layer.Spec
		}

		// 1. Targets: last non-empty list wins.
		if len(layer.Spec.Targets) > 0 {
			eff.Targets = append([]string(nil), layer.Spec.Targets...)
		}

		// 2. MCP: replace whole MCPServerSpec per key in deterministic order.
		if len(layer.Spec.MCP) > 0 {
			if eff.MCP == nil {
				eff.MCP = make(map[string]MCPServerSpec)
			}
			mcpNames := make([]string, 0, len(layer.Spec.MCP))
			for k := range layer.Spec.MCP {
				mcpNames = append(mcpNames, k)
			}
			sort.Strings(mcpNames)
			for _, name := range mcpNames {
				srv := layer.Spec.MCP[name]
				key := LockKey("mcp", name)
				if prev, ok := origins[key]; ok {
					overridden[key] = append(overridden[key], prev)
				}
				origins[key] = layer.Origin
				eff.MCP[name] = cloneMCPServerSpec(srv)
			}
		}

		// 3. Skills: upsert by name with origin-relative SourceSpec rebasing.
		for _, sk := range layer.Spec.Skills {
			rebased, err := RebaseSourceSpec(sk.Source, layer.Origin, projectRoot)
			if err != nil {
				return nil, err
			}
			item := AssetSpec{
				Name:   sk.Name,
				Source: rebased,
			}
			key := LockKey("skill", sk.Name)
			if idx, ok := skillIndex[sk.Name]; ok {
				overridden[key] = append(overridden[key], origins[key])
				eff.Skills[idx] = item
			} else {
				skillIndex[sk.Name] = len(eff.Skills)
				eff.Skills = append(eff.Skills, item)
			}
			origins[key] = layer.Origin
		}

		// 4. Agents: upsert by name with origin-relative SourceSpec rebasing.
		for _, ag := range layer.Spec.Agents {
			rebased, err := RebaseSourceSpec(ag.Source, layer.Origin, projectRoot)
			if err != nil {
				return nil, err
			}
			item := AgentSpec{
				Name:        ag.Name,
				Description: ag.Description,
				Model:       ag.Model,
				Source:      rebased,
			}
			key := LockKey("agent", ag.Name)
			if idx, ok := agentIndex[ag.Name]; ok {
				overridden[key] = append(overridden[key], origins[key])
				eff.Agents[idx] = item
			} else {
				agentIndex[ag.Name] = len(eff.Agents)
				eff.Agents = append(eff.Agents, item)
			}
			origins[key] = layer.Origin
		}

		// 5. Rules: upsert by name with origin-relative SourceSpec rebasing.
		for _, rl := range layer.Spec.Rules {
			rebased, err := RebaseSourceSpec(rl.Source, layer.Origin, projectRoot)
			if err != nil {
				return nil, err
			}
			item := RuleSpec{
				Name:        rl.Name,
				Source:      rebased,
				Description: rl.Description,
				Globs:       append([]string(nil), rl.Globs...),
				AlwaysApply: rl.AlwaysApply,
			}
			key := LockKey("rule", rl.Name)
			if idx, ok := ruleIndex[rl.Name]; ok {
				overridden[key] = append(overridden[key], origins[key])
				eff.Rules[idx] = item
			} else {
				ruleIndex[rl.Name] = len(eff.Rules)
				eff.Rules = append(eff.Rules, item)
			}
			origins[key] = layer.Origin
		}

		// 6. Permissions: deduped union for allow_commands; monotonic additive union for deny_commands.
		for _, ac := range layer.Spec.Settings.Permissions.AllowCommands {
			if !allowSet[ac] {
				allowSet[ac] = true
				eff.Settings.Permissions.AllowCommands = append(eff.Settings.Permissions.AllowCommands, ac)
			}
		}
		for _, dc := range layer.Spec.Settings.Permissions.DenyCommands {
			if !denySet[dc] {
				denySet[dc] = true
				eff.Settings.Permissions.DenyCommands = append(eff.Settings.Permissions.DenyCommands, dc)
			}
		}

		// 7. Hooks: monotonic additive union (enforced/PreToolUse hooks cannot be downgraded or removed).
		if len(layer.Spec.Settings.Hooks) > 0 {
			if eff.Settings.Hooks == nil {
				eff.Settings.Hooks = make(map[string][]HookSpec)
			}
			events := make([]string, 0, len(layer.Spec.Settings.Hooks))
			for ev := range layer.Spec.Settings.Hooks {
				events = append(events, ev)
			}
			sort.Strings(events)
			for _, ev := range events {
				for _, h := range layer.Spec.Settings.Hooks[ev] {
					existing := eff.Settings.Hooks[ev]
					foundIdx := -1
					for idx, cur := range existing {
						if cur.Matcher == h.Matcher && cur.Command.Raw == h.Command.Raw {
							foundIdx = idx
							break
						}
					}
					enforced := h.Enforce || ev == "PreToolUse"
					if foundIdx >= 0 {
						if h.Enforce {
							existing[foundIdx].Enforce = true
						}
						eff.Settings.Hooks[ev] = existing
					} else {
						eff.Settings.Hooks[ev] = append(existing, h)
						hookOrigins = append(hookOrigins, HookOrigin{
							Event:   ev,
							Matcher: h.Matcher,
							Command: h.Command.Raw,
							Enforce: enforced,
							Origin:  layer.Origin,
						})
					}
				}
			}
		}

		// 8. TargetsConfig: monotonic non-downgrade for load_global=false and on_*_unsupported=fail.
		if len(layer.Spec.TargetsConfig) > 0 {
			if eff.TargetsConfig == nil {
				eff.TargetsConfig = make(map[string]TargetConfigSpec)
			}
			targets := make([]string, 0, len(layer.Spec.TargetsConfig))
			for t := range layer.Spec.TargetsConfig {
				targets = append(targets, t)
			}
			sort.Strings(targets)
			for _, t := range targets {
				tc := layer.Spec.TargetsConfig[t]
				cur := eff.TargetsConfig[t]

				if cur.OnUnsupported == "fail" || tc.OnUnsupported == "fail" {
					cur.OnUnsupported = "fail"
				} else if tc.OnUnsupported != "" {
					cur.OnUnsupported = tc.OnUnsupported
				}

				if cur.OnIsolationUnsupported == "fail" || tc.OnIsolationUnsupported == "fail" {
					cur.OnIsolationUnsupported = "fail"
				} else if tc.OnIsolationUnsupported != "" {
					cur.OnIsolationUnsupported = tc.OnIsolationUnsupported
				}

				if cur.LoadGlobal != nil && !*cur.LoadGlobal {
					// Monotonic: once a base sets load_global: false, later layers cannot relax it to true.
				} else if tc.LoadGlobal != nil {
					v := *tc.LoadGlobal
					cur.LoadGlobal = &v
				}

				if tc.RulesMode != "" {
					cur.RulesMode = tc.RulesMode
				}
				if len(tc.Overrides) > 0 {
					if cur.Overrides == nil {
						cur.Overrides = make(map[string]any, len(tc.Overrides))
					}
					for ok, ov := range tc.Overrides {
						cur.Overrides[ok] = ov
					}
				}
				eff.TargetsConfig[t] = cur
				origins["target_config:"+t] = layer.Origin
			}
		}
	}

	if rawRoot != nil && len(rawRoot.Extends) > 0 {
		eff.Extends = append(ExtendsList(nil), rawRoot.Extends...)
	}

	if !allowEmptyTargets {
		if err := ValidateEffective(eff); err != nil {
			return nil, err
		}
	}

	return &ComposedManifest{
		Effective:   eff,
		RawRoot:     rawRoot,
		Layers:      append([]ManifestLayer(nil), layers...),
		Origins:     origins,
		Overridden:  overridden,
		HookOrigins: hookOrigins,
	}, nil
}

// RebaseSourceSpec rebases a relative ./ or ../ SourceSpec against its declaring LayerOrigin.
func RebaseSourceSpec(src SourceSpec, origin LayerOrigin, projectRoot string) (SourceSpec, error) {
	if src.IsStructured {
		return src, nil
	}
	raw := strings.TrimSpace(src.Raw)
	isRelLocal := strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") || raw == "." || raw == ".."
	isAbsLocal := strings.HasPrefix(raw, "/") || filepath.IsAbs(raw)

	if origin.IsRemote {
		if isAbsLocal {
			return SourceSpec{}, NewPolicyError("remote manifest %s cannot reference absolute local path %q", origin.DisplayName(), raw)
		}
		if isRelLocal {
			baseDir := ""
			if origin.Subpath != "" {
				baseDir = path.Dir(strings.TrimPrefix(path.Clean(origin.Subpath), "/"))
				if baseDir == "." {
					baseDir = ""
				}
			}
			joined := path.Clean(path.Join(baseDir, filepath.ToSlash(raw)))
			if joined == ".." || strings.HasPrefix(joined, "../") || path.IsAbs(joined) {
				return SourceSpec{}, NewPolicyError("remote manifest %s relative path %q escapes repository root", origin.DisplayName(), raw)
			}
			if joined == "." {
				joined = ""
			}
			ref := origin.CommitSHA
			if ref == "" {
				ref = origin.Ref
			}
			return SourceSpec{
				IsStructured: true,
				Git:          origin.Source,
				Ref:          ref,
				Path:         joined,
			}, nil
		}
		return src, nil
	}

	if isRelLocal && origin.BaseDir != "" {
		absTarget := filepath.Clean(filepath.Join(origin.BaseDir, filepath.FromSlash(raw)))
		if projectRoot != "" {
			if rel, err := filepath.Rel(projectRoot, absTarget); err == nil {
				slashRel := filepath.ToSlash(rel)
				if slashRel != "." && slashRel != ".." && !strings.HasPrefix(slashRel, "../") && !strings.HasPrefix(slashRel, "/") {
					slashRel = "./" + slashRel
				}
				return SourceSpec{Raw: slashRel}, nil
			}
		}
		return SourceSpec{Raw: filepath.ToSlash(absTarget)}, nil
	}

	return src, nil
}

func cloneMCPServerSpec(in MCPServerSpec) MCPServerSpec {
	out := in
	out.Args = append([]EnvString(nil), in.Args...)
	if in.Env != nil {
		out.Env = make(map[string]EnvString, len(in.Env))
		for k, v := range in.Env {
			out.Env[k] = v
		}
	}
	if in.Headers != nil {
		out.Headers = make(map[string]EnvString, len(in.Headers))
		for k, v := range in.Headers {
			out.Headers[k] = v
		}
	}
	return out
}
