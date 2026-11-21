---
layout: home

hero:
  name: "agentfile"
  text: "Declarative Compiler for AI Coding Agents"
  tagline: "Compile Agentfile.yaml and Agentfile.lock into isolated project configurations for Claude Code, OpenCode, Gemini CLI, Cursor, Codex, Copilot, and Windsurf."
  image:
    src: /logo.svg
    alt: agentfile
  actions:
    - theme: brand
      text: Getting Started
      link: /getting-started
    - theme: alt
      text: CLI Reference
      link: /cli
    - theme: alt
      text: View on GitHub
      link: https://github.com/bettertux007/agentfile

features:
  - title: "Single Manifest, 7 Target Agents"
    details: "Define MCP servers, skills, subagents, instructions, permissions, and lifecycle hooks once in Agentfile.yaml (with extends composition) and compile to Claude, OpenCode, Gemini, Cursor, Codex, Copilot, and Windsurf."
  - title: "Deterministic Git Locking & Audit"
    details: "Pin remote skills, agents, rules, and base manifests in Agentfile.lock with SHA-256 tree verification, comment-preserving add/remove/upgrade commands, and offline supply-chain auditing (agentfile audit --strict)."
  - title: "Fail-Closed Security & Env Lowering"
    details: "Never expands secrets onto disk. Lowers ${VAR} to each tool's native syntax (${VAR}, {env:VAR}, ${env:VAR}, or Codex TOML env_vars) and fails closed on unsupported deny_commands or enforced PreToolUse hooks."
  - title: "Runtime Global Isolation & Fast Path"
    details: "Enforce load_global=false via agentfile run (with a sub-5ms input-digest fast path) and opt-in agentfile shell-hook wrappers without touching ~/.claude, ~/.config/opencode, or OAuth credentials."
---

## Quick Preview

```sh
# Install from source (requires Go 1.26+)
go install github.com/bettertux007/agentfile/cmd/agentfile@latest

# Scaffold or import existing repository agent configs
agentfile init --import

# Preview unified diffs, sync target configs, and run supply-chain audit
agentfile diff
agentfile sync
agentfile audit --strict

# Launch an isolated agent session (sub-5ms warm fast path)
agentfile run claude
```
