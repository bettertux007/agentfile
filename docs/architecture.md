# Architecture & Adapter Guide

`agentfile` is structured as a strict acyclic Go package DAG where target adapters perform zero filesystem, Git, network, or process I/O.

## 1. Acyclic Package DAG

```text
cmd/agentfile
      │
      ▼
internal/cli (init, sync, status, diff, add, remove, upgrade, audit, shell-hook, doctor, run, clean)
      │
      ├──────────────┬───────────────┬────────────────┬──────────────┬─────────────────┐
      ▼              ▼               ▼                ▼              ▼                 ▼
internal/store  internal/spec  internal/engine  internal/runner  internal/diff   internal/config
(extends DAG,   (ParseRaw,     (BuildPlan,      (GatherFacts,    (Myers diff,    (4-layer config
 Git mirror,     ComposeLayers, 3-way JSONC/     bin-versions,    RedactSecrets)  & provenance)
 HashTree)       edit AST)      TOML/MD, Commit) Execute)
```

### Core Architectural Invariants

1. **Strict Acyclic Package Boundaries:**
   * `internal/adapters` defines pure interfaces and value types only and never imports concrete adapters.
   * Concrete adapters (`internal/adapters/{claude,opencode,gemini,cursor,codex,copilot,windsurf}`) are wired exclusively in `internal/adapters/registry/registry.go`.
   * `internal/engine` never imports `internal/adapters/*` or `internal/adapters/registry`.
   * `internal/spec` has zero internal dependencies and never imports `internal/store`.
2. **I/O-Free Target Adapters:** Target adapters declare workspace existence checks via `RequiredProbes()`, consume `ProbeResults` in `Compile(bundle, probes)`, validate post-merge files in `CheckPolicy(bundle, mergedFiles)`, and return pure `LaunchPlan` values in `PlanLaunch(bundle, opts, facts)`.
3. **Zero Global Agent Mutation:** `agentfile` never writes to `~/.claude*`, `~/.config/opencode*`, `~/.gemini*`, `~/.cursor*`, `~/.codex*`, `~/.copilot*`, or `~/.codeium/windsurf*`.
4. **Crash-Safe Transactional Commits:** `engine.Commit` acquires `.agentfile/sync.lock`, backs up modified paths under `.agentfile/backups/`, records `.agentfile/commit-journal.json` for deterministic rollback on failure, updates `.agentfile/state.json`, and syncs `<git-common-dir>/info/exclude`.

## 2. Sync & Fast-Path Data Flow

1. **Composition (`store.ComposeManifest` & `spec.ComposeLayers`):** Resolves any local or remote `extends:` base manifests (up to depth `8`), pins remote bases in `Agentfile.lock` under `extends:`, rebases relative `./...` `SourceSpec`s to their declaring layer, and merges layers with monotonic security non-downgrade rules.
2. **Hydration (`store.ResolveWithOptions`):** Verifies `Agentfile.lock`, fetches missing commits into bare Git mirrors (`<CacheRoot>/git/<url_hash>`), rejects symlinks and `160000` submodule gitlinks, extracts archives into `<CacheRoot>/extracted/<content_hash>`, and verifies canonical SHA-256 tree hashes (`store.HashTree`).
3. **IR & Compilation (`ir.Build` & `Adapter.Compile`):** Tokenizes `${VAR}` / `${VAR:-default}` into `EnvString` segments and lowers the bundle into target-specific `ArtifactOp`s (`OpWriteOwned`, `OpWriteDropIn`, `OpMergeJSONC`, `OpMergeTOML`, `OpManagedBlock`, `OpMaterializeTree`).
4. **Planning & 3-Way Merging (`engine.BuildPlan`):** Performs comment-preserving 3-way `hujson` JSONC patching, `go-toml/v2` TOML patching, and fenced-code-aware Markdown block merging, then validates post-merge files with `Adapter.CheckPolicy`.
5. **Fast-Path Run Cache (`engine.SaveRunFastPath` & `CheckRunFastPath`):** Saves per-target control digests, probe states, and input/output stat+hash snapshots in `.agentfile/run-cache/<target>.json` so `agentfile run <target>` can verify workspace freshness in `<5ms` (using Git-index-style SHA-256 verification for same-second racy files).

## 3. Implementing a New Target Adapter

To add a new target adapter under `internal/adapters/<target>/`:

1. Implement the `adapters.Adapter` interface (`Name`, `BinaryName`, `SchemaContract`, `RequiredProbes`, `Compile`, `CheckPolicy`, `GlobalConfigSpecs`, `IsSessionArgs`, `PlanLaunch`) with zero filesystem or process I/O.
2. Enforce fail-closed security: emit `adapters.DiagError` for unsupported `deny_commands`, enforced `hooks` (`PreToolUse` or `enforce: true`), or unsupported `${VAR:-default}` modifiers unless `bundle.TargetsConfig[a.Name()].OnUnsupported == "warn"`.
3. Register the adapter in `internal/adapters/registry/registry.go`, add the target slug to `knownTargets` in `internal/spec/parser.go` and `schemas/v1/agentfile.schema.json`, and add golden fixture tests in `internal/adapters/<target>/testdata/`.
