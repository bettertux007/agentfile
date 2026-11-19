# CLI Reference

## Global Flags

* `-v`, `--verbose`: Enable verbose diagnostic output.

## Exit Codes

* `0`: Success
* `1`: Runtime or filesystem/Git I/O error
* `2`: Manifest/schema validation error, security policy rejection, lockfile mismatch, unresolved conflict, or CLI usage error

---

## `agentfile init`

Scaffolds `Agentfile.yaml` in the current working directory and probes `$PATH` for installed targets (`claude`, `opencode`, `gemini`, `cursor`, `codex`, `copilot`, `windsurf`; defaults to `[claude, opencode]` if none are found). With `--import`, reverse-compiles existing repository-local agent configurations into `./.agentfile/imported/` and `Agentfile.yaml`.

```sh
agentfile init [--minimal | --full] [--force]
agentfile init --import [--targets <list>] [--force] [--dry-run] [--json]
```

* `--minimal`: Minimal `version: 1` and `targets` template (default when `--import` is not set).
* `--full`: Includes starter `mcp`, `settings`, and commented `skills` / `agents` / `rules` sections.
* `--force`: Overwrite an existing `Agentfile.yaml` (otherwise exits `2`). With `--import`, also atomically replaces `./.agentfile/imported/`.
* `--import`: Scan existing repo-local configurations across all 7 targets (`.mcp.json`, `opencode.json`, `.gemini/settings.json`, `.cursor/mcp.json`, `.vscode/mcp.json`, `.codex/config.toml`, `.claude/settings.json`, skills under `.claude/skills/`, `.opencode/skills/`, `.gemini/skills/`, `.agents/skills/`, agents under `.claude/agents/`, `.opencode/agents/`, and rules under `.claude/rules/*.md`, `.cursor/rules/*.mdc`, `.windsurf/rules/*.md`, `.opencode/agentfile-rules.md`, `GEMINI.md`, `AGENTS.md`, `.github/copilot-instructions.md`), extract skills/agents/rules into `./.agentfile/imported/{skills,agents,rules}/`, normalize `{env:VAR}` and `${env:VAR}` back to `${VAR}`, and write `Agentfile.yaml`. Rejects divergent definitions, literal secrets, or empty discovery with exit `2`. Cannot be combined with `--minimal` or `--full`.
* `--targets <list>`: Comma-separated list of targets for the generated `Agentfile.yaml` (requires `--import`). When omitted, targets are inferred from discovered configurations (defaulting to `codex` if only shared `.agents/skills/` or `AGENTS.md` artifacts are found).
* `--dry-run`: Preview the import summary and generated `Agentfile.yaml` without writing `./.agentfile/imported/` or `Agentfile.yaml` (requires `--import`).
* `--json`: Emit an `ImportSummary` JSON object (`dry_run`, `targets`, `sources_scanned`, `mcp`, `skills`, `agents`, `rules`, `allow_commands`, `deny_commands`, `hooks`, `imported_files`, `manifest_yaml`; requires `--import`).

---

## `agentfile sync`

Resolves dependencies, validates `Agentfile.lock`, compiles active targets, runs post-merge policy checks, updates `.agentfile/state.json` (including `shared` and `shared_owned_dirs` ownership for co-owned artifacts such as `AGENTS.md` and `.agents/skills/<name>`), and manages worktree-safe excludes in `<git-common-dir>/info/exclude` (excluding `.agentfile/{state.json,config.yaml,sync.lock,commit-journal.json,backups/}` and untracked compiled outputs while keeping `.agentfile/imported/` trackable in Git).

```sh
agentfile sync [flags]
```

* `--targets <list>`: Comma-separated target subset (e.g., `--targets claude,opencode`). Preserves other targets' artifacts in `.agentfile/state.json`.
* `--link-mode <copy|symlink>`: Materialization mode (`copy` default; `symlink` restricted to workspace-local sources).
* `--copy`: Shorthand for `--link-mode=copy`.
* `--on-conflict <ask|fail|backup|overwrite>`: Override collision policy for unmanaged files or user-edited managed keys/blocks.
* `--force`: Alias for `--on-conflict=overwrite` (does not bypass lockfile or security policy checks).
* `--git-exclude[=true|false]`: Enable or disable `<git-common-dir>/info/exclude` management.
* `--update[=<kind:name>]`: Re-resolve all remote dependencies (`--update`) or a single dependency (`--update=skill:k8s-hardening`), update `Agentfile.lock`, and print lockfile deltas (`added`, `updated`, `removed`, `unchanged`; emitted as `deltas` in `--json` output).
* `--allow-external-local`: Permit local paths outside the repository root (`../...` or absolute paths).
* `--dry-run`: Plan all operations without writing workspace files or `Agentfile.lock`.
* `--json`: Emit the summary as JSON (for `--dry-run` or `sync`).

