# Technical Specification: `agentfile` (v0.1.0)

* **Author:** bettertux007
* **Status:** Ready for Implementation (Stage 8 & Stage 9 Blockers Resolved)
* **Date:** 2026-11-16

---

## 1. Overview & Objective

`agentfile` is an open-source, cross-platform Go CLI compiler and dependency hydrator for AI coding agents (**Claude Code**, **OpenCode**, **Gemini CLI**, and **Cursor**).

Developers declare project-scoped MCP servers, skills, subagents, instructions, permissions, and lifecycle hooks in a single `Agentfile.yaml` (`version: 1`) at the repository root. `agentfile` resolves remote Git dependencies into a single local store tracked by `Agentfile.lock` (`version: 1`) and compiles isolated project-level configurations for each configured target—without modifying user-global agent files (`~/.claude.json`, `~/.config/opencode/`, `~/.gemini/settings.json`, `~/.cursor/`).

To support individual developer workflows without forcing one layout on every contributor, `agentfile` separates **what is compiled** (`Agentfile.yaml` and `Agentfile.lock`, committed in Git) from **how artifacts are materialized locally** (optional `.agentfile/config.yaml` and symmetric CLI flags).

### Core Principles & Non-Functional Requirements

1. **Idempotent & Declarative:** Running `agentfile sync` repeatedly yields identical filesystem state. Removing entries from `Agentfile.yaml` prunes only `agentfile`-owned files/directories tracked in `.agentfile/state.json`.
2. **Zero Global Agent Mutation:** Never write to `~/.claude*`, `~/.config/opencode*`, `~/.gemini*`, or `~/.cursor*`. Read-only conflict inspection happens only in `agentfile doctor`; `agentfile` writes its own cache/store solely under `<UserCacheDir>/agentfile` (overrideable via `AGENTFILE_CACHE_DIR`).
3. **Translated Env-Var Safety:** Never expand `${ENV_VAR}` secrets onto disk. Parse `${ENV_VAR}` and `${ENV_VAR:-default}` into typed IR tokens and emit each target's native interpolation syntax (`${VAR}` for Claude/Gemini, `{env:VAR}` for OpenCode, `${env:VAR}` for Cursor).
4. **Fail-Closed Security Defaults:** Never silently drop declared `deny_commands` or enforcing `hooks` (`PreToolUse` or `enforce: true`) to warnings. Fail compilation (exit code `2`) when an active target cannot enforce a security control unless explicitly overridden via `targets_config.<target>.on_unsupported: warn`.
5. **Worktree-Safe VCS Hygiene:** Manage `<git-common-dir>/info/exclude` via `git rev-parse --git-common-dir --path-format=absolute` using worktree-keyed blocks (`# BEGIN AGENTFILE:<worktree-id>`) under `<git-common-dir>/agentfile-exclude.lock`, skipping paths already tracked by Git.

---

## 2. Architecture & Package DAG

`agentfile` uses a strict acyclic package DAG under Go 1.23+. No package below `internal/cli` reads CLI flags or prompts on a TTY. `internal/adapters` defines pure interfaces/types and **never** imports concrete `internal/adapters/*` subpackages. `internal/engine` **never** imports `internal/adapters/*`.

```text
cmd/agentfile
      │
      ▼
internal/cli  (Cobra CLI, TTY prompter, registry wiring, exit-code mapping)
      │
      ├──────────────┬───────────────┬───────────────┬──────────────────────┐
      ▼              ▼               ▼               ▼                      ▼
internal/config  internal/spec   internal/store  internal/adapters/registry  internal/engine
(local config    (YAML v1 spec   (local & Git    (wires claude, opencode,    (Plan -> Commit,
 & CLI flags)     & lockfile)     cache/store)    gemini, cursor)             rollback journal,
      │              │               │               │                        state.json)
      │              └───────┬───────┘               ▼                           │
      │                      ▼                 internal/adapters/*               ├──► internal/vcs
      │                 internal/ir            (pure Compile, zero I/O)          │
      │             (normalized bundle,              │                           │
      │              EnvRef, Security)               └───────────► internal/adapters (base types)
      │                                                                          ▲
      └──────────────────────────────────────────────────────────────────────────┘
                     (engine uses internal/lock, internal/vcs, internal/adapters base types)
```

