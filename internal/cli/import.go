package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters/registry"
	"github.com/bettertux007/agentfile/internal/diff"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/bettertux007/agentfile/schemas"
	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
	"gopkg.in/yaml.v3"
)

var (
	reImportSlug     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	reForeignEnvVar  = regexp.MustCompile(`(?:\$)?\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)
	reClaudeBashRule = regexp.MustCompile(`^Bash\((.*)\)$`)
)

var canonicalTargetOrder = []string{
	"claude",
	"opencode",
	"gemini",
	"cursor",
	"codex",
	"copilot",
	"windsurf",
}

// ImportSummary is emitted by `agentfile init --import [--json]`.
type ImportSummary struct {
	DryRun         bool     `json:"dry_run"`
	Targets        []string `json:"targets"`
	SourcesScanned []string `json:"sources_scanned,omitempty"`
	MCP            []string `json:"mcp,omitempty"`
	Skills         []string `json:"skills,omitempty"`
	Agents         []string `json:"agents,omitempty"`
	Rules          []string `json:"rules,omitempty"`
	AllowCommands  []string `json:"allow_commands,omitempty"`
	DenyCommands   []string `json:"deny_commands,omitempty"`
	Hooks          []string `json:"hooks,omitempty"`
	ImportedFiles  []string `json:"imported_files,omitempty"`
	ManifestYAML   string   `json:"manifest_yaml,omitempty"`
}

type importedMCP struct {
	Transport string
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
	Source    string
}

type importedSkill struct {
	Name      string
	SourceDir string
	RelSource string
	TreeHash  string
}

type importedAgent struct {
	Name        string
	Description string
	Model       string
	Body        string
	Source      string
}

type importedRule struct {
	Name                   string
	Description            string
	Globs                  []string
	AlwaysApply            bool
	HasExplicitDescription bool
	HasExplicitGlobs       bool
	HasExplicitAlwaysApply bool
	Body                   string
	Source                 string
}

type importedHook struct {
	Event   string
	Matcher string
	Command string
	Enforce bool
	Source  string
}

type importCollector struct {
	workDir         string
	realRoot        string
	detectedTargets map[string]bool
	hasSharedStd    bool
	sourcesScanned  []string

	mcp           map[string]importedMCP
	skills        map[string]importedSkill
	agents        map[string]importedAgent
	rules         map[string]importedRule
	allowCommands map[string]string // pattern -> source file
	denyCommands  map[string]string // pattern -> source file
	hooks         map[string][]importedHook
}

func newImportCollector(workDir, realRoot string) *importCollector {
	return &importCollector{
		workDir:         workDir,
		realRoot:        realRoot,
		detectedTargets: make(map[string]bool),
		mcp:             make(map[string]importedMCP),
		skills:          make(map[string]importedSkill),
		agents:          make(map[string]importedAgent),
		rules:           make(map[string]importedRule),
		allowCommands:   make(map[string]string),
		denyCommands:    make(map[string]string),
		hooks:           make(map[string][]importedHook),
	}
}

func runInitImport(ec *execContext, requestedTargets []string, force, dryRun, jsonOut bool) error {
	reg := registry.Default()
	var validatedTargets []string
	if len(requestedTargets) > 0 {
		seen := make(map[string]bool, len(requestedTargets))
		for _, raw := range requestedTargets {
			t := strings.TrimSpace(raw)
			if t == "" {
				return spec.NewPolicyError("target name in --targets cannot be empty")
			}
			if _, err := registry.Lookup(reg, t); err != nil {
				return spec.NewPolicyError("unknown target %q (supported: %s)", t, strings.Join(canonicalTargetOrder, ", "))
			}
			if !seen[t] {
				seen[t] = true
				validatedTargets = append(validatedTargets, t)
			}
		}
	}

	targetPath := filepath.Join(ec.workDir, "Agentfile.yaml")
	if _, err := os.Lstat(targetPath); err == nil && !force {
		return spec.NewPolicyError("Agentfile.yaml already exists (pass --force to overwrite)")
	}

	absRoot, err := filepath.Abs(ec.workDir)
	if err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		realRoot = absRoot
	}

	c := newImportCollector(absRoot, realRoot)
	if err := c.scanAll(); err != nil {
		return err
	}

	if c.artifactCount() == 0 {
		return spec.NewPolicyError("no existing agent configurations found to import")
	}

	finalTargets := validatedTargets
	if len(finalTargets) == 0 {
		if c.hasSharedStd && !c.detectedTargets["codex"] && !c.detectedTargets["copilot"] && !c.detectedTargets["windsurf"] {
			c.detectedTargets["codex"] = true
		}
		for _, t := range canonicalTargetOrder {
			if c.detectedTargets[t] {
				finalTargets = append(finalTargets, t)
			}
		}
	}
	if len(finalTargets) == 0 {
		return spec.NewPolicyError("no targets detected for imported configuration (pass --targets)")
	}

	manifestBytes, summary, err := c.buildManifestAndSummary(finalTargets, dryRun)
	if err != nil {
		return err
	}

	if err := schemas.ValidateAgentfileV1(manifestBytes); err != nil {
		return spec.NewPolicyError("generated Agentfile.yaml failed schema validation: %v", err)
	}
	if _, err := spec.Parse(bytes.NewReader(manifestBytes)); err != nil {
		return err
	}

	if !dryRun {
		if err := c.commitFiles(manifestBytes); err != nil {
			return err
		}
	}

	if jsonOut {
		enc := json.NewEncoder(ec.out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}

	printImportHuman(ec.out, summary)
	return nil
}

func (c *importCollector) artifactCount() int {
	hookCount := 0
	for _, list := range c.hooks {
		hookCount += len(list)
	}
	return len(c.mcp) + len(c.skills) + len(c.agents) + len(c.rules) + len(c.allowCommands) + len(c.denyCommands) + hookCount
}

func (c *importCollector) recordSource(relPath string) {
	if !slices.Contains(c.sourcesScanned, relPath) {
		c.sourcesScanned = append(c.sourcesScanned, relPath)
	}
}

func (c *importCollector) markTarget(target string, shared bool) {
	if shared {
		c.hasSharedStd = true
		return
	}
	if target != "" {
		c.detectedTargets[target] = true
	}
}

func (c *importCollector) isSafePath(relPath string) (string, bool, error) {
	candidate := filepath.Join(c.workDir, filepath.FromSlash(relPath))
	if _, err := os.Lstat(candidate); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, nil
	}
	rel, err := filepath.Rel(c.realRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false, nil
	}
	return resolved, true, nil
}

func (c *importCollector) readSafeFile(relPath string) ([]byte, bool, error) {
	resolved, ok, err := c.isSafePath(relPath)
	if err != nil || !ok {
		return nil, false, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if info.IsDir() {
		return nil, false, nil
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (c *importCollector) scanAll() error {
	// 1. MCP configs across all 7 tools.
	if err := c.scanJSONMCP(".mcp.json", "mcpServers", "claude", false); err != nil {
		return err
	}
	if err := c.scanOpenCodeJSON("opencode.json"); err != nil {
		return err
	}
	if err := c.scanJSONMCP(".gemini/settings.json", "mcpServers", "gemini", true); err != nil {
		return err
	}
	if err := c.scanJSONMCP(".cursor/mcp.json", "mcpServers", "cursor", false); err != nil {
		return err
	}
	if err := c.scanJSONMCP(".vscode/mcp.json", "servers", "copilot", false); err != nil {
		return err
	}
	if err := c.scanCodexTOML(".codex/config.toml"); err != nil {
		return err
	}

	// 2. Claude settings.json (permissions + hooks).
	if err := c.scanClaudeSettings(".claude/settings.json"); err != nil {
		return err
	}

	// 3. Skills across all skill roots.
	skillRoots := []struct {
		rel    string
		target string
		shared bool
	}{
		{".claude/skills", "claude", false},
		{".opencode/skills", "opencode", false},
		{".gemini/skills", "gemini", false},
		{".agents/skills", "", true},
	}
	for _, sr := range skillRoots {
		if err := c.scanSkillsDir(sr.rel, sr.target, sr.shared); err != nil {
			return err
		}
	}

	// 4. Agents across Claude and OpenCode.
	for _, ar := range []struct {
		rel    string
		target string
	}{
		{".claude/agents", "claude"},
		{".opencode/agents", "opencode"},
	} {
		if err := c.scanAgentsDir(ar.rel, ar.target); err != nil {
			return err
		}
	}

	// 5. Rules across all 7 tools.
	if err := c.scanRuleDir(".claude/rules", ".md", "claude", "claude"); err != nil {
		return err
	}
	if err := c.scanRuleDir(".cursor/rules", ".mdc", "cursor", "cursor"); err != nil {
		return err
	}
	if err := c.scanRuleDir(".windsurf/rules", ".md", "windsurf", "windsurf"); err != nil {
		return err
	}
	if err := c.scanSingleRuleFile(".opencode/agentfile-rules.md", "opencode", false, "opencode-instructions"); err != nil {
		return err
	}
	if err := c.scanSingleRuleFile("GEMINI.md", "gemini", false, "gemini-instructions"); err != nil {
		return err
	}
	if err := c.scanSingleRuleFile("AGENTS.md", "", true, "agents-instructions"); err != nil {
		return err
	}
	if err := c.scanSingleRuleFile(".github/copilot-instructions.md", "copilot", false, "copilot-instructions"); err != nil {
		return err
	}

	return c.validatePermissionConsistency()
}

func (c *importCollector) validatePermissionConsistency() error {
	for pat, allowSrc := range c.allowCommands {
		if denySrc, exists := c.denyCommands[pat]; exists {
			return spec.NewPolicyError("conflicting permission for command %q: allowed in %s and denied in %s", pat, allowSrc, denySrc)
		}
	}
	return nil
}

func parseJSONCDoc(relPath string, raw []byte) (map[string]any, error) {
	std, err := hujson.Standardize(raw)
	if err != nil {
		return nil, spec.NewPolicyError("parse %s: %v", relPath, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(std, &doc); err != nil {
		return nil, spec.NewPolicyError("parse %s: %v", relPath, err)
	}
	return doc, nil
}

func (c *importCollector) scanJSONMCP(relPath, mcpKey, target string, isGemini bool) error {
	raw, ok, err := c.readSafeFile(relPath)
	if err != nil || !ok {
		return err
	}
	doc, err := parseJSONCDoc(relPath, raw)
	if err != nil {
		return err
	}
	serversRaw, ok := doc[mcpKey]
	if !ok || serversRaw == nil {
		return nil
	}
	servers, ok := serversRaw.(map[string]any)
	if !ok {
		return spec.NewPolicyError("%s: %s must be a JSON object", relPath, mcpKey)
	}
	if len(servers) == 0 {
		return nil
	}
	c.recordSource(relPath)

	names := sortedAnyMapKeys(servers)
	for _, rawName := range names {
		slug, err := sanitizeSlug(rawName)
		if err != nil {
			return spec.NewPolicyError("%s: invalid MCP server name %q: %v", relPath, rawName, err)
		}
		entry, ok := servers[rawName].(map[string]any)
		if !ok {
			return spec.NewPolicyError("%s: %s.%s must be an object", relPath, mcpKey, rawName)
		}
		srv, err := parseGenericJSONMCP(relPath, rawName, entry, isGemini)
		if err != nil {
			return err
		}
		if err := c.addMCP(slug, srv); err != nil {
			return err
		}
		c.markTarget(target, false)
	}
	return nil
}

func parseGenericJSONMCP(relPath, name string, entry map[string]any, isGemini bool) (importedMCP, error) {
	srv := importedMCP{Source: relPath}
	typ := strings.ToLower(strings.TrimSpace(asString(entry["type"])))
	cmd := strings.TrimSpace(asString(entry["command"]))
	url := strings.TrimSpace(asString(entry["url"]))
	httpURL := strings.TrimSpace(asString(entry["httpUrl"]))
	if isGemini && url == "" && httpURL != "" {
		url = httpURL
	}

	if typ == "" {
		if cmd != "" {
			typ = "stdio"
		} else if url != "" {
			typ = "http"
		}
	}

	switch typ {
	case "stdio":
		if cmd == "" {
			return importedMCP{}, spec.NewPolicyError("%s: mcp %q stdio server missing command", relPath, name)
		}
		srv.Transport = "stdio"
		srv.Command = normalizeEnvSyntax(cmd)
		args, err := asStringSlice(entry["args"])
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp %q args: %v", relPath, name, err)
		}
		for _, a := range args {
			srv.Args = append(srv.Args, normalizeEnvSyntax(a))
		}
		envMap, err := asStringMap(entry["env"])
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp %q env: %v", relPath, name, err)
		}
		if len(envMap) > 0 {
			srv.Env = make(map[string]string, len(envMap))
			for k, v := range envMap {
				srv.Env[k] = normalizeEnvSyntax(v)
			}
		}
	case "http", "sse":
		if url == "" {
			return importedMCP{}, spec.NewPolicyError("%s: mcp %q %s server missing url", relPath, name, typ)
		}
		srv.Transport = typ
		srv.URL = normalizeEnvSyntax(url)
		hdrs, err := asStringMap(entry["headers"])
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp %q headers: %v", relPath, name, err)
		}
		if len(hdrs) > 0 {
			srv.Headers = make(map[string]string, len(hdrs))
			for k, v := range hdrs {
				srv.Headers[k] = normalizeEnvSyntax(v)
			}
		}
	default:
		return importedMCP{}, spec.NewPolicyError("%s: mcp %q has unsupported or missing transport %q", relPath, name, typ)
	}

	if err := checkMCPSecrets(relPath, name, srv); err != nil {
		return importedMCP{}, err
	}
	return srv, nil
}

func (c *importCollector) scanOpenCodeJSON(relPath string) error {
	raw, ok, err := c.readSafeFile(relPath)
	if err != nil || !ok {
		return err
	}
	doc, err := parseJSONCDoc(relPath, raw)
	if err != nil {
		return err
	}
	importedAny := false

	if mcpRaw, exists := doc["mcp"]; exists && mcpRaw != nil {
		mcpMap, ok := mcpRaw.(map[string]any)
		if !ok {
			return spec.NewPolicyError("%s: mcp must be a JSON object", relPath)
		}
		for _, rawName := range sortedAnyMapKeys(mcpMap) {
			slug, err := sanitizeSlug(rawName)
			if err != nil {
				return spec.NewPolicyError("%s: invalid MCP server name %q: %v", relPath, rawName, err)
			}
			entry, ok := mcpMap[rawName].(map[string]any)
			if !ok {
				return spec.NewPolicyError("%s: mcp.%s must be an object", relPath, rawName)
			}
			srv, err := parseOpenCodeMCP(relPath, rawName, entry)
			if err != nil {
				return err
			}
			if err := c.addMCP(slug, srv); err != nil {
				return err
			}
			importedAny = true
		}
	}

	if permRaw, exists := doc["permission"]; exists && permRaw != nil {
		permMap, ok := permRaw.(map[string]any)
		if ok {
			if bashRaw, hasBash := permMap["bash"]; hasBash && bashRaw != nil {
				bashMap, ok := bashRaw.(map[string]any)
				if !ok {
					return spec.NewPolicyError("%s: permission.bash must be a JSON object", relPath)
				}
				for _, pat := range sortedAnyMapKeys(bashMap) {
					action := strings.ToLower(strings.TrimSpace(asString(bashMap[pat])))
					trimmedPat := strings.TrimSpace(pat)
					if trimmedPat == "" {
						continue
					}
					switch action {
					case "allow":
						if _, ok := c.allowCommands[trimmedPat]; !ok {
							c.allowCommands[trimmedPat] = relPath
						}
						importedAny = true
					case "deny":
						if _, ok := c.denyCommands[trimmedPat]; !ok {
							c.denyCommands[trimmedPat] = relPath
						}
						importedAny = true
					}
				}
			}
		}
	}

	if importedAny {
		c.recordSource(relPath)
		c.markTarget("opencode", false)
	}
	return nil
}

func parseOpenCodeMCP(relPath, name string, entry map[string]any) (importedMCP, error) {
	srv := importedMCP{Source: relPath}
	typ := strings.ToLower(strings.TrimSpace(asString(entry["type"])))
	switch typ {
	case "local", "stdio":
		srv.Transport = "stdio"
		switch cmdVal := entry["command"].(type) {
		case []any:
			if len(cmdVal) == 0 {
				return importedMCP{}, spec.NewPolicyError("%s: mcp.%s command array cannot be empty", relPath, name)
			}
			srv.Command = normalizeEnvSyntax(asString(cmdVal[0]))
			for _, a := range cmdVal[1:] {
				srv.Args = append(srv.Args, normalizeEnvSyntax(asString(a)))
			}
		case string:
			srv.Command = normalizeEnvSyntax(strings.TrimSpace(cmdVal))
			args, err := asStringSlice(entry["args"])
			if err != nil {
				return importedMCP{}, spec.NewPolicyError("%s: mcp.%s args: %v", relPath, name, err)
			}
			for _, a := range args {
				srv.Args = append(srv.Args, normalizeEnvSyntax(a))
			}
		default:
			return importedMCP{}, spec.NewPolicyError("%s: mcp.%s local server requires command", relPath, name)
		}
		if strings.TrimSpace(srv.Command) == "" {
			return importedMCP{}, spec.NewPolicyError("%s: mcp.%s command cannot be empty", relPath, name)
		}
		envSrc := entry["environment"]
		if envSrc == nil {
			envSrc = entry["env"]
		}
		envMap, err := asStringMap(envSrc)
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp.%s environment: %v", relPath, name, err)
		}
		if len(envMap) > 0 {
			srv.Env = make(map[string]string, len(envMap))
			for k, v := range envMap {
				srv.Env[k] = normalizeEnvSyntax(v)
			}
		}
	case "remote", "http", "sse":
		if typ == "sse" {
			srv.Transport = "sse"
		} else {
			srv.Transport = "http"
		}
		url := strings.TrimSpace(asString(entry["url"]))
		if url == "" {
			return importedMCP{}, spec.NewPolicyError("%s: mcp.%s remote server missing url", relPath, name)
		}
		srv.URL = normalizeEnvSyntax(url)
		hdrs, err := asStringMap(entry["headers"])
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp.%s headers: %v", relPath, name, err)
		}
		if len(hdrs) > 0 {
			srv.Headers = make(map[string]string, len(hdrs))
			for k, v := range hdrs {
				srv.Headers[k] = normalizeEnvSyntax(v)
			}
		}
	default:
		return importedMCP{}, spec.NewPolicyError("%s: mcp.%s has unsupported type %q", relPath, name, typ)
	}

	if err := checkMCPSecrets(relPath, name, srv); err != nil {
		return importedMCP{}, err
	}
	return srv, nil
}

func (c *importCollector) scanCodexTOML(relPath string) error {
	raw, ok, err := c.readSafeFile(relPath)
	if err != nil || !ok {
		return err
	}
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return spec.NewPolicyError("parse %s: %v", relPath, err)
	}
	mcpRaw, exists := doc["mcp_servers"]
	if !exists || mcpRaw == nil {
		return nil
	}
	mcpMap, ok := mcpRaw.(map[string]any)
	if !ok {
		return spec.NewPolicyError("%s: mcp_servers must be a TOML table", relPath)
	}
	if len(mcpMap) == 0 {
		return nil
	}
	c.recordSource(relPath)

	for _, rawName := range sortedAnyMapKeys(mcpMap) {
		slug, err := sanitizeSlug(rawName)
		if err != nil {
			return spec.NewPolicyError("%s: invalid MCP server name %q: %v", relPath, rawName, err)
		}
		entry, ok := mcpMap[rawName].(map[string]any)
		if !ok {
			return spec.NewPolicyError("%s: mcp_servers.%s must be a table", relPath, rawName)
		}
		srv, err := parseCodexMCP(relPath, rawName, entry)
		if err != nil {
			return err
		}
		if err := c.addMCP(slug, srv); err != nil {
			return err
		}
		c.markTarget("codex", false)
	}
	return nil
}

func parseCodexMCP(relPath, name string, entry map[string]any) (importedMCP, error) {
	srv := importedMCP{Source: relPath}
	cmd := strings.TrimSpace(asString(entry["command"]))
	url := strings.TrimSpace(asString(entry["url"]))

	switch {
	case cmd != "":
		srv.Transport = "stdio"
		srv.Command = normalizeEnvSyntax(cmd)
		args, err := asStringSlice(entry["args"])
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp_servers.%s.args: %v", relPath, name, err)
		}
		for _, a := range args {
			srv.Args = append(srv.Args, normalizeEnvSyntax(a))
		}
		envMap, err := asStringMap(entry["env"])
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp_servers.%s.env: %v", relPath, name, err)
		}
		envVars, err := asStringSlice(entry["env_vars"])
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp_servers.%s.env_vars: %v", relPath, name, err)
		}
		if len(envMap) > 0 || len(envVars) > 0 {
			srv.Env = make(map[string]string, len(envMap)+len(envVars))
			for k, v := range envMap {
				srv.Env[k] = normalizeEnvSyntax(v)
			}
			for _, vName := range envVars {
				vName = strings.TrimSpace(vName)
				if vName == "" {
					continue
				}
				ph := "${" + vName + "}"
				if prev, exists := srv.Env[vName]; exists && prev != ph {
					return importedMCP{}, spec.NewPolicyError("%s: mcp_servers.%s defines conflicting env.%s and env_vars %q", relPath, name, vName, vName)
				}
				srv.Env[vName] = ph
			}
		}
	case url != "":
		srv.Transport = "http"
		srv.URL = normalizeEnvSyntax(url)
		staticHdrs, err := asStringMap(entry["http_headers"])
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp_servers.%s.http_headers: %v", relPath, name, err)
		}
		envHdrs, err := asStringMap(entry["env_http_headers"])
		if err != nil {
			return importedMCP{}, spec.NewPolicyError("%s: mcp_servers.%s.env_http_headers: %v", relPath, name, err)
		}
		bearerVar := strings.TrimSpace(asString(entry["bearer_token_env_var"]))

		hdrs := make(map[string]string)
		for k, v := range staticHdrs {
			hdrs[k] = normalizeEnvSyntax(v)
		}
		for hdrName, varName := range envHdrs {
			varName = strings.TrimSpace(varName)
			ph := "${" + varName + "}"
			if prev, exists := hdrs[hdrName]; exists && prev != ph {
				return importedMCP{}, spec.NewPolicyError("%s: mcp_servers.%s defines conflicting http_headers and env_http_headers for %q", relPath, name, hdrName)
			}
			hdrs[hdrName] = ph
		}
		if bearerVar != "" {
			authVal := "Bearer ${" + bearerVar + "}"
			for _, k := range sortedStringMapKeys(hdrs) {
				if !strings.EqualFold(k, "Authorization") {
					continue
				}
				if hdrs[k] != authVal {
					return importedMCP{}, spec.NewPolicyError("%s: mcp_servers.%s defines conflicting Authorization header and bearer_token_env_var", relPath, name)
				}
				if k != "Authorization" {
					delete(hdrs, k)
				}
			}
			hdrs["Authorization"] = authVal
		}
		if len(hdrs) > 0 {
			srv.Headers = hdrs
		}
	default:
		return importedMCP{}, spec.NewPolicyError("%s: mcp_servers.%s must specify command or url", relPath, name)
	}

	if err := checkMCPSecrets(relPath, name, srv); err != nil {
		return importedMCP{}, err
	}
	return srv, nil
}

func checkMCPSecrets(relPath, name string, srv importedMCP) error {
	if srv.URL != "" && diff.ContainsLiteralSecret(srv.URL) {
		return spec.NewPolicyError("refusing literal secret in %s (mcp.%s.url): replace with ${ENV_VAR} placeholder", relPath, name)
	}
	if srv.Command != "" && diff.ContainsLiteralSecret(srv.Command) {
		return spec.NewPolicyError("refusing literal secret in %s (mcp.%s.command): replace with ${ENV_VAR} placeholder", relPath, name)
	}
	for i, arg := range srv.Args {
		if diff.ContainsLiteralSecret(arg) {
			return spec.NewPolicyError("refusing literal secret in %s (mcp.%s.args[%d]): replace with ${ENV_VAR} placeholder", relPath, name, i)
		}
	}
	for _, k := range sortedStringMapKeys(srv.Env) {
		v := srv.Env[k]
		if diff.ContainsLiteralSecret(k+"="+v) || diff.ContainsLiteralSecret(v) {
			return spec.NewPolicyError("refusing literal secret in %s (mcp.%s.env.%s): replace with ${ENV_VAR} placeholder", relPath, name, k)
		}
	}
	for _, k := range sortedStringMapKeys(srv.Headers) {
		v := srv.Headers[k]
		if diff.ContainsLiteralSecret(k+": "+v) || diff.ContainsLiteralSecret(v) {
			return spec.NewPolicyError("refusing literal secret in %s (mcp.%s.headers.%s): replace with ${ENV_VAR} placeholder", relPath, name, k)
		}
	}
	return nil
}

func (c *importCollector) addMCP(name string, srv importedMCP) error {
	if prev, exists := c.mcp[name]; exists {
		if !mcpEqual(prev, srv) {
			return spec.NewPolicyError("divergent definitions for mcp server %q between %s and %s", name, prev.Source, srv.Source)
		}
		return nil
	}
	c.mcp[name] = srv
	return nil
}

func mcpEqual(a, b importedMCP) bool {
	if a.Transport != b.Transport || a.Command != b.Command || a.URL != b.URL {
		return false
	}
	if !slices.Equal(a.Args, b.Args) {
		return false
	}
	if !stringMapEqual(a.Env, b.Env) {
		return false
	}
	return stringMapEqual(a.Headers, b.Headers)
}

func (c *importCollector) scanClaudeSettings(relPath string) error {
	raw, ok, err := c.readSafeFile(relPath)
	if err != nil || !ok {
		return err
	}
	doc, err := parseJSONCDoc(relPath, raw)
	if err != nil {
		return err
	}
	importedAny := false

	if permsRaw, exists := doc["permissions"]; exists && permsRaw != nil {
		perms, ok := permsRaw.(map[string]any)
		if !ok {
			return spec.NewPolicyError("%s: permissions must be a JSON object", relPath)
		}
		allows, err := asStringSlice(perms["allow"])
		if err != nil {
			return spec.NewPolicyError("%s: permissions.allow: %v", relPath, err)
		}
		for _, rule := range allows {
			if cmd, ok := parseClaudeBashRule(rule); ok {
				if _, exists := c.allowCommands[cmd]; !exists {
					c.allowCommands[cmd] = relPath
				}
				importedAny = true
			}
		}
		denies, err := asStringSlice(perms["deny"])
		if err != nil {
			return spec.NewPolicyError("%s: permissions.deny: %v", relPath, err)
		}
		for _, rule := range denies {
			if cmd, ok := parseClaudeBashRule(rule); ok {
				if _, exists := c.denyCommands[cmd]; !exists {
					c.denyCommands[cmd] = relPath
				}
				importedAny = true
			}
		}
	}

	if hooksRaw, exists := doc["hooks"]; exists && hooksRaw != nil {
		hooksMap, ok := hooksRaw.(map[string]any)
		if !ok {
			return spec.NewPolicyError("%s: hooks must be a JSON object", relPath)
		}
		for _, event := range sortedAnyMapKeys(hooksMap) {
			arr, ok := hooksMap[event].([]any)
			if !ok {
				return spec.NewPolicyError("%s: hooks.%s must be an array", relPath, event)
			}
			for _, itemRaw := range arr {
				item, ok := itemRaw.(map[string]any)
				if !ok {
					return spec.NewPolicyError("%s: hooks.%s entry must be an object", relPath, event)
				}
				matcher := strings.TrimSpace(asString(item["matcher"]))
				if innerRaw, hasInner := item["hooks"]; hasInner && innerRaw != nil {
					innerArr, ok := innerRaw.([]any)
					if !ok {
						return spec.NewPolicyError("%s: hooks.%s[].hooks must be an array", relPath, event)
					}
					for _, hRaw := range innerArr {
						hObj, ok := hRaw.(map[string]any)
						if !ok {
							continue
						}
						hType := strings.TrimSpace(asString(hObj["type"]))
						if hType != "" && hType != "command" {
							continue
						}
						cmd := normalizeEnvSyntax(strings.TrimSpace(asString(hObj["command"])))
						if cmd == "" {
							continue
						}
						if err := c.addHook(relPath, event, matcher, cmd); err != nil {
							return err
						}
						importedAny = true
					}
				} else if cmdRaw := strings.TrimSpace(asString(item["command"])); cmdRaw != "" {
					cmd := normalizeEnvSyntax(cmdRaw)
					if err := c.addHook(relPath, event, matcher, cmd); err != nil {
						return err
					}
					importedAny = true
				}
			}
		}
	}

	if importedAny {
		c.recordSource(relPath)
		c.markTarget("claude", false)
	}
	return nil
}

func (c *importCollector) addHook(relPath, event, matcher, cmd string) error {
	if diff.ContainsLiteralSecret(cmd) {
		return spec.NewPolicyError("refusing literal secret in %s (hooks.%s command): replace with ${ENV_VAR} placeholder", relPath, event)
	}
	hk := importedHook{
		Event:   event,
		Matcher: matcher,
		Command: cmd,
		Enforce: event == "PreToolUse",
		Source:  relPath,
	}
	for _, existing := range c.hooks[event] {
		if existing.Matcher == hk.Matcher && existing.Command == hk.Command && existing.Enforce == hk.Enforce {
			return nil
		}
	}
	c.hooks[event] = append(c.hooks[event], hk)
	return nil
}

func parseClaudeBashRule(rule string) (string, bool) {
	m := reClaudeBashRule.FindStringSubmatch(strings.TrimSpace(rule))
	if len(m) != 2 {
		return "", false
	}
	inner := strings.TrimSpace(m[1])
	inner = strings.TrimSuffix(inner, ":*")
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return "", false
	}
	return inner, true
}

func (c *importCollector) scanSkillsDir(relRoot, target string, shared bool) error {
	resolvedRoot, ok, err := c.isSafePath(relRoot)
	if err != nil || !ok {
		return err
	}
	entries, err := os.ReadDir(resolvedRoot)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		relSkill := relRoot + "/" + name
		resolvedSkill, safe, err := c.isSafePath(relSkill)
		if err != nil {
			return err
		}
		if !safe {
			continue
		}
		st, err := os.Stat(resolvedSkill)
		if err != nil || !st.IsDir() {
			continue
		}
		slug, err := sanitizeSlug(name)
		if err != nil {
			return spec.NewPolicyError("%s: invalid skill name %q: %v", relSkill, name, err)
		}
		hash, err := store.HashTree(resolvedSkill)
		if err != nil {
			if errors.Is(err, store.ErrSymlinkNotAllowed) {
				return spec.NewPolicyError("%s: %v", relSkill, err)
			}
			return err
		}

		c.recordSource(relSkill)
		if prev, exists := c.skills[slug]; exists {
			if prev.TreeHash != hash {
				return spec.NewPolicyError("divergent definitions for skill %q between %s and %s", slug, prev.RelSource, relSkill)
			}
		} else {
			c.skills[slug] = importedSkill{
				Name:      slug,
				SourceDir: resolvedSkill,
				RelSource: relSkill,
				TreeHash:  hash,
			}
		}
		c.markTarget(target, shared)
	}
	return nil
}

func (c *importCollector) scanAgentsDir(relRoot, target string) error {
	resolvedRoot, ok, err := c.isSafePath(relRoot)
	if err != nil || !ok {
		return err
	}
	entries, err := os.ReadDir(resolvedRoot)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		relFile := relRoot + "/" + name
		raw, safe, err := c.readSafeFile(relFile)
		if err != nil || !safe {
			if err != nil {
				return err
			}
			continue
		}
		base := name[:len(name)-len(".md")]
		slug, err := sanitizeSlug(base)
		if err != nil {
			return spec.NewPolicyError("%s: invalid agent name %q: %v", relFile, base, err)
		}

		fmYAML, hasFM, body := splitFrontmatter(string(raw))
		var desc, model string
		if hasFM {
			var fm struct {
				Description string `yaml:"description"`
				Model       string `yaml:"model"`
			}
			if err := yaml.Unmarshal([]byte(fmYAML), &fm); err != nil {
				return spec.NewPolicyError("%s: invalid YAML frontmatter: %v", relFile, err)
			}
			desc = strings.TrimSpace(fm.Description)
			model = strings.TrimSpace(fm.Model)
		}
		normBody := normalizeMarkdownBody(body)
		ag := importedAgent{
			Name:        slug,
			Description: desc,
			Model:       model,
			Body:        normBody,
			Source:      relFile,
		}
		c.recordSource(relFile)
		if prev, exists := c.agents[slug]; exists {
			if prev.Description != ag.Description || prev.Model != ag.Model || prev.Body != ag.Body {
				return spec.NewPolicyError("divergent definitions for agent %q between %s and %s", slug, prev.Source, relFile)
			}
		} else {
			c.agents[slug] = ag
		}
		c.markTarget(target, false)
	}
	return nil
}

func (c *importCollector) scanRuleDir(relRoot, ext, flavor, target string) error {
	resolvedRoot, ok, err := c.isSafePath(relRoot)
	if err != nil || !ok {
		return err
	}
	entries, err := os.ReadDir(resolvedRoot)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ext) {
			continue
		}
		relFile := relRoot + "/" + name
		raw, safe, err := c.readSafeFile(relFile)
		if err != nil || !safe {
			if err != nil {
				return err
			}
			continue
		}
		base := name[:len(name)-len(ext)]
		slug, err := sanitizeSlug(base)
		if err != nil {
			return spec.NewPolicyError("%s: invalid rule name %q: %v", relFile, base, err)
		}
		rl, err := parseDropInRule(relFile, slug, flavor, string(raw))
		if err != nil {
			return err
		}
		if flavor == "cursor" && strings.HasPrefix(base, "agentfile-skill-") {
			skillStem := strings.TrimPrefix(base, "agentfile-skill-")
			if skillStem != "" && rl.Description == "Skill: "+skillStem && !rl.AlwaysApply && len(rl.Globs) == 0 {
				c.markTarget(target, false)
				continue
			}
		}
		c.recordSource(relFile)
		if err := c.addRule(rl); err != nil {
			return err
		}
		c.markTarget(target, false)
	}
	return nil
}

func parseDropInRule(relFile, slug, flavor, raw string) (importedRule, error) {
	fmYAML, hasFM, body := splitFrontmatter(raw)
	rl := importedRule{
		Name:   slug,
		Source: relFile,
	}
	if hasFM {
		var fm struct {
			Description      string `yaml:"description"`
			Globs            any    `yaml:"globs"`
			Paths            any    `yaml:"paths"`
			AlwaysApply      *bool  `yaml:"alwaysApply"`
			AlwaysApplySnake *bool  `yaml:"always_apply"`
			Trigger          string `yaml:"trigger"`
		}
		if err := yaml.Unmarshal([]byte(fmYAML), &fm); err != nil {
			return importedRule{}, spec.NewPolicyError("%s: invalid YAML frontmatter: %v", relFile, err)
		}
		rl.Description = strings.TrimSpace(fm.Description)
		rl.HasExplicitDescription = rl.Description != ""
		if fm.Globs != nil {
			rl.Globs = parseGlobList(fm.Globs)
		} else if fm.Paths != nil {
			rl.Globs = parseGlobList(fm.Paths)
		}
		rl.HasExplicitGlobs = len(rl.Globs) > 0
		switch {
		case fm.AlwaysApply != nil:
			rl.AlwaysApply = *fm.AlwaysApply
			rl.HasExplicitAlwaysApply = true
		case fm.AlwaysApplySnake != nil:
			rl.AlwaysApply = *fm.AlwaysApplySnake
			rl.HasExplicitAlwaysApply = true
		case flavor == "windsurf":
			switch strings.ToLower(strings.TrimSpace(fm.Trigger)) {
			case "always_on":
				rl.AlwaysApply = true
				rl.HasExplicitAlwaysApply = false
			case "glob":
				rl.AlwaysApply = false
				rl.HasExplicitAlwaysApply = true
			}
		}
	}
	rl.Body = normalizeMarkdownBody(body)
	return rl, nil
}

func (c *importCollector) scanSingleRuleFile(relFile, target string, shared bool, fallbackSlug string) error {
	raw, ok, err := c.readSafeFile(relFile)
	if err != nil || !ok {
		return err
	}
	content := normalizeEOL(string(raw))
	if inner, found, err := extractManagedBlock(content); err != nil {
		return spec.NewPolicyError("%s: %v", relFile, err)
	} else if found {
		content = inner
	}

	rules, err := parseMultiSectionRules(relFile, content, fallbackSlug)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		return nil
	}
	c.recordSource(relFile)
	for _, rl := range rules {
		if err := c.addRule(rl); err != nil {
			return err
		}
	}
	c.markTarget(target, shared)
	return nil
}

func parseMultiSectionRules(relFile, content, fallbackSlug string) ([]importedRule, error) {
	lines := strings.Split(normalizeEOL(content), "\n")
	type rawSection struct {
		title string
		lines []string
	}

	var preamble []string
	var sections []rawSection
	curIdx := -1

	inFence := false
	var fenceChar byte
	var fenceLen int

	for _, line := range lines {
		if fc, fl, ok := parseFenceLine(line); ok {
			if !inFence {
				inFence = true
				fenceChar = fc
				fenceLen = fl
			} else if fc == fenceChar && fl >= fenceLen && strings.TrimSpace(strings.TrimSpace(line)[fl:]) == "" {
				inFence = false
			}
		} else if !inFence && strings.HasPrefix(line, "## ") {
			title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if title != "" {
				sections = append(sections, rawSection{title: title})
				curIdx = len(sections) - 1
				continue
			}
		}

		if curIdx < 0 {
			preamble = append(preamble, line)
		} else {
			sections[curIdx].lines = append(sections[curIdx].lines, line)
		}
	}

	if len(sections) == 0 {
		body := normalizeMarkdownBody(content)
		if body == "" {
			return nil, nil
		}
		return []importedRule{{
			Name:   fallbackSlug,
			Body:   body,
			Source: relFile,
		}}, nil
	}

	var out []importedRule
	// Keep non-header preamble if the file had substantive text before the first ## heading.
	cleanedPreamble := stripLeadingH1Title(preamble)
	if body := normalizeMarkdownBody(strings.Join(cleanedPreamble, "\n")); body != "" {
		out = append(out, importedRule{
			Name:   fallbackSlug,
			Body:   body,
			Source: relFile,
		})
	}

	for _, sec := range sections {
		slug, err := sanitizeSlug(sec.title)
		if err != nil {
			return nil, spec.NewPolicyError("%s: section %q: %v", relFile, sec.title, err)
		}
		secLines := trimBlankAndSeparatorLines(sec.lines)
		var desc string
		var globs []string
		bodyStart := 0
		for bodyStart < len(secLines) {
			trimmed := strings.TrimSpace(secLines[bodyStart])
			if trimmed == "" {
				bodyStart++
				continue
			}
			if strings.HasPrefix(trimmed, "*Scope:*") {
				desc = strings.TrimSpace(strings.TrimPrefix(trimmed, "*Scope:*"))
				bodyStart++
				continue
			}
			if strings.HasPrefix(trimmed, "*Applies to:*") {
				rawGlobs := strings.TrimSpace(strings.TrimPrefix(trimmed, "*Applies to:*"))
				globs = parseBacktickOrCSVGlobs(rawGlobs)
				bodyStart++
				continue
			}
			break
		}
		body := normalizeMarkdownBody(strings.Join(secLines[bodyStart:], "\n"))
		if body == "" && desc == "" && len(globs) == 0 {
			continue
		}
		out = append(out, importedRule{
			Name:                   slug,
			Description:            desc,
			Globs:                  globs,
			HasExplicitDescription: desc != "",
			HasExplicitGlobs:       len(globs) > 0,
			Body:                   body,
			Source:                 relFile,
		})
	}
	return out, nil
}

func stripLeadingH1Title(lines []string) []string {
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "# ") {
		i++
	}
	return lines[i:]
}

func trimBlankAndSeparatorLines(lines []string) []string {
	end := len(lines)
	for end > 0 {
		t := strings.TrimSpace(lines[end-1])
		if t == "" || t == "---" {
			end--
			continue
		}
		break
	}
	start := 0
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	return lines[start:end]
}

func parseBacktickOrCSVGlobs(raw string) []string {
	parts := strings.Split(raw, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "`")
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *importCollector) addRule(rl importedRule) error {
	prev, exists := c.rules[rl.Name]
	if !exists {
		c.rules[rl.Name] = rl
		return nil
	}
	if prev.Body != rl.Body {
		return spec.NewPolicyError("divergent definitions for rule %q between %s and %s", rl.Name, prev.Source, rl.Source)
	}
	if prev.HasExplicitDescription && rl.HasExplicitDescription && prev.Description != rl.Description {
		return spec.NewPolicyError("divergent definitions for rule %q between %s and %s", rl.Name, prev.Source, rl.Source)
	}
	if prev.HasExplicitGlobs && rl.HasExplicitGlobs && !slices.Equal(prev.Globs, rl.Globs) {
		return spec.NewPolicyError("divergent definitions for rule %q between %s and %s", rl.Name, prev.Source, rl.Source)
	}
	if prev.HasExplicitAlwaysApply && rl.HasExplicitAlwaysApply && prev.AlwaysApply != rl.AlwaysApply {
		return spec.NewPolicyError("divergent definitions for rule %q between %s and %s", rl.Name, prev.Source, rl.Source)
	}

	if !prev.HasExplicitDescription && rl.HasExplicitDescription {
		prev.Description = rl.Description
		prev.HasExplicitDescription = true
	}
	if !prev.HasExplicitGlobs && rl.HasExplicitGlobs {
		prev.Globs = append([]string(nil), rl.Globs...)
		prev.HasExplicitGlobs = true
	}
	switch {
	case prev.HasExplicitAlwaysApply:
		// Keep explicit AlwaysApply from prev.
	case rl.HasExplicitAlwaysApply:
		prev.AlwaysApply = rl.AlwaysApply
		prev.HasExplicitAlwaysApply = true
	default:
		prev.AlwaysApply = prev.AlwaysApply || rl.AlwaysApply
	}
	c.rules[rl.Name] = prev
	return nil
}

type yamlImportManifest struct {
	Version  int                 `yaml:"version"`
	Targets  []string            `yaml:"targets"`
	MCP      map[string]yamlMCP  `yaml:"mcp,omitempty"`
	Skills   []yamlSkill         `yaml:"skills,omitempty"`
	Agents   []yamlAgent         `yaml:"agents,omitempty"`
	Rules    []yamlRule          `yaml:"rules,omitempty"`
	Settings *yamlImportSettings `yaml:"settings,omitempty"`
}

type yamlMCP struct {
	Transport string            `yaml:"transport"`
	Command   string            `yaml:"command,omitempty"`
	Args      []string          `yaml:"args,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	URL       string            `yaml:"url,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
}

type yamlSkill struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
}