---

## `agentfile status`

Performs a read-only inspection of `Agentfile.yaml`, `Agentfile.lock`, resolved configuration (`.agentfile/config.yaml` and user-global config), local/remote dependency hydration, `.agentfile/commit-journal.json` state, workspace drift (including shared artifacts attributed to every owning target), unmanaged/edited conflicts, and per-target `load_global`/isolation status without invoking Git or mutating files.

```sh
agentfile status [--targets <list>] [--check] [--json]
```

* `--targets <list>`: Comma-separated subset of targets to inspect.
* `--check`: Exit `2` if `Agentfile.yaml` is invalid, `Agentfile.lock` is `missing`/`out_of_sync`/`invalid`, a pending commit journal exists, dependencies are unhydrated or missing, workspace drift is detected, or any inspected target is not `in_sync` (`drifted`, `unhydrated`, `conflict`, or `not_synced`).
* `--json`: Emit a structured `StatusReport` JSON object (`manifest_valid`, `schema_errors`, `lock_status`, `effective_config`, `unhydrated_deps`, `missing_local_deps`, `commit_journal_pending`, `drifted`, `drift_reason`, `per_target`, `warnings`).

---

## `agentfile diff`

Previews unified diffs between `Agentfile.yaml` and current workspace files without modifying disk or `Agentfile.lock`. Literal secrets in rendered diffs and warnings are masked (`[REDACTED]`) while preserving `${VAR}`, `{env:VAR}`, and `${env:VAR}` placeholders. Individual files larger than 1 MiB emit a size-difference summary, and total diff output is capped at 10 MiB.

```sh
agentfile diff [--targets <list>] [--stat | --name-only] [--check] [--fetch]
```

* `--targets <list>`: Comma-separated subset of targets to diff.
* `--stat`: Show a diffstat summary (`+`/`-` counts per file and totals) instead of full hunks (mutually exclusive with `--name-only`).
* `--name-only`: Show only changed file paths (mutually exclusive with `--stat`).
* `--check`: Exit `2` if any diff is pending, `Agentfile.lock` is `missing`/`out_of_sync`/`invalid`, conflicts exist, or workspace drift is detected.
* `--fetch`: Hydrate cold remote dependencies into the local cache (`AGENTFILE_CACHE_DIR` or `<UserCacheDir>/agentfile`) without writing `Agentfile.lock`. Without `--fetch`, unhydrated remote dependencies exit `2`.

---

## `agentfile add`

Adds or replaces a `skill`, `agent`, `rule`, or `mcp` entry in `Agentfile.yaml` using comment-preserving YAML AST edits, validates against JSON Schema v1 and `spec.Parse`, and runs a scoped `sync` (reconciling only the added/replaced dependency key in `Agentfile.lock`) unless `--no-sync` is passed.

```sh
agentfile add skill <name> <source> [flags]
agentfile add agent <name> <source> [flags]
agentfile add rule  <name> <source> [flags]
agentfile add mcp   <name> [flags] [-- <command> [args...]]
```

Common flags across all `add` subcommands:

* `--replace`: Overwrite an existing entry with the same name (otherwise exits `2`).
* `--no-sync`: Update `Agentfile.yaml` only without updating `Agentfile.lock` or syncing workspace artifacts.
* `--on-conflict <ask|fail|backup|overwrite>`: Override collision policy during the automatic sync.
* `--allow-external-local`: Permit local dependency paths outside the repository root.

Subcommand-specific flags:

* `add agent`: `--description <text>`, `--model <model>`
* `add rule`: `--description <text>`, `--globs <pattern1,pattern2>`, `--always-apply`
* `add mcp`:
  * `--transport <stdio|http|sse>`: Transport type (inferred as `stdio` from `-- <command>` or `http` from `--url` if omitted).
  * `--url <url>`: Endpoint URL (required for `http`/`sse`; cannot be combined with `-- <command>`).
  * `--env <KEY=VAL>`: Environment variable for `stdio` servers (repeatable).
  * `--env-ref <VAR>`: Environment variable reference for `stdio` servers, emitted as `VAR: "${VAR}"` (repeatable).
  * `--header <Name: Value>`: HTTP header for `http`/`sse` servers (repeatable).
  * `--header-env <VAR:HEADER>`: Header env reference for `http`/`sse` servers; emits `Authorization: Bearer ${VAR}` for `Authorization` and `${VAR}` for other headers (repeatable).
  * Literal secrets in `--url`, `-- <command> [args...]`, `--env`, or `--header` are rejected with exit `2` (use `--env-ref`, `--header-env`, or single-quoted `'${VAR}'` placeholders instead).

---

## `agentfile remove`

Removes a named `skill`, `agent`, `rule`, or `mcp` entry from `Agentfile.yaml` while preserving surrounding comments, and syncs `Agentfile.lock` and workspace artifacts (pruning orphaned files/entries) unless `--no-sync` is passed. Exits `2` if the kind is invalid or the entry does not exist.

