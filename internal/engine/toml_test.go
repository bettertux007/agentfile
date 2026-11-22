package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/pelletier/go-toml/v2"
)

func TestMergeTOMLCreateFromScratchAndDeterministicSorting(t *testing.T) {
	t.Parallel()

	ms := &adapters.TOMLMergeSpec{
		TableEntries: map[string]any{
			"mcp_servers.zeta": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@mcp/zeta"},
			},
			"mcp_servers.alpha": map[string]any{
				"url": "https://alpha.example.com/mcp",
			},
		},
	}

	out, rec, needsBackup, err := MergeTOML(nil, false, ms, nil, func(detail string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("unexpected conflict: %s", detail)
	})
	if err != nil {
		t.Fatalf("MergeTOML failed: %v", err)
	}
	if needsBackup {
		t.Fatalf("expected needsBackup=false on fresh file")
	}
	if rec.ArtifactType != TypeTOMLPatch {
		t.Fatalf("expected ArtifactType=%q, got %q", TypeTOMLPatch, rec.ArtifactType)
	}
	if !rec.CreatedByAgentfile {
		t.Fatalf("expected CreatedByAgentfile=true")
	}
	if rec.SHA256 != hashBytes(out) {
		t.Fatalf("expected SHA256=%q, got %q", hashBytes(out), rec.SHA256)
	}
	if len(rec.TOMLKeys) != 2 || rec.TOMLKeys[0] != "mcp_servers.alpha" || rec.TOMLKeys[1] != "mcp_servers.zeta" {
		t.Fatalf("expected sorted TOMLKeys [mcp_servers.alpha mcp_servers.zeta], got %v", rec.TOMLKeys)
	}

	txt := string(out)
	alphaIdx := strings.Index(txt, "alpha")
	zetaIdx := strings.Index(txt, "zeta")
	if alphaIdx < 0 || zetaIdx < 0 || alphaIdx > zetaIdx {
		t.Fatalf("expected alpha sorted before zeta in TOML output:\n%s", txt)
	}

	// Valid TOML round-trip.
	var parsed map[string]any
	if err := toml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid TOML: %v\n%s", err, txt)
	}

	// Idempotent re-merge must produce byte-identical output and never prompt.
	out2, rec2, _, err := MergeTOML(out, true, ms, &rec, func(detail string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("unexpected conflict on resync: %s", detail)
	})
	if err != nil {
		t.Fatalf("idempotent MergeTOML failed: %v", err)
	}
	if string(out2) != string(out) {
		t.Fatalf("expected byte-identical output on resync:\nfirst:\n%s\nsecond:\n%s", out, out2)
	}
	if !recordsEqual(rec, rec2) {
		t.Fatalf("expected equal ArtifactRecords on resync: %+v vs %+v", rec, rec2)
	}
}

func TestMergeTOMLPreservesExistingUnownedKeys(t *testing.T) {
	t.Parallel()

	existing := []byte(`model = "gpt-5"

[mcp_servers.personal]
command = "python3"
args = ["my_server.py"]
`)
	ms := &adapters.TOMLMergeSpec{
		TableEntries: map[string]any{
			"mcp_servers.github": map[string]any{
				"url": "https://api.github.com/mcp",
			},
		},
	}

	out, rec, needsBackup, err := MergeTOML(existing, true, ms, nil, func(detail string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("unexpected conflict: %s", detail)
	})
	if err != nil {
		t.Fatalf("MergeTOML failed: %v", err)
	}
	if needsBackup || rec.CreatedByAgentfile {
		t.Fatalf("unexpected needsBackup=%v CreatedByAgentfile=%v", needsBackup, rec.CreatedByAgentfile)
	}

	var parsed map[string]any
	if err := toml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid output TOML: %v", err)
	}
	if parsed["model"] != "gpt-5" {
		t.Fatalf("expected unowned top-level key model='gpt-5' preserved, got %v", parsed["model"])
	}
	mcps, ok := parsed["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp_servers table, got %T", parsed["mcp_servers"])
	}
	if _, ok := mcps["personal"]; !ok {
		t.Fatalf("expected unowned [mcp_servers.personal] preserved:\n%s", out)
	}
	if _, ok := mcps["github"]; !ok {
		t.Fatalf("expected managed [mcp_servers.github] added:\n%s", out)
	}
}