Shared advisory file locking (`flock` on Unix `//go:build unix`, `LockFileEx` on Windows `//go:build windows`) lives in `internal/lock` and is reused by `internal/store`, `internal/vcs`, and `internal/engine`.

### Sync Data Flow

1. `cli` loads `Agentfile.yaml` (`spec.Parse`), `Agentfile.lock` (`spec.LoadLock`), and `.agentfile/config.yaml` + CLI flags (`config.Load`).
2. `store.Resolve` validates `Agentfile.lock` against `Agentfile.yaml` (failing with exit code `2` if `Agentfile.lock` exists and remote dependencies diverge without `--update`), fetches/verifies remote Git dependencies, and verifies local `./...` paths exist inside the workspace.
3. `ir.Build(spec, resolvedDeps)` constructs the normalized `ir.Bundle`.
4. `cli` collects `RequiredProbes()` from active adapters, runs `engine.CollectProbes(root, probes)` to gather workspace file-existence facts (e.g., `AGENTS.md`), and calls `adapter.Compile(bundle, probeResults)` for each active target.
5. `engine.Plan` checks case-normalized inter-adapter collisions, executes 3-way JSONC/YAML-safe merges, runs each adapter's post-merge `CheckPolicy(bundle, mergedFiles)` check to catch runtime `"Always allow"` contradictions against `deny_commands`, and resolves all `on_conflict` prompts (`--dry-run` exits here with zero disk/lock writes).
6. `engine.Commit` acquires `.agentfile/sync.lock`, writes `.agentfile/commit-journal.json`, backs up collided files if `on_conflict == backup`, stages temp files in `filepath.Dir(target)` with `0644`/`0755` modes, updates `.agentfile/state.json`, prunes stale artifacts and `owned_dirs` within active `--targets`, deletes `.agentfile/commit-journal.json`, and updates `<git-common-dir>/info/exclude`.

---

## 3. Repository Structure

```text
agentfile/
├── cmd/
│   └── agentfile/
│       └── main.go                            # Calls os.Exit(cli.Execute(os.Args[1:]))
├── schemas/
│   ├── embed.go                               # //go:embed v1/agentfile.schema.json
│   └── v1/
│       └── agentfile.schema.json              # Draft 2020-12 JSON Schema for Agentfile.yaml v1
├── internal/
│   ├── lock/
│   │   ├── lock_unix.go                       # //go:build unix (golang.org/x/sys/unix flock)
│   │   ├── lock_windows.go                    # //go:build windows (golang.org/x/sys/windows LockFileEx)
│   │   └── lock_test.go
│   ├── spec/
│   │   ├── types.go                           # ProjectSpec, EnvString, Lockfile structs
│   │   ├── parser.go                          # Strict YAML v1 parser + EnvString tokenizer
│   │   ├── parser_test.go
│   │   ├── lock.go                            # YAML Agentfile.lock v1 serializer + VerifyMatches
│   │   └── lock_test.go
│   ├── config/
│   │   ├── config.go                          # LocalConfig (.agentfile/config.yaml) + CLI overrides
│   │   └── config_test.go
│   ├── ir/
│   │   ├── ir.go                              # ir.Bundle, EnvRef, SecurityControl, AdvisoryItem
│   │   ├── builder.go                         # Build(spec *spec.ProjectSpec, deps map[string]ResolvedDep)
│   │   └── builder_test.go
│   ├── store/
│   │   ├── source.go                          # Source parser (<repo>[#<ref>][//<subpath>]) & canonicalizer
│   │   ├── source_test.go
│   │   ├── hash.go                            # Canonical SHA-256 tree hasher (raw bytes, 100MB/5k cap)
│   │   ├── hash_test.go
│   │   ├── fetcher.go                         # GitRunner, bare mirror cache, extracted store, TTL prune
│   │   └── fetcher_test.go
│   ├── vcs/
│   │   ├── git.go                             # Worktree-keyed info/exclude & batched ls-files -z
│   │   └── git_test.go
│   ├── engine/
│   │   ├── state.go                           # Target-namespaced state.json v1 (with owned_dirs)
│   │   ├── jsonc.go                           # hujson 3-way map & string-array patcher
│   │   ├── merge_test.go
│   │   ├── planner.go                         # Plan, collision checks, policy check, Prompter
│   │   ├── executor.go                        # Commit, commit-journal rollback, atomic staging, pruning
│   │   └── engine_test.go
│   ├── adapters/
│   │   ├── adapter.go                         # Pure interfaces & ArtifactOp / Diagnostic types
│   │   ├── registry/
│   │   │   └── registry.go                    # Target registry (M1: claude, opencode; M2: +gemini, cursor)
│   │   ├── claude/
│   │   │   ├── claude.go                      # M1: .mcp.json, .claude/settings.json, skills, agents, rules
│   │   │   ├── claude_test.go
│   │   │   └── testdata/                      # Golden fixtures
│   │   ├── opencode/
│   │   │   ├── opencode.go                    # M1: opencode.json (V1), .opencode/{skills,agents,rules}
│   │   │   ├── opencode_test.go
│   │   │   └── testdata/                      # Golden fixtures
│   │   ├── gemini/                            # M2
│   │   │   ├── gemini.go
│   │   │   ├── gemini_test.go
│   │   │   └── testdata/
│   │   └── cursor/                            # M2
│   │       ├── cursor.go
│   │       ├── cursor_test.go
│   │       └── testdata/
│   └── cli/
│       ├── root.go                            # Cobra root, Execute(args []string) int, ExitError
│       ├── init.go                            # M1: `agentfile init`
│       ├── sync.go                            # M1: `agentfile sync`
│       ├── clean.go                           # M1: `agentfile clean`
│       ├── doctor.go                          # M2: `agentfile doctor`
│       └── cli_test.go                        # M1 unit + M2 integration tests
├── examples/
│   └── starter/
│       └── Agentfile.yaml
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## 4. Core Go Contracts & File Schemas

### 4.1 `Agentfile.yaml` (`version: 1`, integer)

```yaml
version: 1
targets:
  - claude
  - opencode