```sh
agentfile remove <skill|agent|rule|mcp> <name> [flags]
```

* `--no-sync`: Update `Agentfile.yaml` only without updating `Agentfile.lock` or pruning workspace artifacts.
* `--on-conflict <ask|fail|backup|overwrite>`: Override collision policy during the automatic sync.
* `--allow-external-local`: Permit local dependency paths outside the repository root.

---

## `agentfile upgrade`

Re-resolves remote Git dependencies (all remote dependencies by default, or a single dependency specified as `<kind>:<name>` or `<kind> <name>`), updates `Agentfile.lock`, syncs workspace configurations, and prints per-dependency lockfile deltas (`added`, `updated`, `removed`, `unchanged`). Dependencies pinned to a Git tag (`refs/tags/...`) or an explicit 40-character commit SHA remain unchanged (`pinned: true`).

```sh
agentfile upgrade [<kind>:<name> | <kind> <name>] [flags]
```

* `--targets <list>`: Comma-separated subset of targets to sync.
* `--dry-run`: Preview lockfile deltas and workspace changes without writing files.
* `--on-conflict <ask|fail|backup|overwrite>`: Override collision policy.
* `--force`: Alias for `--on-conflict=overwrite`.
* `--allow-external-local`: Permit local dependency paths outside the repository root.
* `--json`: Emit `UpgradeReport` JSON (`deltas` and `summary`).

---

## `agentfile doctor`

Validates `Agentfile.yaml` against the embedded JSON Schema v1 (`schemas/v1/agentfile.schema.json`) and `spec.ParseFile`, checks `Agentfile.lock`, probes installed target CLIs against `SchemaContract` (`MinVersion` and `MinIsolationVersion`), displays the resolved `.agentfile/config.yaml` and user-global config (`~/.config/agentfile/config.yaml` when active) with per-field provenance and per-target `load_global`/`isolation_mode` state, and inspects user-global configs read-only (`~/.claude.json`, `~/.claude/{skills,agents,rules,CLAUDE.md}`, `~/.config/opencode/`, `~/.agents/skills`, `~/.gemini/`, `~/.cursor/`, `~/.codex/{config.toml,AGENTS.md}`, `~/.copilot/{skills,mcp-config.json}`, `~/.codeium/windsurf/{mcp_config.json,memories/global_rules.md}`) for shadowing MCP servers and non-empty global skills/rules/agents.

```sh
agentfile doctor [--json]
```

* `--json`: Output a structured JSON report (`DoctorReport`, including `effective_config.user_config_path` when active and per-field `effective_config.sources` provenance).

---

## `agentfile run`

Syncs target configuration and launches `<target>`, optionally disabling native user-global config loading (`load_global: false`). All `agentfile` flags must appear **before** `<target>`; every argument from `<target>` onward (including `-v`, `--help`, and `--`) is forwarded verbatim.

```sh
agentfile run [flags] <target> [target-args...]
```

* `--load-global`: Load native user-global agent configs (`true` / `false`). Overrides `Agentfile.yaml` `load_global: false` with a `stderr` warning.
* `--no-global`: Shorthand for `--load-global=false`.
* `--sync <auto|check|never>`: Pre-launch sync mode (`auto` default runs quiet `--targets <target>` sync; `check` fails exit `2` if workspace/lockfile drifted; `never` skips sync).
* `--no-sync`: Shorthand for `--sync=never`.
* `--dry-run`: Print resolved binary, argv, redacted `AGENTFILE_*` env vars, isolation mode, and sync action without writing files or launching `<target>`.
* `--json`: Emit `--dry-run` report as JSON (requires `--dry-run`).

---

## `agentfile clean`

Removes workspace artifacts tracked in `.agentfile/state.json`, strips managed JSONC keys/array entries, TOML keys/tables, and Markdown blocks without deleting user-authored content, removes empty `owned_dirs` and `shared_owned_dirs`, and clears this worktree's exclude block. Shared artifacts (`AGENTS.md`, `.agents/skills/<name>`) are reference-counted across owning targets: `--targets <list>` removes only the named target(s) from `owners` and preserves the shared artifact on disk while at least one other target still owns it.

```sh
agentfile clean [--dry-run] [--targets <list>] [--cache [--max-age <duration>]] [--json]
```

* `--dry-run`: Show files or cache entries that would be modified/deleted without writing to disk.
* `--targets <list>`: Clean only the specified target namespaces.
* `--cache`: Prune stale extracted store entries under `AGENTFILE_CACHE_DIR` / `<UserCacheDir>/agentfile/extracted/` instead of workspace files.
* `--max-age <duration>`: Age threshold for `--cache` (Go duration, default `720h`).
* `--json`: Emit the clean summary as JSON.
