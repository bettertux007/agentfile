---
name: release-check
description: Pre-release and pre-PR verification workflow for agentfile covering formatting, linting, race tests, cross-compilation, coverage, self-audit, and VitePress docs build.
---

# Skill: Pre-Release & PR Verification (`release-check`)

Use this skill before cutting a release tag or opening/merging a pull request in `agentfile`.

## Verification Sequence

Run each step in order from the repository root and stop on any non-zero exit code:

1. **Format & Lint Verification:**
   ```sh
   make fmt
   make lint
   ```
   Verify `go vet ./...` passes and `gofmt -l .` produces zero unformatted Go files.

2. **Full Test Suite (Race Detector) & Cross-Platform Build Check:**
   ```sh
   make check
   ```
   Verifies `lint`, `go test -race ./...`, cross-compilation for `GOOS=windows` and `GOOS=linux`, and offline strict self-audit (`go run ./cmd/agentfile audit --strict --no-store`).

3. **Aggregate Statement Coverage Gate (`>= 85.0%`):**
   ```sh
   make coverage
   ```
   Verify total statement coverage reported by `go tool cover -func=coverage.out` is at least `85.0%`.

4. **Hydrated Dogfooding & Lockfile Integrity Check:**
   ```sh
   make build
   ./agentfile sync
   ./agentfile status --check
   ./agentfile audit --strict
   ```
   Verifies that `./Agentfile.yaml` and `./Agentfile.lock` are synchronized, all remote dependencies in `<CacheRoot>/extracted/` match their locked SHA-256 content hashes, and zero errors or warnings are emitted under `--strict`.

5. **VitePress Documentation Build:**
   ```sh
   make docs-build
   ```
   Verifies the documentation site under `docs/` builds cleanly with zero broken links.

6. **Clean Working Tree Verification:**
   ```sh
   git status --short
   ```
   Verify no untracked build artifacts or unexpected modifications remain in the working tree.
