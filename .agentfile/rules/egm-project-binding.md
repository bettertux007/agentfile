### EGM Project Binding & Artifact Directory Contract

* **EGM artifact directory:** `.egm`
  * All Elephant-Goldfish Model (EGM) artifacts—including `concepts_brief.md`, `design_draft.md`, finalized design documents (`.egm/designs/*.md`), and stage review reports (`zoom_out_report.md`, `document_review_report.md`, `review_report.md`)—must be written under `.egm/`, never at the repository root.
* **Project-Scoped EGM Bundle Root (`load_global: false`):**
  * **OpenCode:** `EGM bundle root: .opencode/skills/egm-bundle`
  * **Claude Code:** `EGM bundle root: .claude/skills/egm-bundle`
  * Resolve `agents/*.md`, `resources/document_standards/*.md`, `resources/execution_instructions.md`, and `docs/{opencode,claude,harnesses}.md` against the workspace-materialized `egm-bundle` directory above.
  * Resolve `references/` against the active skill directory (`.opencode/skills/egm-<skill>/references/` or `.claude/skills/egm-<skill>/references/`).
* **Repository Architectural Contract:**
  * Read `./AGENTS.md` and `./docs/architecture.md` for the repository's 7 non-negotiable architectural invariants (strict acyclic package DAG, I/O-free target adapters, zero global agent mutation, env-var safety, fail-closed security, Git & store safety, and `.egm/` VCS hygiene).
