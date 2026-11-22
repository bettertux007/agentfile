package schemas

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertux007/agentfile/internal/spec"
)

func TestStarterExampleConformsToSchemaAndSpec(t *testing.T) {
	t.Parallel()
	starterPath := filepath.Join("..", "examples", "starter", "Agentfile.yaml")
	raw, err := os.ReadFile(starterPath)
	if err != nil {
		t.Fatalf("read starter Agentfile.yaml: %v", err)
	}
	if err := ValidateAgentfileV1(raw); err != nil {
		t.Fatalf("schema validation failed on starter example: %v", err)
	}
	if _, err := spec.ParseFile(starterPath); err != nil {
		t.Fatalf("spec.ParseFile failed on starter example: %v", err)
	}
}

func TestRootAgentfileConformsToSchemaAndSpec(t *testing.T) {
	t.Parallel()
	rootManifestPath := filepath.Join("..", "Agentfile.yaml")
	rawManifest, err := os.ReadFile(rootManifestPath)
	if err != nil {
		t.Fatalf("read root Agentfile.yaml: %v", err)
	}
	if err := ValidateAgentfileV1(rawManifest); err != nil {
		t.Fatalf("schema validation failed on root Agentfile.yaml: %v", err)
	}
	ps, err := spec.ParseFile(rootManifestPath)
	if err != nil {
		t.Fatalf("spec.ParseFile failed on root Agentfile.yaml: %v", err)
	}
	if len(ps.Targets) != 2 || ps.Targets[0] != "opencode" || ps.Targets[1] != "claude" {
		t.Fatalf("unexpected root targets: %v", ps.Targets)
	}
	for _, target := range ps.Targets {
		tc, ok := ps.TargetsConfig[target]
		if !ok || tc.LoadGlobal == nil || *tc.LoadGlobal {
			t.Fatalf("expected root Agentfile.yaml target %q to enforce load_global: false", target)
		}
	}

	rootLockPath := filepath.Join("..", "Agentfile.lock")
	rawLock, err := os.ReadFile(rootLockPath)
	if err != nil {
		t.Fatalf("read root Agentfile.lock: %v", err)
	}
	if err := ValidateLockfileV1(rawLock); err != nil {
		t.Fatalf("schema validation failed on root Agentfile.lock: %v", err)
	}
	lf, err := spec.LoadLock(rootLockPath)
	if err != nil {
		t.Fatalf("spec.LoadLock failed on root Agentfile.lock: %v", err)
	}
	if len(lf.Dependencies) != 18 {
		t.Fatalf("expected 18 locked remote dependencies in root Agentfile.lock, got %d", len(lf.Dependencies))
	}
}

func TestValidateAgentfileV1RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if err := ValidateAgentfileV1([]byte("version: 2\ntargets: [claude]\n")); err == nil {
		t.Fatalf("expected schema error for version 2")
	}
	if err := ValidateAgentfileV1([]byte(":\n\t[invalid")); err == nil {
		t.Fatalf("expected error on invalid YAML syntax")
	}
	if err := ValidateAgentfileV1([]byte("version: 1\ntargets: [claude]\nmcp:\n  a:\n    transport: stdio\n    command: npx\n    url: https://x\n")); err == nil {
		t.Fatalf("expected schema error when stdio defines url")
	}
}

func TestValidateLocalConfigAndLockSchemas(t *testing.T) {
	t.Parallel()
	validCfg := []byte("version: 1\nlink_mode: copy\non_conflict: ask\ngit_exclude: true\n")
	if err := ValidateLocalConfigV1(validCfg); err != nil {
		t.Fatalf("expected valid local config: %v", err)
	}
	if err := ValidateLocalConfigV1([]byte("version: 1\nload_global: false\n")); err != nil {
		t.Fatalf("expected valid local config with load_global: %v", err)
	}
	if err := ValidateAgentfileV1([]byte("version: 1\ntargets: [claude]\ntargets_config:\n  claude:\n    load_global: false\n    on_isolation_unsupported: warn\n")); err != nil {
		t.Fatalf("expected valid agentfile with load_global: %v", err)
	}
	if err := ValidateLocalConfigV1([]byte("version: 1\nlink_mode: hardlink\n")); err == nil {
		t.Fatalf("expected error on invalid link_mode")
	}

	validLock := []byte("version: 1\ndependencies:\n  skill:k8s:\n    source: https://github.com/org/repo.git\n    ref: v1.0.0\n    commit_sha: " +
		strings.Repeat("a", 40) + "\n    content_hash: sha256:" + strings.Repeat("b", 64) + "\n")
	if err := ValidateLockfileV1(validLock); err != nil {
		t.Fatalf("expected valid lockfile: %v", err)
	}
	if err := ValidateLockfileV1([]byte("version: 1\ndependencies:\n  badkey:\n    source: x\n    commit_sha: abc\n    content_hash: xyz\n")); err == nil {
		t.Fatalf("expected error on invalid lockfile entry")
	}
}

