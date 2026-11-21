package spec

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func assertPolicyError(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected PolicyError, got nil", context)
	}
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: expected *PolicyError, got %T: %v", context, err, err)
	}
}

func TestAddSkillPreservesCommentsAndFormats(t *testing.T) {
	const input = `# Header comment for Agentfile

# Second header line
version: 1
targets:
  - claude # primary target
# Skills section comment
skills:
  - name: alpha # existing alpha comment
    source: ./skills/alpha
# Rules section stays untouched
rules:
  - name: style
    source: ./RULES.md # keep rule comment
`

	out, err := AddSkill([]byte(input), AddSkillInput{
		Name:   "beta",
		Source: "https://github.com/org/repo.git#v1.0.0//skills/beta",
	})
	if err != nil {
		t.Fatalf("AddSkill failed: %v", err)
	}

	got := string(out)
	for _, want := range []string{
		"# Header comment for Agentfile\n\n# Second header line",
		"- claude # primary target",
		"# Skills section comment",
		"- name: alpha # existing alpha comment",
		"# Rules section stays untouched",
		"source: ./RULES.md # keep rule comment",
		"  - name: beta",
		"    source: https://github.com/org/repo.git#v1.0.0//skills/beta",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in output:\n%s", want, got)
		}
	}

	ps, err := Parse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("round-trip Parse failed: %v\n%s", err, got)
	}
	if len(ps.Skills) != 2 || ps.Skills[1].Name != "beta" || ps.Skills[1].Source.Raw != "https://github.com/org/repo.git#v1.0.0//skills/beta" {
		t.Fatalf("unexpected parsed skills: %+v", ps.Skills)
	}
}

func TestAddAgentAndRulePreservesComments(t *testing.T) {
	const input = `# Agentfile manifest
version: 1
targets: [claude, cursor]

agents:
  - name: reviewer # keep reviewer
    source: ./agents/reviewer.md
`

	out, err := AddAgent([]byte(input), AddAgentInput{
		Name:        "architect",
		Source:      "./agents/architect.md",
		Description: "Designs architecture",
		Model:       "sonnet",
	})
	if err != nil {
		t.Fatalf("AddAgent failed: %v", err)
	}

	out, err = AddRule(out, AddRuleInput{
		Name:        "go-style",
		Source:      "./rules/go.md",
		Description: "Go conventions",
		Globs:       []string{"**/*.go", "cmd/**/*.go"},
		AlwaysApply: true,
	})
	if err != nil {
		t.Fatalf("AddRule failed: %v", err)
	}

	got := string(out)
	for _, want := range []string{
		"# Agentfile manifest",
		"- name: reviewer # keep reviewer",
		"  - name: architect",
		"    description: Designs architecture",
		"    model: sonnet",
		"    source: ./agents/architect.md",
		"rules:",
		"  - name: go-style",
		"    source: ./rules/go.md",
		"    description: Go conventions",
		"    always_apply: true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in output:\n%s", want, got)
		}
	}

	ps, err := Parse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("round-trip Parse failed: %v\n%s", err, got)
	}
	if len(ps.Agents) != 2 || ps.Agents[1].Model != "sonnet" {
		t.Fatalf("unexpected parsed agents: %+v", ps.Agents)
	}
	if len(ps.Rules) != 1 || !ps.Rules[0].AlwaysApply || len(ps.Rules[0].Globs) != 2 {
		t.Fatalf("unexpected parsed rules: %+v", ps.Rules)
	}
}

