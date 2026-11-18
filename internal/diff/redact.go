package diff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const redactedMarker = "[REDACTED]"

const secretKeyPattern = `(?:` +
	`[a-z0-9_.-]*(?:api[_-]?key|apikey|secret(?:[_-]?(?:key|access[_-]?key))?|token|password|passwd|passphrase|private[_-]?key|access[_-]?key|credentials?)` +
	`|(?:[a-z0-9_.-]*[._-])?(?:pat|pwd|auth|authorization|x-api-key|x_api_key|(?:aws|openai|anthropic|gemini|github|client|app|signing|webhook|jwt|encryption|master)[_-]?key)` +
	`)`

var (
	rePurePlaceholder    = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}|\$\{env:[A-Za-z_][A-Za-z0-9_]*\}|\{env:[A-Za-z_][A-Za-z0-9_]*\}`)
	reDefaultPlaceholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*):-([^}]*)\}`)

	rePEMPrivateKey = regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`)
	reAuthScheme    = regexp.MustCompile(`(?i)\b(Bearer|Basic)([ \t]+)(\[REDACTED\]|[^\s"',;}\]]+)`)
	reURLUserInfo   = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)([^:@/\s"'<>]+):(\[REDACTED\]|[^@/\s"'<>]+)@`)
	reKnownPrefixes = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_-]{6,}|github_pat_[A-Za-z0-9_]{6,}|sk-(?:ant-|proj-)?[A-Za-z0-9_-]{6,}|A(?:KIA|SIA)[0-9A-Z]{8,}|xox[baprs]-[A-Za-z0-9-]{8,}|AIza[0-9A-Za-z_-]{20,}|(?:pypi|glpat)-[A-Za-z0-9_-]{8,}|npm_[A-Za-z0-9_-]{8,})`)

	reQuotedKV = regexp.MustCompile(
		`(?i)(["'])(` + secretKeyPattern + `)(["']\s*:\s*)(?:"((?:\\.|[^"\\\n])*)"|'((?:\\.|[^'\\\n])*)')`,
	)
	reUnquotedKV = regexp.MustCompile(
		`(?i)(^|[\s,{(\[&?;])((?:--?)?` + secretKeyPattern + `)([ \t]*[:=][ \t]*)(?:"((?:\\.|[^"\\\n])*)"|'((?:\\.|[^'\\\n])*)'|(\[REDACTED\]|[^\s,;}\]&"'\n]+))`,
	)
	reHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)
)

// RedactSecrets masks literal credential values in text while preserving env-var
// placeholders (${VAR}, {env:VAR}, ${env:VAR}, and benign ${VAR:-default}).
// Literal secrets embedded inside ${VAR:-default} defaults are redacted in place.
func RedactSecrets(input string) string {
	if input == "" {
		return ""
	}

	phPrefix := "__AF_PH_"
	for strings.Contains(input, phPrefix) {
		phPrefix += "X_"
	}

	var tokens []string
	addToken := func(val string) string {
		tok := fmt.Sprintf("%s%d__", phPrefix, len(tokens))
		tokens = append(tokens, val)
		return tok
	}

	// Pass 1: Temporarily tokenize pure placeholders so outer scanners preserve them.
	masked := rePurePlaceholder.ReplaceAllStringFunc(input, func(m string) string {
		return addToken(m)
	})

	// Pass 2: Handle ${VAR:-default} by scanning the default sub-value and restoring wrapper.
	masked = reDefaultPlaceholder.ReplaceAllStringFunc(masked, func(m string) string {
		sub := reDefaultPlaceholder.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		varName := sub[1]
		defVal := sub[2]
		redactedDef := redactBody(defVal, phPrefix)
		return addToken("${" + varName + ":-" + redactedDef + "}")
	})

	// Pass 3: Redact secrets in the remaining outer text.
	out := redactBody(masked, phPrefix)

	// Pass 4: Restore tokenized placeholders in reverse order (handles nested tokens).
	for i := len(tokens) - 1; i >= 0; i-- {
		tok := fmt.Sprintf("%s%d__", phPrefix, i)
		out = strings.ReplaceAll(out, tok, tokens[i])
	}

	return out
}

// RedactUnifiedDiff masks literal secrets across unified diff lines while preserving
// leading diff markers (' ', '+', '-', '@@ ', '--- ', '+++ ').
func RedactUnifiedDiff(diffText string) string {
	if diffText == "" {
		return ""
	}
	lines := strings.Split(diffText, "\n")
	oldRem, newRem := 0, 0
	for i, line := range lines {
		if oldRem > 0 || newRem > 0 {
			switch {
			case line == `\ No newline at end of file`:
				continue
			case strings.HasPrefix(line, "-"):
				oldRem--
				lines[i] = "-" + RedactSecrets(line[1:])
				continue
			case strings.HasPrefix(line, "+"):
				newRem--
				lines[i] = "+" + RedactSecrets(line[1:])
				continue
			case strings.HasPrefix(line, " "):
				oldRem--
				newRem--
				lines[i] = " " + RedactSecrets(line[1:])
				continue
			default:
				// Unexpected non-hunk line; reset hunk state and handle normally.
				oldRem, newRem = 0, 0
			}
		}

		if sub := reHunkHeader.FindStringSubmatch(line); len(sub) == 5 {
			oldRem = parseHunkCount(sub[2])
			newRem = parseHunkCount(sub[4])
			continue
		}

		switch {
		case line == "",
			line == `\ No newline at end of file`,
			isDiffFileHeader(line):
			continue
		case strings.HasPrefix(line, "+"),
			strings.HasPrefix(line, "-"),
			strings.HasPrefix(line, " "):
			lines[i] = line[:1] + RedactSecrets(line[1:])
		default:
			lines[i] = RedactSecrets(line)
		}
	}
	return strings.Join(lines, "\n")
}

func isDiffFileHeader(line string) bool {
	return strings.HasPrefix(line, "--- a/") ||
		line == "--- /dev/null" ||
		strings.HasPrefix(line, "+++ b/") ||
		line == "+++ /dev/null"
}

func parseHunkCount(s string) int {
	if s == "" {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ContainsLiteralSecret reports whether s contains a non-placeholder literal secret
// that RedactSecrets would mask.
func ContainsLiteralSecret(s string) bool {
	return RedactSecrets(s) != s
}

func redactBody(s, phPrefix string) string {
	out := rePEMPrivateKey.ReplaceAllString(s, redactedMarker)

	out = reAuthScheme.ReplaceAllStringFunc(out, func(m string) string {
		sub := reAuthScheme.FindStringSubmatch(m)
		if len(sub) != 4 {
			return m
		}
		scheme, ws, tok := sub[1], sub[2], sub[3]
		if tok == redactedMarker || isPurePlaceholderToken(tok, phPrefix) {
			return m
		}
		return scheme + ws + redactedMarker
	})

	out = reURLUserInfo.ReplaceAllStringFunc(out, func(m string) string {
		sub := reURLUserInfo.FindStringSubmatch(m)
		if len(sub) != 4 {
			return m
		}
		scheme, user, pass := sub[1], sub[2], sub[3]
		if pass == redactedMarker || isPurePlaceholderToken(pass, phPrefix) {
			return m
		}
		return scheme + user + ":" + redactedMarker + "@"
	})

	out = reKnownPrefixes.ReplaceAllString(out, redactedMarker)

	out = reQuotedKV.ReplaceAllStringFunc(out, func(m string) string {
		sub := reQuotedKV.FindStringSubmatch(m)
		if len(sub) != 6 {
			return m
		}
		qOpen, key, mid := sub[1], sub[2], sub[3]
		var val, quote string
		if sub[5] != "" || (sub[4] == "" && strings.HasSuffix(m, "'")) {
			val = sub[5]
			quote = "'"
		} else {
			val = sub[4]
			quote = `"`
		}
		if shouldPreserveSecretValue(val, phPrefix, true) {
			return m
		}
		return qOpen + key + mid + quote + redactedMarker + quote
	})

	out = reUnquotedKV.ReplaceAllStringFunc(out, func(m string) string {
		sub := reUnquotedKV.FindStringSubmatch(m)
		if len(sub) != 7 {
			return m
		}
		lead, key, sep := sub[1], sub[2], sub[3]
		switch {
		case sub[6] != "":
			val := sub[6]
			if shouldPreserveSecretValue(val, phPrefix, false) {
				return m
			}
			return lead + key + sep + redactedMarker
		case sub[5] != "" || strings.HasSuffix(m, "'"):
			val := sub[5]
			if shouldPreserveSecretValue(val, phPrefix, true) {
				return m
			}
			return lead + key + sep + "'" + redactedMarker + "'"
		default:
			val := sub[4]
			if shouldPreserveSecretValue(val, phPrefix, true) {
				return m
			}
			return lead + key + sep + `"` + redactedMarker + `"`
		}
	})

	return out
}

func shouldPreserveSecretValue(val, phPrefix string, quoted bool) bool {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" || trimmed == redactedMarker {
		return true
	}
	if isPurePlaceholderToken(trimmed, phPrefix) {
		return true
	}
	if isPreservedAuthHeaderValue(trimmed, phPrefix) {
		return true
	}
	if !quoted {
		if strings.EqualFold(trimmed, "Bearer") || strings.EqualFold(trimmed, "Basic") {
			return true
		}
		switch strings.ToLower(trimmed) {
		case "true", "false", "null", "nil", "undefined":
			return true
		}
	}
	return false
}

func isPreservedAuthHeaderValue(val, phPrefix string) bool {
	lower := strings.ToLower(val)
	var rest string
	switch {
	case strings.HasPrefix(lower, "bearer "):
		rest = strings.TrimSpace(val[len("bearer "):])
	case strings.HasPrefix(lower, "basic "):
		rest = strings.TrimSpace(val[len("basic "):])
	default:
		return false
	}
	return rest == redactedMarker || isPurePlaceholderToken(rest, phPrefix)
}

func isPurePlaceholderToken(s, phPrefix string) bool {
	if !strings.HasPrefix(s, phPrefix) || !strings.HasSuffix(s, "__") {
		return false
	}
	numStr := s[len(phPrefix) : len(s)-2]
	if numStr == "" {
		return false
	}
	_, err := strconv.Atoi(numStr)
	return err == nil
}