func TestValidateUserGlobalConfigSchema(t *testing.T) {
	t.Parallel()
	userGlobalCfg := []byte("version: 1\nlink_mode: symlink\non_conflict: backup\ngit_exclude: false\nload_global: true\n")
	if err := ValidateLocalConfigV1(userGlobalCfg); err != nil {
		t.Fatalf("expected valid user-global config payload: %v", err)
	}
	if err := ValidateLocalConfigV1([]byte("version: 1\nunknown_pref: true\n")); err == nil {
		t.Fatalf("expected error on unknown field in user-global config")
	}

	schemaBytes, err := os.ReadFile(filepath.Join("v1", "agentfile-config.schema.json"))
	if err != nil {
		t.Fatalf("read agentfile-config.schema.json: %v", err)
	}
	schemaText := string(schemaBytes)
	if !strings.Contains(schemaText, ".agentfile/config.yaml") || !strings.Contains(schemaText, "~/.config/agentfile/config.yaml") {
		t.Fatalf("expected agentfile-config.schema.json description to mention both .agentfile/config.yaml and ~/.config/agentfile/config.yaml")
	}
}

func TestValidateAgentfileV1Sprint2TargetsAndRulesMode(t *testing.T) {
	t.Parallel()

	valid := []byte(`
version: 1
targets: [claude, opencode, gemini, cursor, codex, copilot, windsurf]
targets_config:
  claude:
    rules_mode: dropin
  codex:
    rules_mode: agents_md
  copilot:
    rules_mode: copilot
  windsurf:
    rules_mode: dropin
`)
	if err := ValidateAgentfileV1(valid); err != nil {
		t.Fatalf("expected Sprint 2 targets and rules_mode to validate: %v", err)
	}

	invalidCases := [][]byte{
		[]byte("version: 1\ntargets: [aider]\n"),
		[]byte("version: 1\ntargets: [codex]\ntargets_config:\n  aider:\n    on_unsupported: warn\n"),
		[]byte("version: 1\ntargets: [codex]\ntargets_config:\n  codex:\n    rules_mode: legacy\n"),
	}
	for _, tc := range invalidCases {
		if err := ValidateAgentfileV1(tc); err == nil {
			t.Fatalf("expected schema error for:\n%s", tc)
		}
	}
}

func TestValidateAgentfileAndLockExtendsSchema(t *testing.T) {
	t.Parallel()

	// Single string extends without targets is valid at raw schema layer.
	if err := ValidateAgentfileV1([]byte("version: 1\nextends: ./base/Agentfile.yaml\n")); err != nil {
		t.Fatalf("expected valid extends without targets: %v", err)
	}
	// List of extends with structured git source.
	if err := ValidateAgentfileV1([]byte("version: 1\nextends:\n  - ./base/Agentfile.yaml\n  - git: https://github.com/org/base.git\n    ref: v1.0.0\n    path: base/Agentfile.yaml\ntargets: [claude]\n")); err != nil {
		t.Fatalf("expected valid extends list: %v", err)
	}
	// Missing both extends and targets must fail.
	if err := ValidateAgentfileV1([]byte("version: 1\n")); err == nil {
		t.Fatalf("expected schema error when both extends and targets are omitted")
	}
	// Empty extends array must fail.
	if err := ValidateAgentfileV1([]byte("version: 1\nextends: []\n")); err == nil {
		t.Fatalf("expected schema error on empty extends array")
	}

	// Lockfile with extends map.
	validLockWithExtends := []byte("version: 1\nextends:\n  \"https://github.com/org/base.git//base/Agentfile.yaml\":\n    source: https://github.com/org/base.git\n    ref: v1.0.0\n    commit_sha: " +
		strings.Repeat("a", 40) + "\n    content_hash: sha256:" + strings.Repeat("b", 64) + "\ndependencies: {}\n")
	if err := ValidateLockfileV1(validLockWithExtends); err != nil {
		t.Fatalf("expected valid lockfile with extends: %v", err)
	}
}
