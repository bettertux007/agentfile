package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/lock"
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

type binVersionCacheFile struct {
	Version int                        `json:"version"`
	Entries map[string]binVersionEntry `json:"entries"`
}

type binVersionEntry struct {
	Realpath         string `json:"realpath"`
	Size             int64  `json:"size"`
	ModTimeUnixNano  int64  `json:"mod_time_unix_nano"`
	PkgPath          string `json:"pkg_path,omitempty"`
	PkgSize          int64  `json:"pkg_size,omitempty"`
	PkgMtimeUnixNano int64  `json:"pkg_mtime_unix_nano,omitempty"`
	Version          string `json:"version"`
}

// ProbeBinaryVersionCached probes `<binPath> --version` and caches the result in
// `<cacheRoot>/bin-versions.json` keyed by resolved realpath, size, mtime, and
// adjacent package.json metadata, bypassing the cache for dynamic version-manager shims.
func ProbeBinaryVersionCached(ctx context.Context, cacheRoot, binPath string, useCache bool) string {
	if !useCache || cacheRoot == "" || binPath == "" {
		return ProbeBinaryVersion(ctx, binPath)
	}
	realpath := binPath
	if eval, err := filepath.EvalSymlinks(binPath); err == nil && eval != "" {
		realpath = eval
	}
	if isDynamicVersionManagerShim(binPath) || isDynamicVersionManagerShim(realpath) {
		return ProbeBinaryVersion(ctx, binPath)
	}
	info, err := os.Stat(realpath)
	if err != nil || info.IsDir() {
		return ProbeBinaryVersion(ctx, binPath)
	}
	pkgPath, pkgSize, pkgMtime := findAdjacentPackageJSON(realpath)
	want := binVersionEntry{
		Realpath:         realpath,
		Size:             info.Size(),
		ModTimeUnixNano:  info.ModTime().UnixNano(),
		PkgPath:          pkgPath,
		PkgSize:          pkgSize,
		PkgMtimeUnixNano: pkgMtime,
	}

	cacheFile := filepath.Join(cacheRoot, "bin-versions.json")
	if raw, rerr := os.ReadFile(cacheFile); rerr == nil {
		var cf binVersionCacheFile
		if jerr := json.Unmarshal(raw, &cf); jerr == nil && cf.Version == 1 {
			if got, ok := cf.Entries[binPath]; ok &&
				got.Realpath == want.Realpath &&
				got.Size == want.Size &&
				got.ModTimeUnixNano == want.ModTimeUnixNano &&
				got.PkgPath == want.PkgPath &&
				got.PkgSize == want.PkgSize &&
				got.PkgMtimeUnixNano == want.PkgMtimeUnixNano &&
				got.Version != "" {
				return got.Version
			}
		}
	}

	ver := ProbeBinaryVersion(ctx, binPath)
	if ver == "" {
		return ""
	}
	want.Version = ver

	lockPath := filepath.Join(cacheRoot, "locks", "bin-versions.lock")
	if l, lerr := lock.Acquire(lockPath); lerr == nil {
		defer func() { _ = l.Close() }()
		cf := binVersionCacheFile{
			Version: 1,
			Entries: make(map[string]binVersionEntry),
		}
		if raw, rerr := os.ReadFile(cacheFile); rerr == nil {
			_ = json.Unmarshal(raw, &cf)
			if cf.Version != 1 || cf.Entries == nil {
				cf.Version = 1
				cf.Entries = make(map[string]binVersionEntry)
			}
		}
		cf.Entries[binPath] = want
		if b, merr := json.MarshalIndent(&cf, "", "  "); merr == nil {
			b = append(b, '\n')
			if tmp, terr := os.CreateTemp(cacheRoot, ".bin-versions-*"); terr == nil {
				tmpPath := tmp.Name()
				_, werr := tmp.Write(b)
				cerr := tmp.Close()
				if werr == nil && cerr == nil {
					_ = os.Rename(tmpPath, cacheFile)
				} else {
					_ = os.Remove(tmpPath)
				}
			}
		}
	}
	return ver
}

func isDynamicVersionManagerShim(p string) bool {
	slash := strings.ToLower(filepath.ToSlash(p))
	for _, marker := range []string{
		"/shims/",
		"/.asdf/",
		"/mise/shims",
		"/.local/share/mise/shims",
		"/.pyenv/shims",
		"/.nodenv/shims",
		"/.rbenv/shims",
		"/.volta/bin/",
	} {
		if strings.Contains(slash, marker) {
			return true
		}
	}
	return false
}

func findAdjacentPackageJSON(realpath string) (string, int64, int64) {
	dir := filepath.Dir(realpath)
	for i := 0; i < 3 && dir != "" && dir != filepath.Dir(dir); i++ {
		cand := filepath.Join(dir, "package.json")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand, info.Size(), info.ModTime().UnixNano()
		}
		dir = filepath.Dir(dir)
	}
	return "", 0, 0
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
	return GatherFactsWithOptions(ctx, projectRoot, adp, probes, loadGlobal, userArgs, true)
}

// GatherFactsWithOptions collects launch facts with configurable binary version probe caching.
func GatherFactsWithOptions(
	ctx context.Context,
	projectRoot string,
	adp adapters.Adapter,
	probes adapters.ProbeResults,
	loadGlobal bool,
	userArgs []string,
	useVersionCache bool,
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
			facts.BinaryVersion = ProbeBinaryVersionCached(ctx, cacheRoot, binPath, useVersionCache)
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

// ExecuteOptions configures project root, launch working directory, and banner behavior.
type ExecuteOptions struct {
	ProjectRoot string
	LaunchDir   string
	QuietBanner bool
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
	return ExecuteWithOptions(ctx, pr, ExecuteOptions{
		ProjectRoot: projectRoot,
		LaunchDir:   projectRoot,
	}, adp, plan, userArgs, loadGlobal, cacheRoot, stderr, verbose)
}

// ExecuteWithOptions materializes any needed runtime artifacts, exports AGENTFILE_ROOT,
// optionally suppresses the stderr banner, and launches the target binary in LaunchDir.
func ExecuteWithOptions(
	ctx context.Context,
	pr ProcessRunner,
	opts ExecuteOptions,
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

	extraEnv := make(map[string]string, len(plan.EnvOverrides)+1)
	for k, v := range plan.EnvOverrides {
		extraEnv[k] = v
	}
	if opts.ProjectRoot != "" {
		extraEnv["AGENTFILE_ROOT"] = opts.ProjectRoot
	}
	env := BuildEnv(adp.Name(), loadGlobal, plan.Mode, extraEnv)

	if stderr != nil {
		if !opts.QuietBanner || verbose {
			fmt.Fprintf(stderr, "agentfile: target=%s load_global=%t mode=%s\n", adp.Name(), loadGlobal, plan.Mode)
		}
		if verbose {
			fmt.Fprintf(stderr, "agentfile: exec %s %s\n", absBin, strings.Join(argv, " "))
		}
	}

	launchDir := opts.LaunchDir
	if launchDir == "" {
		launchDir = opts.ProjectRoot
	}
	return pr.Run(absBin, argv, env, launchDir)
}
