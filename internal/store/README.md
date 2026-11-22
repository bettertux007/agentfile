# `internal/store`

Dependency source parsing, canonical tree hashing, and hardened Git cache/store hydration under `AGENTFILE_CACHE_DIR` or `<UserCacheDir>/agentfile/`.

* `source.go`: Validates local paths (`./...`) and Git specs (`<repo>[#<ref>][//<subpath>]` or `{git, ref, path}`), stripping embedded credentials and rejecting `:` subpath ambiguity and `ext::`.
* `hash.go`: Computes OS-independent SHA-256 tree hashes (`"sha256:<hex>"`) over sorted POSIX paths, rejecting symlinks (`ErrSymlinkNotAllowed`) and enforcing 100 MB / 5,000 entry bounds.
* `fetcher.go` + `cmd_*.go`: Hardened `ExecGitRunner` (`core.hooksPath=/dev/null`, `submodule.recurse=false`, `protocol.ext.allow=never`, `core.autocrlf=false`, `GIT_TERMINAL_PROMPT=0`, process-group timeout kills), bare mirror caching, gitlink (`160000`) rejection, safe `archive/tar` extraction, `.last_used` tracking, and `PruneCache`.