func TestMergeTOMLUpdateConflictAndSkip(t *testing.T) {
	t.Parallel()

	// 1. Clean update of unmodified owned key without invoking resolveConflict.
	ms1 := &adapters.TOMLMergeSpec{
		TableEntries: map[string]any{
			"mcp_servers.gh": map[string]any{
				"url":     "https://v1.example.com",
				"timeout": 30,
			},
		},
	}
	out1, rec1, _, err := MergeTOML(nil, false, ms1, nil, nil)
	if err != nil {
		t.Fatalf("initial MergeTOML failed: %v", err)
	}

	// Re-merge with float64(30.0) vs int(30) must NOT trigger conflict or drift.
	ms1Float := &adapters.TOMLMergeSpec{
		TableEntries: map[string]any{
			"mcp_servers.gh": map[string]any{
				"timeout": float64(30),
				"url":     "https://v1.example.com",
			},
		},
	}
	out1b, _, _, err := MergeTOML(out1, true, ms1Float, &rec1, func(detail string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("unexpected conflict on canonical equivalence: %s", detail)
	})
	if err != nil {
		t.Fatalf("canonical equivalence MergeTOML failed: %v", err)
	}
	if string(out1b) != string(out1) {
		t.Fatalf("expected byte-identical output for canonically equal TOML values")
	}

	// Update owned key cleanly without prompting.
	ms2 := &adapters.TOMLMergeSpec{
		TableEntries: map[string]any{
			"mcp_servers.gh": map[string]any{
				"url":     "https://v2.example.com",
				"timeout": 60,
			},
		},
	}
	out2, rec2, _, err := MergeTOML(out1, true, ms2, &rec1, func(detail string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("unexpected conflict on clean update: %s", detail)
	})
	if err != nil {
		t.Fatalf("clean update failed: %v", err)
	}
	if !strings.Contains(string(out2), "https://v2.example.com") {
		t.Fatalf("expected updated url in output:\n%s", out2)
	}

	// 2. User edits owned table on disk -> resolveConflict must be called.
	tampered := strings.ReplaceAll(string(out2), "https://v2.example.com", "https://user-edited.example.com")
	var pe *spec.PolicyError
	_, _, _, err = MergeTOML([]byte(tampered), true, ms2, &rec2, func(detail string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("conflict: %s", detail)
	})
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on user-modified owned table, got %v", err)
	}

	// ChoiceSkip (overwrite=false) preserves user edit on disk.
	skippedOut, _, _, err := MergeTOML([]byte(tampered), true, ms2, &rec2, func(detail string) (bool, bool, error) {
		return false, false, nil
	})
	if err != nil {
		t.Fatalf("ChoiceSkip MergeTOML failed: %v", err)
	}
	if !strings.Contains(string(skippedOut), "https://user-edited.example.com") {
		t.Fatalf("expected user-edited value preserved on ChoiceSkip:\n%s", skippedOut)
	}

	// 3. Unowned colliding table on disk -> resolveConflict called; backup=true propagated.
	unowned := []byte("[mcp_servers.gh]\nurl = \"https://handwritten.example.com\"\n")
	_, _, _, err = MergeTOML(unowned, true, ms2, nil, func(detail string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("collision: %s", detail)
	})
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on unowned collision, got %v", err)
	}

	overwritten, recOW, needsBackup, err := MergeTOML(unowned, true, ms2, nil, func(detail string) (bool, bool, error) {
		return true, true, nil
	})
	if err != nil {
		t.Fatalf("overwrite MergeTOML failed: %v", err)
	}
	if !needsBackup {
		t.Fatalf("expected needsBackup=true when resolver requests backup")
	}
	if !strings.Contains(string(overwritten), "https://v2.example.com") || recOW.CreatedByAgentfile {
		t.Fatalf("unexpected overwritten output or CreatedByAgentfile=%v:\n%s", recOW.CreatedByAgentfile, overwritten)
	}
}

