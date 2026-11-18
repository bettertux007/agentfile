package diff

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// MaxDiffFileBytes caps the maximum individual file size eligible for line diffing (1 MiB).
	MaxDiffFileBytes = 1 << 20
	// MaxTotalDiffBytes caps the total rendered diff size emitted by DiffTrees (10 MiB).
	MaxTotalDiffBytes = 10 << 20
	// MaxMyersMatrix caps the N*M product and trace budget for the Myers SES solver.
	MaxMyersMatrix = 4_000_000
)

type opKind uint8

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind opKind
	line string
}

// UnifiedDiff computes a unified line diff between oldBytes and newBytes for relPath.
// It normalizes CRLF line endings to LF, guards against binary NUL bytes, invalid UTF-8,
// and files larger than MaxDiffFileBytes, and falls back gracefully when the Myers edit
// matrix exceeds MaxMyersMatrix.
func UnifiedDiff(relPath string, oldBytes, newBytes []byte, contextLines int) (string, bool, error) {
	if relPath == "" {
		return "", false, fmt.Errorf("diff: relPath must not be empty")
	}
	if contextLines < 0 {
		contextLines = 3
	}

	if bytes.Equal(oldBytes, newBytes) {
		return "", false, nil
	}

	rel := normalizeRelPath(relPath)
	oldHeader := "--- a/" + rel
	if len(oldBytes) == 0 {
		oldHeader = "--- /dev/null"
	}
	newHeader := "+++ b/" + rel
	if len(newBytes) == 0 {
		newHeader = "+++ /dev/null"
	}

	if len(oldBytes) > MaxDiffFileBytes || len(newBytes) > MaxDiffFileBytes {
		return fmt.Sprintf("%s\n%s\nFiles %s differ (exceeds 1 MiB diff limit: old=%d bytes, new=%d bytes)\n",
			oldHeader, newHeader, rel, len(oldBytes), len(newBytes)), true, nil
	}

	if bytes.IndexByte(oldBytes, 0) >= 0 || bytes.IndexByte(newBytes, 0) >= 0 {
		return fmt.Sprintf("%s\n%s\nBinary files %s differ\n", oldHeader, newHeader, rel), true, nil
	}

	if !utf8.Valid(oldBytes) || !utf8.Valid(newBytes) {
		return fmt.Sprintf("%s\n%s\nBinary or non-UTF-8 files %s differ\n", oldHeader, newHeader, rel), true, nil
	}

	oldNorm := strings.ReplaceAll(string(oldBytes), "\r\n", "\n")
	newNorm := strings.ReplaceAll(string(newBytes), "\r\n", "\n")
	if oldNorm == newNorm {
		return "", false, nil
	}

	oldLines, oldNoNL := splitLines(oldNorm)
	newLines, newNoNL := splitLines(newNorm)

	ops := computeEditScript(oldLines, newLines, oldNoNL, newNoNL)
	hunks := buildHunks(ops, contextLines)
	if len(hunks) == 0 {
		return "", false, nil
	}

	var b strings.Builder
	b.WriteString(oldHeader)
	b.WriteByte('\n')
	b.WriteString(newHeader)
	b.WriteByte('\n')

	lastOldIdx := len(oldLines) - 1
	lastNewIdx := len(newLines) - 1

	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldCount, h.newStart, h.newCount)
		oldPos := h.oldIdxStart
		newPos := h.newIdxStart
		for _, op := range h.ops {
			switch op.kind {
			case opEqual:
				b.WriteByte(' ')
				b.WriteString(op.line)
				b.WriteByte('\n')
				if oldPos == lastOldIdx && newPos == lastNewIdx && oldNoNL && newNoNL {
					b.WriteString("\\ No newline at end of file\n")
				}
				oldPos++
				newPos++
			case opDelete:
				b.WriteByte('-')
				b.WriteString(op.line)
				b.WriteByte('\n')
				if oldPos == lastOldIdx && oldNoNL {
					b.WriteString("\\ No newline at end of file\n")
				}
				oldPos++
			case opInsert:
				b.WriteByte('+')
				b.WriteString(op.line)
				b.WriteByte('\n')
				if newPos == lastNewIdx && newNoNL {
					b.WriteString("\\ No newline at end of file\n")
				}
				newPos++
			}
		}
	}

	return b.String(), true, nil
}

