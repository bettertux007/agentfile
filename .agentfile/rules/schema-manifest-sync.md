### Schema, Spec, and Manifest Lockstep Synchronization

Whenever modifying manifest types (`internal/spec/`), target adapter capabilities (`internal/adapters/`), or embedded JSON Schemas (`schemas/v1/`):

1. **Keep Types and JSON Schemas in Lockstep:**
   * Update `internal/spec/types.go`, `internal/spec/parser.go`, and `internal/spec/validate.go` in tandem with `schemas/v1/agentfile.schema.json`, `schemas/v1/agentfile-config.schema.json`, and `schemas/v1/agentfile-lock.schema.json`.
   * Remember `schemas/v1/agentfile.schema.json` enforces `"additionalProperties": false` at the root and on nested spec objects.
2. **Validate Both Root and Starter Manifests:**
   * Ensure both root `./Agentfile.yaml` (plus `./Agentfile.lock`) and `./examples/starter/Agentfile.yaml` pass `go test -v ./schemas/...` (`ValidateAgentfileV1`, `ValidateLockfileV1`, `spec.ParseFile`, `spec.LoadLock`).
   * Run `go run ./cmd/agentfile audit --strict --no-store` to verify zero schema, lockfile, or policy regressions.
