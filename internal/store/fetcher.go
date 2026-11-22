package store

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bettertux007/agentfile/internal/lock"
	"github.com/bettertux007/agentfile/internal/spec"
)

var hex40Re = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// GitRunner executes git commands for store hydration.
type GitRunner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// ExecGitRunner invokes the system git binary with security hardening flags.
type ExecGitRunner struct {
	Timeout time.Duration
}

func (r *ExecGitRunner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	if env := os.Getenv("AGENTFILE_GIT_TIMEOUT"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			return d
		}
	}
	return 60 * time.Second
}

// Run executes git with hooks, submodules, ext protocol, autocrlf, and prompts disabled.
func (r *ExecGitRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	fullArgs := append([]string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "submodule.recurse=false",
		"-c", "protocol.ext.allow=never",
		"-c", "core.autocrlf=false",
	}, args...)

	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.WaitDelay = 5 * time.Second
	configureCmdProcessGroup(cmd)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ResolvedAsset is the hydrated dependency path and lock metadata.
type ResolvedAsset struct {
	Kind        string
	Name        string
	IsRemote    bool
	SourcePath  string
	LockKey     string
	LockEntry   spec.LockEntry
	LockUpdated bool
}

// LockDelta describes a change (or pinned no-op) to a remote lockfile entry during resolution.
type LockDelta struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Action  string `json:"action"` // "added" | "updated" | "removed" | "unchanged"
	Ref     string `json:"ref,omitempty"`
	Pinned  bool   `json:"pinned"`
	OldSHA  string `json:"old_sha,omitempty"`
	NewSHA  string `json:"new_sha,omitempty"`
	OldHash string `json:"old_hash,omitempty"`
	NewHash string `json:"new_hash,omitempty"`
}

// InspectReport describes read-only dependency and lockfile inspection results.
type InspectReport struct {
	Assets            map[string]ResolvedAsset
	Unhydrated        []string
	UnhydratedExtends []string
	Corrupt           []string
	SymlinkBlocked    []string
	MissingLocal      []string
	ExternalLocal     []string
	LockStatus        string // "ok" | "not_required" | "missing" | "out_of_sync" | "invalid"
	LockError         error
}

// ResolveOptions configures dependency resolution and lockfile reconciliation.
type ResolveOptions struct {
	UpdateTarget string
	ReconcileKey string
	ParseOpts    ParseOptions
}

// ResolveResult holds resolved assets, the resulting lockfile, and per-dependency lock deltas.
type ResolveResult struct {
	Assets   map[string]ResolvedAsset
	NextLock *spec.Lockfile
	Deltas   []LockDelta
}

// Store manages the local dependency cache and extracted content-addressed store.
type Store struct {
	RootDir string
	Git     GitRunner
}

// ResolveCacheRoot returns the shared agentfile cache directory
// (AGENTFILE_CACHE_DIR if set, otherwise <UserCacheDir>/agentfile).
func ResolveCacheRoot() (string, error) {
	if root := os.Getenv("AGENTFILE_CACHE_DIR"); root != "" {
		return root, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "agentfile"), nil
}

// NewDefaultStore constructs a Store under AGENTFILE_CACHE_DIR or <UserCacheDir>/agentfile.
func NewDefaultStore() (*Store, error) {
	root, err := ResolveCacheRoot()
	if err != nil {
		return nil, err
	}
	return &Store{
		RootDir: root,
		Git:     &ExecGitRunner{},
	}, nil
}