func TestAddToMissingFlowAndNullSections(t *testing.T) {
	t.Run("missing_skills_section", func(t *testing.T) {
		const input = "version: 1\ntargets:\n  - claude\n"
		out, err := AddSkill([]byte(input), AddSkillInput{Name: "s1", Source: "./s1"})
		if err != nil {
			t.Fatalf("AddSkill failed: %v", err)
		}
		if strings.Contains(string(out), "[") {
			t.Fatalf("expected block sequence, got:\n%s", out)
		}
		ps, err := Parse(bytes.NewReader(out))
		if err != nil || len(ps.Skills) != 1 {
			t.Fatalf("unexpected Parse result: %v, %+v", err, ps)
		}
	})

	t.Run("flow_empty_skills", func(t *testing.T) {
		const input = "version: 1\ntargets: [claude]\nskills: [] # empty flow\n"
		out, err := AddSkill([]byte(input), AddSkillInput{Name: "s1", Source: "./s1"})
		if err != nil {
			t.Fatalf("AddSkill failed: %v", err)
		}
		got := string(out)
		if strings.Contains(got, "skills: []") {
			t.Fatalf("expected flow sequence coerced to block style, got:\n%s", got)
		}
		if !strings.Contains(got, "# empty flow") {
			t.Fatalf("expected comment preserved on skills key, got:\n%s", got)
		}
		ps, err := Parse(bytes.NewReader(out))
		if err != nil || len(ps.Skills) != 1 {
			t.Fatalf("unexpected Parse result: %v, %+v", err, ps)
		}
	})

	t.Run("null_skills_section", func(t *testing.T) {
		const input = "version: 1\ntargets: [claude]\nskills: # null section\n"
		out, err := AddSkill([]byte(input), AddSkillInput{Name: "s1", Source: "./s1"})
		if err != nil {
			t.Fatalf("AddSkill failed: %v", err)
		}
		got := string(out)
		if !strings.Contains(got, "# null section") {
			t.Fatalf("expected comment preserved on null skills section, got:\n%s", got)
		}
		if !strings.Contains(got, "  - name: s1") {
			t.Fatalf("expected block sequence item, got:\n%s", got)
		}
		ps, err := Parse(bytes.NewReader(out))
		if err != nil || len(ps.Skills) != 1 {
			t.Fatalf("unexpected Parse result: %v, %+v", err, ps)
		}
	})

	t.Run("flow_empty_mcp_and_null_mcp", func(t *testing.T) {
		for _, input := range []string{
			"version: 1\ntargets: [claude]\nmcp: {} # empty map\n",
			"version: 1\ntargets: [claude]\nmcp: # null map\n",
		} {
			out, err := AddMCP([]byte(input), AddMCPInput{
				Name:      "fs",
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "server-fs"},
			})
			if err != nil {
				t.Fatalf("AddMCP failed on %q: %v", input, err)
			}
			got := string(out)
			if strings.Contains(got, "{}") {
				t.Fatalf("expected flow map coerced to block style, got:\n%s", got)
			}
			if !strings.Contains(got, "  fs:\n") {
				t.Fatalf("expected block mcp entry, got:\n%s", got)
			}
			ps, err := Parse(bytes.NewReader(out))
			if err != nil || len(ps.MCP) != 1 {
				t.Fatalf("unexpected Parse result: %v, %+v", err, ps)
			}
		}
	})
}

