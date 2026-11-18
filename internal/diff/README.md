# `internal/diff`

Zero-dependency pure-Go unified diff and placeholder-safe secret redaction package:

- `UnifiedDiff` / `DiffTrees`: Myers shortest-edit-script line diffing with CRLF normalization, `MaxDiffFileBytes` (1 MiB), `MaxTotalDiffBytes` (10 MiB), `MaxMyersMatrix` (4,000,000) fallback, binary/non-UTF-8 guards, and `os.Lstat`-only directory tree walks (never following symlinks; `FormatSymlinkDiff` / `FormatModeChangeDiff` for symlink and kind transitions).
- `RedactSecrets` / `RedactUnifiedDiff` / `ContainsLiteralSecret`: Multi-pass secret masking across JSON/YAML/env/CLI key-value pairs, `Bearer`/`Basic` auth headers, URL userinfo credentials, and known token prefixes (`ghp_`, `github_pat_`, `sk-`, `AKIA`, etc.) while preserving `${VAR}`, `{env:VAR}`, `${env:VAR}`, and benign `${VAR:-default}` placeholders and redacting literal secrets inside `${VAR:-default}` defaults.
