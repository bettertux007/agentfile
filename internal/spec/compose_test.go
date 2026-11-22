package spec

import (
	"strings"
	"testing"
)

func TestComposeLayersMonotonicSecurityAndRebasing(t *testing.T) {
	t.Parallel()

	fVal := false
	tVal := true
	sha := strings.Repeat("c", 40)

	remoteBase := ManifestLayer{
		Origin: LayerOrigin{
			Source:    "https://github.com/org/base.git",
			Ref:       "v1.0.0",
			CommitSHA: sha,
			Subpath:   "presets/Agentfile.yaml",
			IsRemote:  true,
		},
		Spec: &ProjectSpec{
			Version: 1,
			Targets: []string{"claude", "opencode"},
			MCP: map[string]MCPServerSpec{
				"shared-mcp": {
					Transport: "http",
					URL:       EnvString{Raw: "https://mcp.example.com"},
				},
			},
			Skills: []AssetSpec{
				{Name: "org-sec", Source: SourceSpec{Raw: "./skills/org-sec"}},
				{Name: "overridden-sk", Source: SourceSpec{Raw: "./skills/old"}},
			},
			Rules: []RuleSpec{
				{Name: "org-rule", Source: SourceSpec{Raw: "./rules/org.md"}},
			},
			Settings: SettingsSpec{
				Permissions: PermissionsSpec{
					AllowCommands: []string{"go vet ./..."},
					DenyCommands:  []string{"rm -rf *"},
				},
				Hooks: map[string][]HookSpec{
					"PreToolUse": {
						{Matcher: "Bash", Command: EnvString{Raw: "./scripts/org-guard.sh"}, Enforce: true},
					},
				},
			},
			TargetsConfig: map[string]TargetConfigSpec{
				"claude": {
					LoadGlobal:             &fVal,
					OnUnsupported:          "fail",
					OnIsolationUnsupported: "fail",
				},
			},
		},
	}

	localBase := ManifestLayer{
		Origin: LayerOrigin{
			Source:  "./team/Agentfile.yaml",
			BaseDir: "/repo/team",
		},
		Spec: &ProjectSpec{
			Version: 1,
			Agents: []AgentSpec{
				{Name: "team-agent", Source: SourceSpec{Raw: "./agents/team.md"}},
			},
		},
	}

	child := ManifestLayer{
		Origin: LayerOrigin{
			Source:  "Agentfile.yaml",
			BaseDir: "/repo",
			IsRoot:  true,
		},
		Spec: &ProjectSpec{
			Version: 1,
			Extends: ExtendsList{{Raw: "https://github.com/org/base.git#v1.0.0//presets/Agentfile.yaml"}, {Raw: "./team/Agentfile.yaml"}},
			Targets: []string{"claude"},
			MCP: map[string]MCPServerSpec{
				"shared-mcp": {
					Transport: "stdio",
					Command:   EnvString{Raw: "npx"},
				},
			},
			Skills: []AssetSpec{
				{Name: "overridden-sk", Source: SourceSpec{Raw: "./skills/new"}},
				{Name: "local-sk", Source: SourceSpec{Raw: "./skills/local"}},
			},
			Settings: SettingsSpec{
				Permissions: PermissionsSpec{
					AllowCommands: []string{"go test ./..."},
					DenyCommands:  []string{"sudo *"},
				},
				Hooks: map[string][]HookSpec{
					"PreToolUse": {
						{Matcher: "Write", Command: EnvString{Raw: "./scripts/child-guard.sh"}},
					},
				},
			},
			TargetsConfig: map[string]TargetConfigSpec{
				"claude": {
					LoadGlobal:             &tVal, // Attempted downgrade from false -> true must be blocked
					OnUnsupported:          "warn",
					OnIsolationUnsupported: "warn",
					RulesMode:              "agents_md",
				},
			},
		},
	}

	cm, err := ComposeLayers([]ManifestLayer{remoteBase, localBase, child}, "/repo")
	if err != nil {
		t.Fatalf("ComposeLayers failed: %v", err)
	}

	// 1. Child targets replace base targets.
	if len(cm.Effective.Targets) != 1 || cm.Effective.Targets[0] != "claude" {
		t.Fatalf("expected child targets [claude], got %v", cm.Effective.Targets)
	}

	// 2. Remote base skill ./skills/org-sec rebased to commit-pinned structured Git source.
	if len(cm.Effective.Skills) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(cm.Effective.Skills))
	}
	orgSec := cm.Effective.Skills[0]
	if !orgSec.Source.IsStructured || orgSec.Source.Git != "https://github.com/org/base.git" || orgSec.Source.Ref != sha || orgSec.Source.Path != "presets/skills/org-sec" {
		t.Fatalf("unexpected rebased remote skill source: %+v", orgSec.Source)
	}

	// 3. Local base agent ./agents/team.md rebased with leading "./".
	teamAg := cm.Effective.Agents[0]
	if teamAg.Source.Raw != "./team/agents/team.md" {
		t.Fatalf("expected rebased local agent source ./team/agents/team.md, got %q", teamAg.Source.Raw)
	}

	// 4. Upsert by name & provenance classification.
	if st, orig := cm.EntryProvenance("skill", "org-sec"); st != ProvenanceInheritedOnly || !orig.IsRemote {
		t.Fatalf("expected org-sec to be inherited_only, got %s (%+v)", st, orig)
	}
	if st, orig := cm.EntryProvenance("skill", "overridden-sk"); st != ProvenanceOverriddenLocally || !orig.IsRemote {
		t.Fatalf("expected overridden-sk to be overridden_locally, got %s (%+v)", st, orig)
	}
	if st, _ := cm.EntryProvenance("skill", "local-sk"); st != ProvenanceLocalOnly {
		t.Fatalf("expected local-sk to be local_only, got %s", st)
	}

	// 5. MCP replaced wholesale without mixing http URL and stdio command.
	mcp := cm.Effective.MCP["shared-mcp"]
	if mcp.Transport != "stdio" || mcp.URL.Raw != "" || mcp.Command.Raw != "npx" {
		t.Fatalf("expected wholesale MCP replacement, got %+v", mcp)
	}

	// 6. Monotonic security union on deny_commands and PreToolUse hooks.
	if len(cm.Effective.Settings.Permissions.DenyCommands) != 2 {
		t.Fatalf("expected both base and child deny_commands preserved, got %v", cm.Effective.Settings.Permissions.DenyCommands)
	}
	if len(cm.Effective.Settings.Hooks["PreToolUse"]) != 2 {
		t.Fatalf("expected both base and child PreToolUse hooks preserved, got %+v", cm.Effective.Settings.Hooks["PreToolUse"])
	}

	// 7. Monotonic non-downgrade on load_global=false and on_*_unsupported=fail.
	tc := cm.Effective.TargetsConfig["claude"]
	if tc.LoadGlobal == nil || *tc.LoadGlobal != false {
		t.Fatalf("expected load_global=false non-downgrade enforced, got %+v", tc.LoadGlobal)
	}
	if tc.OnUnsupported != "fail" || tc.OnIsolationUnsupported != "fail" {
		t.Fatalf("expected on_unsupported=fail non-downgrade enforced, got %+v", tc)
	}
	if tc.RulesMode != "agents_md" {
		t.Fatalf("expected non-security rules_mode to take child override, got %q", tc.RulesMode)
	}

	// 8. Remote base escaping repo root via ../ is rejected.
	badRemote := ManifestLayer{
		Origin: LayerOrigin{Source: "https://github.com/org/base.git", Subpath: "Agentfile.yaml", IsRemote: true},
		Spec: &ProjectSpec{
			Version: 1,
			Targets: []string{"claude"},
			Skills:  []AssetSpec{{Name: "escape", Source: SourceSpec{Raw: "../outside"}}},
		},
	}
	if _, err := ComposeLayers([]ManifestLayer{badRemote}, "/repo"); err == nil {
		t.Fatalf("expected error when remote base relative path escapes repository root")
	}

	// 9. ComposeLayersPartial allows omitted targets, and EntryProvenance / DisplayName / cloneMCPServerSpec cover agent, rule, mcp, overrides.
	pBase := ManifestLayer{
		Origin: LayerOrigin{Source: "https://github.com/org/base.git", Ref: "v1.0.0", Subpath: "sub/Agentfile.yaml", IsRemote: true},
		Spec: &ProjectSpec{
			Version: 1,
			MCP: map[string]MCPServerSpec{
				"m1": {
					Transport: "stdio",
					Command:   EnvString{Raw: "npx"},
					Env:       map[string]EnvString{"K": {Raw: "V"}},
				},
			},
			Agents: []AgentSpec{{Name: "ag1", Source: SourceSpec{Raw: "./ag.md"}}},
			Rules:  []RuleSpec{{Name: "rl1", Source: SourceSpec{Raw: "./rl.md"}}},
			TargetsConfig: map[string]TargetConfigSpec{
				"claude": {Overrides: map[string]any{"a": 1}},
			},
		},
	}
	pChild := ManifestLayer{
		Origin: LayerOrigin{IsRoot: true},
		Spec: &ProjectSpec{
			Version: 1,
			Extends: ExtendsList{{Raw: "https://github.com/org/base.git#v1.0.0//sub/Agentfile.yaml"}},
			MCP: map[string]MCPServerSpec{
				"m1": {
					Transport: "http",
					URL:       EnvString{Raw: "https://x"},
					Headers:   map[string]EnvString{"H": {Raw: "V"}},
				},
			},
			Agents: []AgentSpec{{Name: "ag1", Source: SourceSpec{Raw: "./ag2.md"}}},
			Rules:  []RuleSpec{{Name: "rl1", Source: SourceSpec{Raw: "./rl2.md"}}},
			TargetsConfig: map[string]TargetConfigSpec{
				"claude": {Overrides: map[string]any{"b": 2}},
			},
		},
	}
	cmPart, err := ComposeLayersPartial([]ManifestLayer{pBase, pChild}, "/repo")
	if err != nil {
		t.Fatalf("ComposeLayersPartial failed: %v", err)
	}
	if pBase.Origin.DisplayName() != "https://github.com/org/base.git#v1.0.0//sub/Agentfile.yaml" || pChild.Origin.DisplayName() != "Agentfile.yaml" {
		t.Fatalf("unexpected DisplayName: %q / %q", pBase.Origin.DisplayName(), pChild.Origin.DisplayName())
	}
	for _, k := range []string{"agent", "rule", "mcp"} {
		name := "ag1"
		if k == "rule" {
			name = "rl1"
		} else if k == "mcp" {
			name = "m1"
		}
		if st, _ := cmPart.EntryProvenance(k, name); st != ProvenanceOverriddenLocally {
			t.Fatalf("expected %s %s overridden_locally, got %s", k, name, st)
		}
	}
	if st, _ := cmPart.EntryProvenance("skill", "missing"); st != ProvenanceNone {
		t.Fatalf("expected none for missing skill, got %s", st)
	}
}
