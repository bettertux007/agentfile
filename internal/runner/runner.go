package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
)

// ExitCodeError carries a child process's verbatim exit code back to cli.Execute.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("process exited with status %d", e.Code)
}

func (e *ExitCodeError) Unwrap() error {
	return e.Err
}

// ProcessRunner executes or replaces the current process with a target binary.
// argv excludes argv[0] (the binary path itself).
type ProcessRunner interface {
	Run(bin string, argv []string, env []string, dir string) error
}

// OSProcessRunner executes the target using the platform-native mechanism
// (unix.Exec on Unix, os/exec child supervision on Windows).
type OSProcessRunner struct{}

// ProbeBinaryVersion executes `<binPath> --version` with a 2s timeout and returns
// the trimmed first line (empty string on error).
func ProbeBinaryVersion(ctx context.Context, binPath string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binPath, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
	return strings.TrimSpace(line)
}

// ResolveGlobalPath resolves a GlobalConfigSpec against XDG_CONFIG_HOME (if UseXDGConfig)
// or homeDir.
func ResolveGlobalPath(homeDir string, gs adapters.GlobalConfigSpec) string {
	if gs.UseXDGConfig {
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(homeDir, ".config")
		}
		return filepath.Join(xdg, filepath.FromSlash(gs.RelPath))
	}
	return filepath.Join(homeDir, filepath.FromSlash(gs.RelPath))
}

// IsNonEmptyPath returns true if path exists as a non-empty file or a directory
// containing at least one non-OS-metadata entry.
func IsNonEmptyPath(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return info.Size() > 0
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if name != ".DS_Store" && name != "Thumbs.db" {
			return true
		}
	}
	return false
}

// GatherFacts collects read-only workspace, global-hit, cache-root, and optional binary version facts.
func GatherFacts(
	ctx context.Context,
	projectRoot string,
	adp adapters.Adapter,
	probes adapters.ProbeResults,
	loadGlobal bool,
	userArgs []string,
) (adapters.LaunchFacts, error) {
	cacheRoot, err := store.ResolveCacheRoot()
	if err != nil {
		return adapters.LaunchFacts{}, err
	}

	facts := adapters.LaunchFacts{
		ProjectRoot: projectRoot,
		Probes:      probes,
		GlobalHits:  make(map[string]bool),
		CacheRoot:   cacheRoot,
	}

	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		for _, gs := range adp.GlobalConfigSpecs() {
			full := ResolveGlobalPath(homeDir, gs)
			if IsNonEmptyPath(full) {
				facts.GlobalHits[gs.Kind] = true
			}
		}
	}

	if !loadGlobal && adp.IsSessionArgs(userArgs) {
		if binPath, err := exec.LookPath(adp.BinaryName()); err == nil && binPath != "" {
			facts.BinaryVersion = ProbeBinaryVersion(ctx, binPath)
		}
	}

	return facts, nil
}

// BuildEnv constructs the child process environment with AGENTFILE_* metadata and adapter EnvOverrides merged in.
func BuildEnv(target string, loadGlobal bool, mode adapters.IsolationMode, extra map[string]string) []string {
	overrides := map[string]string{
		"AGENTFILE_ACTIVE":         "1",
		"AGENTFILE_TARGET":         target,
		"AGENTFILE_LOAD_GLOBAL":    fmt.Sprintf("%t", loadGlobal),
		"AGENTFILE_ISOLATION_MODE": string(mode),
	}
	for k, v := range extra {
		overrides[k] = v
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]bool, len(overrides))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok {
			if val, replace := overrides[k]; replace {
				out = append(out, k+"="+val)
				seen[k] = true
				continue
			}
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+overrides[k])
	}
	return out
}

// EnsureEmptyMCPConfig atomically writes <cacheRoot>/empty-mcp.json with 0700/0600 permissions.
func EnsureEmptyMCPConfig(cacheRoot string) (string, error) {
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return "", fmt.Errorf("create cache root %s: %w", cacheRoot, err)
	}
	dest := filepath.Join(cacheRoot, "empty-mcp.json")
	const payload = "{\"mcpServers\":{}}\n"
	if cur, err := os.ReadFile(dest); err == nil && string(cur) == payload {
		return dest, nil
	}

	tmp, err := os.CreateTemp(cacheRoot, ".empty-mcp-*")
	if err != nil {
		return "", fmt.Errorf("create temp empty-mcp.json: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(payload); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", err
	}
	cleanup = false
	return dest, nil
}

// Execute materializes any needed runtime artifacts, logs to stderr, and launches the target binary.
func Execute(
	ctx context.Context,
	pr ProcessRunner,
	projectRoot string,
	adp adapters.Adapter,
	plan *adapters.LaunchPlan,
	userArgs []string,
	loadGlobal bool,
	cacheRoot string,
	stderr io.Writer,
	verbose bool,
) error {
	_ = ctx
	if pr == nil {
		pr = OSProcessRunner{}
	}
	if plan.NeedEmptyMCP {
		if _, err := EnsureEmptyMCPConfig(cacheRoot); err != nil {
			return err
		}
	}
	for _, dir := range plan.EnsureDirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create isolated directory %s: %w", dir, err)
		}
	}

	absBin, err := exec.LookPath(adp.BinaryName())
	if err != nil {
		return spec.NewPolicyError("target binary %q not found on $PATH: %v", adp.BinaryName(), err)
	}

	argv := make([]string, 0, len(plan.PrependedArgs)+len(userArgs))
	argv = append(argv, plan.PrependedArgs...)
	argv = append(argv, userArgs...)

	env := BuildEnv(adp.Name(), loadGlobal, plan.Mode, plan.EnvOverrides)

	if stderr != nil {
		fmt.Fprintf(stderr, "agentfile: target=%s load_global=%t mode=%s\n", adp.Name(), loadGlobal, plan.Mode)
		if verbose {
			fmt.Fprintf(stderr, "agentfile: exec %s %s\n", absBin, strings.Join(argv, " "))
		}
	}

	return pr.Run(absBin, argv, env, projectRoot)
}
