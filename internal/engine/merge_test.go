package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/spec"
)

func TestMergeJSONCPreservesCommentsUserKeysAndHTMLChars(t *testing.T) {
	t.Parallel()
	existing := []byte(`{
  // hand-written comment
  "mcpServers": {
    "custom": {
      "command": "python3",
    },
  },
}
`)
	ms := &adapters.JSONMergeSpec{
		MapEntries: map[string]any{
			"/mcpServers/github": map[string]any{
				"type": "http",
				"url":  "https://api.example.com/mcp?a=1&b=2",
			},
		},
	}

	out, rec, needsBackup, err := MergeJSONC(existing, true, ms, nil, func(detail string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("unexpected conflict: %s", detail)
	})
	if err != nil {
		t.Fatalf("MergeJSONC failed: %v", err)
	}
	if needsBackup || rec.CreatedByAgentfile {
		t.Fatalf("unexpected backup=%v created=%v", needsBackup, rec.CreatedByAgentfile)
	}
	txt := string(out)
	if !strings.Contains(txt, "// hand-written comment") {
		t.Fatalf("comment lost:\n%s", txt)
	}
	if !strings.Contains(txt, `"custom"`) {
		t.Fatalf("custom server lost:\n%s", txt)
	}
	if !strings.Contains(txt, "https://api.example.com/mcp?a=1&b=2") {
		t.Fatalf("HTML escaping corrupted URL:\n%s", txt)
	}

	// Now prune github via RemoveJSONC -> custom and comment must remain.
	afterRemove, deleteFile, err := RemoveJSONC(out, rec)
	if err != nil {
		t.Fatalf("RemoveJSONC failed: %v", err)
	}
	if deleteFile {
		t.Fatalf("expected deleteFile=false for user-owned file")
	}
	if strings.Contains(string(afterRemove), `"github"`) || !strings.Contains(string(afterRemove), `"custom"`) {
		t.Fatalf("unexpected RemoveJSONC output:\n%s", afterRemove)
	}
}

func TestMergeJSONCArrayPreservesRuntimeApprovalsAndConflictDetection(t *testing.T) {
	t.Parallel()
	existing := []byte(`{
  "permissions": {
    "allow": ["Bash(git status:*)"]
  }
}
`)
	ms1 := &adapters.JSONMergeSpec{
		MapEntries: map[string]any{
			"/hooks/SessionStart": []map[string]any{{"command": "echo hi"}},
		},
		ArrayEntries: map[string][]string{
			"/permissions/allow": {"Bash(go test ./...)"},
		},
	}

	out1, rec1, _, err := MergeJSONC(existing, true, ms1, nil, func(d string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("%s", d)
	})
	if err != nil {
		t.Fatalf("first MergeJSONC failed: %v", err)
	}
	if !strings.Contains(string(out1), "Bash(git status:*)") || !strings.Contains(string(out1), "Bash(go test ./...)") {
		t.Fatalf("expected both user and managed allow entries:\n%s", out1)
	}

	// Simulate user editing the owned /hooks/SessionStart key on disk.
	edited := strings.ReplaceAll(string(out1), "echo hi", "echo edited")
	_, _, _, err = MergeJSONC([]byte(edited), true, ms1, &rec1, func(d string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("%s", d)
	})
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on user-modified owned key, got %v", err)
	}

	// Update removes /hooks/SessionStart and prunes /permissions/allow entry cleanly.
	ms2 := &adapters.JSONMergeSpec{
		MapEntries:   map[string]any{},
		ArrayEntries: map[string][]string{},
	}
	out2, rec2, _, err := MergeJSONC(out1, true, ms2, &rec1, func(d string) (bool, bool, error) {
		return true, false, nil
	})
	if err != nil {
		t.Fatalf("MergeJSONC prune failed: %v", err)
	}
	if strings.Contains(string(out2), "SessionStart") || strings.Contains(string(out2), "Bash(go test ./...)") || !strings.Contains(string(out2), "Bash(git status:*)") {
		t.Fatalf("expected owned entries pruned and user entry kept:\n%s", out2)
	}
	_ = rec2
}

