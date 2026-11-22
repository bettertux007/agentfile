package store

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/spec"
)

// MaxExtendsDepth is the maximum allowed inheritance depth for extends chains.
const MaxExtendsDepth = 8

// ComposeResult holds the composed manifest, updated lockfile (with Extends entries),
// extends lock deltas, and all resolved local manifest file paths.
type ComposeResult struct {
	Composed   *spec.ComposedManifest
	NextLock   *spec.Lockfile
	Deltas     []LockDelta
	LocalFiles []string
}

// ComposeManifest loads Agentfile.yaml (or uses overrideRaw), resolves and hydrates
// all local and remote extends base manifests, verifies or updates Agentfile.lock's
// extends entries, and merges all layers via spec.ComposeLayers.
func (s *Store) ComposeManifest(
	ctx context.Context,
	projectRoot string,
	overrideRaw *spec.ProjectSpec,
	lf *spec.Lockfile,
	opts ResolveOptions,
) (*ComposeResult, error) {
	if s.Git == nil {
		s.Git = &ExecGitRunner{}
	}

	rootManifestPath := filepath.Join(projectRoot, "Agentfile.yaml")
	rawRoot := overrideRaw
	if rawRoot == nil {
		parsedRoot, err := spec.ParseRawFile(rootManifestPath)
		if err != nil {
			return nil, err
		}
		rawRoot = parsedRoot
	} else {
		if err := spec.ValidateRaw(rawRoot, false); err != nil {
			return nil, err
		}
	}

	nextLock := &spec.Lockfile{
		Version:            1,
		Extends:            make(map[string]spec.LockEntry),
		Dependencies:       make(map[string]spec.LockEntry),
		FromInitialCompose: lf == nil,
	}
	if lf != nil {
		for k, v := range lf.Extends {
			nextLock.Extends[k] = v
		}
		for k, v := range lf.Dependencies {
			nextLock.Dependencies[k] = v
		}
	}

	rootCanon := rootManifestPath
	if eval, err := filepath.EvalSymlinks(rootManifestPath); err == nil {
		rootCanon = eval
	}
	rootOrigin := spec.LayerOrigin{
		Source:  "Agentfile.yaml",
		BaseDir: projectRoot,
		IsRoot:  true,
	}

	var layers []spec.ManifestLayer
	completed := make(map[string]bool)
	seenRefs := make(map[string]string)
	activeExtendsKeys := make(map[string]bool)
	localFiles := []string{rootManifestPath}
	var deltas []LockDelta

	var visitLayer func(curSpec *spec.ProjectSpec, curOrigin spec.LayerOrigin, stack []string, depth int) error
	visitLayer = func(curSpec *spec.ProjectSpec, curOrigin spec.LayerOrigin, stack []string, depth int) error {
		if len(curSpec.Extends) == 0 {
			return nil
		}
		if depth >= MaxExtendsDepth {
			return spec.NewPolicyError("extends depth exceeds maximum limit (%d)", MaxExtendsDepth)
		}

		for _, extSrc := range curSpec.Extends {
			rebased, err := spec.RebaseSourceSpec(extSrc, curOrigin, projectRoot)
			if err != nil {
				return err
			}
			parsed, err := ParseSource(rebased, projectRoot, opts.ParseOpts)
			if err != nil {
				return err
			}

			if parsed.IsLocal {
				localPath := parsed.LocalPath
				if info, serr := os.Stat(localPath); serr == nil && info.IsDir() {
					localPath = filepath.Join(localPath, "Agentfile.yaml")
				}
				canonPath := localPath
				if eval, eerr := filepath.EvalSymlinks(localPath); eerr == nil {
					canonPath = eval
				}
				nodeID := "local:" + canonPath
				if containsString(stack, nodeID) {
					return spec.NewPolicyError("cyclic extends detected: %s -> %s", strings.Join(stack, " -> "), nodeID)
				}
				if completed[nodeID] {
					continue
				}

				baseSpec, err := spec.ParseBaseFile(localPath)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						return spec.NewPolicyError("local extends manifest %s not found", localPath)
					}
					return err
				}
				localFiles = append(localFiles, localPath)

				relSource := filepath.ToSlash(localPath)
				if rel, rerr := filepath.Rel(projectRoot, localPath); rerr == nil {
					relSource = filepath.ToSlash(rel)
				}
				childOrigin := spec.LayerOrigin{
					Source:   relSource,
					BaseDir:  filepath.Dir(localPath),
					IsRemote: false,
				}
				if err := visitLayer(baseSpec, childOrigin, append(stack, nodeID), depth+1); err != nil {
					return err
				}
				completed[nodeID] = true
				layers = append(layers, spec.ManifestLayer{
					Origin: childOrigin,
					Spec:   baseSpec,
				})
				continue
			}

			// Remote Git base manifest.
			lockSubpath := parsed.Subpath
			fetchParsed := *parsed
			if fetchParsed.Subpath == "" {
				fetchParsed.Subpath = "Agentfile.yaml"
			}
			ekey := spec.ExtendsLockKey(parsed.CanonicalURL, lockSubpath)
			if prevRef, ok := seenRefs[ekey]; ok && prevRef != parsed.Ref {
				return spec.NewPolicyError("conflicting refs %q and %q for extends base %s", prevRef, parsed.Ref, ekey)
			}
			seenRefs[ekey] = parsed.Ref

			nodeID := "git:" + ekey + "#" + parsed.Ref
			if containsString(stack, nodeID) {
				return spec.NewPolicyError("cyclic extends detected: %s -> %s", strings.Join(stack, " -> "), nodeID)
			}
			activeExtendsKeys[ekey] = true
			if completed[nodeID] {
				continue
			}

			targetKey := "extends:" + ekey
			shouldUpdate := lf == nil ||
				opts.UpdateTarget == "*" ||
				opts.UpdateTarget == "extends" ||
				opts.UpdateTarget == targetKey ||
				opts.UpdateTarget == ekey ||
				opts.ReconcileKey == targetKey ||
				opts.ReconcileKey == "extends"

			var prevEntry spec.LockEntry
			var hadPrev bool
			if lf != nil && lf.Extends != nil {
				prevEntry, hadPrev = lf.Extends[ekey]
			}
			if !shouldUpdate && lf != nil {
				if !hadPrev {
					return spec.NewPolicyError("extends %s is missing from Agentfile.lock (run 'agentfile sync --update')", ekey)
				}
				if prevEntry.Source != parsed.CanonicalURL || prevEntry.Ref != parsed.Ref {
					return spec.NewPolicyError("extends %s changed in Agentfile.yaml relative to Agentfile.lock (run 'agentfile sync --update=%s')", ekey, targetKey)
				}
				if prevEntry.CommitSHA == "" || prevEntry.ContentHash == "" {
					return spec.NewPolicyError("lock entry for extends %s is incomplete (run 'agentfile sync --update=%s')", ekey, targetKey)
				}
			}

			var existing *spec.LockEntry
			if !shouldUpdate && hadPrev {
				cp := prevEntry
				existing = &cp
			}

			entry, extractedDir, pinned, err := s.resolveGit(ctx, &fetchParsed, existing)
			if err != nil {
				return err
			}
			yamlPath := resolveExtractedSourcePath(extractedDir, fetchParsed.Subpath, "extends")
			baseSpec, err := spec.ParseBaseFile(yamlPath)
			if err != nil {
				return err
			}

			updated := !hadPrev || prevEntry != entry
			if updated {
				nextLock.ExtendsReconciled = true
			}
			nextLock.Extends[ekey] = entry
			if shouldUpdate {
				action := "unchanged"
				if !hadPrev {
					action = "added"
				} else if updated {
					action = "updated"
				}
				deltas = append(deltas, LockDelta{
					Key:     targetKey,
					Kind:    "extends",
					Name:    ekey,
					Action:  action,
					Ref:     parsed.Ref,
					Pinned:  pinned,
					OldSHA:  prevEntry.CommitSHA,
					NewSHA:  entry.CommitSHA,
					OldHash: prevEntry.ContentHash,
					NewHash: entry.ContentHash,
				})
			}

			originSubpath := normalizeOriginSubpath(fetchParsed.Subpath, yamlPath, extractedDir)
			childOrigin := spec.LayerOrigin{
				Source:    parsed.CanonicalURL,
				Ref:       parsed.Ref,
				CommitSHA: entry.CommitSHA,
				Subpath:   originSubpath,
				IsRemote:  true,
			}
			if err := visitLayer(baseSpec, childOrigin, append(stack, nodeID), depth+1); err != nil {
				return err
			}
			completed[nodeID] = true
			layers = append(layers, spec.ManifestLayer{
				Origin: childOrigin,
				Spec:   baseSpec,
			})
		}
		return nil
	}

	rootNodeID := "local:" + rootCanon
	if err := visitLayer(rawRoot, rootOrigin, []string{rootNodeID}, 0); err != nil {
		return nil, err
	}

	if opts.UpdateTarget == "extends" && len(activeExtendsKeys) == 0 {
		return nil, spec.NewPolicyError("no remote extends entries configured in Agentfile.yaml")
	}
	if strings.HasPrefix(opts.UpdateTarget, "extends:") {
		wantKey := strings.TrimPrefix(opts.UpdateTarget, "extends:")
		if !activeExtendsKeys[wantKey] {
			return nil, spec.NewPolicyError("unknown update target %q: not found in Agentfile.yaml", opts.UpdateTarget)
		}
	}

	if lf == nil || opts.UpdateTarget == "*" || opts.UpdateTarget == "extends" || opts.ReconcileKey != "" {
		for k, oldEntry := range nextLock.Extends {
			if !activeExtendsKeys[k] {
				delete(nextLock.Extends, k)
				nextLock.ExtendsReconciled = true
				deltas = append(deltas, LockDelta{
					Key:     "extends:" + k,
					Kind:    "extends",
					Name:    k,
					Action:  "removed",
					Ref:     oldEntry.Ref,
					Pinned:  hex40Re.MatchString(oldEntry.Ref),
					OldSHA:  oldEntry.CommitSHA,
					OldHash: oldEntry.ContentHash,
				})
			}
		}
	} else if lf != nil {
		for k := range lf.Extends {
			if !activeExtendsKeys[k] {
				return nil, spec.NewPolicyError("stale extends %s in Agentfile.lock not present in Agentfile.yaml (run 'agentfile sync --update')", k)
			}
		}
	}

	layers = append(layers, spec.ManifestLayer{
		Origin: rootOrigin,
		Spec:   rawRoot,
	})
	composed, err := spec.ComposeLayers(layers, projectRoot)
	if err != nil {
		return nil, err
	}

	sort.Slice(deltas, func(i, j int) bool {
		return deltas[i].Key < deltas[j].Key
	})
	if len(nextLock.Extends) == 0 {
		nextLock.Extends = nil
	}

	return &ComposeResult{
		Composed:   composed,
		NextLock:   nextLock,
		Deltas:     deltas,
		LocalFiles: localFiles,
	}, nil
}