// InspectDependencies performs a strictly read-only inspection of local paths,
// cached remote trees, and lockfile alignment without invoking git, acquiring locks,
// touching .last_used timestamps, or deleting corrupt cache entries.
func (s *Store) InspectDependencies(
	projectRoot string,
	ps *spec.ProjectSpec,
	lf *spec.Lockfile,
	parseOpts ParseOptions,
) (*InspectReport, error) {
	if ps == nil {
		return nil, spec.NewPolicyError("ProjectSpec is nil")
	}

	rep := &InspectReport{
		Assets: make(map[string]ResolvedAsset),
	}
	hasRemote := false

	inspectItem := func(kind, name string, src spec.SourceSpec) error {
		key := spec.LockKey(kind, name)
		parsed, err := ParseSource(src, projectRoot, parseOpts)
		if err != nil {
			return err
		}
		if parsed.IsLocal {
			if isPathOutsideRoot(projectRoot, parsed.LocalPath) {
				rep.ExternalLocal = append(rep.ExternalLocal, key)
			}
			linfo, lerr := os.Lstat(parsed.LocalPath)
			if lerr != nil {
				rep.MissingLocal = append(rep.MissingLocal, key)
				return nil
			}
			if linfo.Mode()&os.ModeSymlink != 0 {
				rep.SymlinkBlocked = append(rep.SymlinkBlocked, key)
				rep.MissingLocal = append(rep.MissingLocal, key)
				return nil
			}
			if linfo.IsDir() {
				if _, herr := HashTree(parsed.LocalPath); errors.Is(herr, ErrSymlinkNotAllowed) {
					rep.SymlinkBlocked = append(rep.SymlinkBlocked, key)
					rep.MissingLocal = append(rep.MissingLocal, key)
					return nil
				}
			}
			rep.Assets[key] = ResolvedAsset{
				Kind:       kind,
				Name:       name,
				IsRemote:   false,
				SourcePath: parsed.LocalPath,
				LockKey:    key,
			}
			return nil
		}

		hasRemote = true
		if lf == nil {
			rep.Unhydrated = append(rep.Unhydrated, key)
			return nil
		}
		entry, ok := lf.Dependencies[key]
		if !ok || entry.Source != parsed.CanonicalURL || entry.Ref != parsed.Ref || entry.CommitSHA == "" || entry.ContentHash == "" {
			rep.Unhydrated = append(rep.Unhydrated, key)
			return nil
		}
		extractedDir := s.extractedPath(entry.ContentHash)
		info, err := os.Stat(extractedDir)
		if err != nil || !info.IsDir() {
			rep.Unhydrated = append(rep.Unhydrated, key)
			return nil
		}
		actualHash, herr := HashTree(extractedDir)
		if herr != nil || actualHash != entry.ContentHash {
			if errors.Is(herr, ErrSymlinkNotAllowed) {
				rep.SymlinkBlocked = append(rep.SymlinkBlocked, key)
			} else {
				rep.Corrupt = append(rep.Corrupt, key)
			}
			rep.Unhydrated = append(rep.Unhydrated, key)
			return nil
		}
		rep.Assets[key] = ResolvedAsset{
			Kind:       kind,
			Name:       name,
			IsRemote:   true,
			SourcePath: resolveExtractedSourcePath(extractedDir, parsed.Subpath, kind),
			LockKey:    key,
			LockEntry:  entry,
		}
		return nil
	}

	for _, sk := range ps.Skills {
		if err := inspectItem("skill", sk.Name, sk.Source); err != nil {
			return nil, err
		}
	}
	for _, ag := range ps.Agents {
		if err := inspectItem("agent", ag.Name, ag.Source); err != nil {
			return nil, err
		}
	}
	for _, rl := range ps.Rules {
		if err := inspectItem("rule", rl.Name, rl.Source); err != nil {
			return nil, err
		}
	}

	sort.Strings(rep.Unhydrated)
	sort.Strings(rep.Corrupt)
	sort.Strings(rep.SymlinkBlocked)
	sort.Strings(rep.MissingLocal)
	sort.Strings(rep.ExternalLocal)

	if len(ps.Extends) > 0 || (lf != nil && len(lf.Extends) > 0) {
		hasRemote = true
	}

	if lf == nil {
		if !hasRemote {
			rep.LockStatus = "not_required"
		} else {
			rep.LockStatus = "missing"
			rep.LockError = spec.NewPolicyError("Agentfile.lock is missing (run 'agentfile sync')")
		}
		return rep, nil
	}

	verr := spec.VerifyMatches(lf, ps, sourceInspectorFunc(projectRoot, parseOpts))
	if verr != nil {
		rep.LockStatus = "out_of_sync"
		rep.LockError = verr
	} else {
		rep.LockStatus = "ok"
	}
	return rep, nil
}