func TestRemoveJSONCDeletesAgentfileCreatedEmptyFile(t *testing.T) {
	t.Parallel()
	ms := &adapters.JSONMergeSpec{
		SchemaURL:  "https://opencode.ai/config.json",
		MapEntries: map[string]any{"/mcp/a": map[string]any{"type": "local"}},
	}
	out, rec, _, err := MergeJSONC(nil, false, ms, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, del, err := RemoveJSONC(out, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !del {
		t.Fatalf("expected deleteFile=true for agentfile-created file with only owned keys")
	}
}

func TestMergeAndRemoveMarkdownBlockFencedAwareAndCRLF(t *testing.T) {
	t.Parallel()
	existing := []byte("# User Notes\r\n```markdown\r\n<!-- BEGIN AGENTFILE:v1 fake -->\r\n```\r\n")
	out, rec, _, err := MergeMarkdownBlock(existing, true, []byte("## Rules\nFollow gofmt."), nil, nil)
	if err != nil {
		t.Fatalf("MergeMarkdownBlock failed: %v", err)
	}
	if !strings.Contains(string(out), "\r\n") {
		t.Fatalf("expected CRLF preservation")
	}
	if !strings.Contains(string(out), "<!-- BEGIN AGENTFILE:v1 fake -->") {
		t.Fatalf("fenced marker must remain untouched")
	}

	// Update replaces only the real block outside fence.
	out2, rec2, _, err := MergeMarkdownBlock(out, true, []byte("## Rules\nFollow gofmt v2."), &rec, nil)
	if err != nil {
		t.Fatalf("second MergeMarkdownBlock failed: %v", err)
	}
	if !strings.Contains(string(out2), "Follow gofmt v2.") {
		t.Fatalf("expected updated block:\n%s", out2)
	}

	// User edits inside block trigger conflict resolver.
	tampered := strings.ReplaceAll(string(out2), "Follow gofmt v2.", "Tampered.")
	_, _, _, err = MergeMarkdownBlock([]byte(tampered), true, []byte("## Rules\nV3."), &rec2, func(d string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("%s", d)
	})
	var pe *spec.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on tampered markdown block, got %v", err)
	}

	// RemoveMarkdownBlock preserves user notes and fenced example.
	cleaned, del, err := RemoveMarkdownBlock(out2, rec2)
	if err != nil || del {
		t.Fatalf("unexpected RemoveMarkdownBlock result: del=%v err=%v", del, err)
	}
	if strings.Contains(string(cleaned), "Follow gofmt v2.") || !strings.Contains(string(cleaned), "# User Notes") {
		t.Fatalf("unexpected cleaned markdown:\n%s", cleaned)
	}

	// Agentfile-created markdown block deletes file on RemoveMarkdownBlock.
	fresh, freshRec, _, err := MergeMarkdownBlock(nil, false, []byte("Only agentfile"), nil, nil)
	if err != nil || !freshRec.CreatedByAgentfile {
		t.Fatalf("fresh MergeMarkdownBlock failed: %v", err)
	}
	_, delFresh, err := RemoveMarkdownBlock(fresh, freshRec)
	if err != nil || !delFresh {
		t.Fatalf("expected deleteFile=true for agentfile-created markdown, got del=%v err=%v", delFresh, err)
	}

	// Unclosed marker fails with PolicyError.
	_, _, _, err = MergeMarkdownBlock([]byte("<!-- BEGIN AGENTFILE:v1 abc -->\nno end"), true, []byte("x"), nil, nil)
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on unclosed marker, got %v", err)
	}

	// RemoveMarkdownBlock on file without markers returns unchanged bytes.
	unchanged, delNone, err := RemoveMarkdownBlock([]byte("# Plain file\n"), ArtifactRecord{})
	if err != nil || delNone || string(unchanged) != "# Plain file\n" {
		t.Fatalf("expected plain file unchanged, got del=%v err=%v out=%q", delNone, err, unchanged)
	}

	// MergeJSONC on existing key with Ask -> ChoiceSkip preserves disk value.
	skippedOut, _, _, err := MergeJSONC([]byte(`{"mcpServers":{"gh":{"url":"https://user"}}}`), true, &adapters.JSONMergeSpec{
		MapEntries: map[string]any{"/mcpServers/gh": map[string]any{"url": "https://new"}},
	}, nil, func(string) (bool, bool, error) {
		return false, false, nil
	})
	if err != nil || !strings.Contains(string(skippedOut), "https://user") {
		t.Fatalf("expected skipped key preserved, got %q err=%v", skippedOut, err)
	}

	// Clean update of an unmodified owned key + RemoveJSONC emptying array.
	msA := &adapters.JSONMergeSpec{
		MapEntries:   map[string]any{"/mcpServers/gh": map[string]any{"url": "https://v1"}},
		ArrayEntries: map[string][]string{"/permissions/allow": {"Bash(go test:*)"}},
	}
	b1, r1, _, err := MergeJSONC([]byte("{}\n"), true, msA, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	msB := &adapters.JSONMergeSpec{
		MapEntries:   map[string]any{"/mcpServers/gh": map[string]any{"url": "https://v2"}},
		ArrayEntries: map[string][]string{"/permissions/allow": {"Bash(go vet:*)"}},
	}
	b2, r2, _, err := MergeJSONC(b1, true, msB, &r1, nil)
	if err != nil || !strings.Contains(string(b2), "https://v2") || strings.Contains(string(b2), "go test") {
		t.Fatalf("expected clean owned key update & array replacement, got:\n%s\nerr=%v", b2, err)
	}
	b3, _, err := RemoveJSONC(b2, r2)
	if err != nil || strings.Contains(string(b3), "go vet") || strings.Contains(string(b3), "https://v2") {
		t.Fatalf("expected RemoveJSONC to clean all owned entries, got:\n%s\nerr=%v", b3, err)
	}

	// Verify fresh MergeJSONC produces properly indented multi-line JSON (2 spaces, no single-line wrapping).
	freshJSON, _, _, err := MergeJSONC(nil, false, &adapters.JSONMergeSpec{
		MapEntries: map[string]any{
			"/mcpServers/fs": map[string]any{"command": "npx", "args": []string{"-y", "@mcp/fs"}},
		},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantFresh := "{\n  \"mcpServers\": {\n    \"fs\": {\n      \"args\": [\n        \"-y\",\n        \"@mcp/fs\"\n      ],\n      \"command\": \"npx\"\n    }\n  }\n}\n"
	if string(freshJSON) != wantFresh {
		t.Fatalf("unexpected formatted JSON:\ngot:\n%s\nwant:\n%s", freshJSON, wantFresh)
	}

	// Re-merging commented JSONC with identical desired entries must return byte-identical output.
	commented := []byte("{\n  // keep comment\n  \"mcpServers\": {\"custom\": {\"command\": \"echo\"}}\n}\n")
	c1, cRec1, _, err := MergeJSONC(commented, true, &adapters.JSONMergeSpec{
		MapEntries: map[string]any{"/mcpServers/gh": map[string]any{"url": "https://x"}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c2, _, _, err := MergeJSONC(c1, true, &adapters.JSONMergeSpec{
		MapEntries: map[string]any{"/mcpServers/gh": map[string]any{"url": "https://x"}},
	}, &cRec1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(c1) != string(c2) {
		t.Fatalf("expected idempotent commented JSONC output:\nc1:\n%s\nc2:\n%s", c1, c2)
	}

	// Invalid JSONC returns PolicyError on MergeJSONC and RemoveJSONC.
	if _, _, _, err := MergeJSONC([]byte("{invalid"), true, &adapters.JSONMergeSpec{}, nil, nil); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on invalid JSONC merge")
	}
	if _, _, err := RemoveJSONC([]byte("{invalid"), ArtifactRecord{}); !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on invalid JSONC remove")
	}
}
