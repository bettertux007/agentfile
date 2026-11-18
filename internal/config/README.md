# `internal/config`

Loads optional machine-local preferences from `.agentfile/config.yaml` (`version: 1`) and merges CLI flag overrides (`--link-mode`, `--copy`, `--on-conflict`, `--git-exclude`, `--force`) with TTY/CI detection for `on_conflict` defaults.