type yamlAgent struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Model       string `yaml:"model,omitempty"`
	Source      string `yaml:"source"`
}

type yamlRule struct {
	Name        string   `yaml:"name"`
	Source      string   `yaml:"source"`
	Description string   `yaml:"description,omitempty"`
	Globs       []string `yaml:"globs,omitempty"`
	AlwaysApply bool     `yaml:"always_apply,omitempty"`
}

type yamlImportSettings struct {
	Permissions *yamlImportPermissions `yaml:"permissions,omitempty"`
	Hooks       map[string][]yamlHook  `yaml:"hooks,omitempty"`
}

type yamlImportPermissions struct {
	AllowCommands []string `yaml:"allow_commands,omitempty"`
	DenyCommands  []string `yaml:"deny_commands,omitempty"`
}

type yamlHook struct {
	Matcher string `yaml:"matcher,omitempty"`
	Command string `yaml:"command"`
	Enforce bool   `yaml:"enforce,omitempty"`
}

func (c *importCollector) buildManifestAndSummary(targets []string, dryRun bool) ([]byte, *ImportSummary, error) {
	m := yamlImportManifest{
		Version: 1,
		Targets: targets,
	}
	summary := &ImportSummary{
		DryRun:         dryRun,
		Targets:        append([]string(nil), targets...),
		SourcesScanned: append([]string(nil), c.sourcesScanned...),
	}

	if len(c.mcp) > 0 {
		m.MCP = make(map[string]yamlMCP, len(c.mcp))
		names := make([]string, 0, len(c.mcp))
		for k := range c.mcp {
			names = append(names, k)
		}
		sort.Strings(names)
		summary.MCP = names
		for _, k := range names {
			srv := c.mcp[k]
			m.MCP[k] = yamlMCP{
				Transport: srv.Transport,
				Command:   srv.Command,
				Args:      srv.Args,
				Env:       srv.Env,
				URL:       srv.URL,
				Headers:   srv.Headers,
			}
		}
	}

	if len(c.skills) > 0 {
		names := make([]string, 0, len(c.skills))
		for k := range c.skills {
			names = append(names, k)
		}
		sort.Strings(names)
		summary.Skills = names
		for _, k := range names {
			relSrc := "./.agentfile/imported/skills/" + k
			m.Skills = append(m.Skills, yamlSkill{
				Name:   k,
				Source: relSrc,
			})
			summary.ImportedFiles = append(summary.ImportedFiles, ".agentfile/imported/skills/"+k)
		}
	}

	if len(c.agents) > 0 {
		names := make([]string, 0, len(c.agents))
		for k := range c.agents {
			names = append(names, k)
		}
		sort.Strings(names)
		summary.Agents = names
		for _, k := range names {
			ag := c.agents[k]
			relSrc := "./.agentfile/imported/agents/" + k + ".md"
			m.Agents = append(m.Agents, yamlAgent{
				Name:        k,
				Description: ag.Description,
				Model:       ag.Model,
				Source:      relSrc,
			})
			summary.ImportedFiles = append(summary.ImportedFiles, ".agentfile/imported/agents/"+k+".md")
		}
	}

	if len(c.rules) > 0 {
		names := make([]string, 0, len(c.rules))
		for k := range c.rules {
			names = append(names, k)
		}
		sort.Strings(names)
		summary.Rules = names
		for _, k := range names {
			rl := c.rules[k]
			relSrc := "./.agentfile/imported/rules/" + k + ".md"
			m.Rules = append(m.Rules, yamlRule{
				Name:        k,
				Source:      relSrc,
				Description: rl.Description,
				Globs:       rl.Globs,
				AlwaysApply: rl.AlwaysApply,
			})
			summary.ImportedFiles = append(summary.ImportedFiles, ".agentfile/imported/rules/"+k+".md")
		}
	}

	if len(c.allowCommands) > 0 || len(c.denyCommands) > 0 || len(c.hooks) > 0 {
		settings := &yamlImportSettings{}
		if len(c.allowCommands) > 0 || len(c.denyCommands) > 0 {
			perms := &yamlImportPermissions{}
			if len(c.allowCommands) > 0 {
				perms.AllowCommands = sortedStringMapKeys(c.allowCommands)
				summary.AllowCommands = append([]string(nil), perms.AllowCommands...)
			}
			if len(c.denyCommands) > 0 {
				perms.DenyCommands = sortedStringMapKeys(c.denyCommands)
				summary.DenyCommands = append([]string(nil), perms.DenyCommands...)
			}
			settings.Permissions = perms
		}
		if len(c.hooks) > 0 {
			settings.Hooks = make(map[string][]yamlHook, len(c.hooks))
			events := make([]string, 0, len(c.hooks))
			for ev := range c.hooks {
				events = append(events, ev)
			}
			sort.Strings(events)
			for _, ev := range events {
				list := append([]importedHook(nil), c.hooks[ev]...)
				sort.Slice(list, func(i, j int) bool {
					if list[i].Matcher != list[j].Matcher {
						return list[i].Matcher < list[j].Matcher
					}
					return list[i].Command < list[j].Command
				})
				for _, hk := range list {
					settings.Hooks[ev] = append(settings.Hooks[ev], yamlHook{
						Matcher: hk.Matcher,
						Command: hk.Command,
						Enforce: hk.Enforce,
					})
					summary.Hooks = append(summary.Hooks, ev+": "+hk.Command)
				}
			}
		}
		m.Settings = settings
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&m); err != nil {
		return nil, nil, fmt.Errorf("marshal Agentfile.yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	manifestBytes := buf.Bytes()
	summary.ImportedFiles = append(summary.ImportedFiles, "Agentfile.yaml")
	summary.ManifestYAML = string(manifestBytes)
	return manifestBytes, summary, nil
}

func (c *importCollector) commitFiles(manifestBytes []byte) error {
	importedRoot := filepath.Join(c.workDir, ".agentfile", "imported")
	// Stage under a sibling temp directory so any write failure leaves existing .agentfile/imported intact.
	parentDir := filepath.Join(c.workDir, ".agentfile")
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return err
	}
	stageDir, err := os.MkdirTemp(parentDir, ".imported-stage-*")
	if err != nil {
		return err
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageDir)
		}
	}()

	for name, sk := range c.skills {
		dst := filepath.Join(stageDir, "skills", name)
		if err := copyImportDir(sk.SourceDir, dst); err != nil {
			return fmt.Errorf("extract skill %s: %w", name, err)
		}
	}
	for name, ag := range c.agents {
		dst := filepath.Join(stageDir, "agents", name+".md")
		if err := writeFileAtomic(dst, []byte(ag.Body), 0o644); err != nil {
			return fmt.Errorf("extract agent %s: %w", name, err)
		}
	}
	for name, rl := range c.rules {
		dst := filepath.Join(stageDir, "rules", name+".md")
		if err := writeFileAtomic(dst, []byte(rl.Body), 0o644); err != nil {
			return fmt.Errorf("extract rule %s: %w", name, err)
		}
	}

	if err := os.RemoveAll(importedRoot); err != nil {
		return err
	}
	if err := os.Rename(stageDir, importedRoot); err != nil {
		return err
	}
	cleanupStage = false

	manifestPath := filepath.Join(c.workDir, "Agentfile.yaml")
	if err := writeFileAtomic(manifestPath, manifestBytes, 0o644); err != nil {
		return fmt.Errorf("write Agentfile.yaml: %w", err)
	}
	return nil
}

