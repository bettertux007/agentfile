# agentfile

[![CI](https://github.com/bettertux007/agentfile/actions/workflows/ci.yml/badge.svg)](https://github.com/bettertux007/agentfile/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/bettertux007/agentfile.svg)](https://pkg.go.dev/github.com/bettertux007/agentfile)
[![License: MIT](https://img.shields.io/badge/License-MIT-teal.svg)](LICENSE)

Declarative project compiler and dependency hydrator for AI coding agents (**Claude Code**, **OpenCode**, **Gemini CLI**, **Cursor**, **Codex CLI**, **GitHub Copilot**, and **Windsurf**).

Define project-scoped MCP servers, skills, subagents, instructions, permissions, and lifecycle hooks once in `Agentfile.yaml` (`version: 1`). `agentfile` resolves remote Git dependencies into a local cache tracked by `Agentfile.lock` (`version: 1`) and compiles isolated, project-level configurations for each target agent without mutating global config files (`~/.claude.json`, `~/.config/opencode/`, `~/.gemini/settings.json`, `~/.cursor/`, `~/.codex/`, `~/.copilot/`, `~/.codeium/windsurf/`, `~/.agents/skills/`).

## Installation

```sh
# Install with Go 1.26+
go install github.com/bettertux007/agentfile/cmd/agentfile@latest

# Or clone and install to ~/.local/bin
git clone https://github.com/bettertux007/agentfile.git
cd agentfile
make install
```

Prebuilt binaries for macOS, Linux, and Windows (`amd64` and `arm64`) are also published on [GitHub Releases](https://github.com/bettertux007/agentfile/releases).

## Quickstart

```sh
# Build and install locally
make install

# 1. Scaffold Agentfile.yaml (detects targets on $PATH, or use --import to migrate existing configs)
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
| `agentfile init` | `--minimal`, `--full`, `--import`, `--targets`, `--dry-run`, `--json`, `--force` | Scaffolds `Agentfile.yaml` from detected targets on `$PATH`, or with `--import` reverse-compiles existing repo configs into `./.agentfile/imported/` and `Agentfile.yaml`. |
| `agentfile sync` | `--targets`, `--link-mode`, `--copy`, `--on-conflict`, `--git-exclude`, `--update[=<kind:name>]`, `--dry-run`, `--force`, `--allow-external-local`, `--json` | Resolves dependencies, verifies `Agentfile.lock`, compiles target configs, prints lock deltas on `--update`, and updates worktree Git excludes. |
| `agentfile status` | `--targets`, `--check`, `--json` | Read-only check of manifest validity, lockfile alignment, config sources, dependency hydration, workspace drift, conflicts, and per-target isolation. |
| `agentfile diff` | `--targets`, `--stat`, `--name-only`, `--check`, `--fetch` | Previews secret-redacted unified diffs (or diffstat/paths) between `Agentfile.yaml` and workspace files without modifying disk or `Agentfile.lock`. |
| `agentfile add` | `skill\|agent\|rule\|mcp`, `--replace`, `--no-sync`, `--on-conflict`, `--allow-external-local` | Adds or replaces an entry in `Agentfile.yaml` (preserving comments), rejects literal secrets, and reconciles `Agentfile.lock` + workspace via sync. |
| `agentfile remove` | `<skill\|agent\|rule\|mcp> <name>`, `--no-sync`, `--on-conflict`, `--allow-external-local` | Removes an entry from `Agentfile.yaml` and prunes orphaned lockfile entries and workspace artifacts. |
| `agentfile upgrade` | `[<kind>:<name> \| <kind> <name> \| extends[:<url>]]`, `--targets`, `--dry-run`, `--on-conflict`, `--force`, `--allow-external-local`, `--json` | Refreshes remote Git dependencies and `extends:` bases, updates `Agentfile.lock`, syncs targets, and reports lock deltas. |
| `agentfile audit` | `--strict`, `--no-store`, `--json` | Runs a deterministic offline security and supply-chain audit separating executable inventory from secret-redacted findings. |
| `agentfile shell-hook` | `<zsh\|bash\|fish>`, `--targets`, `--only-isolated` | Emits opt-in shell wrapper functions routing bare target CLI calls through `agentfile run` inside `agentfile` repositories. |
| `agentfile doctor` | `--json` | Validates `Agentfile.yaml` against JSON Schema v1, checks `Agentfile.lock`, probes installed target CLIs (`MinVersion` / `MinIsolationVersion`), prints resolved config layers, and warns on shadowing global MCPs, skills, rules, and agents. |
| `agentfile run` | `--load-global`, `--no-global`, `--sync`, `--no-sync`, `--no-cache`, `--project-root`, `--if-configured`, `--only-isolated`, `--dry-run`, `--json` | Syncs target configuration (with a `<5ms` per-target input-digest fast path) and launches `<target>` with optional runtime global isolation. |
| `agentfile clean` | `--dry-run`, `--targets`, `--cache`, `--max-age`, `--json` | Removes tracked workspace artifacts/JSONC patches/excludes/run-cache, or prunes stale extracted cache entries when `--cache` is passed. |

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
.agentfile/config.yaml + CLI flags + probes ──► engine.Plan (JSONC/TOML/Markdown 3-way merge,
                                                             post-merge policy check)
                                                       │
                                                       ▼
                                                engine.Commit (commit-journal rollback,
                                                               state.json v1 + shared owners, vcs.Exclude)
```

* **Zero Global Mutation:** Never writes to user-global agent directories. Local store/cache lives solely under `AGENTFILE_CACHE_DIR` or `<UserCacheDir>/agentfile/`.
* **Translated Env Refs:** `${VAR}` and `${VAR:-default}` are tokenized into IR segments and lowered per target (`${VAR}` for Claude/Gemini, `{env:VAR}` for OpenCode, `${env:VAR}` for Cursor/Copilot, and `env_vars` / `env_http_headers` / `bearer_token_env_var` for Codex).
* **Fail-Closed Security:** Unsupported `deny_commands` or enforced `hooks` (`PreToolUse` or `enforce: true`) fail compilation (exit `2`) unless `targets_config.<target>.on_unsupported: warn` is explicitly set.
* **Non-Destructive Coexistence & Shared Ownership:** Uses drop-in rule files where supported, `hujson` comment-preserving 3-way JSONC patching (`SetEscapeHTML(false)`), `go-toml/v2` 3-way TOML patching (`.codex/config.toml`), fenced-code-aware Markdown blocks, reference-counted shared ownership (`AGENTS.md`, `.agents/skills/<name>`), and worktree-keyed `<git-common-dir>/info/exclude` entries that keep `.agentfile/imported/` trackable in Git.

## Target Matrix

| Target | Outputs | `rules_mode` | Env Syntax |
| :--- | :--- | :--- | :--- |
| `claude` | `.mcp.json`, `.claude/settings.json`, `.claude/skills/`, `.claude/agents/`, `.claude/rules/agentfile-*.md` | drop-in | `${VAR}` / `${VAR:-default}` |
| `opencode` | `opencode.json`, `.opencode/skills/`, `.opencode/agents/`, `.opencode/agentfile-rules.md` | single file | `{env:VAR}` |
| `gemini` | `.gemini/settings.json`, `.gemini/skills/`, `GEMINI.md` managed block | managed block | `${VAR}` |
| `cursor` | `.cursor/mcp.json`, `.cursor/rules/agentfile-*.mdc`, `.cursor/rules/agentfile-skill-*.mdc` | drop-in | `${env:VAR}` |
| `codex` | `.codex/config.toml`, `.agents/skills/` (shared), `AGENTS.md` managed block (shared) | `agents_md` (default) | `env_vars` / `env_http_headers` / `bearer_token_env_var` |
| `copilot` | `.vscode/mcp.json`, `.agents/skills/` (shared), `.github/copilot-instructions.md` or `AGENTS.md` (shared) | `copilot` (default) \| `agents_md` | `${env:VAR}` |
| `windsurf` | `.windsurf/rules/agentfile-*.md` or `AGENTS.md` (shared), `.agents/skills/` (shared); no project MCP (`PROJECT_MCP_UNSUPPORTED` warning) | `dropin` (default) \| `agents_md` | N/A |

## Further Documentation

* [docs/README.md](docs/README.md) — Documentation index & guides ([Getting Started](docs/getting-started.md), [Brownfield Import](docs/guides/brownfield-import.md), [Targets & Shared Ownership](docs/guides/targets-and-sharing.md), [Security & `extends:`](docs/guides/security-and-env.md), [Architecture](docs/architecture.md))
* [docs/cli.md](docs/cli.md) — Full CLI command, flag, and exit-code reference
* [docs/configuration.md](docs/configuration.md) — `Agentfile.yaml`, `Agentfile.lock`, and `.agentfile/config.yaml` reference
* [SPEC.md](SPEC.md) — Technical specification and architecture contracts
* [AGENTS.md](AGENTS.md) — Guidelines for AI coding agents working in this repository
* [internal/adapters/README.md](internal/adapters/README.md) — Adapter contract and contribution guide
* [schemas/README.md](schemas/README.md) — Manifest JSON Schema v1 details
* [examples/starter/README.md](examples/starter/README.md) — Starter `Agentfile.yaml` walkthrough
* [CONTRIBUTING.md](CONTRIBUTING.md) — Contribution guidelines and PR checklist
* [SECURITY.md](SECURITY.md) — Security policy and vulnerability reporting
* [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) — Contributor Covenant Code of Conduct

## Development

```sh
make fmt        # Format Go files
make test-race  # Run tests with race detector
make coverage   # Measure statement coverage
make lint       # Run go vet + gofmt (or golangci-lint)
make check      # Run lint, test-race, and Windows/Linux cross-compile checks
make build-all  # Build release binaries into dist/
```

## License

Released under the [MIT License](LICENSE).
