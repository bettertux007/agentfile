### Go Testing Hermeticity & Cross-Platform Portability (`**/*_test.go`)

Every unit and integration test in `agentfile` must remain hermetic across Linux (`ubuntu-latest`), macOS (`macos-latest`), and Windows (`windows-latest`):

1. **Workspace & Cache Isolation:**
   * Always create workspace roots with `t.TempDir()`.
   * Always isolate the content-addressed store with `t.Setenv("AGENTFILE_CACHE_DIR", t.TempDir())`.
2. **User-Global Config & CI Environment Isolation:**
   * Unless testing user-global config discovery (`internal/config`), isolate from host `~/.config/agentfile/config.yaml` with `t.Setenv("AGENTFILE_NO_USER_CONFIG", "1")` (or clear `GITHUB_ACTIONS` and `CI` via `t.Setenv("GITHUB_ACTIONS", "")` and `t.Setenv("CI", "")` when testing default user-config discovery locally and in GitHub Actions).
3. **Hermetic Local Git Repositories (Zero Network Calls in Tests):**
   * Never fetch from remote network Git hosts in unit or integration tests.
   * Create local bare Git repositories inside `t.TempDir()`, set `t.Setenv("AGENTFILE_ALLOW_FILE_GIT", "1")`, and pass `-c core.autocrlf=false` when committing test fixtures so SHA-256 tree hashes match across Windows and Unix.
4. **Windows Path & Executable Portability:**
   * Normalize relative path assertions with `filepath.ToSlash(...)`.
   * When creating mock target binaries for `internal/runner` or version-probe tests on Windows (`runtime.GOOS == "windows"`), write a `.bat` wrapper alongside the Unix shell script (`chmod 0755`).
