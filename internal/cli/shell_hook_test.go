package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShellHookGenerationAndSubprocessExecution(t *testing.T) {
	setupHermeticTestEnv(t)

	var out, errOut bytes.Buffer
	// 1. Default zsh shell-hook wraps CLI session targets (claude, opencode, gemini, codex)
	// and excludes BinaryOptional desktop IDE binaries (code, cursor, windsurf).
	if code := ExecuteWithIO([]string{"shell-hook", "zsh"}, t.TempDir(), strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("shell-hook zsh failed: code=%d err=%s", code, errOut.String())
	}
	zshHook := out.String()
	for _, want := range []string{"claude()", "opencode()", "gemini()", "codex()", "unalias claude"} {
		if !strings.Contains(zshHook, want) {
			t.Fatalf("expected %q in zsh shell-hook:\n%s", want, zshHook)
		}
	}
	for _, excluded := range []string{"\ncode()", "\ncursor()", "\nwindsurf()"} {
		if strings.Contains(zshHook, excluded) {
			t.Fatalf("did not expect desktop IDE binary %q wrapped by default:\n%s", excluded, zshHook)
		}
	}

	// 2. Explicit --targets copilot and --only-isolated works, and fish syntax works.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"shell-hook", "fish", "--targets", "claude,copilot", "--only-isolated"}, t.TempDir(), strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
		t.Fatalf("shell-hook fish failed: code=%d err=%s", code, errOut.String())
	}
	fishHook := out.String()
	if !strings.Contains(fishHook, "function claude") || !strings.Contains(fishHook, "function code") || !strings.Contains(fishHook, "--only-isolated") {
		t.Fatalf("unexpected fish hook output:\n%s", fishHook)
	}

	// Unsupported shell returns exit code 2.
	out.Reset()
	errOut.Reset()
	if code := ExecuteWithIO([]string{"shell-hook", "powershell"}, t.TempDir(), strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 2 {
		t.Fatalf("expected exit 2 on unsupported shell, got %d", code)
	}

	if runtime.GOOS == "windows" {
		return
	}

	// 3. Live bash/zsh subprocess execution test:
	// Verify pre-existing alias safety, subdirectory upward walk to Agentfile.yaml,
	// AGENTFILE_ACTIVE=1 + AGENTFILE_ROOT recursion bypass, and non-repo pass-through.
	binDir := t.TempDir()
	fakeAgentfile := filepath.Join(binDir, "agentfile")
	fakeClaude := filepath.Join(binDir, "claude")

	_ = os.WriteFile(fakeAgentfile, []byte("#!/bin/sh\necho \"VIA_AGENTFILE:$*\"\n"), 0o755)
	_ = os.WriteFile(fakeClaude, []byte("#!/bin/sh\necho \"DIRECT_CLAUDE:$*\"\n"), 0o755)

	repoRoot := t.TempDir()
	if eval, err := filepath.EvalSymlinks(repoRoot); err == nil {
		repoRoot = eval
	}
	subDir := filepath.Join(repoRoot, "services", "api")
	_ = os.MkdirAll(subDir, 0o755)
	_ = os.WriteFile(filepath.Join(repoRoot, "Agentfile.yaml"), []byte("version: 1\ntargets: [claude]\n"), 0o644)
	outsideDir := t.TempDir()

	for _, sh := range []string{"bash", "zsh"} {
		if _, err := exec.LookPath(sh); err != nil {
			continue
		}
		out.Reset()
		errOut.Reset()
		if code := ExecuteWithIO([]string{"shell-hook", sh, "--targets", "claude"}, repoRoot, strings.NewReader(""), &out, &errOut, func() bool { return false }); code != 0 {
			t.Fatalf("shell-hook %s failed: %v", sh, errOut.String())
		}
		hookScript := out.String()

		// (a) From subdirectory inside repo (even with a pre-existing alias claude='echo bad'): routes through agentfile run.
		cmdSub := exec.Command(sh, "-c", "alias claude='echo bad'\n"+hookScript+"\nclaude -p 'hello world'")
		cmdSub.Dir = subDir
		cmdSub.Env = append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"HOME="+t.TempDir(),
			"CI=",
			"AGENTFILE_ACTIVE=",
			"AGENTFILE_ROOT=",
			"AGENTFILE_NO_HOOK=",
		)
		subOut, err := cmdSub.CombinedOutput()
		if err != nil {
			t.Fatalf("%s subdirectory hook execution failed: %v\n%s", sh, err, subOut)
		}
		if !strings.Contains(string(subOut), "VIA_AGENTFILE:run --if-configured --project-root "+repoRoot+" claude -p hello world") {
			t.Fatalf("unexpected %s subdirectory output: %q", sh, string(subOut))
		}

		// (b) When AGENTFILE_ACTIVE=1 and AGENTFILE_ROOT==repoRoot: bypasses directly to command claude.
		cmdActive := exec.Command(sh, "-c", hookScript+"\nclaude -p 'inside session'")
		cmdActive.Dir = subDir
		cmdActive.Env = append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"HOME="+t.TempDir(),
			"CI=",
			"AGENTFILE_ACTIVE=1",
			"AGENTFILE_ROOT="+repoRoot,
		)
		actOut, err := cmdActive.CombinedOutput()
		if err != nil {
			t.Fatalf("%s active bypass failed: %v\n%s", sh, err, actOut)
		}
		if !strings.Contains(string(actOut), "DIRECT_CLAUDE:-p inside session") {
			t.Fatalf("expected DIRECT_CLAUDE when AGENTFILE_ACTIVE=1 in same repo, got %q", string(actOut))
		}

		// (c) Outside any Agentfile.yaml repo: bypasses directly to command claude without invoking agentfile.
		cmdOutside := exec.Command(sh, "-c", hookScript+"\nclaude --version")
		cmdOutside.Dir = outsideDir
		cmdOutside.Env = append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"HOME="+t.TempDir(),
			"CI=",
			"AGENTFILE_ACTIVE=",
		)
		outBytes, err := cmdOutside.CombinedOutput()
		if err != nil {
			t.Fatalf("%s outside execution failed: %v\n%s", sh, err, outBytes)
		}
		if !strings.Contains(string(outBytes), "DIRECT_CLAUDE:--version") {
			t.Fatalf("expected DIRECT_CLAUDE outside repo, got %q", string(outBytes))
		}
	}
}