// ResolveAll resolves all local and remote dependencies for spec, validating or updating lockfile.
func (s *Store) ResolveAll(
	ctx context.Context,
	projectRoot string,
	ps *spec.ProjectSpec,
	lf *spec.Lockfile,
	updateTarget string,
	parseOpts ParseOptions,
) (map[string]ResolvedAsset, *spec.Lockfile, error) {
	res, err := s.ResolveWithOptions(ctx, projectRoot, ps, lf, ResolveOptions{
		UpdateTarget: updateTarget,
		ParseOpts:    parseOpts,
	})
	if err != nil {
		return nil, nil, err
	}
	return res.Assets, res.NextLock, nil
}

// ResolveWithOptions resolves dependencies with explicit update/reconciliation options
// and returns per-dependency lock deltas.
func (s *Store) ResolveWithOptions(
	ctx context.Context,
	projectRoot string,
	ps *spec.ProjectSpec,
	lf *spec.Lockfile,
	opts ResolveOptions,
) (*ResolveResult, error) {
	if s.Git == nil {
		s.Git = &ExecGitRunner{}
	}
	if ps == nil {
		return nil, spec.NewPolicyError("ProjectSpec is nil")
	}

	isExtendsUpdate := opts.UpdateTarget == "extends" || strings.HasPrefix(opts.UpdateTarget, "extends:")
	if opts.UpdateTarget != "" && opts.UpdateTarget != "*" && !isExtendsUpdate {
		if err := validateRemoteUpdateTarget(projectRoot, ps, opts.UpdateTarget, opts.ParseOpts); err != nil {
			return nil, err
		}
	}

	nextLock := &spec.Lockfile{
		Version:      1,
		Dependencies: make(map[string]spec.LockEntry),
	}
	if lf != nil {
		if len(lf.Extends) > 0 {
			nextLock.Extends = make(map[string]spec.LockEntry, len(lf.Extends))
			for k, v := range lf.Extends {
				nextLock.Extends[k] = v
			}
		}
		for k, v := range lf.Dependencies {
			nextLock.Dependencies[k] = v
		}
	}

	depsInitial := lf == nil || lf.FromInitialCompose
	extendsReconciled := lf != nil && (lf.ExtendsReconciled || isExtendsUpdate)

	// Enforce lockfile consistency on untouched entries when Agentfile.lock exists
	// and we are not doing a wildcard update.
	if !depsInitial && !extendsReconciled && opts.UpdateTarget != "*" {
		inspector := sourceInspectorFunc(projectRoot, opts.ParseOpts)
		var verr error
		if opts.UpdateTarget == "" && opts.ReconcileKey == "" && len(lf.Extends) == 0 {
			verr = spec.VerifyMatches(lf, ps, inspector)
		} else {
			filteredSpec, filteredLock := filterSpecAndLock(ps, lf, opts.UpdateTarget, opts.ReconcileKey)
			verr = spec.VerifyMatches(filteredLock, filteredSpec, inspector)
		}
		if verr != nil {
			return nil, verr
		}
	}

	results := make(map[string]ResolvedAsset)
	var deltas []LockDelta

	resolveItem := func(kind, name string, src spec.SourceSpec) error {
		key := spec.LockKey(kind, name)
		parsed, err := ParseSource(src, projectRoot, opts.ParseOpts)
		if err != nil {
			return err
		}
		if parsed.IsLocal {
			if _, err := os.Stat(parsed.LocalPath); err != nil {
				return spec.NewPolicyError("local %s %q source %s not found: %v", kind, name, parsed.LocalPath, err)
			}
			results[key] = ResolvedAsset{
				Kind:       kind,
				Name:       name,
				IsRemote:   false,
				SourcePath: parsed.LocalPath,
				LockKey:    key,
			}
			return nil
		}

		var prevEntry spec.LockEntry
		var hadPrev bool
		if !depsInitial && lf != nil {
			prevEntry, hadPrev = lf.Dependencies[key]
		}
		shouldUpdate := depsInitial ||
			opts.UpdateTarget == "*" ||
			opts.UpdateTarget == key ||
			opts.ReconcileKey == key ||
			(extendsReconciled && (!hadPrev || prevEntry.Source != parsed.CanonicalURL || prevEntry.Ref != parsed.Ref))
		var existing *spec.LockEntry
		if !shouldUpdate && hadPrev {
			cp := prevEntry
			existing = &cp
		}

		entry, extractedPath, pinned, err := s.resolveGit(ctx, parsed, existing)
		if err != nil {
			return err
		}
		updated := !hadPrev || prevEntry != entry
		nextLock.Dependencies[key] = entry
		results[key] = ResolvedAsset{
			Kind:        kind,
			Name:        name,
			IsRemote:    true,
			SourcePath:  resolveExtractedSourcePath(extractedPath, parsed.Subpath, kind),
			LockKey:     key,
			LockEntry:   entry,
			LockUpdated: updated,
		}

		if shouldUpdate {
			action := "unchanged"
			if !hadPrev {
				action = "added"
			} else if updated {
				action = "updated"
			}
			deltas = append(deltas, LockDelta{
				Key:     key,
				Kind:    kind,
				Name:    name,
				Action:  action,
				Ref:     parsed.Ref,
				Pinned:  pinned,
				OldSHA:  prevEntry.CommitSHA,
				NewSHA:  entry.CommitSHA,
				OldHash: prevEntry.ContentHash,
				NewHash: entry.ContentHash,
			})
		}
		return nil
	}

	for _, sk := range ps.Skills {
		if err := resolveItem("skill", sk.Name, sk.Source); err != nil {
			return nil, err
		}
	}
	for _, ag := range ps.Agents {
		if err := resolveItem("agent", ag.Name, ag.Source); err != nil {
			return nil, err
		}
	}
	for _, rl := range ps.Rules {
		if err := resolveItem("rule", rl.Name, rl.Source); err != nil {
			return nil, err
		}
	}

	// Prune removed remote keys from nextLock when creating fresh, updating all, or reconciling.
	if depsInitial || extendsReconciled || opts.UpdateTarget == "*" || opts.ReconcileKey != "" {
		for k, oldEntry := range nextLock.Dependencies {
			if res, ok := results[k]; !ok || !res.IsRemote {
				delete(nextLock.Dependencies, k)
				kind, name := splitLockKey(k)
				deltas = append(deltas, LockDelta{
					Key:     k,
					Kind:    kind,
					Name:    name,
					Action:  "removed",
					Ref:     oldEntry.Ref,
					Pinned:  hex40Re.MatchString(oldEntry.Ref),
					OldSHA:  oldEntry.CommitSHA,
					OldHash: oldEntry.ContentHash,
				})
			}
		}
	}

	sort.Slice(deltas, func(i, j int) bool {
		return deltas[i].Key < deltas[j].Key
	})

	return &ResolveResult{
		Assets:   results,
		NextLock: nextLock,
		Deltas:   deltas,
	}, nil
}

