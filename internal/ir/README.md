# `internal/ir`

Normalizes a validated `spec.ProjectSpec` and `store.ResolvedAsset` map into a target-agnostic `ir.Bundle`.

* Classifies `deny_commands` and `PreToolUse` / `enforce: true` hooks as enforced `SecurityControl` items vs. advisory items.
* Loads agent/rule Markdown bytes and skill `SKILL.md` bytes so `internal/adapters/*` remain free of filesystem I/O.
