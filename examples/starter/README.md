# Starter Example (`examples/starter`)

`Agentfile.yaml` in this directory demonstrates a multi-target v1 manifest targeting `claude`, `opencode`, `gemini`, and `cursor`:

* **MCP servers:** Both `stdio` (`filesystem`) and `http` (`github` with `${GITHUB_PAT}` placeholder preserved/translated per target).
* **Local assets:** A skill (`./skills/terraform-auditor`), a subagent (`./agents/security-reviewer.md`), and a glob-scoped rule (`./docs/AGENT_RULES.md`).
* **Permissions & target overrides:** `allow_commands`, `deny_commands`, and `targets_config.<target>.on_unsupported: warn` for secondary targets that do not natively enforce shell command deny rules.

Validated automatically in `schemas/schema_test.go` against both `schemas/v1/agentfile.schema.json` and `spec.ParseFile`.