func sourceInspectorFunc(projectRoot string, parseOpts ParseOptions) spec.SourceInspector {
	return func(src spec.SourceSpec) (string, string, bool, error) {
		parsed, err := ParseSource(src, projectRoot, parseOpts)
		if err != nil {
			return "", "", false, err
		}
		if parsed.IsLocal {
			return parsed.LocalPath, "", false, nil
		}
		return parsed.CanonicalURL, parsed.Ref, true, nil
	}
}

func validateRemoteUpdateTarget(projectRoot string, ps *spec.ProjectSpec, target string, parseOpts ParseOptions) error {
	find := func() (spec.SourceSpec, bool) {
		for _, sk := range ps.Skills {
			if spec.LockKey("skill", sk.Name) == target {
				return sk.Source, true
			}
		}
		for _, ag := range ps.Agents {
			if spec.LockKey("agent", ag.Name) == target {
				return ag.Source, true
			}
		}
		for _, rl := range ps.Rules {
			if spec.LockKey("rule", rl.Name) == target {
				return rl.Source, true
			}
		}
		return spec.SourceSpec{}, false
	}

	src, ok := find()
	if !ok {
		if strings.HasPrefix(target, "mcp:") {
			return spec.NewPolicyError("target %q is not a remote Git dependency (MCP servers are not lock-tracked)", target)
		}
		return spec.NewPolicyError("unknown update target %q: not found in Agentfile.yaml", target)
	}
	parsed, err := ParseSource(src, projectRoot, parseOpts)
	if err != nil {
		return err
	}
	if parsed.IsLocal {
		return spec.NewPolicyError("dependency %q is local and cannot be updated from Git", target)
	}
	return nil
}

