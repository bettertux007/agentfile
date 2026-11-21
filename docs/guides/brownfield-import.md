# Brownfield Import (`agentfile init --import`)

Adopting `agentfile` in an existing repository does not require rewriting your configuration by hand. `agentfile init --import` scans existing repo-local configurations across all 7 supported targets, deduplicates them, extracts skills/agents/rules into `./.agentfile/imported/`, normalizes environment placeholders back to `${VAR}`, and writes a clean `Agentfile.yaml`.

## Running a Dry-Run Preview

Preview what `agentfile init --import` will discover without writing any files:

```sh
agentfile init --import --dry-run
```

For machine-readable output:

```sh
agentfile init --import --dry-run --json
```

## Importing and Committing

```sh
agentfile init --import
agentfile sync
```

### What Gets Scanned

* **MCP Servers:** `.mcp.json`, `opencode.json`, `.gemini/settings.json`, `.cursor/mcp.json`, `.vscode/mcp.json`, `.codex/config.toml`
* **Permissions & Hooks:** `.claude/settings.json`, `opencode.json`
* **Skills:** `.claude/skills/`, `.opencode/skills/`, `.gemini/skills/`, `.agents/skills/`
* **Subagents:** `.claude/agents/*.md`, `.opencode/agents/*.md`
* **Rules & Instructions:** `.claude/rules/*.md`, `.cursor/rules/*.mdc`, `.windsurf/rules/*.md`, `.opencode/agentfile-rules.md`, `GEMINI.md`, `AGENTS.md`, `.github/copilot-instructions.md`

### Safety Guardrails

1. **Env Placeholder Normalization:** Target-specific tokens (`{env:VAR}` from OpenCode, `${env:VAR}` from Cursor/Copilot, and Codex `env_vars` / `bearer_token_env_var` / `env_http_headers`) are normalized back to canonical `${VAR}` syntax in `Agentfile.yaml`.
2. **Fail-Closed Literal Secret Rejection:** If an existing config file on disk contains a literal API key, bearer token, or private key, `init --import` fails closed with exit `2` so credentials are never written into `Agentfile.yaml`. Replace the literal token with an environment placeholder first.
3. **Conflict Detection:** If two existing target files define the same MCP server, skill, agent, or rule with divergent definitions, `init --import` exits `2` and reports the conflicting files.
4. **Git-Trackable `.agentfile/imported/`:** Extracted Markdown and skill assets are saved under `./.agentfile/imported/{skills,agents,rules}/`. `agentfile sync` narrows its Git exclude entries to runtime state files (`.agentfile/{state.json,config.yaml,sync.lock,commit-journal.json,backups/,run-cache/}`) so `./.agentfile/imported/` remains trackable in Git.
