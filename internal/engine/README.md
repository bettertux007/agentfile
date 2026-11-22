# `internal/engine`

Two-phase Plan-then-Commit execution engine, target-namespaced ownership state, non-destructive JSONC/Markdown patchers, and deterministic rollback recovery. Must **never** import `internal/adapters/*`.

* `state.go`: `.agentfile/state.json` (`version: 1`) partitioned by `targets.<name>` (`artifacts` and `owned_dirs`).
* `jsonc.go`: `hujson` comment-preserving 3-way patching (`MergeJSONC`, `RemoveJSONC`) for named map keys and exact string arrays using `MarshalJSONNoEscape`.
* `markdown.go`: CommonMark fence-aware, EOL-preserving `<!-- BEGIN AGENTFILE:v1 ... -->` block splicing and removal.
* `planner.go`: Probe collection (`CollectProbes`), case-normalized collision checks, 3-way diffing, post-merge `PolicyValidator` checks, and `on_conflict` resolution (`BuildPlan`).
* `executor.go`: Atomic staging, `.agentfile/commit-journal.json` rollback (`Commit`, `RecoverJournal`), Windows transient rename backoff, `dir <-> symlink` replacement, `Clean`, and `vcs.SyncExcludes` integration.
