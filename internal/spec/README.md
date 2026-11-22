# `internal/spec`

Manifest (`Agentfile.yaml` v1) and lockfile (`Agentfile.lock` v1) schemas, strict YAML decoding, and validation:

* `types.go`: `ProjectSpec`, `SourceSpec`, `EnvString` / `ParseEnvString`, `Lockfile`, and `PolicyError`.
* `parser.go`: Strict `KnownFields(true)` single-document YAML decoding, slug validation, and manifest invariant checks without calling `os.ExpandEnv`.
* `lock.go`: Deterministic YAML `Agentfile.lock` serialization (`LoadLock`, `SaveLock`) and `VerifyMatches`.
