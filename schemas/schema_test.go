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