func filterSpecAndLock(ps *spec.ProjectSpec, lf *spec.Lockfile, excludeKeys ...string) (*spec.ProjectSpec, *spec.Lockfile) {
	excluded := make(map[string]bool, len(excludeKeys))
	for _, k := range excludeKeys {
		if k != "" {
			excluded[k] = true
		}
	}
	filteredSpec := *ps
	filteredSpec.Skills = make([]spec.AssetSpec, 0, len(ps.Skills))
	for _, sk := range ps.Skills {
		if !excluded[spec.LockKey("skill", sk.Name)] {
			filteredSpec.Skills = append(filteredSpec.Skills, sk)
		}
	}
	filteredSpec.Agents = make([]spec.AgentSpec, 0, len(ps.Agents))
	for _, ag := range ps.Agents {
		if !excluded[spec.LockKey("agent", ag.Name)] {
			filteredSpec.Agents = append(filteredSpec.Agents, ag)
		}
	}
	filteredSpec.Rules = make([]spec.RuleSpec, 0, len(ps.Rules))
	for _, rl := range ps.Rules {
		if !excluded[spec.LockKey("rule", rl.Name)] {
			filteredSpec.Rules = append(filteredSpec.Rules, rl)
		}
	}
	filteredLock := &spec.Lockfile{
		Version:      lf.Version,
		Dependencies: make(map[string]spec.LockEntry, len(lf.Dependencies)),
	}
	for k, v := range lf.Dependencies {
		if !excluded[k] {
			filteredLock.Dependencies[k] = v
		}
	}
	return &filteredSpec, filteredLock
}

func splitLockKey(key string) (string, string) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", key
}

func (s *Store) resolveGit(ctx context.Context, parsed *ParsedSource, locked *spec.LockEntry) (spec.LockEntry, string, bool, error) {
	if locked != nil && locked.CommitSHA != "" && locked.ContentHash != "" {
		extractedDir := s.extractedPath(locked.ContentHash)
		if _, err := os.Stat(extractedDir); err == nil {
			actualHash, err := HashTree(extractedDir)
			if err == nil && actualHash == locked.ContentHash {
				s.touchLastUsed(locked.ContentHash)
				return *locked, extractedDir, hex40Re.MatchString(locked.Ref), nil
			}
			// Corrupt local cache entry: clean it under lock and re-fetch pinned commit.
			_ = s.removeExtracted(locked.ContentHash)
		}
		entry, extracted, err := s.fetchAndExtract(ctx, parsed, locked.CommitSHA)
		if err != nil {
			return spec.LockEntry{}, "", false, err
		}
		if entry.ContentHash != locked.ContentHash {
			return spec.LockEntry{}, "", false, spec.NewPolicyError(
				"integrity mismatch for %s (%s): locked %s, got %s",
				parsed.CanonicalURL, locked.CommitSHA, locked.ContentHash, entry.ContentHash,
			)
		}
		return entry, extracted, hex40Re.MatchString(locked.Ref), nil
	}

	commitSHA, pinned, err := s.resolveRefDetailed(ctx, parsed.GitURL, parsed.Ref)
	if err != nil {
		return spec.LockEntry{}, "", false, err
	}
	entry, extracted, err := s.fetchAndExtract(ctx, parsed, commitSHA)
	if err != nil {
		return spec.LockEntry{}, "", false, err
	}
	return entry, extracted, pinned, nil
}

