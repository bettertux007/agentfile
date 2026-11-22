# `internal/config`

Resolves 4-layer machine-local configuration (`default` -> `user_config` -> `local_config` -> `CLI`) across:

1. Built-in defaults (`link_mode: copy`, `git_exclude: true`, `load_global: true`, `on_conflict: ask` on interactive non-CI TTYs or `fail` otherwise)
2. User-global personal defaults (`$XDG_CONFIG_HOME/agentfile/config.yaml`, `$HOME/.config/agentfile/config.yaml`, `os.UserConfigDir()` fallback, or explicit `AGENTFILE_CONFIG_FILE`; skipped in CI or when `AGENTFILE_NO_USER_CONFIG=1`)
3. Repo-local config (`.agentfile/config.yaml`, `version: 1`)
4. CLI flag overrides (`--link-mode`, `--copy`, `--on-conflict`, `--git-exclude`, `--force`)

Also resolves per-target `load_global` with the non-downgrade precedence chain `CLI > Agentfile(false) > local_config > Agentfile(true) > user_config > default(true)` and records field-level provenance (`FieldSources`) and warnings (`Warnings`).
