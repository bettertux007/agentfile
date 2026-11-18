# `internal/runner`

Prepares runtime facts and executes target CLIs for `agentfile run`:

* `GatherFacts`: Evaluates `adp.GlobalConfigSpecs()` (honoring `XDG_CONFIG_HOME` when `UseXDGConfig` is set), checks non-empty files/directories via `IsNonEmptyPath`, resolves `<CacheRoot>` via `store.ResolveCacheRoot()`, and probes `--version` when `load_global == false` and `adp.IsSessionArgs(userArgs)`.
* `ProbeBinaryVersion`: Captures the trimmed first line of `<bin> --version` cleanly with a 2s timeout.
* `Execute` & `ProcessRunner`: Atomically writes `<CacheRoot>/empty-mcp.json` (`{"mcpServers":{}}`) when `plan.NeedEmptyMCP` is true, merges `AGENTFILE_*` metadata env vars into `os.Environ()`, logs one-line status to `stderr`, and executes via `unix.Exec` (`exec_unix.go`) or `os/exec` with `signal.Ignore(os.Interrupt)` and `ExitCodeError` propagation (`exec_windows.go`).
