package diff

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedDiff(t *testing.T) {
	t.Run("identical inputs", func(t *testing.T) {
		data := []byte("alpha\nbeta\ngamma\n")
		text, changed, err := UnifiedDiff("config.json", data, data, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if changed || text != "" {
			t.Fatalf("expected unchanged empty diff, got changed=%v text=%q", changed, text)
		}
	})

	t.Run("create file", func(t *testing.T) {
		newData := []byte("line1\nline2\n")
		text, changed, err := UnifiedDiff("dir/new.txt", nil, newData, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Fatalf("expected changed=true")
		}
		if !strings.Contains(text, "--- /dev/null\n+++ b/dir/new.txt\n") {
			t.Fatalf("missing create headers in diff:\n%s", text)
		}
		if !strings.Contains(text, "@@ -0,0 +1,2 @@\n+line1\n+line2\n") {
			t.Fatalf("unexpected create hunk in diff:\n%s", text)
		}
	})

	t.Run("delete file", func(t *testing.T) {
		oldData := []byte("line1\nline2\n")
		text, changed, err := UnifiedDiff("dir/old.txt", oldData, nil, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Fatalf("expected changed=true")
		}
		if !strings.Contains(text, "--- a/dir/old.txt\n+++ /dev/null\n") {
			t.Fatalf("missing delete headers in diff:\n%s", text)
		}
		if !strings.Contains(text, "@@ -1,2 +0,0 @@\n-line1\n-line2\n") {
			t.Fatalf("unexpected delete hunk in diff:\n%s", text)
		}
	})

	t.Run("multi-hunk modifications with 3-line context", func(t *testing.T) {
		var oldLines, newLines []string
		for i := 1; i <= 15; i++ {
			line := "line_" + string(rune('A'+i-1))
			oldLines = append(oldLines, line)
			switch i {
			case 2:
				newLines = append(newLines, "line_B_mod")
			case 14:
				newLines = append(newLines, "line_N_mod")
			default:
				newLines = append(newLines, line)
			}
		}
		oldBytes := []byte(strings.Join(oldLines, "\n") + "\n")
		newBytes := []byte(strings.Join(newLines, "\n") + "\n")

		text, changed, err := UnifiedDiff("multi.txt", oldBytes, newBytes, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Fatalf("expected changed=true")
		}
		if count := strings.Count(text, "@@ "); count != 2 {
			t.Fatalf("expected 2 separate hunks, got %d:\n%s", count, text)
		}
		if !strings.Contains(text, "-line_B\n+line_B_mod\n") || !strings.Contains(text, "-line_N\n+line_N_mod\n") {
			t.Fatalf("missing expected edits in diff:\n%s", text)
		}
		if strings.Contains(text, "line_G") || strings.Contains(text, "line_I") {
			t.Fatalf("unexpected middle lines outside 3-line context:\n%s", text)
		}
	})

	t.Run("CRLF normalization", func(t *testing.T) {
		crlf := []byte("first\r\nsecond\r\n")
		lf := []byte("first\nsecond\n")
		text, changed, err := UnifiedDiff("crlf.txt", crlf, lf, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if changed || text != "" {
			t.Fatalf("expected CRLF vs LF identical text to report unchanged, got changed=%v text=%q", changed, text)
		}

		crlfMod := []byte("first\r\nsecond_mod\r\n")
		text2, changed2, err2 := UnifiedDiff("crlf.txt", crlf, crlfMod, 3)
		if err2 != nil {
			t.Fatalf("unexpected error: %v", err2)
		}
		if !changed2 {
			t.Fatalf("expected changed=true")
		}
		if strings.Contains(text2, "\r") {
			t.Fatalf("expected CR characters normalized out of diff output: %q", text2)
		}
		if !strings.Contains(text2, "-second\n+second_mod\n") {
			t.Fatalf("unexpected diff output:\n%s", text2)
		}
	})

	t.Run("binary NUL-byte guard", func(t *testing.T) {
		oldBin := []byte("PNG\x00\x01\x02")
		newBin := []byte("PNG\x00\x01\x03")
		text, changed, err := UnifiedDiff("asset.png", oldBin, newBin, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Fatalf("expected changed=true for differing binary inputs")
		}
		if strings.ContainsRune(text, '\x00') {
			t.Fatalf("diff output must not contain NUL bytes")
		}
		if !strings.Contains(strings.ToLower(text), "binary") {
			t.Fatalf("expected binary notice in diff output, got:\n%s", text)
		}
	})

	t.Run("invalid UTF-8 guard", func(t *testing.T) {
		oldBad := []byte{0xff, 0xfe, 'a', '\n'}
		newGood := []byte("valid\n")
		text, changed, err := UnifiedDiff("bad.txt", oldBad, newGood, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Fatalf("expected changed=true for invalid UTF-8 vs valid text")
		}
		if !strings.Contains(strings.ToLower(text), "utf-8") && !strings.Contains(strings.ToLower(text), "binary") {
			t.Fatalf("expected non-UTF-8/binary notice in diff output, got:\n%s", text)
		}
	})

	t.Run("> 1 MiB file guard", func(t *testing.T) {
		large := bytes.Repeat([]byte("a\n"), (MaxDiffFileBytes/2)+10)
		small := []byte("small\n")
		text, changed, err := UnifiedDiff("huge.log", small, large, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Fatalf("expected changed=true for >1 MiB file")
		}
		if len(text) > 1024 {
			t.Fatalf("expected concise size-limit notice, got %d bytes", len(text))
		}
		if !strings.Contains(text, "1 MiB") {
			t.Fatalf("expected 1 MiB limit notice in diff output, got:\n%s", text)
		}
	})

	t.Run("matrix cap fallback", func(t *testing.T) {
		// 2500 x 2500 = 6,250,000 > MaxMyersMatrix (4,000,000), yet well under MaxDiffFileBytes.
		var oldBuf, newBuf strings.Builder
		for i := 0; i < 2500; i++ {
			oldBuf.WriteString("old_line_content\n")
			newBuf.WriteString("new_line_content\n")
		}
		oldBytes := []byte(oldBuf.String())
		newBytes := []byte(newBuf.String())
		if len(oldBytes) > MaxDiffFileBytes || len(newBytes) > MaxDiffFileBytes {
			t.Fatalf("test setup exceeded MaxDiffFileBytes: %d / %d", len(oldBytes), len(newBytes))
		}

		text, changed, err := UnifiedDiff("matrix.txt", oldBytes, newBytes, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Fatalf("expected changed=true")
		}
		if !strings.Contains(text, "--- a/matrix.txt\n+++ b/matrix.txt\n") {
			t.Fatalf("missing headers in fallback diff")
		}
		if !strings.Contains(text, "-old_line_content\n") || !strings.Contains(text, "+new_line_content\n") {
			t.Fatalf("missing edit lines in fallback diff")
		}
	})
}

func TestDiffTreesLstatSymlinkSafety(t *testing.T) {
	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("DO_NOT_READ_ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldTree := t.TempDir()
	newTree := t.TempDir()

	// Matching regular file in both trees.
	if err := os.WriteFile(filepath.Join(oldTree, "same.md"), []byte("# Same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newTree, "same.md"), []byte("# Same\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Matching symlink pointing outside relRoot in both trees -> must report unchanged and never read outsideSecret.
	if err := os.Symlink(outsideSecret, filepath.Join(oldTree, "ext-link")); err != nil {
		t.Skipf("symlinks unsupported in this environment: %v", err)
	}
	if err := os.Symlink(outsideSecret, filepath.Join(newTree, "ext-link")); err != nil {
		t.Fatal(err)
	}

	text, changed, filesChanged, additions, deletions, err := DiffTrees(".claude/skills/demo", oldTree, newTree, true, true, 3)
	if err != nil {
		t.Fatalf("unexpected error on matching trees: %v", err)
	}
	if changed || filesChanged != 0 || additions != 0 || deletions != 0 || text != "" {
		t.Fatalf("expected identical trees with symlink to be unchanged; got changed=%v files=%d +%d -%d text=%q",
			changed, filesChanged, additions, deletions, text)
	}

	// Now add a new symlink in newTree pointing outside relRoot, and modify a file.
	if err := os.Symlink(outsideDir, filepath.Join(newTree, "escape-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newTree, "same.md"), []byte("# Same\nupdated\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	text, changed, filesChanged, additions, deletions, err = DiffTrees(".claude/skills/demo", oldTree, newTree, true, true, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed || filesChanged != 2 {
		t.Fatalf("expected changed=true filesChanged=2, got changed=%v filesChanged=%d\n%s", changed, filesChanged, text)
	}
	if additions < 2 {
		t.Fatalf("expected at least 2 additions, got %d", additions)
	}
	if strings.Contains(text, "DO_NOT_READ_ME") {
		t.Fatalf("symlink target was followed! secret content leaked into diff:\n%s", text)
	}
	if !strings.Contains(text, "symlink -> "+outsideDir) {
		t.Fatalf("expected symlink metadata in diff output:\n%s", text)
	}

	// Verify mode change helper as well.
	modeText := FormatModeChangeDiff(".claude/skills/demo/item", "file", "symlink", "", "/tmp/target")
	if !strings.Contains(modeText, "-mode: file\n+mode: symlink -> /tmp/target\n") {
		t.Fatalf("unexpected mode change diff:\n%s", modeText)
	}
}

func TestRedactSecretsAndPlaceholders(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "multi-match single JSON line",
			in:   `{"token": "abc", "api_key": "xyz"}`,
			want: `{"token": "[REDACTED]", "api_key": "[REDACTED]"}`,
		},
		{
			name: "HTTP Bearer",
			in:   `Authorization: Bearer secret-tok-123`,
			want: `Authorization: Bearer [REDACTED]`,
		},
		{
			name: "HTTP Basic",
			in:   `Authorization: Basic dXNlcjpwYXNz`,
			want: `Authorization: Basic [REDACTED]`,
		},
		{
			name: "URL raw password",
			in:   `https://user:rawpass@host/repo.git`,
			want: `https://user:[REDACTED]@host/repo.git`,
		},
		{
			name: "URL placeholder password preserved",
			in:   `https://user:${PASS}@host/repo.git`,
			want: `https://user:${PASS}@host/repo.git`,
		},
		{
			name: "ghp_ prefix",
			in:   `token is ghp_secret123456 here`,
			want: `token is [REDACTED] here`,
		},
		{
			name: "github_pat_ prefix",
			in:   `pat=github_pat_11ABCDEFG_0123456789`,
			want: `pat=[REDACTED]`,
		},
		{
			name: "sk- prefix",
			in:   `key is sk-proj-1234567890abcdef`,
			want: `key is [REDACTED]`,
		},
		{
			name: "AKIA prefix",
			in:   `aws_key=AKIAIOSFODNN7EXAMPLE`,
			want: `aws_key=[REDACTED]`,
		},
		{
			name: "preserve Claude/Gemini ${VAR}",
			in:   `"Authorization": "Bearer ${GITHUB_PAT}"`,
			want: `"Authorization": "Bearer ${GITHUB_PAT}"`,
		},
		{
			name: "preserve OpenCode {env:VAR}",
			in:   `"OPENAI_API_KEY": "{env:OPENAI_API_KEY}"`,
			want: `"OPENAI_API_KEY": "{env:OPENAI_API_KEY}"`,
		},
		{
			name: "preserve Cursor ${env:VAR}",
			in:   `"API_KEY": "${env:CURSOR_API_KEY}"`,
			want: `"API_KEY": "${env:CURSOR_API_KEY}"`,
		},
		{
			name: "preserve benign ${LOG_LEVEL:-info}",
			in:   `"LOG_LEVEL": "${LOG_LEVEL:-info}"`,
			want: `"LOG_LEVEL": "${LOG_LEVEL:-info}"`,
		},
		{
			name: "redact literal secret inside ${TOKEN:-ghp_secret123456}",
			in:   `"GITHUB_TOKEN": "${TOKEN:-ghp_secret123456}"`,
			want: `"GITHUB_TOKEN": "${TOKEN:-[REDACTED]}"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Fatalf("RedactSecrets(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
			// Must be idempotent.
			if again := RedactSecrets(got); again != got {
				t.Fatalf("RedactSecrets is not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestRedactUnifiedDiffSecretRotation(t *testing.T) {
	oldJSON := []byte("{\n  \"name\": \"app\",\n  \"API_KEY\": \"old_secret_1\",\n  \"region\": \"us-east-1\"\n}\n")
	newJSON := []byte("{\n  \"name\": \"app\",\n  \"API_KEY\": \"new_secret_2\",\n  \"region\": \"us-east-1\"\n}\n")

	rawDiff, changed, err := UnifiedDiff(".mcp.json", oldJSON, newJSON, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}

	redacted := RedactUnifiedDiff(rawDiff)
	if strings.Contains(redacted, "old_secret_1") || strings.Contains(redacted, "new_secret_2") {
		t.Fatalf("secret leaked in redacted diff:\n%s", redacted)
	}
	if !strings.Contains(redacted, "-  \"API_KEY\": \"[REDACTED]\",\n") {
		t.Fatalf("expected masked - line in redacted diff:\n%s", redacted)
	}
	if !strings.Contains(redacted, "+  \"API_KEY\": \"[REDACTED]\",\n") {
		t.Fatalf("expected masked + line in redacted diff:\n%s", redacted)
	}
	if !strings.Contains(redacted, "--- a/.mcp.json\n+++ b/.mcp.json\n") || !strings.Contains(redacted, "@@ ") {
		t.Fatalf("expected diff headers preserved:\n%s", redacted)
	}
}

func TestContainsLiteralSecret(t *testing.T) {
	shouldDetect := []string{
		"ghp_secret123456",
		"Bearer rawtok",
		"https://u:p@h",
		`${TOKEN:-ghp_secret123456}`,
		`{"api_key": "xyz"}`,
	}
	for _, s := range shouldDetect {
		if !ContainsLiteralSecret(s) {
			t.Errorf("expected ContainsLiteralSecret(%q) == true", s)
		}
	}

	shouldIgnore := []string{
		"Bearer ${GITHUB_PAT}",
		"${VAR}",
		"{env:VAR}",
		"${env:VAR}",
		"${LOG_LEVEL:-info}",
		"https://user:${PASS}@host",
		"Authorization: Bearer [REDACTED]",
	}
	for _, s := range shouldIgnore {
		if ContainsLiteralSecret(s) {
			t.Errorf("expected ContainsLiteralSecret(%q) == false", s)
		}
	}
}

func TestDiffAndTreeEdgeCases(t *testing.T) {
	t.Run("empty relPath error", func(t *testing.T) {
		if _, _, err := UnifiedDiff("", []byte("a\n"), []byte("b\n"), -1); err == nil {
			t.Fatalf("expected error on empty relPath")
		}
	})

	t.Run("no trailing newline handling", func(t *testing.T) {
		oldB := []byte("alpha\nbeta")
		newB := []byte("alpha\nbeta\n")
		text, changed, err := UnifiedDiff("nnl.txt", oldB, newB, -1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed || !strings.Contains(text, `\ No newline at end of file`) {
			t.Fatalf("expected no-newline marker in diff:\n%s", text)
		}

		// Both lack trailing newline on changed final line.
		text2, changed2, err2 := UnifiedDiff("nnl2.txt", []byte("a\nb"), []byte("a\nc"), 3)
		if err2 != nil || !changed2 || strings.Count(text2, `\ No newline at end of file`) != 2 {
			t.Fatalf("expected 2 no-newline markers, got err=%v changed=%v:\n%s", err2, changed2, text2)
		}

		// RedactUnifiedDiff preserves \ No newline marker and handles empty input.
		if got := RedactUnifiedDiff(""); got != "" {
			t.Fatalf("expected empty RedactUnifiedDiff, got %q", got)
		}
		red := RedactUnifiedDiff(text)
		if !strings.Contains(red, `\ No newline at end of file`) {
			t.Fatalf("expected no-newline marker preserved:\n%s", red)
		}
	})

	t.Run("mid-file pure insertion and deletion", func(t *testing.T) {
		oldB := []byte("a\nb\nc\nd\n")
		newB := []byte("a\nb\ninserted\nc\nd\n")
		text, changed, err := UnifiedDiff("ins.txt", oldB, newB, 0)
		if err != nil || !changed || !strings.Contains(text, "@@ -2,0 +3,1 @@\n+inserted\n") {
			t.Fatalf("unexpected insertion diff (err=%v changed=%v):\n%s", err, changed, text)
		}

		textDel, changedDel, errDel := UnifiedDiff("del.txt", newB, oldB, 0)
		if errDel != nil || !changedDel || !strings.Contains(textDel, "@@ -3,1 +2,0 @@\n-inserted\n") {
			t.Fatalf("unexpected deletion diff (err=%v changed=%v):\n%s", errDel, changedDel, textDel)
		}
	})

	t.Run("FormatSymlinkDiff variants", func(t *testing.T) {
		if got := FormatSymlinkDiff("a", "t", "t"); got != "" {
			t.Fatalf("expected empty for equal targets, got %q", got)
		}
		if got := FormatSymlinkDiff("a", "old", ""); !strings.Contains(got, "+++ /dev/null\n@@ -1,1 +0,0 @@\n-symlink -> old\n") {
			t.Fatalf("unexpected delete symlink diff:\n%s", got)
		}
		if got := FormatSymlinkDiff("a", "old", "new"); !strings.Contains(got, "-symlink -> old\n+symlink -> new\n") {
			t.Fatalf("unexpected modify symlink diff:\n%s", got)
		}
		mode := FormatModeChangeDiff("a", "symlink", "file", "/old/sym", "")
		if !strings.Contains(mode, "-mode: symlink -> /old/sym\n+mode: file\n") {
			t.Fatalf("unexpected symlink->file mode diff:\n%s", mode)
		}
	})

	t.Run("DiffTrees error, root-symlink, and truncation", func(t *testing.T) {
		if _, _, _, _, _, err := DiffTrees("r", filepath.Join(t.TempDir(), "missing"), "", true, false, 3); err == nil {
			t.Fatalf("expected error when oldExists=true on missing dir")
		}
		if _, _, _, _, _, err := DiffTrees("r", "", filepath.Join(t.TempDir(), "missing"), false, true, 3); err == nil {
			t.Fatalf("expected error when newExists=true on missing dir")
		}

		// Same-size differing >1 MiB files trigger filesEqualStream returning false.
		d1, d2 := t.TempDir(), t.TempDir()
		big1 := bytes.Repeat([]byte("0123456789abcdef"), (MaxDiffFileBytes/16)+10)
		big2 := append([]byte{}, big1...)
		big2[len(big2)-1] = 'X'
		if err := os.WriteFile(filepath.Join(d1, "same-len.bin"), big1, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d2, "same-len.bin"), big2, 0o644); err != nil {
			t.Fatal(err)
		}
		text, ch, fc, _, _, err := DiffTrees(".", d1, d2, true, true, 3)
		if err != nil || !ch || fc != 1 || !strings.Contains(text, "exceeds 1 MiB diff limit") {
			t.Fatalf("unexpected same-size >1MiB result: err=%v ch=%v fc=%d text=%q", err, ch, fc, text)
		}

		// Root itself as a regular file vs symlink.
		fRoot := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(fRoot, []byte("hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if text, ch, fc, _, _, err := DiffTrees("file", "", fRoot, false, true, 3); err != nil || !ch || fc != 1 || !strings.Contains(text, "+hi\n") {
			t.Fatalf("unexpected file-root DiffTrees: err=%v ch=%v fc=%d text=%q", err, ch, fc, text)
		}
		sRoot := filepath.Join(t.TempDir(), "symroot")
		if err := os.Symlink("/target", sRoot); err == nil {
			if text, ch, fc, _, _, err := DiffTrees("", "", sRoot, false, true, 3); err != nil || !ch || fc != 1 || !strings.Contains(text, "symlink -> /target") {
				t.Fatalf("unexpected symlink-root DiffTrees: err=%v ch=%v fc=%d text=%q", err, ch, fc, text)
			}
		}

		// MaxTotalDiffBytes truncation across 12 ~950 KiB files.
		truncDir := t.TempDir()
		chunk950 := bytes.Repeat([]byte("0123456789abcdef0123456789abcde\n"), 30_000)
		for i := 0; i < 12; i++ {
			name := filepath.Join(truncDir, "f"+string(rune('a'+i))+".txt")
			if err := os.WriteFile(name, chunk950, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		truncText, ch, fc, _, _, err := DiffTrees("bulk", "", truncDir, false, true, 3)
		if err != nil || !ch || fc != 12 || !strings.Contains(truncText, "diff truncated") {
			t.Fatalf("expected truncated diff across 12 large files: err=%v ch=%v fc=%d len=%d", err, ch, fc, len(truncText))
		}
	})

	t.Run("DiffTrees full lifecycle and >1MiB streaming", func(t *testing.T) {
		// Neither exists.
		if text, ch, fc, add, del, err := DiffTrees("r", "", "", false, false, 3); err != nil || ch || fc != 0 || add != 0 || del != 0 || text != "" {
			t.Fatalf("unexpected result for non-existent trees: %v %v %d %d %d %q", err, ch, fc, add, del, text)
		}

		oldDir := t.TempDir()
		newDir := t.TempDir()

		// Old-only file + old-only empty file + old-only subdir + symlink changed + mode change (file -> dir).
		if err := os.WriteFile(filepath.Join(oldDir, "deleted.txt"), []byte("gone\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oldDir, "empty-del.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(oldDir, "oldsub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oldDir, "oldsub", "x.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oldDir, "kind"), []byte("was-file\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		// Large identical >1 MiB file in both trees -> streaming equality check returns unchanged.
		big := bytes.Repeat([]byte("0123456789abcdef"), (MaxDiffFileBytes/16)+10)
		if err := os.WriteFile(filepath.Join(oldDir, "big-same.bin"), big, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(newDir, "big-same.bin"), big, 0o644); err != nil {
			t.Fatal(err)
		}

		// Large modified >1 MiB file.
		bigMod := append([]byte{}, big...)
		bigMod[len(bigMod)-1] = 'Z'
		if err := os.WriteFile(filepath.Join(oldDir, "big-diff.bin"), big, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(newDir, "big-diff.bin"), bigMod, 0o644); err != nil {
			t.Fatal(err)
		}

		// New-only files (normal + empty) + dir replacing file.
		if err := os.WriteFile(filepath.Join(newDir, "added.txt"), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(newDir, "empty-add.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(newDir, "kind"), 0o755); err != nil {
			t.Fatal(err)
		}

		// Symlink retarget and symlink deletion/creation.
		if err := os.Symlink("/old/target", filepath.Join(oldDir, "sym")); err == nil {
			if err := os.Symlink("/new/target", filepath.Join(newDir, "sym")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/del/target", filepath.Join(oldDir, "sym-del")); err != nil {
				t.Fatal(err)
			}
		}

		text, ch, fc, _, _, err := DiffTrees("", oldDir, newDir, true, true, 3)
		if err != nil {
			t.Fatalf("DiffTrees failed: %v", err)
		}
		if !ch || fc < 6 {
			t.Fatalf("expected changed=true and >=6 changed files, got ch=%v fc=%d\n%s", ch, fc, text)
		}
		if strings.Contains(text, "big-same.bin") {
			t.Fatalf("identical >1 MiB file should not appear in diff:\n%s", text)
		}
		if !strings.Contains(text, "big-diff.bin") || !strings.Contains(text, "exceeds 1 MiB diff limit") {
			t.Fatalf("expected size limit notice for big-diff.bin:\n%s", text)
		}
		if !strings.Contains(text, "-mode: file\n+mode: dir\n") {
			t.Fatalf("expected file->dir mode change:\n%s", text)
		}

		// Tree create only (!oldExists, newExists) and delete only (oldExists, !newExists).
		if _, chCreate, _, addCreate, _, err := DiffTrees("root", "", newDir, false, true, 3); err != nil || !chCreate || addCreate == 0 {
			t.Fatalf("expected create-only tree diff to report additions, got err=%v ch=%v add=%d", err, chCreate, addCreate)
		}
		if _, chDel, _, _, delDel, err := DiffTrees("root", oldDir, "", true, false, 3); err != nil || !chDel || delDel == 0 {
			t.Fatalf("expected delete-only tree diff to report deletions, got err=%v ch=%v del=%d", err, chDel, delDel)
		}
	})
}

func TestRedactEdgeCases(t *testing.T) {
	if got := RedactSecrets(""); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}

	// Single-quoted JSON/YAML, PEM key, unquoted booleans, and sentinel prefix collision resistance.
	in := strings.Join([]string{
		`{'token': 'raw1', 'api_key': ''}`,
		`password='raw2' secret="raw3" token=true`,
		`{"Authorization": "Basic ${CREDS}", "other": "Basic rawbasic"}`,
		`__AF_PH_0__ token=raw4 api_key=__AF_PH___`,
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
		`raw line without secrets`,
	}, "\n")
	got := RedactSecrets(in)
	for _, leaked := range []string{"raw1", "raw2", "raw3", "raw4", "rawbasic", "MIIEowIBAAKCAQEA"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("secret %q leaked in output:\n%s", leaked, got)
		}
	}
	if !strings.Contains(got, `"Authorization": "Basic ${CREDS}"`) {
		t.Fatalf("expected Basic placeholder preserved:\n%s", got)
	}
	if !strings.Contains(got, "__AF_PH_0__ token=[REDACTED] api_key=[REDACTED]") {
		t.Fatalf("sentinel-like literal in input was corrupted:\n%s", got)
	}
	if !strings.Contains(got, "token=true") {
		t.Fatalf("unquoted boolean should be preserved:\n%s", got)
	}
	if !strings.Contains(got, "'api_key': ''") {
		t.Fatalf("empty single-quoted value should be preserved:\n%s", got)
	}
	// RedactUnifiedDiff also handles non-marker metadata lines.
	if gotDiff := RedactUnifiedDiff("Binary files token=abc differ\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-token=xyz\n+token=${TOK}\n"); strings.Contains(gotDiff, "abc") || strings.Contains(gotDiff, "xyz") || !strings.Contains(gotDiff, "+token=${TOK}") {
		t.Fatalf("unexpected RedactUnifiedDiff output:\n%s", gotDiff)
	}
}