// FormatSymlinkDiff formats a unified-style diff for a created, deleted, or modified symlink.
func FormatSymlinkDiff(relPath, oldTarget, newTarget string) string {
	if oldTarget == newTarget {
		return ""
	}
	rel := normalizeRelPath(relPath)
	switch {
	case oldTarget == "":
		return fmt.Sprintf("--- /dev/null\n+++ b/%s\n@@ -0,0 +1,1 @@\n+symlink -> %s\n", rel, newTarget)
	case newTarget == "":
		return fmt.Sprintf("--- a/%s\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-symlink -> %s\n", rel, oldTarget)
	default:
		return fmt.Sprintf("--- a/%s\n+++ b/%s\n@@ -1,1 +1,1 @@\n-symlink -> %s\n+symlink -> %s\n", rel, rel, oldTarget, newTarget)
	}
}

// FormatModeChangeDiff formats a unified-style diff when a workspace entry changes kind
// (for example, between "file", "symlink", and "dir").
func FormatModeChangeDiff(relPath, oldKind, newKind, oldTarget, newTarget string) string {
	rel := normalizeRelPath(relPath)
	oldDesc := "mode: " + oldKind
	if oldKind == "symlink" && oldTarget != "" {
		oldDesc += " -> " + oldTarget
	}
	newDesc := "mode: " + newKind
	if newKind == "symlink" && newTarget != "" {
		newDesc += " -> " + newTarget
	}
	return fmt.Sprintf("--- a/%s\n+++ b/%s\n@@ -1,1 +1,1 @@\n-%s\n+%s\n", rel, rel, oldDesc, newDesc)
}

type treeEntry struct {
	kind     string // "file", "symlink", "dir"
	fullPath string
	target   string
	size     int64
}

