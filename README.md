# agentfile

Declarative project compiler and dependency hydrator for AI coding agents (**Claude Code**, **OpenCode**, **Gemini CLI**, and **Cursor**).

Define project-scoped MCP servers, skills, subagents, instructions, permissions, and lifecycle hooks once in `Agentfile.yaml` (`version: 1`). `agentfile` resolves remote Git dependencies into a local cache tracked by `Agentfile.lock` (`version: 1`) and compiles isolated, project-level configurations for each target agent without mutating global config files (`~/.claude.json`, `~/.config/opencode/`, `~/.gemini/settings.json`, `~/.cursor/`).

## Quickstart

```sh
# Build and install locally
make install

# 1. Scaffold Agentfile.yaml (detects targets on $PATH)
agentfile init

# 2. Preview planned changes without touching disk
agentfile sync --dry-run

# 3. Resolve dependencies, update Agentfile.lock, and compile target configs
agentfile sync

# 4. Inspect manifest, lockfile, hydration, drift, and isolation status (read-only)
agentfile status

# 5. Preview unified diffs between Agentfile.yaml and workspace files
agentfile diff

# 6. Validate manifest, lockfile, installed CLIs, and global config shadowing
agentfile doctor

# 7. Remove agentfile-managed workspace artifacts cleanly
agentfile clean
```

See [examples/starter/README.md](examples/starter/README.md) and [examples/starter/Agentfile.yaml](examples/starter/Agentfile.yaml) for a full starter manifest.

## CLI Reference

| Command | Flags | Description |
| :--- | :--- | :--- |
| `agentfile init` | `--minimal`, `--full`, `--force` | Scaffolds `Agentfile.yaml` with targets detected on `$PATH`. |
| `agentfile sync` | `--targets`, `--link-mode`, `--copy`, `--on-conflict`, `--git-exclude`, `--update[=<kind:name>]`, `--dry-run`, `--force`, `--allow-external-local`, `--json` | Resolves dependencies, verifies `Agentfile.lock`, compiles target configs, prints lock deltas on `--update`, and updates worktree Git excludes. |
| `agentfile status` | `--targets`, `--check`, `--json` | Read-only check of manifest validity, lockfile alignment, config sources, dependency hydration, workspace drift, conflicts, and per-target isolation. |
| `agentfile diff` | `--targets`, `--stat`, `--name-only`, `--check`, `--fetch` | Previews secret-redacted unified diffs (or diffstat/paths) between `Agentfile.yaml` and workspace files without modifying disk or `Agentfile.lock`. |
| `agentfile add` | `skill\|agent\|rule\|mcp`, `--replace`, `--no-sync`, `--on-conflict`, `--allow-external-local` | Adds or replaces an entry in `Agentfile.yaml` (preserving comments), rejects literal secrets, and reconciles `Agentfile.lock` + workspace via sync. |
| `agentfile remove` | `<skill\|agent\|rule\|mcp> <name>`, `--no-sync`, `--on-conflict`, `--allow-external-local` | Removes an entry from `Agentfile.yaml` and prunes orphaned lockfile entries and workspace artifacts. |
| `agentfile upgrade` | `[<kind>:<name> \| <kind> <name>]`, `--targets`, `--dry-run`, `--on-conflict`, `--force`, `--allow-external-local`, `--json` | Refreshes remote Git dependencies, updates `Agentfile.lock`, syncs targets, and reports per-dependency lock deltas. |
| `agentfile doctor` | `--json` | Validates `Agentfile.yaml` against JSON Schema v1, checks `Agentfile.lock`, probes installed target CLIs (`MinVersion` / `MinIsolationVersion`), prints resolved config layers, and warns on shadowing global MCPs, skills, rules, and agents. |
| `agentfile run` | `--load-global`, `--no-global`, `--sync`, `--no-sync`, `--dry-run`, `--json` | Syncs target configuration and launches `<target>` (forwarding all arguments after `<target>` verbatim) with optional runtime global isolation. |
| `agentfile clean` | `--dry-run`, `--targets`, `--cache`, `--max-age`, `--json` | Removes tracked workspace artifacts/JSONC patches/excludes, or prunes stale extracted cache entries when `--cache` is passed. |

Exit codes: `0` success, `1` I/O or runtime error, `2` validation, security policy, lockfile mismatch, conflict failure, or CLI usage error.

## Local & User-Global Tool Configuration (`config.yaml`)

Personal workflow preferences stay separate from committed `Agentfile.yaml` and `Agentfile.lock` in `~/.config/agentfile/config.yaml` (user-global defaults) and `.agentfile/config.yaml` (repo-local overrides, automatically excluded via `<git-common-dir>/info/exclude` when `git_exclude: true`):