func printImportHuman(w io.Writer, s *ImportSummary) {
	action := "Created"
	if s.DryRun {
		action = "Dry-run: would create"
	}
	fmt.Fprintf(w, "%s Agentfile.yaml (targets: %v)\n", action, s.Targets)
	fmt.Fprintf(w, "Imported: %d mcp, %d skills, %d agents, %d rules, %d allow_commands, %d deny_commands, %d hooks\n",
		len(s.MCP), len(s.Skills), len(s.Agents), len(s.Rules), len(s.AllowCommands), len(s.DenyCommands), len(s.Hooks))
	if len(s.ImportedFiles) > 0 {
		fmt.Fprintln(w, "Outputs:")
		for _, f := range s.ImportedFiles {
			fmt.Fprintf(w, "  + %s\n", f)
		}
	}
}

func copyImportDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", store.ErrSymlinkNotAllowed, p)
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular file %s", p)
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return writeFileAtomic(target, data, mode)
	})
}

func normalizeEnvSyntax(s string) string {
	return reForeignEnvVar.ReplaceAllStringFunc(s, func(match string) string {
		sub := reForeignEnvVar.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		return "${" + sub[1] + "}"
	})
}

func sanitizeSlug(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "agentfile-")
	if reImportSlug.MatchString(s) {
		return s, nil
	}
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		isAllowedPunct := r == '.' || r == '_' || r == '-'
		switch {
		case isAlnum:
			b.WriteRune(r)
			prevDash = false
		case isAllowedPunct:
			b.WriteRune(r)
			prevDash = r == '-'
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" || !reImportSlug.MatchString(out) {
		return "", spec.NewPolicyError("cannot derive valid identifier slug from %q", raw)
	}
	return out, nil
}

