# `internal/cli`

Cobra command wiring, TTY conflict prompting, secret redaction, and exit-code mapping (`0` success, `1` runtime/I/O, `2` validation/policy/usage).

* `root.go`: Root command, `Execute` / `ExecuteWithIO`, build metadata (`Version`, `Commit`, `Date`), and `ExitCode` classification.
* `init.go`: `agentfile init` (`$PATH` probing, `--minimal`, `--full`, `--force`).
* `sync.go`: `agentfile sync` orchestrating `spec -> store -> ir -> probes -> adapters -> engine.Plan -> engine.Commit -> spec.SaveLock` plus interactive `ttyPrompter`.
* `clean.go`: `agentfile clean` for workspace cleanup and `--cache --max-age` store pruning.
* `doctor.go`: `agentfile doctor` validating JSON Schema v1, lockfiles, CLI `SchemaContract()` binaries, effective config, and read-only global MCP shadowing.