```yaml
version: 1
link_mode: copy       # copy (default) | symlink (workspace-local sources only)
on_conflict: ask      # ask (TTY default) | fail (non-TTY/CI default) | backup | overwrite
git_exclude: true     # true (default) | false
load_global: true     # true (default) | false (machine-local runtime isolation default)
```

* **4-layer precedence:** `default` -> `user_config` (`~/.config/agentfile/config.yaml` or `$XDG_CONFIG_HOME/agentfile/config.yaml`) -> `local_config` (`.agentfile/config.yaml`) -> CLI flags (`--link-mode`, `--copy`, `--on-conflict`, `--git-exclude`, `--force`).
* **User-global env controls & CI isolation:** `AGENTFILE_NO_USER_CONFIG=1` disables user-global config; `AGENTFILE_CONFIG_FILE=<path>` forces an explicit config file (fail-closed if missing); CI environments (`CI`, `GITHUB_ACTIONS`, `BUILDKITE`, `GITLAB_CI`, `CIRCLECI`) automatically skip default user-global discovery.
* **`load_global` non-downgrade:** `CLI` > `Agentfile.yaml (load_global: false)` > `local_config` > `Agentfile.yaml (load_global: true)` > `user_config` > `default (true)`. See [docs/configuration.md](docs/configuration.md) for full details.

## Architecture

```text
Agentfile.yaml ──► spec.Parse ──► ir.Build (EnvRef + Security Tiers) ──► Pure Adapters
                                                                               │
Agentfile.lock ──► store.Resolve (locks / --update) ───────────────────────────┤
                                                                               ▼
.agentfile/config.yaml + CLI flags + probes ──► engine.Plan (JSONC/Markdown 3-way merge,
                                                             post-merge policy check)
                                                       │
                                                       ▼
                                                engine.Commit (commit-journal rollback,
                                                               state.json v1, vcs.Exclude)
```

* **Zero Global Mutation:** Never writes to user-global agent directories. Local store/cache lives solely under `AGENTFILE_CACHE_DIR` or `<UserCacheDir>/agentfile/`.
* **Translated Env Refs:** `${VAR}` and `${VAR:-default}` are tokenized into IR segments and lowered per target (`${VAR}` for Claude/Gemini, `{env:VAR}` for OpenCode, `${env:VAR}` for Cursor).
* **Fail-Closed Security:** Unsupported `deny_commands` or enforced `hooks` (`PreToolUse` or `enforce: true`) fail compilation (exit `2`) unless `targets_config.<target>.on_unsupported: warn` is explicitly set.
* **Non-Destructive Coexistence:** Uses drop-in rule files where supported, `hujson` comment-preserving 3-way JSONC patching (`SetEscapeHTML(false)`), fenced-code-aware Markdown blocks, and worktree-keyed `<git-common-dir>/info/exclude` entries.

## Target Matrix

| Target | Outputs | Env Syntax |
| :--- | :--- | :--- |
| `claude` | `.mcp.json`, `.claude/settings.json`, `.claude/skills/`, `.claude/agents/`, `.claude/rules/agentfile-*.md` | `${VAR}` / `${VAR:-default}` |
| `opencode` | `opencode.json`, `.opencode/skills/`, `.opencode/agents/`, `.opencode/agentfile-rules.md` | `{env:VAR}` |
| `gemini` | `.gemini/settings.json`, `.gemini/skills/`, `GEMINI.md` managed block | `${VAR}` |
| `cursor` | `.cursor/mcp.json`, `.cursor/rules/agentfile-*.mdc` | `${env:VAR}` |

## Further Documentation

* [docs/README.md](docs/README.md) — Documentation index
* [docs/cli.md](docs/cli.md) — Full CLI command, flag, and exit-code reference
* [docs/configuration.md](docs/configuration.md) — `Agentfile.yaml`, `Agentfile.lock`, and `.agentfile/config.yaml` reference
* [SPEC.md](SPEC.md) — Technical specification and architecture contracts
* [AGENTS.md](AGENTS.md) — Guidelines for AI coding agents working in this repository
* [internal/adapters/README.md](internal/adapters/README.md) — Adapter contract and contribution guide
* [schemas/README.md](schemas/README.md) — Manifest JSON Schema v1 details
* [examples/starter/README.md](examples/starter/README.md) — Starter `Agentfile.yaml` walkthrough

## Development

```sh
make fmt        # Format Go files
make test-race  # Run tests with race detector
make coverage   # Measure statement coverage
make lint       # Run go vet + gofmt (or golangci-lint)
make check      # Run lint, test-race, and Windows/Linux cross-compile checks
make build-all  # Build release binaries into dist/
```
