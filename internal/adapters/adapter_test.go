package adapters

import (
	"strings"
	"testing"
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