// DiffTrees compares two directory trees using os.Lstat without following symlinks.
// It returns the concatenated unified diff (capped at MaxTotalDiffBytes), whether any
// differences exist, and counts of changed files, added lines, and deleted lines.
func DiffTrees(relRoot string, oldDir, newDir string, oldExists, newExists bool, contextLines int) (string, bool, int, int, int, error) {
	if !oldExists && !newExists {
		return "", false, 0, 0, 0, nil
	}

	var oldEntries map[string]treeEntry
	var newEntries map[string]treeEntry
	var err error

	if oldExists {
		oldEntries, err = walkTreeLstat(oldDir)
		if err != nil {
			return "", false, 0, 0, 0, err
		}
	} else {
		oldEntries = map[string]treeEntry{}
	}

	if newExists {
		newEntries, err = walkTreeLstat(newDir)
		if err != nil {
			return "", false, 0, 0, 0, err
		}
	} else {
		newEntries = map[string]treeEntry{}
	}

	pathSet := make(map[string]struct{}, len(oldEntries)+len(newEntries))
	for p := range oldEntries {
		pathSet[p] = struct{}{}
	}
	for p := range newEntries {
		pathSet[p] = struct{}{}
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out strings.Builder
	var filesChanged, additions, deletions int
	truncated := false

	appendChunk := func(chunk string) {
		if chunk == "" {
			return
		}
		filesChanged++
		add, del := countDiffLines(chunk)
		additions += add
		deletions += del

		if truncated {
			return
		}
		if out.Len()+len(chunk) > MaxTotalDiffBytes {
			truncated = true
			fmt.Fprintf(&out, "... diff truncated (exceeded %d MiB total diff limit)\n", MaxTotalDiffBytes/(1<<20))
			return
		}
		out.WriteString(chunk)
	}

	for _, rel := range paths {
		oe, hasOld := oldEntries[rel]
		ne, hasNew := newEntries[rel]
		hdrPath := joinRel(relRoot, rel)

		switch {
		case !hasOld && hasNew:
			if ne.kind == "dir" {
				continue
			}
			if ne.kind == "symlink" {
				appendChunk(FormatSymlinkDiff(hdrPath, "", ne.target))
				continue
			}
			chunk, ch, err := diffRegularFiles(hdrPath, nil, &ne, contextLines)
			if err != nil {
				return "", false, 0, 0, 0, err
			}
			if !ch {
				// Empty newly created regular file.
				chunk = fmt.Sprintf("--- /dev/null\n+++ b/%s\n", hdrPath)
			}
			appendChunk(chunk)

		case hasOld && !hasNew:
			if oe.kind == "dir" {
				continue
			}
			if oe.kind == "symlink" {
				appendChunk(FormatSymlinkDiff(hdrPath, oe.target, ""))
				continue
			}
			chunk, ch, err := diffRegularFiles(hdrPath, &oe, nil, contextLines)
			if err != nil {
				return "", false, 0, 0, 0, err
			}
			if !ch {
				// Empty deleted regular file.
				chunk = fmt.Sprintf("--- a/%s\n+++ /dev/null\n", hdrPath)
			}
			appendChunk(chunk)

		case hasOld && hasNew:
			if oe.kind != ne.kind {
				appendChunk(FormatModeChangeDiff(hdrPath, oe.kind, ne.kind, oe.target, ne.target))
				continue
			}
			if oe.kind == "dir" {
				continue
			}
			if oe.kind == "symlink" {
				if oe.target != ne.target {
					appendChunk(FormatSymlinkDiff(hdrPath, oe.target, ne.target))
				}
				continue
			}
			chunk, ch, err := diffRegularFiles(hdrPath, &oe, &ne, contextLines)
			if err != nil {
				return "", false, 0, 0, 0, err
			}
			if ch {
				appendChunk(chunk)
			}
		}
	}

	return out.String(), filesChanged > 0, filesChanged, additions, deletions, nil
}

func diffRegularFiles(hdrPath string, oldEntry, newEntry *treeEntry, contextLines int) (string, bool, error) {
	var oldSize, newSize int64
	if oldEntry != nil {
		oldSize = oldEntry.size
	}
	if newEntry != nil {
		newSize = newEntry.size
	}

	if oldSize > MaxDiffFileBytes || newSize > MaxDiffFileBytes {
		if oldEntry != nil && newEntry != nil && oldSize == newSize {
			equal, err := filesEqualStream(oldEntry.fullPath, newEntry.fullPath)
			if err != nil {
				return "", false, err
			}
			if equal {
				return "", false, nil
			}
		}
		oldHeader := "--- a/" + hdrPath
		if oldEntry == nil || oldSize == 0 {
			oldHeader = "--- /dev/null"
		}
		newHeader := "+++ b/" + hdrPath
		if newEntry == nil || newSize == 0 {
			newHeader = "+++ /dev/null"
		}
		return fmt.Sprintf("%s\n%s\nFiles %s differ (exceeds 1 MiB diff limit: old=%d bytes, new=%d bytes)\n",
			oldHeader, newHeader, hdrPath, oldSize, newSize), true, nil
	}

	var oldBytes, newBytes []byte
	var err error
	if oldEntry != nil {
		oldBytes, err = os.ReadFile(oldEntry.fullPath)
		if err != nil {
			return "", false, err
		}
	}
	if newEntry != nil {
		newBytes, err = os.ReadFile(newEntry.fullPath)
		if err != nil {
			return "", false, err
		}
	}

	return UnifiedDiff(hdrPath, oldBytes, newBytes, contextLines)
}

func filesEqualStream(pathA, pathB string) (bool, error) {
	fa, err := os.Open(pathA)
	if err != nil {
		return false, err
	}
	defer fa.Close()

	fb, err := os.Open(pathB)
	if err != nil {
		return false, err
	}
	defer fb.Close()

	bufA := make([]byte, 64*1024)
	bufB := make([]byte, 64*1024)
	for {
		nA, errA := io.ReadFull(fa, bufA)
		nB, errB := io.ReadFull(fb, bufB)
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return false, nil
		}
		if errA == io.EOF && errB == io.EOF {
			return true, nil
		}
		if errA == io.ErrUnexpectedEOF && errB == io.ErrUnexpectedEOF {
			return true, nil
		}
		if errA != nil && errA != io.ErrUnexpectedEOF {
			return false, errA
		}
		if errB != nil && errB != io.ErrUnexpectedEOF {
			return false, errB
		}
		if (errA == io.ErrUnexpectedEOF) != (errB == io.ErrUnexpectedEOF) {
			return false, nil
		}
	}
}