func TestAddDuplicateAndReplaceInPlace(t *testing.T) {
	const input = `version: 1
targets: [claude]
skills:
  # Keep alpha comment
  - name: alpha
    source: ./old-alpha
  # Keep beta comment
  - name: beta # inline beta
    source: ./beta
mcp:
  # Keep github comment
  github:
    transport: http
    url: https://old.example.com/mcp
  # Keep local comment
  local:
    transport: stdio
    command: npx # inline command
`

	// Reject duplicate skill without Replace.
	_, err := AddSkill([]byte(input), AddSkillInput{Name: "alpha", Source: "./new-alpha"})
	assertPolicyError(t, err, "duplicate skill without Replace")

	// Reject duplicate mcp without Replace.
	_, err = AddMCP([]byte(input), AddMCPInput{Name: "github", Transport: "http", URL: "https://new.example.com"})
	assertPolicyError(t, err, "duplicate mcp without Replace")

	// Replace skill in place and verify order + sibling comments.
	out, err := AddSkill([]byte(input), AddSkillInput{Name: "alpha", Source: "./new-alpha", Replace: true})
	if err != nil {
		t.Fatalf("AddSkill replace failed: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "# Keep alpha comment") || !strings.Contains(got, "# Keep beta comment") || !strings.Contains(got, "- name: beta # inline beta") {
		t.Fatalf("lost comments on skill replace:\n%s", got)
	}
	if strings.Index(got, "alpha") > strings.Index(got, "beta") {
		t.Fatalf("expected alpha to remain before beta:\n%s", got)
	}
	if strings.Contains(got, "./old-alpha") || !strings.Contains(got, "./new-alpha") {
		t.Fatalf("expected alpha source updated:\n%s", got)
	}

	// Replace mcp in place and verify order + sibling comments.
	out, err = AddMCP(out, AddMCPInput{
		Name:      "github",
		Transport: "http",
		URL:       "https://new.example.com/mcp",
		Headers:   map[string]string{"Authorization": "Bearer ${GITHUB_PAT}"},
		Replace:   true,
	})
	if err != nil {
		t.Fatalf("AddMCP replace failed: %v", err)
	}
	got = string(out)
	if !strings.Contains(got, "# Keep github comment") || !strings.Contains(got, "# Keep local comment") || !strings.Contains(got, "command: npx # inline command") {
		t.Fatalf("lost comments on mcp replace:\n%s", got)
	}
	if strings.Index(got, "github:") > strings.Index(got, "local:") {
		t.Fatalf("expected github to remain before local:\n%s", got)
	}
	if strings.Contains(got, "old.example.com") || !strings.Contains(got, "new.example.com") {
		t.Fatalf("expected github URL updated:\n%s", got)
	}
}

func TestAddMCPDeterministicSortedMapsAndEnvPreservation(t *testing.T) {
	t.Setenv("GITHUB_PAT", "MUST_NOT_EXPAND")
	t.Setenv("API_KEY", "MUST_NOT_EXPAND")

	const base = "version: 1\ntargets: [claude, opencode]\n"

	out, err := AddMCP([]byte(base), AddMCPInput{
		Name:      "stdio-srv",
		Transport: "stdio",
		Command:   "${BIN_DIR:-/usr/local/bin}/server",
		Args:      []string{"--token", "${API_KEY}"},
		Env: map[string]string{
			"Z_VAR": "z",
			"A_VAR": "${API_KEY}",
			"M_VAR": "cost=$$5",
		},
	})
	if err != nil {
		t.Fatalf("AddMCP stdio failed: %v", err)
	}

	out, err = AddMCP(out, AddMCPInput{
		Name:      "http-srv",
		Transport: "sse",
		URL:       "https://mcp.example.com/${STAGE:-prod}",
		Headers: map[string]string{
			"X-Zebra":       "1",
			"Authorization": "Bearer ${GITHUB_PAT}",
		},
	})
	if err != nil {
		t.Fatalf("AddMCP sse failed: %v", err)
	}

	got := string(out)
	if strings.Contains(got, "MUST_NOT_EXPAND") {
		t.Fatalf("env var expanded unexpectedly:\n%s", got)
	}
	if strings.Index(got, "A_VAR:") > strings.Index(got, "M_VAR:") || strings.Index(got, "M_VAR:") > strings.Index(got, "Z_VAR:") {
		t.Fatalf("expected env keys sorted A_VAR < M_VAR < Z_VAR:\n%s", got)
	}
	if strings.Index(got, "Authorization:") > strings.Index(got, "X-Zebra:") {
		t.Fatalf("expected header keys sorted Authorization < X-Zebra:\n%s", got)
	}

	ps, err := Parse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("round-trip Parse failed: %v\n%s", err, got)
	}
	if ps.MCP["stdio-srv"].Env["A_VAR"].Raw != "${API_KEY}" {
		t.Fatalf("expected raw env placeholder preserved, got %+v", ps.MCP["stdio-srv"].Env["A_VAR"])
	}
	if ps.MCP["http-srv"].Headers["Authorization"].Raw != "Bearer ${GITHUB_PAT}" {
		t.Fatalf("expected raw header placeholder preserved, got %+v", ps.MCP["http-srv"].Headers["Authorization"])
	}

	// Invalid transport / conflicting fields must fail with PolicyError.
	_, err = AddMCP([]byte(base), AddMCPInput{
		Name:      "bad",
		Transport: "stdio",
		Command:   "npx",
		URL:       "https://bad.example.com",
	})
	assertPolicyError(t, err, "stdio with url")

	_, err = AddMCP([]byte(base), AddMCPInput{
		Name:      "bad2",
		Transport: "http",
		URL:       "https://good.example.com",
		Command:   "npx",
	})
	assertPolicyError(t, err, "http with command")
}