func TestMergeTOMLPruneAndRemoveTOMLCleanup(t *testing.T) {
	t.Parallel()

	// 1. Existing user file with model = "gpt-5": add two mcp_servers, prune one, then RemoveTOML the last.
	existing := []byte("model = \"gpt-5\"\n")
	ms1 := &adapters.TOMLMergeSpec{
		TableEntries: map[string]any{
			"mcp_servers.a": map[string]any{"url": "https://a.example.com"},
			"mcp_servers.b": map[string]any{"url": "https://b.example.com"},
		},
	}
	out1, rec1, _, err := MergeTOML(existing, true, ms1, nil, nil)
	if err != nil {
		t.Fatalf("MergeTOML 1 failed: %v", err)
	}

	// Remove "mcp_servers.b" via MergeTOML.
	ms2 := &adapters.TOMLMergeSpec{
		TableEntries: map[string]any{
			"mcp_servers.a": map[string]any{"url": "https://a.example.com"},
		},
	}
	out2, rec2, _, err := MergeTOML(out1, true, ms2, &rec1, nil)
	if err != nil {
		t.Fatalf("MergeTOML prune failed: %v", err)
	}
	if strings.Contains(string(out2), "b.example.com") || !strings.Contains(string(out2), "a.example.com") {
		t.Fatalf("expected mcp_servers.b pruned and mcp_servers.a kept:\n%s", out2)
	}

	// RemoveTOML removes remaining owned key and cleans up emptied [mcp_servers] parent table.
	afterRemove, deleteFile, err := RemoveTOML(out2, rec2)
	if err != nil {
		t.Fatalf("RemoveTOML failed: %v", err)
	}
	if deleteFile {
		t.Fatalf("expected deleteFile=false when user key remains")
	}
	if strings.Contains(string(afterRemove), "mcp_servers") {
		t.Fatalf("expected empty [mcp_servers] parent table cleaned up, got:\n%s", afterRemove)
	}
	if !strings.Contains(string(afterRemove), `model = 'gpt-5'`) && !strings.Contains(string(afterRemove), `model = "gpt-5"`) {
		t.Fatalf("expected user key model preserved:\n%s", afterRemove)
	}

	// 2. Agentfile-created file returns deleteFile=true when emptied by RemoveTOML.
	freshOut, freshRec, _, err := MergeTOML(nil, false, ms2, nil, nil)
	if err != nil {
		t.Fatalf("fresh MergeTOML failed: %v", err)
	}
	_, delFresh, err := RemoveTOML(freshOut, freshRec)
	if err != nil {
		t.Fatalf("RemoveTOML on fresh file failed: %v", err)
	}
	if !delFresh {
		t.Fatalf("expected deleteFile=true when agentfile-created TOML becomes empty")
	}

	// 3. Stale key modified on disk with ChoiceSkip preserves user edit and drops ownership.
	tamperedB := strings.ReplaceAll(string(out1), "https://b.example.com", "https://user-kept.example.com")
	skippedPrune, skippedRec, _, err := MergeTOML([]byte(tamperedB), true, ms2, &rec1, func(string) (bool, bool, error) {
		return false, false, nil
	})
	if err != nil {
		t.Fatalf("stale key ChoiceSkip failed: %v", err)
	}
	if !strings.Contains(string(skippedPrune), "https://user-kept.example.com") {
		t.Fatalf("expected user-modified stale key preserved on ChoiceSkip:\n%s", skippedPrune)
	}
	if len(skippedRec.TOMLKeys) != 1 || skippedRec.TOMLKeys[0] != "mcp_servers.a" {
		t.Fatalf("expected ownership relinquished on skipped stale key, got %v", skippedRec.TOMLKeys)
	}
}

func TestMergeTOMLAndRemoveTOMLInvalidSyntax(t *testing.T) {
	t.Parallel()

	var pe *spec.PolicyError
	_, _, _, err := MergeTOML([]byte("not = [valid toml"), true, &adapters.TOMLMergeSpec{}, nil, nil)
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on invalid TOML in MergeTOML, got %v", err)
	}

	_, _, err = RemoveTOML([]byte("not = [valid toml"), ArtifactRecord{ArtifactType: TypeTOMLPatch})
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on invalid TOML in RemoveTOML, got %v", err)
	}

	// Non-table parent fails closed with PolicyError.
	_, _, _, err = MergeTOML([]byte("mcp_servers = \"scalar\"\n"), true, &adapters.TOMLMergeSpec{
		TableEntries: map[string]any{"mcp_servers.gh": map[string]any{"url": "https://x"}},
	}, nil, nil)
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError on non-table parent, got %v", err)
	}
}

