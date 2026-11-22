# Schemas (`schemas/`)

Contains Draft 2020-12 JSON Schemas for all `agentfile` YAML files (`version: 1`) and the Go `//go:embed` validation helpers used by `agentfile doctor`.

## Schema Files

* `v1/agentfile.schema.json`: Full IDE autocomplete & validation schema for `Agentfile.yaml` (including `targets_config.<target>.load_global` and `on_isolation_unsupported`).
* `v1/agentfile-config.schema.json`: Schema for machine-local `.agentfile/config.yaml` (including `load_global`).
* `v1/agentfile-lock.schema.json`: Schema for `Agentfile.lock`.
* `embed.go`: Embeds all v1 schemas and exposes `ValidateAgentfileV1`, `ValidateLocalConfigV1`, and `ValidateLockfileV1`.
* `schema_test.go`: Validates `examples/starter/Agentfile.yaml` and valid/invalid config & lockfile fixtures.

## IDE Autocomplete Setup

Add a `yaml-language-server` modeline comment at the top of `Agentfile.yaml` or `.agentfile/config.yaml` (using comments keeps strict `KnownFields(true)` YAML parsing happy):

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/bettertux007/agentfile/main/schemas/v1/agentfile.schema.json
version: 1
targets:
  - claude
```

Or map schemas globally/per-workspace in `.vscode/settings.json` (with the [YAML extension](https://marketplace.visualstudio.com/items?itemName=redhat.vscode-yaml)):

```json
{
  "yaml.schemas": {
    "./schemas/v1/agentfile.schema.json": "Agentfile.yaml",
    "./schemas/v1/agentfile-config.schema.json": ".agentfile/config.yaml",
    "./schemas/v1/agentfile-lock.schema.json": "Agentfile.lock"
  }
}
```