func TestRejectsAnchorsAliasesAndMergeKeysInMutatedSections(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		fn   func([]byte) ([]byte, error)
	}{
		{
			name: "anchor_in_skills",
			yaml: "version: 1\ntargets: [claude]\nskills:\n  - name: a\n    source: &src ./a\n",
			fn: func(b []byte) ([]byte, error) {
				return AddSkill(b, AddSkillInput{Name: "b", Source: "./b"})
			},
		},
		{
			name: "alias_in_skills",
			yaml: "version: 1\ntargets: [claude]\nx_src: &src ./a\nskills:\n  - name: a\n    source: *src\n",
			fn: func(b []byte) ([]byte, error) {
				return AddSkill(b, AddSkillInput{Name: "b", Source: "./b"})
			},
		},
		{
			name: "merge_key_in_mcp",
			yaml: "version: 1\ntargets: [claude]\nx_base: &base\n  transport: stdio\nmcp:\n  a:\n    <<: *base\n    command: npx\n",
			fn: func(b []byte) ([]byte, error) {
				return AddMCP(b, AddMCPInput{Name: "b", Transport: "stdio", Command: "node"})
			},
		},
		{
			name: "anchor_on_remove",
			yaml: "version: 1\ntargets: [claude]\nskills:\n  - &first\n    name: a\n    source: ./a\n",
			fn: func(b []byte) ([]byte, error) {
				return RemoveEntry(b, "skill", "a")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fn([]byte(tc.yaml))
			assertPolicyError(t, err, tc.name)
		})
	}
}

