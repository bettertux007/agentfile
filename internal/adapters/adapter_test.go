package adapters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/ir"
)

func TestMarshalJSONNoEscape(t *testing.T) {
	t.Parallel()
	b, err := MarshalJSONNoEscape(map[string]string{
		"url": "https://example.com/?a=1&b=2<test>",
	})
	if err != nil {
		t.Fatalf("MarshalJSONNoEscape failed: %v", err)
	}
	if !strings.Contains(string(b), "https://example.com/?a=1&b=2<test>") {
		t.Fatalf("expected unescaped URL, got %s", b)
	}
}

func TestCompareSemverAndValidateForbiddenFlags(t *testing.T) {
	t.Parallel()
	ok, err := CompareSemver("2.1.278 (Claude Code)", "2.1.0")
	if err != nil || !ok {
		t.Fatalf("expected >= 2.1.0, got ok=%v err=%v", ok, err)
	}
	ok, err = CompareSemver("claude 1.2.3", "2.1.0")
	if err != nil || ok {
		t.Fatalf("expected < 2.1.0, got ok=%v err=%v", ok, err)
	}
	if _, err := CompareSemver("unknown", "2.1.0"); err == nil {
		t.Fatalf("expected parse error for 'unknown'")
	}

	// Prompt text after -p and tokens after -- must NOT trigger forbidden flag errors.
	okArgs := []string{"-p", "--bare", "--model", "sonnet", "--", "--setting-sources", "user"}
	if diags := ValidateForbiddenFlags("claude", okArgs, []string{"--setting-sources", "--bare"}, []string{"-p", "--model"}); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics for prompt/post--- args: %+v", diags)
	}

	// Actual forbidden flags (--flag or --flag=val) before -- must fail.
	badArgs := []string{"-p", "hi", "--setting-sources=user"}
	if diags := ValidateForbiddenFlags("claude", badArgs, []string{"--setting-sources"}, []string{"-p"}); len(diags) != 1 {
		t.Fatalf("expected 1 forbidden flag error, got %+v", diags)
	}
}

func TestArtifactOpContractsAndFormatManagedRulesBlock(t *testing.T) {
	t.Parallel()

	// Verify OpMergeTOML, TOMLMergeSpec, Shared, and TOMLMerge contract fields.
	if OpMergeTOML != "merge_toml" {
		t.Fatalf("unexpected OpMergeTOML: %q", OpMergeTOML)
	}
	op := ArtifactOp{
		Target:  "codex",
		RelPath: ".codex/config.toml",
		Kind:    OpMergeTOML,
		Shared:  true,
		TOMLMerge: &TOMLMergeSpec{
			TableEntries: map[string]any{"mcp_servers.github": map[string]any{"url": "https://example.com"}},
		},
	}
	if !op.Shared || op.TOMLMerge == nil || len(op.TOMLMerge.TableEntries) != 1 {
		t.Fatalf("unexpected ArtifactOp fields: %+v", op)
	}

	// Empty rules must return nil.
	if got := FormatManagedRulesBlock(nil); got != nil {
		t.Fatalf("expected nil for nil rules, got %q", got)
	}
	if got := FormatManagedRulesBlock([]ir.IRRule{}); got != nil {
		t.Fatalf("expected nil for empty rules, got %q", got)
	}

	// Single rule must match gemini/testdata/gemini.golden.md layout byte-for-byte.
	golden, err := os.ReadFile(filepath.Join("gemini", "testdata", "gemini.golden.md"))
	if err != nil {
		t.Fatalf("read gemini golden: %v", err)
	}
	gotSingle := FormatManagedRulesBlock([]ir.IRRule{
		{
			Name:        "coding",
			Description: "Go rules",
			Globs:       []string{"**/*.go"},
			Content:     []byte("Always run go test.\n"),
		},
	})
	if string(gotSingle) != string(golden) {
		t.Fatalf("FormatManagedRulesBlock single rule mismatch:\ngot:\n%q\nwant:\n%q", gotSingle, golden)
	}

	// Multiple rules: deterministic separator and trailing newline normalization.
	gotMulti := FormatManagedRulesBlock([]ir.IRRule{
		{
			Name:    "first",
			Content: []byte("Rule one"),
		},
		{
			Name:        "second",
			Description: "Second scope",
			Content:     []byte("Rule two\n"),
		},
	})
	wantMulti := "# Project Agent Instructions\n\n## first\n\nRule one\n\n---\n\n## second\n\n*Scope:* Second scope\n\nRule two\n"
	if string(gotMulti) != wantMulti {
		t.Fatalf("FormatManagedRulesBlock multi rule mismatch:\ngot:\n%q\nwant:\n%q", gotMulti, wantMulti)
	}
}
