package adapters

import (
	"bytes"
	"strings"

	"github.com/bettertux007/agentfile/internal/ir"
)

// FormatManagedRulesBlock renders rules into a deterministic Markdown document
// suitable for OpManagedBlock targets (e.g. GEMINI.md, AGENTS.md, copilot-instructions.md).
// Returns nil if rules is empty.
func FormatManagedRulesBlock(rules []ir.IRRule) []byte {
	if len(rules) == 0 {
		return nil
	}
	var buf bytes.Buffer
	buf.WriteString("# Project Agent Instructions\n\n")
	for i, rl := range rules {
		if i > 0 {
			buf.WriteString("\n---\n\n")
		}
		buf.WriteString("## ")
		buf.WriteString(rl.Name)
		buf.WriteString("\n\n")
		if rl.Description != "" {
			buf.WriteString("*Scope:* ")
			buf.WriteString(rl.Description)
			buf.WriteString("\n")
		}
		if len(rl.Globs) > 0 {
			buf.WriteString("*Applies to:* `")
			buf.WriteString(strings.Join(rl.Globs, "`, `"))
			buf.WriteString("`\n")
		}
		if rl.Description != "" || len(rl.Globs) > 0 {
			buf.WriteString("\n")
		}
		buf.Write(rl.Content)
		if len(rl.Content) > 0 && rl.Content[len(rl.Content)-1] != '\n' {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}
