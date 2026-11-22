# AGENTS.md

Instructions for AI coding agents working in the `agentfile` repository.

## Project Overview

`agentfile` is a Go 1.26+ CLI compiler and dependency hydrator (`cmd/agentfile`, `internal/*`, `schemas/`) that compiles `Agentfile.yaml` (`version: 1`) and `Agentfile.lock` (`version: 1`) into isolated project-scoped configurations for Claude Code, OpenCode, Gemini CLI, Cursor, Codex CLI, GitHub Copilot, and Windsurf.

Read `docs/architecture.md` for the full architectural and adapter specification.

## Build, Test, and Verification Commands

Run these commands from the repository root:

```sh
make build       # Build ./agentfile binary
make test        # Run all unit & integration tests
make test-race   # Run full test suite with -race
make coverage    # Generate coverage.out and print aggregate statement coverage
make lint        # Run go vet and gofmt check (or golangci-lint if installed)
make check       # Run lint, test-race, cross-compile checks (windows + linux), and offline strict self-audit
make fmt         # Format all Go source files with gofmt
```

Exit codes returned by `agentfile`:
* `0`: success
* `1`: I/O or runtime error
* `2`: validation, security policy, lockfile mismatch, conflict failure, or CLI usage error

## Architectural Invariants (Do Not Violate)

1. **Strict Acyclic Package DAG:**
   * `cmd/agentfile` -> `internal/cli` -> `{config, spec, store, runner, engine, diff, adapters/registry, schemas}`
   * `internal/adapters` (`adapter.go`) defines interfaces and value types only; it must **never** import `internal/adapters/*`.
   * Concrete adapters (`internal/adapters/{claude,opencode,gemini,cursor,codex,copilot,windsurf}`) are wired only in `internal/adapters/registry/registry.go`.
   * `internal/engine` must **never** import `internal/adapters/*` (or `internal/adapters/registry`).
   * `internal/runner` handles global-hit probing, `<CacheRoot>/empty-mcp.json`, and process execution (`unix.Exec` / Windows `os/exec`); adapters remain I/O-free via `GlobalConfigSpecs()`, `IsSessionArgs()`, and `PlanLaunch()`.
   * `internal/ir` depends only on `internal/spec` and `internal/store`.
2. **I/O-Free Adapters:** Target adapters must perform zero filesystem, Git, network, or TTY I/O. Declare workspace file-existence checks via `RequiredProbes()` and consume `ProbeResults` in `Compile`.
3. **Zero Global Agent Mutation:** Never write to `~/.claude*`, `~/.config/opencode*`, `~/.gemini*`, `~/.cursor*`, `~/.codex*`, `~/.copilot*`, or `~/.codeium/windsurf*`. Store/cache data goes solely under `AGENTFILE_CACHE_DIR` or `<UserCacheDir>/agentfile` (`internal/store`).
4. **Env-Var Safety:** Never call `os.ExpandEnv` on manifest values. Tokenize `${VAR}` / `${VAR:-default}` into `spec.EnvString` / `ir` segments and lower to each target's native interpolation syntax (`${VAR}` for Claude/Gemini/Codex/Windsurf, `{env:VAR}` for OpenCode, `${env:VAR}` for Cursor, `${input:VAR}` for Copilot).
5. **Fail-Closed Security:** Unsupported `deny_commands` or enforced `hooks` (`PreToolUse` or `enforce: true`) must emit `adapters.DiagError` (exit `2`) unless `targets_config.<target>.on_unsupported: warn` is explicitly set. `engine.Plan` must also run `CheckPolicy` on post-merge JSONC/TOML files before `Commit`.
6. **Git & Store Safety:** Never unpack symlinks from dependency trees (`store.ErrSymlinkNotAllowed`), always pass Git hardening flags (`core.hooksPath=/dev/null`, `submodule.recurse=false`, `protocol.ext.allow=never`, `core.autocrlf=false`, `GIT_TERMINAL_PROMPT=0`), and use `internal/lock` without unlinking lockfiles.
7. **VCS Hygiene for EGM Artifacts:** Any brainstorming, design drafts, or review reports (EGM outputs) must be placed in `.egm/` (already gitignored in `.gitignore`), never at the repository root.
   * `EGM artifact directory: .egm`

## Dogfooding (`Agentfile.yaml`)

This repository is managed by its own root `Agentfile.yaml` (`targets: [opencode, claude]`, `load_global: false`) and `Agentfile.lock`, with project-specific rules, agents, and skills stored under `./.agentfile/{rules,agents,skills}/`:

```sh
make build && ./agentfile sync              # Hydrate EGM bundle and compile opencode + claude configs
./agentfile status --check                  # Verify workspace and lockfile synchronization
./agentfile audit --strict --no-store       # Run offline strict security and supply-chain audit
```

## Testing Expectations

* Place golden fixtures in `internal/adapters/<target>/testdata/`.
* Keep tests hermetic using `t.TempDir()`, `t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())`, and `AGENTFILE_ALLOW_FILE_GIT=1` with local bare Git repos.
* Update `schemas/v1/agentfile.schema.json` alongside `internal/spec/types.go` and keep both root `Agentfile.yaml` and `examples/starter/Agentfile.yaml` passing `schemas.ValidateAgentfileV1` and `spec.ParseFile`.