mcp:
  github:
    transport: http # stdio | http | sse
    url: "https://api.githubcopilot.com/mcp/"
    headers:
      Authorization: "Bearer ${GITHUB_PAT}"
  filesystem:
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "./docs"]
    env:
      LOG_LEVEL: "info"

skills:
  - name: terraform-auditor
    source: ./local/skills/terraform-auditor
  - name: k8s-hardening
    source: https://github.com/org/agent-skills.git#v1.0.0//skills/k8s-hardening

agents:
  - name: security-reviewer
    description: "Audits IaC and application code"
    model: sonnet
    source: ./agents/security-reviewer.md

rules:
  - name: coding-standards
    source: ./docs/AGENT_RULES.md
    description: "Go and Terraform conventions"
    globs: ["**/*.go", "**/*.tf"]
    always_apply: false

settings:
  permissions:
    allow_commands: ["go test ./...", "terraform plan"]
    deny_commands: ["rm -rf *"]
  hooks:
    SessionStart:
      - command: "echo 'Project environment ready'"
    PreToolUse:
      - matcher: "Bash"
        command: "./scripts/validate-cmd.sh"
        enforce: true

targets_config:
  opencode:
    on_unsupported: warn # fail | warn (default: fail for SecurityControl, warn for AdvisoryItem)
    overrides: {}        # deep-merged into compiled opencode.json before output
