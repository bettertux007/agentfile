package engine

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/bettertux007/agentfile/internal/spec"
)

const (
	mdBeginPrefix = "<!-- BEGIN AGENTFILE:v1 "
	mdBeginSuffix = " -->"
	mdEndMarker   = "<!-- END AGENTFILE:v1 -->"
)

// MergeMarkdownBlock inserts or replaces the managed block inside existing Markdown content,
// ignoring markers inside fenced code blocks (``` or ~~~) and preserving dominant line endings.
func MergeMarkdownBlock(
	existingBytes []byte,
	exists bool,
	innerContent []byte,
	prev *ArtifactRecord,
	resolveConflict ConflictResolver,
) ([]byte, ArtifactRecord, bool, error) {
	created := !exists || len(bytes.TrimSpace(existingBytes)) == 0
	eol := detectEOL(existingBytes)
	inner := normalizeEOL(strings.TrimRight(string(innerContent), "\r\n"), "\n")
	innerHash := hashBytes([]byte(inner))

	block := fmt.Sprintf("%s%s%s\n%s\n%s", mdBeginPrefix, innerHash, mdBeginSuffix, inner, mdEndMarker)

	if created {
		out := normalizeEOL(block+"\n", eol)
		rec := ArtifactRecord{
			ArtifactType:       TypeMarkdownBlock,
			SHA256:             hashBytes([]byte(out)),
			BlockHash:          innerHash,
			CreatedByAgentfile: true,
		}
		return []byte(out), rec, false, nil
	}

	rawNorm := normalizeEOL(string(existingBytes), "\n")
	startLine, endLine, diskInner, found, err := findManagedBlock(rawNorm)
	if err != nil {
		return nil, ArtifactRecord{}, false, err
	}

	needsBackup := false
	if found {
		diskHash := hashBytes([]byte(diskInner))
		if prev != nil && prev.BlockHash != "" && diskHash != prev.BlockHash && diskHash != innerHash {
			overwrite, backup, cerr := resolveConflict("managed Markdown block was modified on disk")
			if cerr != nil {
				return nil, ArtifactRecord{}, false, cerr
			}
			needsBackup = backup
			if !overwrite {
				rec := *prev
				return existingBytes, rec, false, nil
			}
		} else if prev == nil && diskHash != innerHash {
			overwrite, backup, cerr := resolveConflict("untracked AGENTFILE Markdown block already exists")
			if cerr != nil {
				return nil, ArtifactRecord{}, false, cerr
			}
			needsBackup = backup
			if !overwrite {
				return existingBytes, ArtifactRecord{ArtifactType: TypeMarkdownBlock, BlockHash: diskHash}, false, nil
			}
		}

		lines := strings.Split(rawNorm, "\n")
		var rebuilt []string
		rebuilt = append(rebuilt, lines[:startLine]...)
		rebuilt = append(rebuilt, strings.Split(block, "\n")...)
		rebuilt = append(rebuilt, lines[endLine+1:]...)
		joined := strings.Join(rebuilt, "\n")
		if !strings.HasSuffix(joined, "\n") {
			joined += "\n"
		}
		out := normalizeEOL(joined, eol)
		rec := ArtifactRecord{
			ArtifactType:       TypeMarkdownBlock,
			SHA256:             hashBytes([]byte(out)),
			BlockHash:          innerHash,
			CreatedByAgentfile: prev != nil && prev.CreatedByAgentfile,
		}
		return []byte(out), rec, needsBackup, nil
	}

	// No existing block: append cleanly at the end.
	trimmed := strings.TrimRight(rawNorm, "\n")
	var joined string
	if trimmed == "" {
		joined = block + "\n"
	} else {
		joined = trimmed + "\n\n" + block + "\n"
	}
	out := normalizeEOL(joined, eol)
	rec := ArtifactRecord{
		ArtifactType:       TypeMarkdownBlock,
		SHA256:             hashBytes([]byte(out)),
		BlockHash:          innerHash,
		CreatedByAgentfile: prev != nil && prev.CreatedByAgentfile,
	}
	return []byte(out), rec, false, nil
}

// RemoveMarkdownBlock strips the managed block, returning deleteFile=true if created by agentfile and now empty.
func RemoveMarkdownBlock(existingBytes []byte, prev ArtifactRecord) ([]byte, bool, error) {
	if len(bytes.TrimSpace(existingBytes)) == 0 {
		return nil, prev.CreatedByAgentfile, nil
	}
	eol := detectEOL(existingBytes)
	rawNorm := normalizeEOL(string(existingBytes), "\n")
	startLine, endLine, _, found, err := findManagedBlock(rawNorm)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return existingBytes, false, nil
	}

	lines := strings.Split(rawNorm, "\n")
	var remaining []string
	remaining = append(remaining, lines[:startLine]...)
	remaining = append(remaining, lines[endLine+1:]...)
	joined := strings.TrimSpace(strings.Join(remaining, "\n"))
	if joined == "" && prev.CreatedByAgentfile {
		return nil, true, nil
	}
	if joined == "" {
		return []byte(""), false, nil
	}
	out := normalizeEOL(joined+"\n", eol)
	return []byte(out), false, nil
}

func findManagedBlock(normContent string) (startLine, endLine int, inner string, found bool, err error) {
	lines := strings.Split(normContent, "\n")
	inFence := false
	var fenceChar byte
	var fenceLen int

	startLine = -1
	endLine = -1
	beginCount := 0
	endCount := 0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fc, fl, ok := parseFenceLine(line); ok {
			if !inFence {
				inFence = true
				fenceChar = fc
				fenceLen = fl
			} else if fc == fenceChar && fl >= fenceLen && strings.TrimSpace(trimmed[fl:]) == "" {
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}

		if strings.HasPrefix(trimmed, mdBeginPrefix) && strings.HasSuffix(trimmed, mdBeginSuffix) {
			beginCount++
			if beginCount > 1 {
				return 0, 0, "", false, spec.NewPolicyError("duplicate AGENTFILE BEGIN marker at line %d", i+1)
			}
			startLine = i
		} else if trimmed == mdEndMarker {
			endCount++
			if beginCount == 0 || endCount > 1 {
				return 0, 0, "", false, spec.NewPolicyError("unexpected AGENTFILE END marker at line %d", i+1)
			}
			endLine = i
		}
	}

	if beginCount != endCount {
		return 0, 0, "", false, spec.NewPolicyError("unclosed AGENTFILE Markdown block marker")
	}
	if beginCount == 0 {
		return 0, 0, "", false, nil
	}
	inner = strings.Join(lines[startLine+1:endLine], "\n")
	return startLine, endLine, inner, true, nil
}

func parseFenceLine(line string) (byte, int, bool) {
	// CommonMark allows up to 3 leading spaces before a fence.
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' && spaces < 4 {
		spaces++
	}
	if spaces == 4 || spaces >= len(line) {
		return 0, 0, false
	}
	ch := line[spaces]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	count := 0
	for i := spaces; i < len(line) && line[i] == ch; i++ {
		count++
	}
	if count < 3 {
		return 0, 0, false
	}
	return ch, count, true
}

func detectEOL(b []byte) string {
	if bytes.Contains(b, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

func normalizeEOL(s, eol string) string {
	lf := strings.ReplaceAll(s, "\r\n", "\n")
	if eol == "\n" {
		return lf
	}
	return strings.ReplaceAll(lf, "\n", "\r\n")
}