// InspectComposedManifest performs a strictly read-only, offline composition and
// dependency inspection without running git, acquiring locks, or touching .last_used.
func (s *Store) InspectComposedManifest(
	projectRoot string,
	overrideRaw *spec.ProjectSpec,
	lf *spec.Lockfile,
	parseOpts ParseOptions,
) (*spec.ComposedManifest, *InspectReport, error) {
	rootManifestPath := filepath.Join(projectRoot, "Agentfile.yaml")
	rawRoot := overrideRaw
	if rawRoot == nil {
		parsedRoot, err := spec.ParseRawFile(rootManifestPath)
		if err != nil {
			return nil, nil, err
		}
		rawRoot = parsedRoot
	} else {
		if err := spec.ValidateRaw(rawRoot, false); err != nil {
			return nil, nil, err
		}
	}

	rootCanon := rootManifestPath
	if eval, err := filepath.EvalSymlinks(rootManifestPath); err == nil {
		rootCanon = eval
	}
	rootOrigin := spec.LayerOrigin{
		Source:  "Agentfile.yaml",
		BaseDir: projectRoot,
		IsRoot:  true,
	}

	var layers []spec.ManifestLayer
	completed := make(map[string]bool)
	seenRefs := make(map[string]string)
	activeExtendsKeys := make(map[string]bool)

	var unhydratedExtends []string
	var corruptExtends []string
	var symlinkExtends []string
	var missingLocalExtends []string
	var externalLocalExtends []string
	hasRemoteExtends := false
	extendsLockOutOfSync := false
	var extendsLockErr error

	var visitLayer func(curSpec *spec.ProjectSpec, curOrigin spec.LayerOrigin, stack []string, depth int) error
	visitLayer = func(curSpec *spec.ProjectSpec, curOrigin spec.LayerOrigin, stack []string, depth int) error {
		if len(curSpec.Extends) == 0 {
			return nil
		}
		if depth >= MaxExtendsDepth {
			return spec.NewPolicyError("extends depth exceeds maximum limit (%d)", MaxExtendsDepth)
		}

		for _, extSrc := range curSpec.Extends {
			rebased, err := spec.RebaseSourceSpec(extSrc, curOrigin, projectRoot)
			if err != nil {
				return err
			}
			parsed, err := ParseSource(rebased, projectRoot, parseOpts)
			if err != nil {
				return err
			}

			if parsed.IsLocal {
				localPath := parsed.LocalPath
				if info, serr := os.Stat(localPath); serr == nil && info.IsDir() {
					localPath = filepath.Join(localPath, "Agentfile.yaml")
				}
				if isPathOutsideRoot(projectRoot, localPath) {
					rel := filepath.ToSlash(localPath)
					if r, rerr := filepath.Rel(projectRoot, localPath); rerr == nil {
						rel = filepath.ToSlash(r)
					}
					externalLocalExtends = append(externalLocalExtends, "extends:"+rel)
				}
				canonPath := localPath
				if eval, eerr := filepath.EvalSymlinks(localPath); eerr == nil {
					canonPath = eval
				}
				nodeID := "local:" + canonPath
				if containsString(stack, nodeID) {
					return spec.NewPolicyError("cyclic extends detected: %s -> %s", strings.Join(stack, " -> "), nodeID)
				}
				if completed[nodeID] {
					continue
				}

				linfo, lerr := os.Lstat(localPath)
				if lerr != nil {
					missingLocalExtends = append(missingLocalExtends, "extends:"+filepath.ToSlash(localPath))
					continue
				}
				if linfo.Mode()&os.ModeSymlink != 0 {
					symlinkExtends = append(symlinkExtends, "extends:"+filepath.ToSlash(localPath))
					missingLocalExtends = append(missingLocalExtends, "extends:"+filepath.ToSlash(localPath))
					continue
				}

				baseSpec, err := spec.ParseBaseFile(localPath)
				if err != nil {
					return err
				}
				relSource := filepath.ToSlash(localPath)
				if rel, rerr := filepath.Rel(projectRoot, localPath); rerr == nil {
					relSource = filepath.ToSlash(rel)
				}
				childOrigin := spec.LayerOrigin{
					Source:   relSource,
					BaseDir:  filepath.Dir(localPath),
					IsRemote: false,
				}
				if err := visitLayer(baseSpec, childOrigin, append(stack, nodeID), depth+1); err != nil {
					return err
				}
				completed[nodeID] = true
				layers = append(layers, spec.ManifestLayer{
					Origin: childOrigin,
					Spec:   baseSpec,
				})
				continue
			}

			// Remote Git base manifest (strictly offline inspection).
			hasRemoteExtends = true
			lockSubpath := parsed.Subpath
			fetchSubpath := parsed.Subpath
			if fetchSubpath == "" {
				fetchSubpath = "Agentfile.yaml"
			}
			ekey := spec.ExtendsLockKey(parsed.CanonicalURL, lockSubpath)
			if prevRef, ok := seenRefs[ekey]; ok && prevRef != parsed.Ref {
				return spec.NewPolicyError("conflicting refs %q and %q for extends base %s", prevRef, parsed.Ref, ekey)
			}
			seenRefs[ekey] = parsed.Ref

			nodeID := "git:" + ekey + "#" + parsed.Ref
			if containsString(stack, nodeID) {
				return spec.NewPolicyError("cyclic extends detected: %s -> %s", strings.Join(stack, " -> "), nodeID)
			}
			activeExtendsKeys[ekey] = true
			if completed[nodeID] {
				continue
			}

			targetKey := "extends:" + ekey
			if lf == nil || lf.Extends == nil {
				unhydratedExtends = append(unhydratedExtends, targetKey)
				continue
			}
			entry, ok := lf.Extends[ekey]
			if !ok {
				extendsLockOutOfSync = true
				if extendsLockErr == nil {
					extendsLockErr = spec.NewPolicyError("extends %s is missing from Agentfile.lock (run 'agentfile sync --update')", ekey)
				}
				unhydratedExtends = append(unhydratedExtends, targetKey)
				continue
			}
			if entry.Source != parsed.CanonicalURL || entry.Ref != parsed.Ref || entry.CommitSHA == "" || entry.ContentHash == "" {
				extendsLockOutOfSync = true
				if extendsLockErr == nil {
					extendsLockErr = spec.NewPolicyError("extends %s changed in Agentfile.yaml relative to Agentfile.lock (run 'agentfile sync --update=%s')", ekey, targetKey)
				}
				unhydratedExtends = append(unhydratedExtends, targetKey)
				continue
			}

			extractedDir := s.extractedPath(entry.ContentHash)
			info, err := os.Stat(extractedDir)
			if err != nil || !info.IsDir() {
				unhydratedExtends = append(unhydratedExtends, targetKey)
				continue
			}
			actualHash, herr := HashTree(extractedDir)
			if herr != nil || actualHash != entry.ContentHash {
				if errors.Is(herr, ErrSymlinkNotAllowed) {
					symlinkExtends = append(symlinkExtends, targetKey)
				} else {
					corruptExtends = append(corruptExtends, targetKey)
				}
				unhydratedExtends = append(unhydratedExtends, targetKey)
				continue
			}

			yamlPath := resolveExtractedSourcePath(extractedDir, fetchSubpath, "extends")
			baseSpec, err := spec.ParseBaseFile(yamlPath)
			if err != nil {
				return err
			}
			originSubpath := normalizeOriginSubpath(fetchSubpath, yamlPath, extractedDir)
			childOrigin := spec.LayerOrigin{
				Source:    parsed.CanonicalURL,
				Ref:       parsed.Ref,
				CommitSHA: entry.CommitSHA,
				Subpath:   originSubpath,
				IsRemote:  true,
			}
			if err := visitLayer(baseSpec, childOrigin, append(stack, nodeID), depth+1); err != nil {
				return err
			}
			completed[nodeID] = true
			layers = append(layers, spec.ManifestLayer{
				Origin: childOrigin,
				Spec:   baseSpec,
			})
		}
		return nil
	}

	rootNodeID := "local:" + rootCanon
	if err := visitLayer(rawRoot, rootOrigin, []string{rootNodeID}, 0); err != nil {
		return nil, nil, err
	}

	layers = append(layers, spec.ManifestLayer{
		Origin: rootOrigin,
		Spec:   rawRoot,
	})

	partial := len(unhydratedExtends) > 0 || len(missingLocalExtends) > 0
	var composed *spec.ComposedManifest
	var cerr error
	if partial {
		composed, cerr = spec.ComposeLayersPartial(layers, projectRoot)
	} else {
		composed, cerr = spec.ComposeLayers(layers, projectRoot)
	}
	if cerr != nil {
		return nil, nil, cerr
	}

	rep, err := s.InspectDependencies(projectRoot, composed.Effective, lf, parseOpts)
	if err != nil {
		return nil, nil, err
	}

	rep.UnhydratedExtends = append(rep.UnhydratedExtends, unhydratedExtends...)
	rep.Unhydrated = append(rep.Unhydrated, unhydratedExtends...)
	rep.Corrupt = append(rep.Corrupt, corruptExtends...)
	rep.SymlinkBlocked = append(rep.SymlinkBlocked, symlinkExtends...)
	rep.MissingLocal = append(rep.MissingLocal, missingLocalExtends...)
	rep.ExternalLocal = append(rep.ExternalLocal, externalLocalExtends...)

	sort.Strings(rep.UnhydratedExtends)
	sort.Strings(rep.Unhydrated)
	sort.Strings(rep.Corrupt)
	sort.Strings(rep.SymlinkBlocked)
	sort.Strings(rep.MissingLocal)
	sort.Strings(rep.ExternalLocal)

	// Re-evaluate lockfile alignment across both Extends and Dependencies.
	if lf == nil {
		if hasRemoteExtends || hasRemoteAssets(composed.Effective, projectRoot, parseOpts) {
			rep.LockStatus = "missing"
			rep.LockError = spec.NewPolicyError("Agentfile.lock is missing (run 'agentfile sync')")
		} else {
			rep.LockStatus = "not_required"
			rep.LockError = nil
		}
		return composed, rep, nil
	}

	if extendsLockOutOfSync {
		rep.LockStatus = "out_of_sync"
		rep.LockError = extendsLockErr
		return composed, rep, nil
	}

	if len(unhydratedExtends) > 0 {
		// When a remote base is unhydrated offline, verify child-known remote dependencies exist in lf.Dependencies
		// without flagging base-declared dependencies in lf.Dependencies as stale.
		if verr := verifyPartialLockMatches(lf, composed, activeExtendsKeys, projectRoot, parseOpts); verr != nil {
			rep.LockStatus = "out_of_sync"
			rep.LockError = verr
		} else {
			rep.LockStatus = "ok"
			rep.LockError = nil
		}
		return composed, rep, nil
	}

	if verr := spec.VerifyMatchesComposed(lf, composed, extendsSourceInspectorFunc(projectRoot, parseOpts)); verr != nil {
		rep.LockStatus = "out_of_sync"
		rep.LockError = verr
	} else {
		if !hasRemoteExtends && !hasRemoteAssets(composed.Effective, projectRoot, parseOpts) && !lf.HasRemoteEntries() {
			rep.LockStatus = "not_required"
		} else {
			rep.LockStatus = "ok"
		}
		rep.LockError = nil
	}

	return composed, rep, nil
}