func walkTreeLstat(root string) (map[string]treeEntry, error) {
	entries := make(map[string]treeEntry)

	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(root)
		if err != nil {
			return nil, err
		}
		entries[""] = treeEntry{kind: "symlink", fullPath: root, target: target}
		return entries, nil
	}
	if !info.IsDir() {
		if info.Mode().IsRegular() {
			entries[""] = treeEntry{kind: "file", fullPath: root, size: info.Size()}
		}
		return entries, nil
	}

	var walkDir func(absDir, relDir string) error
	walkDir = func(absDir, relDir string) error {
		dirEntries, err := os.ReadDir(absDir)
		if err != nil {
			return err
		}
		sort.Slice(dirEntries, func(i, j int) bool {
			return dirEntries[i].Name() < dirEntries[j].Name()
		})
		for _, de := range dirEntries {
			name := de.Name()
			absChild := filepath.Join(absDir, name)
			relChild := name
			if relDir != "" {
				relChild = relDir + "/" + name
			}

			linfo, err := os.Lstat(absChild)
			if err != nil {
				return err
			}
			if linfo.Mode()&fs.ModeSymlink != 0 {
				target, err := os.Readlink(absChild)
				if err != nil {
					return err
				}
				entries[relChild] = treeEntry{kind: "symlink", fullPath: absChild, target: target}
				continue
			}
			if linfo.IsDir() {
				entries[relChild] = treeEntry{kind: "dir", fullPath: absChild}
				if err := walkDir(absChild, relChild); err != nil {
					return err
				}
				continue
			}
			if linfo.Mode().IsRegular() {
				entries[relChild] = treeEntry{kind: "file", fullPath: absChild, size: linfo.Size()}
			}
		}
		return nil
	}

	if err := walkDir(root, ""); err != nil {
		return nil, err
	}
	return entries, nil
}

func normalizeRelPath(p string) string {
	s := filepath.ToSlash(p)
	s = strings.TrimPrefix(s, "./")
	if s == "" {
		return "."
	}
	return s
}

func joinRel(relRoot, rel string) string {
	r := strings.TrimSuffix(normalizeRelPath(relRoot), "/")
	if r == "" || r == "." {
		if rel == "" {
			return "."
		}
		return rel
	}
	if rel == "" {
		return r
	}
	return r + "/" + rel
}

func countDiffLines(chunk string) (additions, deletions int) {
	for _, line := range strings.Split(chunk, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ") {
			additions++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- ") {
			deletions++
		}
	}
	return additions, deletions
}

func splitLines(s string) ([]string, bool) {
	if s == "" {
		return nil, false
	}
	if strings.HasSuffix(s, "\n") {
		return strings.Split(s[:len(s)-1], "\n"), false
	}
	return strings.Split(s, "\n"), true
}

func computeEditScript(oldLines, newLines []string, oldNoNL, newNoNL bool) []diffOp {
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	midOld := oldLines[prefix : len(oldLines)-suffix]
	midNew := newLines[prefix : len(newLines)-suffix]

	// Handle trailing-newline-only changes on the final line.
	if len(midOld) == 0 && len(midNew) == 0 && oldNoNL != newNoNL && len(oldLines) > 0 {
		ops := make([]diffOp, 0, len(oldLines)+1)
		for i := 0; i < len(oldLines)-1; i++ {
			ops = append(ops, diffOp{kind: opEqual, line: oldLines[i]})
		}
		last := oldLines[len(oldLines)-1]
		ops = append(ops, diffOp{kind: opDelete, line: last})
		ops = append(ops, diffOp{kind: opInsert, line: last})
		return ops
	}

	midOps := myersSES(midOld, midNew)

	ops := make([]diffOp, 0, prefix+len(midOps)+suffix)
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{kind: opEqual, line: oldLines[i]})
	}
	ops = append(ops, midOps...)
	for i := len(oldLines) - suffix; i < len(oldLines); i++ {
		ops = append(ops, diffOp{kind: opEqual, line: oldLines[i]})
	}
	return ops
}

