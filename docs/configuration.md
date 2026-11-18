# Configuration & Schema Reference

## 1. `Agentfile.yaml` (`version: 1`)

Committed at the repository root. Unknown YAML fields and multi-document YAML streams are rejected.

### Top-Level Fields

* `version`: Integer `1` (required).
* `targets`: Non-empty list of targets from `claude`, `opencode`, `gemini`, `cursor` (required).
* `mcp`: Map of MCP server definitions keyed by slug (`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`).
  * `transport`: `stdio`, `http`, or `sse`.
  * `stdio` fields: `command`, `args`, `env`.
  * `http` / `sse` fields: `url`, `headers`.
* `skills`: List of `{name, source}` directory-backed skill assets (`SKILL.md`).
* `agents`: List of `{name, description, model, source}` subagent Markdown files.
* `rules`: List of `{name, source, description, globs, always_apply}` instruction Markdown files.
* `settings`:
  * `permissions.allow_commands`: List of allowed command patterns.
  * `permissions.deny_commands`: List of denied command patterns (enforced security control).
  * `hooks`: Map of event name (e.g., `SessionStart`, `PreToolUse`) to `[{matcher, command, enforce}]`. Any `PreToolUse` hook or hook with `enforce: true` is treated as an enforced security control.
* `targets_config`: Per-target compilation and launch isolation configuration:
  * `on_unsupported`: `fail` (default for enforced security controls and env default modifiers) or `warn` (downgrade to warning and omit unsupported items).
  * `load_global`: Boolean (`true` default). When `false`, `agentfile run <target>` enforces native global-config isolation (`claude` uses `--setting-sources project,local --strict-mcp-config --mcp-config ...`; `opencode` uses `OPENCODE_CONFIG_DIR=<CacheRoot>/empty-opencode-config`, `OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1`, and `--standalone`).
  * `on_isolation_unsupported`: `fail` (default) or `warn` (degrade to `partial_warn` when full global isolation is unsupported on a target or when `~/.claude/skills` or `~/.agents/skills` is non-empty).
  * `rules_mode`: `dropin` (default).
  * `overrides`: Map of target-specific keys merged into that target's JSON config file.

### Dependency Source Syntax

* **Local workspace paths:** `./...` (must stay within the repository root unless `--allow-external-local` is passed). Local paths are **not** recorded in `Agentfile.lock`.
* **Git string syntax:** `<repo_url>[#<ref>][//<subpath>]`
  * Allowed schemes: `https://`, `ssh://`, `git@host:org/repo.git` (`file://` requires `AGENTFILE_ALLOW_FILE_GIT=1`; `ext::` is rejected).
  * Uses `//<subpath>` instead of `:` so SCP SSH URLs and ports never collide.
* **Git structured YAML syntax:**
  ```yaml
  source:
    git: https://github.com/org/agent-skills.git
    ref: v1.0.0
    path: skills/k8s-hardening
  ```

### Environment Variable Interpolation

`agentfile` never calls `os.ExpandEnv`. Placeholders are parsed into typed tokens and lowered per target:

* `${VAR}` -> `${VAR}` (Claude, Gemini), `{env:VAR}` (OpenCode), `${env:VAR}` (Cursor)
* `${VAR:-default}` -> `${VAR:-default}` (Claude); fails closed on OpenCode, Gemini, and Cursor unless `on_unsupported: warn` (which strips the default).
* `$$` -> literal `$`.

---

## 2. `Agentfile.lock` (`version: 1`)

Sorted-key YAML lockfile pinning remote Git dependencies by `<kind>:<name>` (e.g., `skill:k8s-hardening`):

```yaml
version: 1
dependencies:
  skill:k8s-hardening:
    source: https://github.com/org/agent-skills.git
    ref: v1.0.0
    commit_sha: <40-char-sha>
    content_hash: sha256:<64-char-hex>
```

`agentfile sync` fails with exit code `2` if an existing `Agentfile.lock` diverges from `Agentfile.yaml` without `--update`.

