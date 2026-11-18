package store

import (
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/bettertux007/agentfile/internal/spec"
)

// ParsedSource is a validated, canonicalized local or remote dependency source.
type ParsedSource struct {
	IsLocal      bool
	LocalPath    string // Absolute path when IsLocal == true
	GitURL       string // Original fetch URL (with scheme/SCP preserved)
	CanonicalURL string // Normalized URL without embedded credentials
	Ref          string
	Subpath      string
}

// ParseOptions configures source parsing security policies.
type ParseOptions struct {
	AllowExternalLocal bool
	AllowFileScheme    bool
}

// ParseSource validates and normalizes a dependency source specification.
func ParseSource(src spec.SourceSpec, repoRoot string, opts ParseOptions) (*ParsedSource, error) {
	if src.IsStructured {
		return parseGitParts(src.Git, src.Ref, src.Path, opts)
	}
	raw := strings.TrimSpace(src.Raw)
	if raw == "" {
		return nil, spec.NewPolicyError("source cannot be empty")
	}
	if strings.HasPrefix(raw, "ext::") {
		return nil, spec.NewPolicyError("disallowed git protocol 'ext::' in %q", raw)
	}

	if isLocalSource(raw) {
		return parseLocalSource(raw, repoRoot, opts)
	}

	// Split #<ref> and //<subpath> unambiguously.
	var repoPart, ref, subpath string
	if hashIdx := strings.IndexByte(raw, '#'); hashIdx >= 0 {
		repoPart = raw[:hashIdx]
		rest := raw[hashIdx+1:]
		if subIdx := strings.Index(rest, "//"); subIdx >= 0 {
			ref = rest[:subIdx]
			subpath = rest[subIdx+2:]
		} else {
			ref = rest
		}
	} else {
		searchStart := 0
		if schemeEnd := strings.Index(raw, "://"); schemeEnd >= 0 {
			searchStart = schemeEnd + 3
		}
		if relIdx := strings.Index(raw[searchStart:], "//"); relIdx >= 0 {
			idx := searchStart + relIdx
			repoPart = raw[:idx]
			subpath = raw[idx+2:]
		} else {
			repoPart = raw
		}
	}

	return parseGitParts(repoPart, ref, subpath, opts)
}

func isLocalSource(raw string) bool {
	if strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") || raw == "." || raw == ".." {
		return true
	}
	if strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) {
		return true
	}
	return false
}

func parseLocalSource(raw, repoRoot string, opts ParseOptions) (*ParsedSource, error) {
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	var target string
	if filepath.IsAbs(raw) {
		target = filepath.Clean(raw)
	} else {
		target = filepath.Clean(filepath.Join(absRoot, raw))
	}

	if !opts.AllowExternalLocal {
		rel, err := filepath.Rel(absRoot, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, spec.NewPolicyError("local dependency path %q escapes repository root (pass --allow-external-local to permit)", raw)
		}
	}
	return &ParsedSource{
		IsLocal:   true,
		LocalPath: target,
	}, nil
}

func parseGitParts(repoURL, ref, subpath string, opts ParseOptions) (*ParsedSource, error) {
	repoURL = strings.TrimSpace(repoURL)
	ref = strings.TrimSpace(ref)
	subpath = strings.TrimSpace(subpath)

	if repoURL == "" {
		return nil, spec.NewPolicyError("git repository URL cannot be empty")
	}
	if strings.HasPrefix(repoURL, "ext::") {
		return nil, spec.NewPolicyError("disallowed git protocol 'ext::' in %q", repoURL)
	}

	cleanSub, err := validateSubpath(subpath)
	if err != nil {
		return nil, err
	}

	canon, err := validateAndCanonicalizeGitURL(repoURL, opts)
	if err != nil {
		return nil, err
	}

	return &ParsedSource{
		IsLocal:      false,
		GitURL:       repoURL,
		CanonicalURL: canon,
		Ref:          ref,
		Subpath:      cleanSub,
	}, nil
}

func validateSubpath(subpath string) (string, error) {
	if subpath == "" {
		return "", nil
	}
	if strings.ContainsRune(subpath, '\\') {
		return "", spec.NewPolicyError("subpath %q must use forward slashes", subpath)
	}
	if strings.HasPrefix(subpath, "/") {
		return "", spec.NewPolicyError("subpath %q must be relative, not absolute", subpath)
	}
	clean := path.Clean(subpath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", spec.NewPolicyError("invalid subpath %q", subpath)
	}
	return clean, nil
}

func validateAndCanonicalizeGitURL(raw string, opts ParseOptions) (string, error) {
	// SCP-style git@host:org/repo[.git]
	if !strings.Contains(raw, "://") {
		atIdx := strings.IndexByte(raw, '@')
		colonIdx := strings.IndexByte(raw, ':')
		if atIdx <= 0 || colonIdx <= atIdx+1 {
			return "", spec.NewPolicyError("unsupported git source URL %q (expected https://, ssh://, or git@host:path)", raw)
		}
		user := raw[:atIdx]
		host := strings.ToLower(raw[atIdx+1 : colonIdx])
		repoPath := raw[colonIdx+1:]
		if strings.ContainsRune(repoPath, ':') {
			return "", spec.NewPolicyError("unexpected ':' subpath delimiter in %q (use '//<subpath>')", raw)
		}
		repoPath = strings.TrimSuffix(strings.TrimPrefix(repoPath, "/"), "/")
		if repoPath == "" {
			return "", spec.NewPolicyError("missing repository path in %q", raw)
		}
		return user + "@" + host + ":" + repoPath, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", spec.NewPolicyError("invalid git URL %q: %v", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https", "ssh":
	case "file":
		if !opts.AllowFileScheme {
			return "", spec.NewPolicyError("file:// git URLs are disabled in %q", raw)
		}
	default:
		return "", spec.NewPolicyError("unsupported git URL scheme %q in %q", u.Scheme, raw)
	}

	if scheme != "file" && u.Host == "" {
		return "", spec.NewPolicyError("git URL %q is missing host", raw)
	}
	if strings.ContainsRune(u.Path, ':') {
		return "", spec.NewPolicyError("unexpected ':' subpath delimiter in %q (use '//<subpath>')", raw)
	}

	u.Scheme = scheme
	u.User = nil // strip embedded credentials
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
