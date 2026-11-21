# `agentfile` Documentation

## Getting Started & Guides

* [Getting Started](getting-started.md) — installation, prerequisites, and 5-minute quickstart
* [Brownfield Import (`agentfile init --import`)](guides/brownfield-import.md) — migrating existing `.claude/`, `.mcp.json`, `opencode.json`, `.cursor/`, `.codex/`, and `AGENTS.md` setups
* [7 Targets, Shared Ownership & Runtime Isolation](guides/targets-and-sharing.md) — target output matrix, reference-counted `AGENTS.md` / `.agents/skills/`, `load_global: false`, and `agentfile shell-hook`
* [Security, `extends:` Composition & Env Lowering](guides/security-and-env.md) — base manifest inheritance, monotonic non-downgrade rules, `${VAR}` lowering, post-merge `CheckPolicy`, and `agentfile audit`

## Reference & Architecture

* [CLI Reference](cli.md) — `init`, `sync`, `status`, `diff`, `add`, `remove`, `upgrade`, `audit`, `shell-hook`, `doctor`, `run`, `clean`, flags, and exit codes
* [Configuration & Lockfile Reference](configuration.md) — `Agentfile.yaml` (`extends:`), `.agentfile/config.yaml`, `~/.config/agentfile/config.yaml`, `Agentfile.lock`, and env-var syntax
* [JSON Schema v1 & IDE Autocomplete](schemas.md) — YAML language server modelines and VS Code `yaml.schemas` setup
* [Architecture & Adapter Guide](architecture.md) — acyclic package DAG, sync & fast-path data flow, and implementing new target adapters
* [Starter Example](../examples/starter/README.md) — multi-target example manifest
* [Technical Specification](../SPEC.md) — full architectural and milestone specification