func extendsSourceInspectorFunc(projectRoot string, parseOpts ParseOptions) spec.ExtendsSourceInspector {
	return func(src spec.SourceSpec) (string, string, string, bool, error) {
		parsed, err := ParseSource(src, projectRoot, parseOpts)
		if err != nil {
			return "", "", "", false, err
		}
		if parsed.IsLocal {
			return parsed.LocalPath, "", "", false, nil
		}
		return parsed.CanonicalURL, parsed.Ref, parsed.Subpath, true, nil
	}
}

func hasRemoteAssets(ps *spec.ProjectSpec, projectRoot string, parseOpts ParseOptions) bool {
	if ps == nil {
		return false
	}
	check := func(src spec.SourceSpec) bool {
		parsed, err := ParseSource(src, projectRoot, parseOpts)
		return err == nil && !parsed.IsLocal
	}
	for _, sk := range ps.Skills {
		if check(sk.Source) {
			return true
		}
	}
	for _, ag := range ps.Agents {
		if check(ag.Source) {
			return true
		}
	}
	for _, rl := range ps.Rules {
		if check(rl.Source) {
			return true
		}
	}
	return false
}

func verifyPartialLockMatches(
	lf *spec.Lockfile,
	cm *spec.ComposedManifest,
	activeExtendsKeys map[string]bool,
	projectRoot string,
	parseOpts ParseOptions,
) error {
	for k := range activeExtendsKeys {
		if _, ok := lf.Extends[k]; !ok {
			return spec.NewPolicyError("extends %s is missing from Agentfile.lock (run 'agentfile sync --update')", k)
		}
	}
	inspect := extendsSourceInspectorFunc(projectRoot, parseOpts)
	checkDep := func(kind, name string, src spec.SourceSpec) error {
		canon, ref, _, isRemote, err := inspect(src)
		if err != nil {
			return err
		}
		if !isRemote {
			return nil
		}
		key := spec.LockKey(kind, name)
		got, ok := lf.Dependencies[key]
		if !ok {
			return spec.NewPolicyError("dependency %s is missing from Agentfile.lock (run 'agentfile sync --update')", key)
		}
		if got.Source != canon || got.Ref != ref || got.CommitSHA == "" || got.ContentHash == "" {
			return spec.NewPolicyError("dependency %s changed in Agentfile.yaml relative to Agentfile.lock (run 'agentfile sync --update=%s')", key, key)
		}
		return nil
	}
	for _, sk := range cm.Effective.Skills {
		if err := checkDep("skill", sk.Name, sk.Source); err != nil {
			return err
		}
	}
	for _, ag := range cm.Effective.Agents {
		if err := checkDep("agent", ag.Name, ag.Source); err != nil {
			return err
		}
	}
	for _, rl := range cm.Effective.Rules {
		if err := checkDep("rule", rl.Name, rl.Source); err != nil {
			return err
		}
	}
	return nil
}

func normalizeOriginSubpath(fetchSubpath, yamlPath, extractedDir string) string {
	sub := strings.Trim(strings.TrimSpace(fetchSubpath), "/")
	if sub == "" {
		return "Agentfile.yaml"
	}
	base := path.Base(sub)
	if strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") {
		return sub
	}
	if filepath.Base(yamlPath) == "Agentfile.yaml" && yamlPath == filepath.Join(extractedDir, "Agentfile.yaml") {
		return path.Join(sub, "Agentfile.yaml")
	}
	return sub
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}