func (s *Store) resolveRefDetailed(ctx context.Context, repoURL, ref string) (string, bool, error) {
	if ref != "" && hex40Re.MatchString(ref) {
		return strings.ToLower(ref), true, nil
	}
	out, err := s.Git.Run(ctx, "", "ls-remote", "--symref", repoURL)
	if err != nil {
		return "", false, fmt.Errorf("ls-remote %s: %w", repoURL, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	refs := make(map[string]string)
	var headTarget string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
			headTarget = fields[1]
			continue
		}
		if len(fields) >= 2 {
			refs[fields[1]] = strings.ToLower(fields[0])
		}
	}

	if ref == "" {
		if sha, ok := refs["HEAD"]; ok {
			return sha, false, nil
		}
		if headTarget != "" {
			if sha, ok := refs[headTarget]; ok {
				return sha, false, nil
			}
		}
		return "", false, spec.NewPolicyError("could not resolve default HEAD for %s", repoURL)
	}

	candidates := []struct {
		name   string
		pinned bool
	}{
		{name: "refs/tags/" + ref + "^{}", pinned: true},
		{name: "refs/tags/" + ref, pinned: true},
		{name: "refs/heads/" + ref, pinned: false},
		{name: ref, pinned: strings.HasPrefix(ref, "refs/tags/")},
	}
	for _, c := range candidates {
		if sha, ok := refs[c.name]; ok {
			return sha, c.pinned, nil
		}
	}
	return "", false, spec.NewPolicyError("ref %q not found in %s", ref, repoURL)
}