func myersSES(a, b []string) []diffOp {
	n := len(a)
	m := len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n == 0 {
		ops := make([]diffOp, m)
		for i, line := range b {
			ops[i] = diffOp{kind: opInsert, line: line}
		}
		return ops
	}
	if m == 0 {
		ops := make([]diffOp, n)
		for i, line := range a {
			ops[i] = diffOp{kind: opDelete, line: line}
		}
		return ops
	}
	if int64(n)*int64(m) > MaxMyersMatrix {
		return fallbackReplace(a, b)
	}

	maxD := n + m
	offset := maxD
	v := make([]int, 2*maxD+1)
	trace := make([][]int, 0, min(maxD+1, 2048))
	traceBudget := 0

	solvedD := -1
	for d := 0; d <= maxD; d++ {
		rowLen := 2*d + 1
		traceBudget += rowLen
		if traceBudget > MaxMyersMatrix {
			return fallbackReplace(a, b)
		}
		snap := make([]int, rowLen)
		copy(snap, v[offset-d:offset+d+1])
		trace = append(trace, snap)

		for k := -d; k <= d; k += 2 {
			var x int
			if d == 0 {
				x = 0
			} else if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				solvedD = d
				break
			}
		}
		if solvedD >= 0 {
			break
		}
	}

	if solvedD < 0 {
		return fallbackReplace(a, b)
	}

	// Backtrack from (n, m) down to (0, 0).
	revOps := make([]diffOp, 0, n+m)
	x, y := n, m
	for d := solvedD; d > 0; d-- {
		prevV := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && prevV[k-1+d] < prevV[k+1+d]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := prevV[prevK+d]
		prevY := prevX - prevK

		xStart := prevX
		if prevK == k-1 {
			xStart = prevX + 1
		}
		yStart := xStart - k

		for x > xStart && y > yStart {
			x--
			y--
			revOps = append(revOps, diffOp{kind: opEqual, line: a[x]})
		}
		if xStart == prevX {
			revOps = append(revOps, diffOp{kind: opInsert, line: b[prevY]})
		} else {
			revOps = append(revOps, diffOp{kind: opDelete, line: a[prevX]})
		}
		x, y = prevX, prevY
	}
	for x > 0 && y > 0 {
		x--
		y--
		revOps = append(revOps, diffOp{kind: opEqual, line: a[x]})
	}

	// Reverse in place.
	for i, j := 0, len(revOps)-1; i < j; i, j = i+1, j-1 {
		revOps[i], revOps[j] = revOps[j], revOps[i]
	}
	return revOps
}

func fallbackReplace(a, b []string) []diffOp {
	ops := make([]diffOp, 0, len(a)+len(b))
	for _, line := range a {
		ops = append(ops, diffOp{kind: opDelete, line: line})
	}
	for _, line := range b {
		ops = append(ops, diffOp{kind: opInsert, line: line})
	}
	return ops
}

type hunk struct {
	oldStart    int
	oldCount    int
	newStart    int
	newCount    int
	oldIdxStart int
	newIdxStart int
	ops         []diffOp
}

func buildHunks(ops []diffOp, contextLines int) []hunk {
	// Identify contiguous clusters of non-equal ops.
	type span struct{ start, end int }
	var clusters []span
	for i := 0; i < len(ops); {
		if ops[i].kind == opEqual {
			i++
			continue
		}
		j := i
		for j+1 < len(ops) && ops[j+1].kind != opEqual {
			j++
		}
		clusters = append(clusters, span{start: i, end: j})
		i = j + 1
	}
	if len(clusters) == 0 {
		return nil
	}

	// Merge clusters whose context windows touch or overlap.
	var windows []span
	curStart := max(0, clusters[0].start-contextLines)
	curEnd := min(len(ops)-1, clusters[0].end+contextLines)
	for _, c := range clusters[1:] {
		nextStart := max(0, c.start-contextLines)
		nextEnd := min(len(ops)-1, c.end+contextLines)
		if nextStart <= curEnd+1 {
			if nextEnd > curEnd {
				curEnd = nextEnd
			}
		} else {
			windows = append(windows, span{start: curStart, end: curEnd})
			curStart, curEnd = nextStart, nextEnd
		}
	}
	windows = append(windows, span{start: curStart, end: curEnd})

	// Precompute cumulative old/new line indices before each op index.
	oldBefore := make([]int, len(ops)+1)
	newBefore := make([]int, len(ops)+1)
	for i, op := range ops {
		oldBefore[i+1] = oldBefore[i]
		newBefore[i+1] = newBefore[i]
		switch op.kind {
		case opEqual:
			oldBefore[i+1]++
			newBefore[i+1]++
		case opDelete:
			oldBefore[i+1]++
		case opInsert:
			newBefore[i+1]++
		}
	}

	hunks := make([]hunk, 0, len(windows))
	for _, w := range windows {
		slice := ops[w.start : w.end+1]
		oldPos := oldBefore[w.start]
		newPos := newBefore[w.start]
		oldCount := oldBefore[w.end+1] - oldPos
		newCount := newBefore[w.end+1] - newPos

		oldStart := oldPos + 1
		if oldCount == 0 {
			oldStart = oldPos
		}
		newStart := newPos + 1
		if newCount == 0 {
			newStart = newPos
		}

		hunks = append(hunks, hunk{
			oldStart:    oldStart,
			oldCount:    oldCount,
			newStart:    newStart,
			newCount:    newCount,
			oldIdxStart: oldPos,
			newIdxStart: newPos,
			ops:         slice,
		})
	}
	return hunks
}
