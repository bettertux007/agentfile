# 7 Targets, Shared Ownership & Runtime Isolation

`agentfile` compiles a single `Agentfile.yaml` into project-scoped artifacts for 7 AI coding agents while coordinating shared files (`AGENTS.md`, `.agents/skills/`) and enforcing runtime global isolation (`load_global: false`).

## Target Output Matrix

| Target | Compiled Project Outputs | `rules_mode` | Env Interpolation |
| :--- | :--- | :--- | :--- |
| `claude` | `.mcp.json`, `.claude/settings.json`, `.claude/skills/`, `.claude/agents/`, `.claude/rules/agentfile-*.md` | `dropin` (fixed) | `${VAR}` / `${VAR:-default}` |
| `opencode` | `opencode.json`, `.opencode/skills/`, `.opencode/agents/`, `.opencode/agentfile-rules.md` | single file (fixed) | `{env:VAR}` |
| `gemini` | `.gemini/settings.json`, `.gemini/skills/`, `GEMINI.md` managed block | managed block (fixed) | `${VAR}` |
| `cursor` | `.cursor/mcp.json`, `.cursor/rules/agentfile-*.mdc`, `.cursor/rules/agentfile-skill-*.mdc` | `dropin` (fixed) | `${env:VAR}` |
| `codex` | `.codex/config.toml` (3-way TOML), `.agents/skills/` (shared), `AGENTS.md` managed block (shared) | `agents_md` (default) | `env_vars` / `env_http_headers` / `bearer_token_env_var` |
| `copilot` | `.vscode/mcp.json` (`/servers/*`), `.agents/skills/` (shared), `.github/copilot-instructions.md` or `AGENTS.md` (shared) | `copilot` (default) \| `agents_md` | `${env:VAR}` |
| `windsurf` | `.windsurf/rules/agentfile-*.md` or `AGENTS.md` (shared), `.agents/skills/` (shared); no project MCP | `dropin` (default) \| `agents_md` | N/A |

## Shared Artifact Ownership (`AGENTS.md` & `.agents/skills/`)

When multiple active targets emit open-standard artifacts (`AGENTS.md` and `.agents/skills/<name>`), `agentfile` tracks them in `.agentfile/state.json` under the `shared` namespace with an `owners` list (for example, `["codex", "copilot", "windsurf"]`):

* **Deduplicated Planning:** `engine.BuildPlan` verifies that all owning targets produce identical content and materializes the file or skill tree once per sync.
* **Reference-Counted Pruning:** Running `agentfile clean --targets codex` removes `"codex"` from `owners` while leaving `AGENTS.md` and `.agents/skills/<name>` intact on disk for `copilot` and `windsurf`. The file is deleted only when its last owning target is removed.
* **Multi-Target Drift Attribution:** `agentfile status` attributes drift or conflicts on a shared artifact to every target in its `owners` list.

## Runtime Global Isolation (`load_global: false`)

By default, coding agent CLIs load user-global configurations (`~/.claude/`, `~/.config/opencode/`, etc.) alongside project configs. Setting `load_global: false` tells `agentfile run <target>` to isolate the session to the project while keeping your live authentication intact:

* **Claude Code (`claude >= 2.1.0`):** Passes `--setting-sources project,local --strict-mcp-config --mcp-config <project-or-empty-mcp.json>`.
* **OpenCode (`opencode >= 1.18.0`):** Sets `OPENCODE_CONFIG_DIR=<CacheRoot>/empty-opencode-config`, `OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1`, and `--standalone`.
* **Targets without full CLI isolation (`codex`, `copilot`, `windsurf`, `cursor`, `gemini`):** Fail closed with exit `2` when `load_global: false` unless `targets_config.<target>.on_isolation_unsupported: warn` is explicitly set.

### Automatic Shell Wrapping (`agentfile shell-hook`)

To avoid typing `agentfile run` manually in repositories where `load_global: false` is configured, add the shell hook to your shell startup file:

```sh
# ~/.zshrc
eval "$(agentfile shell-hook zsh)"

# ~/.bashrc
eval "$(agentfile shell-hook bash)"

# ~/.config/fish/config.fish
agentfile shell-hook fish | source
```

Outside `agentfile` repositories, the wrapper executes zero `agentfile` subprocesses. Inside a repository (including subdirectories), it walks upward to locate `Agentfile.yaml` and routes through `agentfile run --if-configured --project-root <dir>` while preserving your current working directory.
