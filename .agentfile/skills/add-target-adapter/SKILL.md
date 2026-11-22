---
name: add-target-adapter
description: Step-by-step workflow for implementing, registering, schema-updating, and golden-testing a target adapter in agentfile without violating the acyclic package DAG.
---

# Skill: Add or Update a Target Adapter (`add-target-adapter`)

Use this skill when adding a new target adapter or extending an existing adapter under `internal/adapters/<target>/`.

## 7-Step Implementation Checklist

1. **Implement the Pure `adapters.TargetAdapter` Interface (`internal/adapters/<target>/<target>.go`):**
   * Keep the package 100% I/O-free (never import `os`, `os/exec`, `net`, or `filepath.Walk`).
   * Implement:
     * `Name() string` and `BinaryName() string`
     * `SchemaContract() adapters.SchemaContract` (`MinVersion`, `MinIsolationVersion`, `MaxTested`, `BinaryOptional`)
     * `RequiredProbes() []adapters.ProbeRequest` for workspace file-existence checks
     * `GlobalConfigSpecs() []adapters.GlobalConfigSpec` for home-directory global-hit detection
     * `IsSessionArgs(args []string) bool` to distinguish interactive/prompt sessions from management subcommands
     * `PlanLaunch(ctx adapters.LaunchContext) adapters.LaunchPlan` for `load_global: false` runtime isolation flags/env and fail-closed `DiagError` checks
     * `Compile(bundle *ir.Bundle, probes adapters.ProbeResults) (*adapters.CompileResult, error)`
     * `CheckPolicy(bundle *ir.Bundle, mergedFiles map[string][]byte) []adapters.Diagnostic`
2. **Wire Concrete Adapter in Registry (`internal/adapters/registry/registry.go`):**
   * Register the adapter constructor in `internal/adapters/registry/registry.go`.
   * Verify `internal/adapters` (`adapter.go`) and `internal/engine` do **not** import the concrete adapter package.
3. **Update Spec Validation & JSON Schema v1:**
   * Add the target identifier to `validTargets` in `internal/spec/validate.go` (and `rules_mode` validation if applicable).
   * Update `schemas/v1/agentfile.schema.json` (`targets` enum and `targets_config` properties).
4. **Enforce Env-Var Lowering & Fail-Closed Security:**
   * Lower `ir.EnvString` tokens (`${VAR}` and `${VAR:-default}`) to the target's native interpolation syntax without calling `os.ExpandEnv`.
   * Emit `adapters.DiagError` (exit `2`) for unsupported `deny_commands` or enforced `hooks` (`PreToolUse` or `enforce: true`) unless `targets_config.<target>.on_unsupported: warn` is configured.
5. **Create Golden Fixtures & Unit Tests (`internal/adapters/<target>/`):**
   * Add golden output fixtures under `internal/adapters/<target>/testdata/`.
   * Write comprehensive unit tests in `internal/adapters/<target>/<target>_test.go` covering `Compile`, `CheckPolicy`, `PlanLaunch` (`load_global: false` and `on_isolation_unsupported: warn`), `IsSessionArgs`, and `GlobalConfigSpecs`.
6. **Add Runner, Schema, & CLI Integration Tests:**
   * Add launch isolation test cases in `internal/runner/runner_test.go`.
   * Add schema conformance test cases in `schemas/schema_test.go`.
   * Add end-to-end sync/status/diff/clean test cases in `internal/cli/cli_test.go`.
7. **Update Documentation & Verify:**
   * Update `./AGENTS.md`, `./docs/architecture.md`, `./docs/targets.md`, and `./docs/compatibility.md`.
   * Run `make fmt`, `make check`, and `make coverage` (`>= 85.0%` required).
