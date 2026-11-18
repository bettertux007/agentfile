# `internal/vcs`

Worktree-safe `<git-common-dir>/info/exclude` management (`SyncExcludes`, `ClearExcludes`):

* Resolves `<git-common-dir>` via `git rev-parse --git-common-dir --path-format=absolute`.
* Keys exclude sections per worktree (`# BEGIN AGENTFILE:<worktree-id>`) under `<common-dir>/agentfile-exclude.lock`.
* Batch-checks tracked files with `git ls-files -z` (skipping tracked paths with warnings) and prunes blocks for dead/prunable worktrees via `git worktree list --porcelain -z`.