```

#### Go Types (`internal/spec/types.go`):
* `ProjectSpec`: `Version int`, `Targets []string`, `MCP map[string]MCPServerSpec`, `Skills []AssetSpec`, `Agents []AgentSpec`, `Rules []RuleSpec`, `Settings SettingsSpec`, `TargetsConfig map[string]TargetConfigSpec`.
* `SourceSpec`: custom YAML unmarshaler accepting either a string (`./local/...` or `<repo_url>[#<ref>][//<subpath>]`) or struct `{Git string, Ref string, Path string}`.
* `EnvString`: `{Raw string, Segments []EnvSegment}` where `EnvSegment{Literal string, Var *EnvRef}` and `EnvRef{Name string, Default string, HasDefault bool}`. `$$` escapes a literal `$`; unclosed `${` or empty var name `${}` returns a `PolicyError` (exit code `2`).
* **Parser rules (`internal/spec/parser.go`):** Uses `yaml.NewDecoder` with `KnownFields(true)`, handles `io.EOF` on empty files as a validation error, executes a second `Decode` to reject trailing multi-document `---` YAML streams, never calls `os.ExpandEnv`, validates all artifact `name` slugs against `^[a-zA-Z0-9][a-zA-Z0-9._-]*$`, and verifies `targets` is non-empty and contains only known target names.

### 4.2 `Agentfile.lock` (`version: 1`, canonical YAML)

* **Format:** Sorted-key YAML (`version: 1`), `dependencies: map[string]LockEntry`.
* **Keying:** `<kind>:<name>` (e.g., `skill:k8s-hardening`, `agent:sec-rev`, `rule:standards`).
* **Local vs. Remote Segmentation:** Only remote Git dependencies are recorded in `Agentfile.lock` (`LockEntry{Source, Ref, CommitSHA, ContentHash}`). In-repo local paths (`./...`) are **not** pinned in `Agentfile.lock` so local workspace edits (and opt-in `symlink` mode) never cause lockfile churn. Local paths escaping the repository root (`../...` or `/abs/...`) are rejected unless `--allow-external-local` is set (in which case they remain unpinned local paths).
* **`VerifyMatches(spec)`:**
  * If `Agentfile.lock` does not exist yet on first `sync`, `sync` resolves remote dependencies and writes `Agentfile.lock`.
  * If `Agentfile.lock` exists, `VerifyMatches` verifies the exact set of `<kind>:<name>` remote keys and each entry's canonicalized `Source` + `Ref` match `spec`. Any mismatch (added, removed, or edited ref/source) returns exit code `2` unless `--update` or `--update=<kind>:<name>` is passed.

### 4.3 `LocalConfig` (`.agentfile/config.yaml`, `version: 1`, YAML)

* `LocalConfig`: `Version int`, `LinkMode string` (`"copy"` default | `"symlink"`), `OnConflict string` (`""` -> resolved to `"ask"` if `IsTTY && !isCI()`, else `"fail"`, or explicit `"ask" | "fail" | "backup" | "overwrite"`), `GitExclude *bool` (default `true` when `nil`).
* Strict `KnownFields(true)` decoding; CLI flags override file config. Passing both `--copy` and `--link-mode=symlink` fails with exit code `2`. `IsCI()` checks `CI`, `GITHUB_ACTIONS`, `BUILDKITE`, `GITLAB_CI`, `CIRCLECI` non-empty. `IsTTY` is injectable for tests.

### 4.4 `internal/ir` (`ir.go`, `builder.go`)

* `ResolvedDep`: `{Kind string, Name string, IsRemote bool, SourcePath string, ContentHash string}`.
* `Bundle`: `Targets []string`, `MCP map[string]IRMCPServer`, `Skills []IRAsset`, `Agents []IRAgent`, `Rules []IRRule`, `AllowCommands []string`, `DenyCommands []SecurityControl`, `Hooks []IRHook`, `TargetsConfig map[string]spec.TargetConfigSpec`.
* `SecurityControl`: `{Kind string, Pattern string, Source string}`.
* `IRHook`: `{Event string, Matcher string, Command string, Enforced bool}` (`Enforced == true` when `Event == "PreToolUse"` or `hook.Enforce == true`).

### 4.5 `internal/adapters` (`adapter.go`, `internal/adapters/registry/registry.go`)

* `ArtifactOpKind`: `OpWriteOwned`, `OpMergeJSONC`, `OpWriteDropIn`, `OpMaterializeTree`. (All adapters use drop-in files for rules—no dual `CLAUDE.md` managed-block mode—except Gemini's `GEMINI.md` in Milestone 2 if drop-in is unavailable, using fenced-aware `OpManagedBlock`.)
* `Adapter` interface:
  * `Name() string`
  * `BinaryName() string`
  * `SchemaContract() SchemaContract` (`{MinVersion string, MaxTested string, BinaryOptional bool}`)
  * `RequiredProbes() []ProbeRequest` (`{Key string, RelPath string}`)
  * `Compile(bundle *ir.Bundle, facts ProbeResults) (*CompileResult, error)`
  * `CheckPolicy(bundle *ir.Bundle, mergedFiles map[string][]byte) []Diagnostic` (validates post-merge JSONC files so preserved runtime entries cannot contradict `deny_commands`)
* `Diagnostic`: `{Level DiagLevel (Error|Warning|Info), Target string, Code string, Message string}`. When `on_unsupported == "fail"` (default for `SecurityControl`), unsupported security items emit `DiagError` and fail compilation (exit `2`). When `on_unsupported == "warn"` (or for `AdvisoryItem`), they emit `DiagWarning` and omit the unsupported field.

---

## 5. Dependency Store & Canonical Hashing (`internal/store`)

* **Single Store Root:** Resolved via `AGENTFILE_CACHE_DIR` env var if set, else `<os.UserCacheDir()>/agentfile/` (no `os.UserDataDir()`).
  * `<root>/git/<url_hash_16>/`: bare Git mirror repo, locked via `internal/lock` (`<root>/locks/git-<url_hash_16>.lock`).
  * `<root>/extracted/<content_hash>/`: extracted subpath tree with standard writable permissions (`0755` dirs, `0644`/`0755` files—never `0444`/`0555`), accompanied by `<root>/extracted/<content_hash>.last_used` timestamp file updated on each read/write.
  * `<root>/tmp/`: staging dir on the same filesystem so `os.Rename` into `extracted/<content_hash>/` is atomic without `EXDEV`.
* **Source Parser (`source.go`):**
  * Rejects `:` subpath separators (validates `git@host:org/repo` SCP syntax and `scheme://host:port/...` vs. extra trailing `:subpath`), splits `#<ref>` and `//<subpath>` unambiguously after `scheme://`, strips userinfo credentials, allowlists `https://`, `ssh://`, `git@` (`file://` gated behind `AllowFileScheme` test option; `ext::` always rejected).
* **Git Runner (`fetcher.go`):**
  * Invokes `git` with `-c core.hooksPath=/dev/null -c submodule.recurse=false -c protocol.ext.allow=never -c core.autocrlf=false`, `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=""`, process-group termination (`Setpgid: true` on Unix, `WaitDelay: 5 * time.Second`), and `60s` default timeout (`AGENTFILE_GIT_TIMEOUT`).
  * Ref resolution order: `refs/tags/<ref>^{}` -> `refs/tags/<ref>` -> `refs/heads/<ref>` -> `HEAD` (`--symref`). For raw 40-char hex SHAs, runs `git fetch --depth 1 origin <sha>` falling back to unshallow `git fetch origin`.
  * Checks `git ls-tree -r -z <sha> -- <subpath>` and fails if any `160000` submodule gitlink exists or if `<subpath>` matches zero entries; extracts via `git archive <sha> -- <subpath>` into `<root>/tmp/<rand>`.
* **Canonical Tree Hasher (`hash.go`):**
  * Walks sorted POSIX relative paths (skipping `.git/`), rejects **all** symlinks (`ErrSymlinkNotAllowed`), enforces cumulative `100 MB` (`io.LimitReader`) and `5,000` entry limits, and feeds `"F\x00" + relPath + "\x00" + mode("0755"|"0644") + "\x00" + decimalSize + "\x00" + rawBytes` (or `"D\x00" + relPath + "\x00"` for empty dirs) into SHA-256, returning `"sha256:" + hex`.

---

## 6. Target Adapter Matrix (`internal/adapters`)

| Target | Milestone | Outputs & `ArtifactOp` | Env Interpolation | Security & Primitive Mapping |
| :--- | :--- | :--- | :--- | :--- |
| **`claude`** | M1 | `.mcp.json` (`OpMergeJSONC`), `.claude/settings.json` (`OpMergeJSONC`), `.claude/skills/<name>` (`OpMaterializeTree`), `.claude/agents/<name>.md` (`OpWriteOwned`), `.claude/rules/agentfile-<name>.md` (`OpWriteDropIn`) | `${VAR}` / `${VAR:-default}` preserved | `allow_commands` -> `permissions.allow` (`Bash(<cmd>:*)`); `deny_commands` -> `permissions.deny` (`Bash(<cmd>:*)`); `hooks` -> native `hooks`. Probes `AGENTS.md` and warns if present. |
| **`opencode`** | M1 | `opencode.json` (`OpMergeJSONC`), `.opencode/skills/<name>` (`OpMaterializeTree`), `.opencode/agents/<name>.md` (`OpWriteOwned`), `.opencode/agentfile-rules.md` (`OpWriteDropIn`) | `{env:VAR}` (`:-default` fails closed, or strips default with warning if `on_unsupported: warn`) | `mcp` `local`/`remote`; `instructions: [".opencode/agentfile-rules.md"]`; V1 `permission.bash` map (`"<cmd> *": "allow" | "deny"`). `hooks` fail closed if `Enforced` (else warn + omit). |
| **`gemini`** | M2 | `.gemini/settings.json` (`OpMergeJSONC`), `.gemini/skills/<name>` (`OpMaterializeTree`), `GEMINI.md` (`OpManagedBlock`) | `${VAR}` (`:-default` fails closed unless `on_unsupported: warn`) | `mcpServers` in `.gemini/settings.json`; `deny_commands` / enforced `hooks` fail closed unless `on_unsupported: warn`; `agents` warn + omit. |
| **`cursor`** | M2 | `.cursor/mcp.json` (`OpMergeJSONC`), `.cursor/rules/agentfile-<name>.mdc` (`OpWriteDropIn`), `.cursor/rules/agentfile-skill-<name>.mdc` (`OpWriteDropIn`) | `${env:VAR}` (`:-default` fails closed unless `on_unsupported: warn`) | `.mdc` frontmatter (`description`, `globs`, `alwaysApply`); `deny_commands` / enforced `hooks` fail closed unless `on_unsupported: warn`; `agents` warn + omit. |

---

## 7. Engine, JSONC Merge, Rollback Journal & VCS (`internal/engine`, `internal/vcs`)

* **Target-Namespaced State (`.agentfile/state.json`, `version: 1`):**
  * Partitioned by `targets.<name>`: `{artifacts: map[RelPath]ArtifactRecord, owned_dirs: []string}`.
  * `ArtifactRecord`: `{artifact_type ("file"|"dir"|"symlink"|"jsonc_patch"|"markdown_block"), sha256, json_pointers []string, array_entries map[string][]string, block_hash string, created_by_agentfile bool}`.
* **JSONC 3-Way Patcher (`internal/engine/jsonc.go`):**
  * Uses `github.com/tailscale/hujson` to preserve comments/trailing commas; marshals new patch values using `json.NewEncoder` with `SetEscapeHTML(false)` before `hujson.Parse`.
  * Creates missing intermediate parent objects before applying `hujson.Patch`.
  * Restricts ownership to **named map keys** (e.g., `/mcpServers/github`, `/mcp/github`, `/permission/bash/rm -rf *`, `/hooks/PreToolUse`) and **exact string entries** inside managed arrays (`permissions.allow`, `permissions.deny`, `instructions`), leaving all unowned user/runtime keys and array items untouched.
  * If a user modified an `agentfile`-owned map key on disk (hash differs from `state.json`), escalates under `on_conflict` (`ask` / `fail` / `backup` / `overwrite`).
* **Deterministic Rollback Journal (`.agentfile/commit-journal.json`):**
  * Acquired under `.agentfile/sync.lock`. Before mutating workspace files, writes backups of any overwritten/deleted files to `.agentfile/backups/<YYYYMMDDTHHMMSSZ>/` and records `{backups, created_paths}` in `.agentfile/commit-journal.json`.
  * If `Commit` fails mid-flight (or if a stale journal exists on the next `sync`/`clean`), `Rollback()` restores backed-up files and deletes newly created partial files automatically before removing the journal.
* **Materialization & Pruning:**
  * Uses Go stdlib buffered copy (`io.Copy`) with explicit `0644`/`0755` permissions (no `clonefile`/`FICLONE` syscalls).
  * Handles `dir <-> symlink` replacement by removing the prior owned entry only after confirming ownership in `state.json`.
  * On Windows, wraps `os.Rename` in a 5-attempt exponential backoff (10ms..160ms) on `ERROR_ACCESS_DENIED` / `ERROR_SHARING_VIOLATION` (injectable for tests).
  * Prunes only artifacts and empty `owned_dirs` belonging to the currently compiled `--targets`.
* **VCS Exclude (`internal/vcs/git.go`):**
  * Resolves `<common-dir>` via `git rev-parse --git-common-dir --path-format=absolute` and `<worktree-id>` as first 12 hex chars of SHA-256 of `filepath.EvalSymlinks(toplevel)`.
  * Acquires `<common-dir>/agentfile-exclude.lock` (if `<common-dir>` is read-only, logs a warning and skips exclude writes).
  * Batch checks tracked paths via `git ls-files -z -- <paths...>`, skips tracked paths with warnings, prunes stale `# BEGIN AGENTFILE:<id>` blocks via `git worktree list --porcelain -z` (removing `prunable` worktrees), and excludes `.agentfile/` local artifacts alongside generated paths when `git_exclude: true`.

---

## 8. CLI Commands (`internal/cli`)

* `cli.Execute(args []string) int`: `SilenceErrors: true`, maps `PolicyError` / Cobra usage errors / `on_conflict: fail` / lock mismatches to exit code `2`, I/O or runtime errors to exit code `1`, success to `0`.
* `agentfile init [--minimal | --full] [--force]` (M1): Probes `$PATH` for installed targets (`claude`, `opencode`, `gemini`, `cursor`) and writes starter `Agentfile.yaml` (`--minimal` or `--full`). Fails with exit `2` if `Agentfile.yaml` exists unless `--force` is passed.
* `agentfile sync [--targets ...] [--link-mode copy|symlink] [--copy] [--on-conflict ask|fail|backup|overwrite] [--git-exclude=true|false] [--update[=<kind:name>]] [--dry-run] [--force] [--json]` (M1): Runs the full `Plan -> Commit` pipeline (`--update` uses `NoOptDefVal = "*"` so `--update` updates all and `--update=skill:foo` updates one). `--force` maps to `--on-conflict=overwrite`. `--dry-run` prints redacted summary/diff (`--json` supported) with zero disk or lock mutations.
* `agentfile clean [--dry-run] [--targets ...] [--cache [--max-age 720h]] [--json]` (M1): Removes workspace artifacts tracked in `state.json`, managed JSONC keys/blocks, `owned_dirs`, `.agentfile/state.json`, and this worktree's exclude block (never touching `Agentfile.yaml`, `Agentfile.lock`, `.agentfile/config.yaml`, or user keys/content). With `--cache`, removes `<cacheDir>/extracted/<hash>` entries whose `.last_used` exceeds `--max-age` (`time.ParseDuration`).
* `agentfile doctor [--json]` (M2): Validates `Agentfile.yaml` against embedded `schemas/v1/agentfile.schema.json` (`github.com/santhosh-tekuri/jsonschema/v6`), verifies `Agentfile.lock`, checks installed CLIs against `SchemaContract()` (warning when optional/GUI binaries like `cursor` are absent from `$PATH`), displays secret-redacted effective `.agentfile/config.yaml`, and warns on shadowing global MCP/rule configs (`~/.claude.json`, `~/.config/opencode/`, `~/.gemini/settings.json`).

---

## 9. Implementation Milestones & Verification Gates

### Milestone 1 (Core Compiler + Claude & OpenCode Adapters)
* **Deliverables:** `go.mod`, `cmd/agentfile/main.go`, `internal/lock`, `internal/spec`, `internal/config`, `internal/ir`, `internal/store`, `internal/vcs`, `internal/engine`, `internal/adapters` (`adapter.go` + `registry/registry.go` wired for `claude` and `opencode`), `internal/adapters/claude`, `internal/adapters/opencode` (with `testdata/` golden fixtures), and `internal/cli` (`root.go`, `init.go`, `sync.go`, `clean.go`, M1 `cli_test.go`).
* **Milestone 1 Verification Gate:**
  * `go build ./...` and `GOOS=windows go build ./...` and `GOOS=linux go build ./...` exit `0`.
  * `go vet ./...` exits `0` and `gofmt -l .` produces no output.
  * `go test -race ./cmd/... ./internal/...` exits `0` (covering unit, bare-repo Git, worktree, JSONC, rollback-journal, golden-adapter, and CLI tests).

### Milestone 2 (Gemini & Cursor Adapters, `doctor`, Schema & Polish)
* **Deliverables:** `schemas/embed.go`, `schemas/v1/agentfile.schema.json`, `internal/adapters/gemini`, `internal/adapters/cursor` (with `testdata/`), `internal/adapters/registry` updated with all 4 targets, `internal/cli/doctor.go`, integration tests in `internal/cli/cli_test.go`, `examples/starter/Agentfile.yaml` (validated against schema + `spec.Parse`), `Makefile`, and `README.md`.
* **Milestone 2 Verification Gate:**
  * `golangci-lint run` exits `0` (or when `golangci-lint` is unavailable, `go vet ./...` exits `0` and `gofmt -l .` produces no output).
  * `go test -race -coverprofile=coverage.out ./...` exits `0` with **85%+ aggregate statement coverage** across `internal/` packages (including `store`, `vcs`, `spec`, `ir`, `engine`, `adapters/*`).