func (s *Store) fetchAndExtract(ctx context.Context, parsed *ParsedSource, commitSHA string) (spec.LockEntry, string, error) {
	urlHash := shortSHA256(parsed.CanonicalURL)
	gitLockPath := filepath.Join(s.RootDir, "locks", "git-"+urlHash+".lock")
	mirrorDir := filepath.Join(s.RootDir, "git", urlHash)

	gl, err := lock.Acquire(gitLockPath)
	if err != nil {
		return spec.LockEntry{}, "", err
	}
	defer func() { _ = gl.Close() }()

	if _, err := os.Stat(filepath.Join(mirrorDir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o755); err != nil {
			return spec.LockEntry{}, "", err
		}
		if _, err := s.Git.Run(ctx, "", "init", "--bare", mirrorDir); err != nil {
			return spec.LockEntry{}, "", err
		}
	}

	// Ensure commitSHA exists in the bare mirror.
	if _, err := s.Git.Run(ctx, mirrorDir, "cat-file", "-e", commitSHA+"^{commit}"); err != nil {
		if _, ferr := s.Git.Run(ctx, mirrorDir, "fetch", "--depth", "1", parsed.GitURL, commitSHA); ferr != nil {
			// Fallback to unshallow fetch if remote disallows reachable SHA1InWant.
			if _, uerr := s.Git.Run(ctx, mirrorDir, "fetch", parsed.GitURL, "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"); uerr != nil {
				return spec.LockEntry{}, "", fmt.Errorf("fetch %s@%s: %w (unshallow fallback: %v)", parsed.CanonicalURL, commitSHA, ferr, uerr)
			}
			if _, cerr := s.Git.Run(ctx, mirrorDir, "cat-file", "-e", commitSHA+"^{commit}"); cerr != nil {
				return spec.LockEntry{}, "", spec.NewPolicyError("commit %s not reachable in %s", commitSHA, parsed.CanonicalURL)
			}
		}
	}

	// Inspect tree for submodule gitlinks (mode 160000) and verify subpath exists.
	lsArgs := []string{"ls-tree", "-r", "-z", commitSHA}
	if parsed.Subpath != "" {
		lsArgs = append(lsArgs, "--", parsed.Subpath)
	}
	treeOut, err := s.Git.Run(ctx, mirrorDir, lsArgs...)
	if err != nil {
		return spec.LockEntry{}, "", err
	}
	if len(bytes.TrimSpace(treeOut)) == 0 {
		return spec.LockEntry{}, "", spec.NewPolicyError("subpath %q not found in %s@%s", parsed.Subpath, parsed.CanonicalURL, commitSHA)
	}
	for _, rec := range bytes.Split(treeOut, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		tabIdx := bytes.IndexByte(rec, '\t')
		if tabIdx < 0 {
			continue
		}
		meta := strings.Fields(string(rec[:tabIdx]))
		if len(meta) >= 2 && meta[0] == "160000" {
			return spec.LockEntry{}, "", spec.NewPolicyError("submodule gitlink %q not permitted in %s", string(rec[tabIdx+1:]), parsed.CanonicalURL)
		}
	}

	// Stage into <root>/tmp on same filesystem before hashing.
	tmpRoot := filepath.Join(s.RootDir, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return spec.LockEntry{}, "", err
	}
	stageDir, err := os.MkdirTemp(tmpRoot, "stage-*")
	if err != nil {
		return spec.LockEntry{}, "", err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	archArgs := []string{"archive", "--format=tar", commitSHA}
	if parsed.Subpath != "" {
		archArgs = append(archArgs, "--", parsed.Subpath)
	}
	tarBytes, err := s.Git.Run(ctx, mirrorDir, archArgs...)
	if err != nil {
		return spec.LockEntry{}, "", err
	}
	if err := extractTarSafe(tarBytes, parsed.Subpath, stageDir); err != nil {
		return spec.LockEntry{}, "", err
	}

	contentHash, err := HashTree(stageDir)
	if err != nil {
		return spec.LockEntry{}, "", err
	}

	extLockPath := filepath.Join(s.RootDir, "locks", "ext-"+strings.TrimPrefix(contentHash, "sha256:")+".lock")
	el, err := lock.Acquire(extLockPath)
	if err != nil {
		return spec.LockEntry{}, "", err
	}
	defer func() { _ = el.Close() }()

	destDir := s.extractedPath(contentHash)
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return spec.LockEntry{}, "", err
	}
	if _, err := os.Stat(destDir); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(stageDir, destDir); err != nil {
			return spec.LockEntry{}, "", fmt.Errorf("commit store entry: %w", err)
		}
	}
	s.touchLastUsed(contentHash)

	return spec.LockEntry{
		Source:      parsed.CanonicalURL,
		Ref:         parsed.Ref,
		CommitSHA:   commitSHA,
		ContentHash: contentHash,
	}, destDir, nil
}

func extractTarSafe(tarBytes []byte, subpath, destDir string) error {
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	prefix := ""
	if subpath != "" {
		prefix = strings.TrimSuffix(subpath, "/") + "/"
	}

	var totalBytes int64
	var entryCount int
	extractedAny := false

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read git archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader {
			continue
		}

		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." {
			continue
		}
		if prefix != "" {
			if name == strings.TrimSuffix(prefix, "/") {
				if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
					name = path.Base(name)
				} else {
					continue
				}
			} else if !strings.HasPrefix(name, prefix) {
				continue
			} else {
				name = strings.TrimPrefix(name, prefix)
			}
		}
		if name == "" || name == "." || strings.HasPrefix(name, "..") || path.IsAbs(name) {
			return spec.NewPolicyError("invalid archive path %q", hdr.Name)
		}

		entryCount++
		if entryCount > MaxTreeEntries {
			return spec.NewPolicyError("dependency archive exceeds max entry limit (%d)", MaxTreeEntries)
		}

		target := filepath.Join(destDir, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if totalBytes+hdr.Size > MaxTreeBytes {
				return spec.NewPolicyError("dependency archive exceeds max size limit (%d bytes)", MaxTreeBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(f, io.LimitReader(tr, MaxTreeBytes-totalBytes+1))
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			totalBytes += n
			if totalBytes > MaxTreeBytes {
				return spec.NewPolicyError("dependency archive exceeds max size limit (%d bytes)", MaxTreeBytes)
			}
			extractedAny = true
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("%w: %s", ErrSymlinkNotAllowed, name)
		default:
			return spec.NewPolicyError("unsupported archive entry type %d for %s", hdr.Typeflag, name)
		}
	}

	if !extractedAny && subpath != "" {
		return spec.NewPolicyError("subpath %q yielded no regular files", subpath)
	}
	return nil
}

