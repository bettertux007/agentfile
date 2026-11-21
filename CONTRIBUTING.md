# Contributing to `agentfile`

Thank you for contributing to `agentfile`! This document outlines the development setup, architectural invariants, and verification steps required for pull requests.

## Prerequisites

* **Go `1.26+`**
* **Git**
* **Node.js `22+`** (optional, only required when editing the VitePress documentation site under `docs/`)

## Development Commands

Run these commands from the repository root:

```sh
make build        # Build ./agentfile binary
make test         # Run unit and integration tests
make test-race    # Run full test suite with -race
make coverage     # Generate coverage.out and print aggregate statement coverage (85%+ required)
make lint         # Run go vet and gofmt check (or golangci-lint if installed)
make check        # Run lint, test-race, and Windows/Linux cross-compile checks
make fmt          # Format all Go source files with gofmt
make docs-build   # Build the VitePress static documentation site in docs/
```

## Architectural Invariants

Every pull request must preserve the project's core invariants (see [SPEC.md](SPEC.md) and [AGENTS.md](AGENTS.md)):

1. **Strict Acyclic Package DAG:**
   * `cmd/agentfile` -> `internal/cli` -> `{config, spec, store, runner, engine, diff, adapters/registry, schemas}`
   * `internal/adapters` (`adapter.go`) defines pure interfaces and value types only and must **never** import concrete adapters (`internal/adapters/*`).
   * Concrete adapters (`internal/adapters/{claude,opencode,gemini,cursor,codex,copilot,windsurf}`) are wired only in `internal/adapters/registry/registry.go`.
   * `internal/engine` must **never** import `internal/adapters/*` or `internal/adapters/registry`.
   * `internal/spec` must **never** import `internal/store`.
2. **I/O-Free Target Adapters:** Target adapters must perform zero filesystem, Git, network, or process I/O. Declare workspace file-existence probes via `RequiredProbes()` and consume `ProbeResults` in `Compile`.
3. **Zero Global Agent Mutation:** Never write to `~/.claude*`, `~/.config/opencode*`, `~/.gemini*`, `~/.cursor*`, `~/.codex*`, `~/.copilot*`, or `~/.codeium/windsurf*`. Cache and store data live solely under `AGENTFILE_CACHE_DIR` or `<UserCacheDir>/agentfile`.
4. **Environment Variable Safety:** Never call `os.ExpandEnv` on manifest values. Tokenize `${VAR}` and `${VAR:-default}` into `spec.EnvString` / `ir` segments and lower them to each target's native interpolation syntax.
5. **Fail-Closed Security:** Unsupported `deny_commands` or enforced `hooks` (`PreToolUse` or `enforce: true`) must emit `adapters.DiagError` (exit code `2`) unless `targets_config.<target>.on_unsupported: warn` is explicitly configured.
6. **Git & Store Hardening:** Reject symlinks in dependency trees (`store.ErrSymlinkNotAllowed`), reject `160000` submodule gitlinks, and pass Git hardening flags (`core.hooksPath=/dev/null`, `submodule.recurse=false`, `protocol.ext.allow=never`, `core.autocrlf=false`, `GIT_TERMINAL_PROMPT=0`).

## Testing Expectations

* Keep all tests hermetic using `t.TempDir()`, `t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())`, `t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")`, and `AGENTFILE_ALLOW_FILE_GIT=1` with local bare Git repositories.
* Place adapter golden fixtures in `internal/adapters/<target>/testdata/`.
* Update `schemas/v1/agentfile.schema.json` alongside `internal/spec/types.go` and verify `examples/starter/Agentfile.yaml` continues to pass `schemas.ValidateAgentfileV1` and `spec.ParseFile`.

## Pull Request Checklist

- [ ] `make fmt` and `make check` pass with zero warnings or errors.
- [ ] `make coverage` maintains `>= 85%` aggregate statement coverage.
- [ ] If documentation under `docs/` was modified, `make docs-build` passes with zero broken links.
- [ ] Commit messages follow Conventional Commits (`feat(scope): ...`, `fix(scope): ...`, `docs: ...`).
