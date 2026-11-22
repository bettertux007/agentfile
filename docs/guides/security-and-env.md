# Security, `extends:` Composition & Env Lowering

`agentfile` enforces a fail-closed security model across manifest composition, environment variable lowering, 3-way config merging, and supply-chain auditing.

## 1. `extends:` Manifest Composition & Monotonic Security

Repositories can inherit from one or more local or Git-hosted base manifests (`extends:` up to depth `8`):

```yaml
version: 1
extends:
  - "https://github.com/org/ai-base.git#v1.2.0//base/Agentfile.yaml"
  - ./presets/team.yaml
targets: [claude, opencode]
```

* **Origin-Anchored Relative Paths:** Relative `./...` paths for `skills`, `agents`, `rules`, and nested `extends:` inside a local base resolve relative to that base file's directory. Relative `./...` paths inside a remote Git base rewrite to commit-pinned Git sources inside the base repository (rejecting `..` escapes above the repository root).
* **Pinned in `Agentfile.lock`:** Remote `extends:` bases are locked by canonical URL and subpath under the `extends:` section of `Agentfile.lock` (`commit_sha` and `content_hash`) and can be refreshed via `agentfile upgrade extends`.
* **Monotonic Security Non-Downgrade:** Layers merge left-to-right across `extends:` bases and then the child `Agentfile.yaml`. Later layers can override `targets`, `mcp` servers, and `skills`/`agents`/`rules` by name, but **cannot weaken security controls** established by an earlier base layer:
  * `settings.permissions.deny_commands` and enforced hooks (`PreToolUse` or `enforce: true`) merge via **additive union**.
  * `targets_config.<target>.load_global: false`, `on_unsupported: fail`, and `on_isolation_unsupported: fail` cannot be relaxed to `true` or `warn` by a later layer.

## 2. Fail-Closed Security & Post-Merge Policy Validation

* **Compile-Time Enforcement:** If `Agentfile.yaml` declares `deny_commands` or enforced `hooks` (`PreToolUse` or `enforce: true`) and an active target cannot enforce them natively (such as `opencode` for hooks, or `gemini`/`cursor`/`copilot`/`windsurf` for `deny_commands` and hooks), compilation fails closed with exit `2` unless `targets_config.<target>.on_unsupported: warn` is explicitly set.
* **Post-Merge `CheckPolicy`:** Before committing 3-way merged JSONC or TOML files, `engine.BuildPlan` runs each adapter's `CheckPolicy` on the merged result so pre-existing user entries on disk (such as a conflicting `permissions.allow` rule in `.claude/settings.json` or `sandbox_mode = "danger-full-access"` in `.codex/config.toml`) cannot silently undermine `deny_commands`.

## 3. Environment Variable Safety

`agentfile` never calls `os.ExpandEnv` on manifest values and never writes live secret values from your environment into compiled files:

* `${VAR}` and `${VAR:-default}` are tokenized into typed `EnvString` segments and lowered to each target's native interpolation syntax.
* `agentfile add mcp` and `agentfile init --import` reject literal credentials with exit `2`.
* `agentfile diff`, `agentfile doctor`, `agentfile sync`, `agentfile run --dry-run`, and `agentfile audit` mask literal secrets via `[REDACTED]` while keeping `${VAR}`, `{env:VAR}`, and `${env:VAR}` tokens inspectable.

## 4. Supply-Chain Auditing (`agentfile audit`)

Run `agentfile audit --strict` in CI to enforce repository supply-chain hygiene offline without installing target agent CLIs:

```sh
agentfile audit --strict --json
```

* **Always-Fail `error` Rules (exit `2`):** `MANIFEST_INVALID`, `EXTENDS_CYCLE`, `LOCK_INVALID`, `CACHE_INTEGRITY_MISMATCH`, `SYMLINK_IN_TREE`, `LITERAL_SECRET`.
* **Strict-Promoted `warning` Rules (exit `2` with `--strict`):** `EXTENDS_BASE_UNHYDRATED`, `FLOATING_REF`, `MUTABLE_TAG`, `LOCK_MISSING_OR_DRIFT`, `EXTERNAL_LOCAL_PATH`, `CLEARTEXT_HTTP_MCP`, `UNPINNED_PACKAGE_RUNNER`, `POLICY_DOWNGRADE_WARN`, `GLOBAL_ISOLATION_OPEN`, `INHERITED_ENTRY_OVERRIDDEN`, `CACHE_ENTRY_MISSING`.
* **Informational `inventory` (never fails `--strict`):** Lists all configured `hooks`, `stdio_mcps`, and executable `skill_scripts` inside hydrated skill directories.