func (s *Store) extractedPath(contentHash string) string {
	slug := strings.TrimPrefix(contentHash, "sha256:")
	return filepath.Join(s.RootDir, "extracted", slug)
}

func resolveExtractedSourcePath(extractedDir, subpath, kind string) string {
	if kind != "agent" && kind != "rule" && kind != "extends" {
		return extractedDir
	}
	if subpath != "" {
		base := filepath.Base(filepath.FromSlash(strings.TrimSuffix(subpath, "/")))
		if base != "" && base != "." {
			cand := filepath.Join(extractedDir, base)
			if info, err := os.Stat(cand); err == nil && !info.IsDir() {
				return cand
			}
		}
	}
	if kind == "extends" {
		cand := filepath.Join(extractedDir, "Agentfile.yaml")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand
		}
	}
	entries, err := os.ReadDir(extractedDir)
	if err == nil && len(entries) == 1 && !entries[0].IsDir() {
		return filepath.Join(extractedDir, entries[0].Name())
	}
	return extractedDir
}

func isPathOutsideRoot(projectRoot, targetPath string) bool {
	if projectRoot == "" || targetPath == "" {
		return false
	}
	absRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}

func (s *Store) touchLastUsed(contentHash string) {
	slug := strings.TrimPrefix(contentHash, "sha256:")
	p := filepath.Join(s.RootDir, "extracted", slug+".last_used")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

func (s *Store) removeExtracted(contentHash string) error {
	slug := strings.TrimPrefix(contentHash, "sha256:")
	extLockPath := filepath.Join(s.RootDir, "locks", "ext-"+slug+".lock")
	el, err := lock.Acquire(extLockPath)
	if err != nil {
		return err
	}
	defer func() { _ = el.Close() }()
	_ = os.Remove(filepath.Join(s.RootDir, "extracted", slug+".last_used"))
	return os.RemoveAll(filepath.Join(s.RootDir, "extracted", slug))
}

// PruneCache removes extracted store entries whose .last_used timestamp exceeds maxAge.
func (s *Store) PruneCache(maxAge time.Duration, dryRun bool) ([]string, error) {
	extRoot := filepath.Join(s.RootDir, "extracted")
	items, err := os.ReadDir(extRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	cutoff := time.Now().Add(-maxAge)
	var removed []string
	for _, it := range items {
		if !it.IsDir() {
			continue
		}
		slug := it.Name()
		luPath := filepath.Join(extRoot, slug+".last_used")
		var ts time.Time
		if b, err := os.ReadFile(luPath); err == nil {
			if parsed, perr := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); perr == nil {
				ts = parsed
			}
		}
		if ts.IsZero() {
			if info, err := it.Info(); err == nil {
				ts = info.ModTime()
			}
		}
		if ts.Before(cutoff) {
			entryPath := filepath.Join(extRoot, slug)
			removed = append(removed, entryPath)
			if !dryRun {
				if err := s.removeExtracted("sha256:" + slug); err != nil {
					return removed, err
				}
			}
		}
	}
	return removed, nil
}

func shortSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