---

## 3. Tool Configuration (`config.yaml`, `version: 1`)

Machine-local and personal workflow preferences share the `version: 1` config schema (`schemas/v1/agentfile-config.schema.json`) and stay separate from committed `Agentfile.yaml` and `Agentfile.lock`:

```yaml
# yaml-language-server: $schema=../schemas/v1/agentfile-config.schema.json
version: 1
link_mode: copy       # copy (default) | symlink (workspace-local sources only)
on_conflict: ask      # ask (interactive non-CI TTY default) | fail (non-TTY/CI default) | backup | overwrite
git_exclude: true     # true (default) | false
load_global: true     # true (default) | false (machine-local runtime isolation default)
```

### Configuration Layers & Precedence

`agentfile` merges 4 configuration layers for `link_mode`, `on_conflict`, and `git_exclude`:

1. **Built-in defaults:** `link_mode: copy`, `git_exclude: true`, and `on_conflict: ask` (in interactive non-CI TTYs) or `fail` (in non-TTY or CI environments).
2. **User-global config:** `~/.config/agentfile/config.yaml` (personal defaults across repositories).
3. **Repo-local config:** `.agentfile/config.yaml` (excluded from Git via `<git-common-dir>/info/exclude` when `git_exclude: true`).
4. **CLI flags:** `--link-mode`, `--copy`, `--on-conflict`, `--force`, and `--git-exclude`.

Note: Setting `on_conflict: overwrite` in the user-global config emits a warning because unmanaged local edits may be overwritten without prompting.

### User-Global Config Discovery & CI Isolation

`agentfile` resolves the user-global config file using the following precedence:

1. `AGENTFILE_NO_USER_CONFIG=1`: Disables user-global config loading completely (takes precedence over `AGENTFILE_CONFIG_FILE`).
2. `AGENTFILE_CONFIG_FILE=<path>`: Loads `<path>` explicitly (even when CI variables are set). Fails closed with exit `2` if the file does not exist, cannot be read, or is a directory.
3. **CI auto-isolation:** If any of `CI`, `GITHUB_ACTIONS`, `BUILDKITE`, `GITLAB_CI`, or `CIRCLECI` is non-empty, default user-global config discovery is skipped so CI runs stay reproducible.
4. `$XDG_CONFIG_HOME/agentfile/config.yaml` (when `XDG_CONFIG_HOME` is set).
5. `$HOME/.config/agentfile/config.yaml` (Unix/macOS).
6. `<os.UserConfigDir()>/agentfile/config.yaml` fallback (e.g., `%AppData%\agentfile\config.yaml` on Windows).

### `load_global` Precedence & Non-Downgrade Rule

Per-target `load_global` resolution for `agentfile run`, `agentfile status`, `agentfile sync`, and `agentfile doctor` follows this order:

`CLI (--no-global / --load-global)` > `Agentfile.yaml (load_global: false)` > `local_config (.agentfile/config.yaml)` > `Agentfile.yaml (load_global: true)` > `user_config (~/.config/agentfile/config.yaml)` > `default (true)`

* **Security non-downgrade:** If `Agentfile.yaml` sets `targets_config.<target>.load_global: false`, neither `.agentfile/config.yaml` nor `~/.config/agentfile/config.yaml` (`load_global: true`) can weaken it; `agentfile` keeps `load_global=false` and emits a warning. Only an explicit CLI `--load-global` flag can override `Agentfile.yaml`'s `load_global: false` (also emitting a `stderr` warning).
* **Local tightening:** When `Agentfile.yaml` sets `load_global: true` or omits it, `.agentfile/config.yaml` or `~/.config/agentfile/config.yaml` can set `load_global: false` to enforce runtime isolation locally.

---

## 4. IDE Autocomplete

See [schemas/README.md](../schemas/README.md) for `# yaml-language-server: $schema=...` modelines and VS Code `yaml.schemas` mappings covering `Agentfile.yaml`, `.agentfile/config.yaml`, and `Agentfile.lock`.
