# CLI Reference

## Exit Codes

* `0`: Success
* `1`: Runtime or filesystem/Git I/O error
* `2`: Manifest/schema validation error, security policy rejection, lockfile mismatch, unresolved conflict, or CLI flag error

---

## `agentfile init`

Scaffolds `Agentfile.yaml` in the current working directory and probes `$PATH` for installed targets (`claude`, `opencode`, `gemini`, `cursor`; defaults to `[claude, opencode]` if none are found).

```sh
agentfile init [--minimal | --full] [--force]
```

* `--minimal`: Minimal `version: 1` and `targets` template (default).
* `--full`: Includes starter `mcp`, `settings`, and commented `skills` / `agents` / `rules` sections.
* `--force`: Overwrite an existing `Agentfile.yaml` (otherwise exits `2`).

---

## `agentfile sync`

Resolves dependencies, validates `Agentfile.lock`, compiles active targets, runs post-merge policy checks, updates `.agentfile/state.json`, and manages worktree-safe excludes in `<git-common-dir>/info/exclude`.

```sh
agentfile sync [flags]
```

* `--targets <list>`: Comma-separated target subset (e.g., `--targets claude,opencode`). Preserves other targets' artifacts in `.agentfile/state.json`.
* `--link-mode <copy|symlink>`: Materialization mode (`copy` default; `symlink` restricted to workspace-local sources).
* `--copy`: Shorthand for `--link-mode=copy`.
* `--on-conflict <ask|fail|backup|overwrite>`: Override collision policy for unmanaged files or user-edited managed keys/blocks.
* `--force`: Alias for `--on-conflict=overwrite` (does not bypass lockfile or security policy checks).
* `--git-exclude[=true|false]`: Enable or disable `<git-common-dir>/info/exclude` management.
* `--update[=<kind:name>]`: Re-resolve all remote dependencies (`--update`) or a single dependency (`--update=skill:k8s-hardening`) and update `Agentfile.lock`.
* `--allow-external-local`: Permit local paths outside the repository root (`../...` or absolute paths).
* `--dry-run`: Plan all operations without writing workspace files or `Agentfile.lock`.
* `--json`: Emit the summary as JSON (for `--dry-run` or `sync`).

---

## `agentfile doctor`

Validates `Agentfile.yaml` against the embedded JSON Schema v1 (`schemas/v1/agentfile.schema.json`) and `spec.ParseFile`, checks `Agentfile.lock`, probes installed target CLIs against `SchemaContract` (`MinVersion` and `MinIsolationVersion`), displays the resolved `.agentfile/config.yaml` and per-target `load_global`/`isolation_mode` state, and inspects user-global configs read-only (`~/.claude.json`, `~/.claude/{skills,agents,rules,CLAUDE.md}`, `~/.config/opencode/`, `~/.agents/skills`, `~/.gemini/`, `~/.cursor/`) for shadowing MCP servers and non-empty global skills/rules/agents.

```sh
agentfile doctor [--json]
```

* `--json`: Output a structured JSON report (`DoctorReport`).

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

Removes workspace artifacts tracked in `.agentfile/state.json`, strips managed JSONC keys/array entries and Markdown blocks without deleting user-authored content, removes empty `owned_dirs`, and clears this worktree's exclude block.

```sh
agentfile clean [--dry-run] [--targets <list>] [--cache [--max-age <duration>]] [--json]
```

* `--dry-run`: Show files or cache entries that would be modified/deleted without writing to disk.
* `--targets <list>`: Clean only the specified target namespaces.
* `--cache`: Prune stale extracted store entries under `AGENTFILE_CACHE_DIR` / `<UserCacheDir>/agentfile/extracted/` instead of workspace files.
* `--max-age <duration>`: Age threshold for `--cache` (Go duration, default `720h`).
* `--json`: Emit the clean summary as JSON.
