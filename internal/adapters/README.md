# Target Adapters (`internal/adapters`)

Target adapters compile a normalized `ir.Bundle` into `adapters.CompileResult` operations without performing any filesystem, Git, network, or TTY I/O.

## Package Layout

* `adapter.go`: Pure interfaces (`Adapter`, `ArtifactOp`, `ProbeRequest`, `ProbeResults`, `SchemaContract`, `Diagnostic`, `CompileResult`) and `MarshalJSONNoEscape`. Must **never** import concrete adapter subpackages.
* `registry/registry.go`: Wires all supported target adapters (`claude`, `opencode`, `gemini`, `cursor`).
* `claude/`, `opencode/`, `gemini/`, `cursor/`: Target-specific compilers, unit tests, and `testdata/` golden fixtures.

## Adding a New Target Adapter

1. Create `internal/adapters/<target>/<target>.go` implementing `adapters.Adapter`:
   * `Name()`, `BinaryName()`, `SchemaContract()`
   * `RequiredProbes()`: declare any workspace file-existence checks needed before compilation (evaluated read-only by `engine.CollectProbes`).
   * `Compile(bundle *ir.Bundle, facts adapters.ProbeResults)`: lower `bundle.MCP`, `bundle.Skills`, `bundle.Agents`, `bundle.Rules`, `bundle.AllowCommands`, `bundle.DenyCommands`, and `bundle.Hooks` into `[]adapters.ArtifactOp`.
     * Translate `spec.EnvString` placeholders into the target's native interpolation syntax.
     * Emit `adapters.DiagError` for unsupported `DenyCommands`, enforced `Hooks`, or unsupported `${VAR:-default}` syntax unless `bundle.TargetsConfig[target].OnUnsupported == "warn"`.
     * Emit `adapters.DiagWarning` for unsupported advisory items (`AllowCommands`, advisory `Hooks`, `Agents`).
   * `CheckPolicy(bundle *ir.Bundle, mergedFiles map[string][]byte)`: inspect post-merge JSONC outputs and emit `adapters.DiagError` if preserved user/runtime entries contradict `bundle.DenyCommands`.
2. Add golden fixtures under `internal/adapters/<target>/testdata/` and tests covering env translation, fail-closed vs. `warn`, overrides, and `CheckPolicy`.
3. Register the adapter in `internal/adapters/registry/registry.go`, add the target name to `knownTargets` in `internal/spec/parser.go`, and add it to `schemas/v1/agentfile.schema.json`.