func TestRemoveEntryBehaviorAndEmptiedSections(t *testing.T) {
	// Missing entry or missing section fails with PolicyError.
	const twoSkills = `# Header
version: 1
targets: [claude]
skills:
  - name: alpha # keep alpha
    source: ./a
  - name: beta
    source: ./b
`
	_, err := RemoveEntry([]byte(twoSkills), "skill", "nonexistent")
	assertPolicyError(t, err, "remove nonexistent skill")

	_, err = RemoveEntry([]byte(twoSkills), "agent", "missing")
	assertPolicyError(t, err, "remove from missing agents section")

	_, err = RemoveEntry([]byte(twoSkills), "unknown", "alpha")
	assertPolicyError(t, err, "remove unknown kind")

	// Removing one of two preserves sibling and comments.
	out, err := RemoveEntry([]byte(twoSkills), "skill", "beta")
	if err != nil {
		t.Fatalf("RemoveEntry failed: %v", err)
	}
	if !strings.Contains(string(out), "- name: alpha # keep alpha") || strings.Contains(string(out), "beta") {
		t.Fatalf("unexpected output after removing beta:\n%s", out)
	}

	// Removing last uncommented entry deletes the section key cleanly.
	out, err = RemoveEntry(out, "skill", "alpha")
	if err != nil {
		t.Fatalf("RemoveEntry last uncommented failed: %v", err)
	}
	if strings.Contains(string(out), "skills") {
		t.Fatalf("expected uncommented emptied skills section deleted, got:\n%s", out)
	}
	if _, err := Parse(bytes.NewReader(out)); err != nil {
		t.Fatalf("Parse failed after deleting empty section: %v\n%s", err, out)
	}

	// Removing last entry when section key/value has comments normalizes to flow [] / {} (never !!null).
	const commentedSkillSection = `version: 1
targets: [claude]
# Keep this skills header comment
skills:
  - name: only
    source: ./only
`
	out, err = RemoveEntry([]byte(commentedSkillSection), "skill", "only")
	if err != nil {
		t.Fatalf("RemoveEntry commented skill section failed: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "# Keep this skills header comment") || !strings.Contains(got, "skills: []") {
		t.Fatalf("expected commented emptied skills section normalized to 'skills: []', got:\n%s", got)
	}
	if _, err := Parse(bytes.NewReader(out)); err != nil {
		t.Fatalf("Parse failed on commented empty skills []: %v\n%s", err, got)
	}

	const commentedMCPSection = `version: 1
targets: [claude]
mcp: # Keep this inline comment
  only:
    transport: stdio
    command: npx
`
	out, err = RemoveEntry([]byte(commentedMCPSection), "mcp", "only")
	if err != nil {
		t.Fatalf("RemoveEntry commented mcp section failed: %v", err)
	}
	got = string(out)
	if !strings.Contains(got, "mcp: {} # Keep this inline comment") {
		t.Fatalf("expected commented emptied mcp section normalized to 'mcp: {} # Keep this inline comment', got:\n%s", got)
	}
	if _, err := Parse(bytes.NewReader(out)); err != nil {
		t.Fatalf("Parse failed on commented empty mcp {}: %v\n%s", err, got)
	}

	// Removing last uncommented mcp deletes mcp key cleanly.
	const uncommentedMCP = `version: 1
targets: [claude]
mcp:
  only:
    transport: stdio
    command: npx
`
	out, err = RemoveEntry([]byte(uncommentedMCP), "mcp", "only")
	if err != nil {
		t.Fatalf("RemoveEntry uncommented mcp failed: %v", err)
	}
	if strings.Contains(string(out), "mcp") {
		t.Fatalf("expected uncommented emptied mcp section deleted, got:\n%s", out)
	}

	// Removing agent and rule entries works too.
	const withAgentAndRule = `version: 1
targets: [claude]
agents:
  - name: ag1
    source: ./ag1.md
rules:
  - name: rl1
    source: ./rl1.md
`
	out, err = RemoveEntry([]byte(withAgentAndRule), "agent", "ag1")
	if err != nil {
		t.Fatalf("RemoveEntry agent failed: %v", err)
	}
	if strings.Contains(string(out), "agents") {
		t.Fatalf("expected agents section removed:\n%s", out)
	}
	out, err = RemoveEntry(out, "rule", "rl1")
	if err != nil {
		t.Fatalf("RemoveEntry rule failed: %v", err)
	}
	if strings.Contains(string(out), "rules") {
		t.Fatalf("expected rules section removed:\n%s", out)
	}
}

func TestEditErrorAndEdgeCases(t *testing.T) {
	t.Parallel()

	// Invalid YAML / empty / multi-doc / non-mapping root.
	badInputs := []struct {
		name string
		yaml string
	}{
		{"empty", ""},
		{"syntax_error", "version: [\n"},
		{"multi_doc", "version: 1\ntargets: [claude]\n---\nversion: 1\n"},
		{"non_mapping_root", "- a\n- b\n"},
		{"bad_skills_type", "version: 1\ntargets: [claude]\nskills: not-a-seq\n"},
		{"bad_mcp_type", "version: 1\ntargets: [claude]\nmcp: [not-a-map]\n"},
	}
	for _, tc := range badInputs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.name == "bad_mcp_type" {
				_, err := AddMCP([]byte(tc.yaml), AddMCPInput{Name: "a", Transport: "stdio", Command: "npx"})
				assertPolicyError(t, err, tc.name)
				_, err = RemoveEntry([]byte(tc.yaml), "mcp", "a")
				assertPolicyError(t, err, tc.name+" remove")
				return
			}
			_, err := AddSkill([]byte(tc.yaml), AddSkillInput{Name: "a", Source: "./a"})
			assertPolicyError(t, err, tc.name)
			if tc.name == "bad_skills_type" {
				_, err = RemoveEntry([]byte(tc.yaml), "skill", "a")
				assertPolicyError(t, err, tc.name+" remove")
			}
		})
	}

	// RemoveEntry edge cases: empty name, null sections, missing mcp key in non-empty map, anchors/aliases.
	_, err := RemoveEntry([]byte("version: 1\ntargets: [claude]\n"), "skill", "")
	assertPolicyError(t, err, "empty name")

	_, err = RemoveEntry([]byte("version: 1\ntargets: [claude]\nskills:\n"), "skill", "a")
	assertPolicyError(t, err, "null skills remove")

	_, err = RemoveEntry([]byte("version: 1\ntargets: [claude]\nmcp:\n"), "mcp", "a")
	assertPolicyError(t, err, "null mcp remove")

	_, err = RemoveEntry([]byte("version: 1\ntargets: [claude]\nmcp:\n  other:\n    transport: stdio\n    command: npx\n"), "mcp", "missing")
	assertPolicyError(t, err, "missing mcp key")

	_, err = RemoveEntry([]byte("version: 1\ntargets: [claude]\nmcp:\n  a: &m\n    transport: stdio\n    command: npx\n"), "mcp", "a")
	assertPolicyError(t, err, "anchor in mcp remove")

	_, err = AddSkill([]byte("version: 1\ntargets: [claude]\n&k skills: []\n"), AddSkillInput{Name: "a", Source: "./a"})
	assertPolicyError(t, err, "anchor on skills key")

	_, err = AddMCP([]byte("version: 1\ntargets: [claude]\n&k mcp: {}\n"), AddMCPInput{Name: "a", Transport: "stdio", Command: "npx"})
	assertPolicyError(t, err, "anchor on mcp key")

	_, err = RemoveEntry([]byte("version: 1\ntargets: [claude]\n&k skills:\n  - name: a\n    source: ./a\n"), "skill", "a")
	assertPolicyError(t, err, "anchor on skills key remove")

	// Invalid slug / empty source caught by round-trip Parse.
	_, err = AddSkill([]byte("version: 1\ntargets: [claude]\n"), AddSkillInput{Name: "../evil", Source: "./a"})
	assertPolicyError(t, err, "invalid slug")

	_, err = AddSkill([]byte("version: 1\ntargets: [claude]\n"), AddSkillInput{Name: "valid", Source: ""})
	assertPolicyError(t, err, "empty source")

	// Sequence containing non-mapping element fails round-trip Parse.
	_, err = AddSkill([]byte("version: 1\ntargets: [claude]\nskills:\n  - not-a-mapping\n"), AddSkillInput{Name: "a", Source: "./a"})
	assertPolicyError(t, err, "non-mapping sequence item")

	// Child manifest with extends and omitted targets can be edited via AddSkill.
	extChild := []byte("version: 1\nextends: ./base/Agentfile.yaml\n")
	editedExt, err := AddSkill(extChild, AddSkillInput{Name: "extra", Source: "./skills/extra"})
	if err != nil {
		t.Fatalf("expected AddSkill to succeed on child manifest with extends and omitted targets: %v", err)
	}
	if !strings.Contains(string(editedExt), "extra") {
		t.Fatalf("expected extra skill in edited output:\n%s", editedExt)
	}
}