func splitFrontmatter(raw string) (fmYAML string, hasFM bool, body string) {
	norm := normalizeEOL(strings.TrimPrefix(raw, "\ufeff"))
	lines := strings.Split(norm, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return "", false, norm
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), true, strings.Join(lines[i+1:], "\n")
		}
	}
	return "", false, norm
}

func normalizeMarkdownBody(s string) string {
	norm := strings.TrimSpace(normalizeEOL(s))
	if norm == "" {
		return ""
	}
	return norm + "\n"
}

func normalizeEOL(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func extractManagedBlock(normContent string) (string, bool, error) {
	const (
		beginPrefix = "<!-- BEGIN AGENTFILE:v1 "
		beginSuffix = " -->"
		endMarker   = "<!-- END AGENTFILE:v1 -->"
	)
	lines := strings.Split(normContent, "\n")
	inFence := false
	var fenceChar byte
	var fenceLen int

	startLine := -1
	endLine := -1
	beginCount := 0
	endCount := 0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fc, fl, ok := parseFenceLine(line); ok {
			if !inFence {
				inFence = true
				fenceChar = fc
				fenceLen = fl
			} else if fc == fenceChar && fl >= fenceLen && strings.TrimSpace(trimmed[fl:]) == "" {
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(trimmed, beginPrefix) && strings.HasSuffix(trimmed, beginSuffix) {
			beginCount++
			if beginCount > 1 {
				return "", false, spec.NewPolicyError("duplicate AGENTFILE BEGIN marker at line %d", i+1)
			}
			startLine = i
		} else if trimmed == endMarker {
			endCount++
			if beginCount == 0 || endCount > 1 {
				return "", false, spec.NewPolicyError("unexpected AGENTFILE END marker at line %d", i+1)
			}
			endLine = i
		}
	}

	if beginCount != endCount {
		return "", false, spec.NewPolicyError("unclosed AGENTFILE Markdown block marker")
	}
	if beginCount == 0 {
		return normContent, false, nil
	}
	return strings.Join(lines[startLine+1:endLine], "\n"), true, nil
}

func parseFenceLine(line string) (byte, int, bool) {
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' && spaces < 4 {
		spaces++
	}
	if spaces == 4 || spaces >= len(line) {
		return 0, 0, false
	}
	ch := line[spaces]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	count := 0
	for i := spaces; i < len(line) && line[i] == ch; i++ {
		count++
	}
	if count < 3 {
		return 0, 0, false
	}
	return ch, count, true
}

func parseGlobList(v any) []string {
	switch x := v.(type) {
	case string:
		var out []string
		for _, part := range strings.Split(x, ",") {
			p := strings.TrimSpace(part)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	case []any:
		var out []string
		for _, item := range x {
			s := strings.TrimSpace(asString(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		var out []string
		for _, item := range x {
			s := strings.TrimSpace(item)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asStringSlice(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...), nil
	case []any:
		out := make([]string, 0, len(x))
		for i, item := range x {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("element %d must be a string", i)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("must be an array of strings")
	}
}

func asStringMap(v any) (map[string]string, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be a key-value map")
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		s, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("key %q value must be a string", k)
		}
		out[k] = s
	}
	return out, nil
}

func sortedAnyMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		if vb, ok := b[k]; !ok || va != vb {
			return false
		}
	}
	return true
}
