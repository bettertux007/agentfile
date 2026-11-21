# Getting Started

`agentfile` is a declarative compiler and dependency hydrator that compiles `Agentfile.yaml` (`version: 1`) and `Agentfile.lock` (`version: 1`) into isolated, project-scoped configurations for **Claude Code**, **OpenCode**, **Gemini CLI**, **Cursor**, **Codex CLI**, **GitHub Copilot**, and **Windsurf**.

## Prerequisites

* **Go `1.26+`** (for building or installing from source)
* **Git** (for hydrating remote Git dependencies and managing worktree-safe excludes)

## Installation

### Option 1: `go install`

```sh
go install github.com/bettertux007/agentfile/cmd/agentfile@latest
```

### Option 2: Clone and `make install`

```sh
git clone https://github.com/bettertux007/agentfile.git
cd agentfile
make install   # Installs to ~/.local/bin/agentfile by default
```

Verify the installation:

```sh
agentfile --version
```

## 5-Minute Quickstart

### 1. Initialize or Import `Agentfile.yaml`

In a new project, scaffold a starter manifest (automatically detects installed agent CLIs on `$PATH`):

```sh
agentfile init --full
```

Or in an existing repository that already has `.claude/`, `.mcp.json`, `opencode.json`, `.cursor/`, or `AGENTS.md`, import your existing setup in one command:

```sh
agentfile init --import
```

### 2. Author `Agentfile.yaml`

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/bettertux007/agentfile/main/schemas/v1/agentfile.schema.json
version: 1
targets:
  - claude
  - opencode
  - codex

mcp:
  github:
    transport: http
    url: "https://api.githubcopilot.com/mcp/"
    headers:
      Authorization: "Bearer ${GITHUB_PAT}"
  filesystem:
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem@2025.1.1", "."]

skills:
  - name: code-review
    source: ./skills/code-review

rules:
  - name: coding-standards
    source: ./docs/RULES.md
    description: "Repository coding standards"
    globs: ["**/*.go"]
    always_apply: true

settings:
  permissions:
    allow_commands: ["go test ./..."]
    deny_commands: ["rm -rf *"]

targets_config:
  claude:
    load_global: false
  opencode:
    load_global: false
    on_unsupported: warn
```

### 3. Preview, Sync, and Inspect

```sh
# Preview secret-redacted unified diffs before writing
agentfile diff

# Compile target configurations and write Agentfile.lock
agentfile sync

# Check workspace drift and isolation status
agentfile status --check

# Run offline supply-chain and security audit
agentfile audit
```

### 4. Launch an Isolated Agent Session

```sh
# Syncs via the sub-5ms fast path and launches claude with global isolation
agentfile run claude
```

To route bare `claude` or `opencode` invocations inside `agentfile` repositories automatically:

```sh
eval "$(agentfile shell-hook zsh)"
```

## Next Steps

* [Brownfield Import (`init --import`)](/guides/brownfield-import)
* [7 Targets, Shared Ownership & Isolation](/guides/targets-and-sharing)
* [Security, `extends:` Composition & Env Lowering](/guides/security-and-env)
* [CLI Reference](/cli)
* [Configuration & Lockfile Reference](/configuration)