func TestStateCloneAndEqualForTOMLFields(t *testing.T) {
	t.Parallel()

	orig := ArtifactRecord{
		ArtifactType:       TypeTOMLPatch,
		SHA256:             "sha256:abc",
		TOMLKeys:           []string{"mcp_servers.a", "mcp_servers.b"},
		TOMLKeyHashes:      map[string]string{"mcp_servers.a": "h1", "mcp_servers.b": "h2"},
		CreatedByAgentfile: true,
	}
	cloned := cloneArtifactRecord(orig)
	if !recordsEqual(orig, cloned) {
		t.Fatalf("expected cloned record equal to original")
	}

	// Mutating clone must not mutate original.
	cloned.TOMLKeys[0] = "mutated"
	cloned.TOMLKeyHashes["mcp_servers.a"] = "changed"
	if orig.TOMLKeys[0] != "mcp_servers.a" || orig.TOMLKeyHashes["mcp_servers.a"] != "h1" {
		t.Fatalf("cloneArtifactRecord did not deep-clone TOML fields")
	}
	if recordsEqual(orig, cloned) {
		t.Fatalf("expected recordsEqual=false when TOML fields differ")
	}
}

func TestParseTOMLPathAndDottedServerNames(t *testing.T) {
	t.Parallel()

	// Valid path parsing (unquoted, quoted dotted, escaped quotes/backslashes).
	good := []struct {
		in   string
		want []string
	}{
		{"mcp_servers.github", []string{"mcp_servers", "github"}},
		{`mcp_servers."acme.github"`, []string{"mcp_servers", "acme.github"}},
		{`mcp_servers."a\"b\\c".url`, []string{"mcp_servers", `a"b\c`, "url"}},
	}
	for _, tc := range good {
		got, err := parseTOMLPath(tc.in)
		if err != nil {
			t.Fatalf("parseTOMLPath(%q) unexpected error: %v", tc.in, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("parseTOMLPath(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("parseTOMLPath(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}

	// Invalid paths must return PolicyError.
	bad := []string{
		"",
		".mcp_servers",
		"mcp_servers.",
		"mcp_servers..github",
		`mcp_servers."unterminated`,
		`mcp_servers."foo"bar`,
		`mcp_servers.foo"bar`,
	}
	for _, p := range bad {
		if _, err := parseTOMLPath(p); err == nil {
			t.Fatalf("parseTOMLPath(%q) expected error, got nil", p)
		}
	}

	// MergeTOML + RemoveTOML with dotted MCP server name mcp_servers."acme.github".
	ms := &adapters.TOMLMergeSpec{
		TableEntries: map[string]any{
			`mcp_servers."acme.github"`: map[string]any{
				"url": "https://acme.example.com/mcp",
			},
		},
	}
	out, rec, _, err := MergeTOML(nil, false, ms, nil, nil)
	if err != nil {
		t.Fatalf("MergeTOML dotted failed: %v", err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("invalid TOML output: %v\n%s", err, out)
	}
	mcps := doc["mcp_servers"].(map[string]any)
	srv, ok := mcps["acme.github"].(map[string]any)
	if !ok || srv["url"] != "https://acme.example.com/mcp" {
		t.Fatalf("expected single key acme.github under mcp_servers (not nested acme.github), got %+v\n%s", mcps, out)
	}

	// Idempotent resync.
	out2, _, _, err := MergeTOML(out, true, ms, &rec, func(detail string) (bool, bool, error) {
		return false, false, spec.NewPolicyError("unexpected conflict: %s", detail)
	})
	if err != nil || string(out2) != string(out) {
		t.Fatalf("expected idempotent dotted TOML resync, err=%v\nout1:\n%s\nout2:\n%s", err, out, out2)
	}

	// RemoveTOML cleans up dotted key and empty file.
	_, del, err := RemoveTOML(out, rec)
	if err != nil || !del {
		t.Fatalf("expected RemoveTOML deleteFile=true, got del=%v err=%v", del, err)
	}
}
