## Summary

<!-- Describe what this PR changes and why. -->

## Verification Checklist

- [ ] `make fmt` and `make check` pass (`go vet`, `go test -race ./...`, Windows & Linux cross-compile checks)
- [ ] `make coverage` maintains `>= 85%` aggregate statement coverage
- [ ] Preserves architectural invariants in `AGENTS.md` and `docs/architecture.md` (acyclic package DAG, I/O-free adapters, zero `os.ExpandEnv`, fail-closed security, zero global agent mutation)
- [ ] If `docs/` was modified, `make docs-build` passes with zero broken links
