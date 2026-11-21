# Security Policy

## Supported Versions

Security updates are applied to the latest release on the `main` branch.

| Version | Supported |
| :--- | :--- |
| `0.1.x` / `main` | Yes |

## Reporting a Vulnerability

Please **do not** report security vulnerabilities through public GitHub issues.

Instead, report vulnerabilities privately via **[GitHub Private Vulnerability Reporting](https://github.com/bettertux007/agentfile/security/advisories/new)** on the repository's Security tab.

Please include:

1. A description of the vulnerability and its potential impact.
2. Minimal reproduction steps or a proof-of-concept `Agentfile.yaml`.
3. The `agentfile --version` output and operating system.

You can expect an initial acknowledgment within 48 hours and a remediation timeline after triage.

## Security Model Guarantees

`agentfile` treats the following behaviors as security invariants; bypasses of any of these are considered security vulnerabilities:

1. **Zero Secret Expansion on Disk:** `agentfile` never invokes `os.ExpandEnv` on manifest values and never writes live environment secret values into compiled target files.Literal credentials in `agentfile add mcp` and `agentfile init --import` are rejected with exit `2`, and `agentfile diff` / `doctor` / `audit` redact literal credentials.
2. **Fail-Closed Security Controls:** Unsupported `settings.permissions.deny_commands`, enforced `settings.hooks` (`PreToolUse` or `enforce: true`), or unsupported `load_global: false` runtime isolation fail closed with exit `2` unless `on_unsupported: warn` or `on_isolation_unsupported: warn` is explicitly set.
3. **Monotonic Inheritance & Non-Downgrade Protection:** In `extends:` chains and multi-layer `config.yaml` resolution, downstream layers and local config files cannot silently drop inherited `deny_commands`, enforced hooks, or `load_global: false`.
4. **Hardened Git & Archive Extraction:** Remote Git hydration disables Git hooks (`core.hooksPath=/dev/null`), submodules (`submodule.recurse=false` and `160000` gitlink rejection), `ext::` remotes (`protocol.ext.allow=never`), and terminal prompts (`GIT_TERMINAL_PROMPT=0`), and rejects all symlinks, hardlinks, and path-traversal entries during tar extraction (`store.ErrSymlinkNotAllowed`).
5. **Zero Global Agent Mutation:** `agentfile` never mutates user-global agent configuration directories (`~/.claude*`, `~/.config/opencode*`, `~/.gemini*`, `~/.cursor*`, `~/.codex*`, `~/.copilot*`, `~/.codeium/windsurf*`).
