# `internal/lock`

Cross-platform exclusive advisory file locking (`Acquire(path) (io.Closer, error)`) using `flock` (`//go:build unix`) and `LockFileEx` (`//go:build windows`), keeping lockfiles on disk across `Close()` to avoid inode-replacement races. Reused by `internal/store`, `internal/vcs`, and `internal/engine`.
