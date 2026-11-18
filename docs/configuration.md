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

## 3. `.agentfile/config.yaml` (`version: 1`)

Optional machine-local tool preferences (excluded from Git via `<git-common-dir>/info/exclude` when `git_exclude: true`):

```yaml
# yaml-language-server: $schema=../schemas/v1/agentfile-config.schema.json
version: 1
link_mode: copy       # copy (default) | symlink (workspace-local sources only)
on_conflict: ask      # ask (interactive TTY default) | fail (non-TTY/CI default) | backup | overwrite
git_exclude: true     # true (default) | false
load_global: true     # true (default) | false (machine-local runtime isolation default)
```

## 4. IDE Autocomplete

See [schemas/README.md](../schemas/README.md) for `# yaml-language-server: $schema=...` modelines and VS Code `yaml.schemas` mappings covering `Agentfile.yaml`, `.agentfile/config.yaml`, and `Agentfile.lock`.
