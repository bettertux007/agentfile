package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/tailscale/hujson"
)

// ConflictResolver handles a collision on an unowned or user-modified key.
// It returns true to overwrite/apply, or an error (e.g. policy abort).
type ConflictResolver func(detail string) (overwrite bool, backup bool, err error)

// MergeJSONC applies a 3-way comment-preserving JSONC merge for named map entries and managed string arrays.
func MergeJSONC(
	existingBytes []byte,
	exists bool,
	mergeSpec *adapters.JSONMergeSpec,
	prev *ArtifactRecord,
	resolveConflict ConflictResolver,
) ([]byte, ArtifactRecord, bool, error) {
	created := !exists || len(bytes.TrimSpace(existingBytes)) == 0
	var root hujson.Value
	var err error
	if created {
		root, err = hujson.Parse([]byte("{}\n"))
	} else {
		root, err = hujson.Parse(existingBytes)
	}
	if err != nil {
		return nil, ArtifactRecord{}, false, spec.NewPolicyError("invalid JSONC syntax: %v", err)
	}

	needsBackup := false
	mutated := created
	nextRec := ArtifactRecord{
		ArtifactType:       TypeJSONCPatch,
		JSONPointerHashes:  make(map[string]string),
		ArrayEntries:       make(map[string][]string),
		CreatedByAgentfile: created || (prev != nil && prev.CreatedByAgentfile),
	}

	if mergeSpec.SchemaURL != "" {
		if _, ok, err := getAtPointer(&root, "/$schema"); err == nil && !ok {
			if err := setAtPointer(&root, "/$schema", mergeSpec.SchemaURL); err != nil {
				return nil, ArtifactRecord{}, false, err
			}
			mutated = true
		}
	}

	// 1. Prune previously owned map pointers no longer desired.
	if prev != nil {
		for _, oldPtr := range prev.JSONPointers {
			if _, stillWanted := mergeSpec.MapEntries[oldPtr]; stillWanted {
				continue
			}
			curRaw, ok, err := getAtPointer(&root, oldPtr)
			if err != nil {
				return nil, ArtifactRecord{}, false, err
			}
			if !ok {
				continue
			}
			curHash := hashCanonicalJSON(curRaw)
			if prev.JSONPointerHashes[oldPtr] != "" && curHash != prev.JSONPointerHashes[oldPtr] {
				overwrite, backup, err := resolveConflict(fmt.Sprintf("owned JSON key %s was modified on disk", oldPtr))
				if err != nil {
					return nil, ArtifactRecord{}, false, err
				}
				if backup {
					needsBackup = true
				}
				if !overwrite {
					// Preserve user-modified value by omitting from removal.
					continue
				}
			}
			if err := removeAtPointer(&root, oldPtr); err != nil {
				return nil, ArtifactRecord{}, false, err
			}
			mutated = true
		}
	}

	// 2. Apply desired named map entries in deterministic order.
	mapPtrs := make([]string, 0, len(mergeSpec.MapEntries))
	for ptr := range mergeSpec.MapEntries {
		mapPtrs = append(mapPtrs, ptr)
	}
	sort.Strings(mapPtrs)

	for _, ptr := range mapPtrs {
		desiredVal := mergeSpec.MapEntries[ptr]
		desiredBytes, err := adapters.MarshalJSONNoEscape(desiredVal)
		if err != nil {
			return nil, ArtifactRecord{}, false, err
		}
		desiredHash := hashCanonicalJSON(desiredBytes)

		curRaw, existsOnDisk, err := getAtPointer(&root, ptr)
		if err != nil {
			return nil, ArtifactRecord{}, false, err
		}
		if existsOnDisk {
			curHash := hashCanonicalJSON(curRaw)
			if curHash == desiredHash {
				nextRec.JSONPointers = append(nextRec.JSONPointers, ptr)
				nextRec.JSONPointerHashes[ptr] = desiredHash
				continue
			}
			var prevHash string
			if prev != nil {
				prevHash = prev.JSONPointerHashes[ptr]
			}
			// Conflict if unowned (prevHash == "") or user edited since last write (curHash != prevHash).
			if prevHash == "" || curHash != prevHash {
				reason := fmt.Sprintf("JSON key %s already exists with different value", ptr)
				if prevHash != "" {
					reason = fmt.Sprintf("owned JSON key %s was modified on disk", ptr)
				}
				overwrite, backup, err := resolveConflict(reason)
				if err != nil {
					return nil, ArtifactRecord{}, false, err
				}
				if backup {
					needsBackup = true
				}
				if !overwrite {
					continue
				}
			}
		}

		if err := setAtPointer(&root, ptr, desiredVal); err != nil {
			return nil, ArtifactRecord{}, false, err
		}
		mutated = true
		nextRec.JSONPointers = append(nextRec.JSONPointers, ptr)
		nextRec.JSONPointerHashes[ptr] = desiredHash
	}

	// 3. Reconcile managed exact-string arrays (preserving unowned user/runtime elements).
	arrPtrSet := make(map[string]bool)
	for ptr := range mergeSpec.ArrayEntries {
		arrPtrSet[ptr] = true
	}
	if prev != nil {
		for ptr := range prev.ArrayEntries {
			arrPtrSet[ptr] = true
		}
	}
	arrPtrs := make([]string, 0, len(arrPtrSet))
	for ptr := range arrPtrSet {
		arrPtrs = append(arrPtrs, ptr)
	}
	sort.Strings(arrPtrs)

	for _, ptr := range arrPtrs {
		desired := mergeSpec.ArrayEntries[ptr]
		var prevOwned []string
		if prev != nil {
			prevOwned = prev.ArrayEntries[ptr]
		}
		curRaw, existsOnDisk, err := getAtPointer(&root, ptr)
		if err != nil {
			return nil, ArtifactRecord{}, false, err
		}

		var curList []string
		if existsOnDisk {
			if err := json.Unmarshal(curRaw, &curList); err != nil {
				return nil, ArtifactRecord{}, false, spec.NewPolicyError("expected string array at %s: %v", ptr, err)
			}
		}

		merged := mergeStringArray(curList, prevOwned, desired)
		if len(merged) == 0 && len(desired) == 0 {
			if existsOnDisk {
				if err := removeAtPointer(&root, ptr); err != nil {
					return nil, ArtifactRecord{}, false, err
				}
				mutated = true
			}
			continue
		}
		if !existsOnDisk || !stringSlicesEqual(curList, merged) {
			if err := setAtPointer(&root, ptr, merged); err != nil {
				return nil, ArtifactRecord{}, false, err
			}
			mutated = true
		}
		if len(desired) > 0 {
			nextRec.ArrayEntries[ptr] = append([]string(nil), desired...)
		}
	}

	if exists && !created && !mutated {
		out := bytes.Clone(existingBytes)
		nextRec.SHA256 = hashBytes(out)
		return out, nextRec, needsBackup, nil
	}

	out, err := formatJSONCOutput(&root)
	if err != nil {
		return nil, ArtifactRecord{}, false, err
	}
	nextRec.SHA256 = hashBytes(out)
	return out, nextRec, needsBackup, nil
}

// RemoveJSONC strips agentfile-owned map keys and array entries from an existing JSONC file.
// Returns deleteFile=true if the file was created by agentfile and only empty objects remain.
func RemoveJSONC(existingBytes []byte, prev ArtifactRecord) ([]byte, bool, error) {
	if len(bytes.TrimSpace(existingBytes)) == 0 {
		return nil, prev.CreatedByAgentfile, nil
	}
	root, err := hujson.Parse(existingBytes)
	if err != nil {
		return nil, false, spec.NewPolicyError("invalid JSONC syntax: %v", err)
	}

	for _, ptr := range prev.JSONPointers {
		_ = removeAtPointer(&root, ptr)
	}

	for ptr, owned := range prev.ArrayEntries {
		curRaw, ok, err := getAtPointer(&root, ptr)
		if err != nil || !ok {
			continue
		}
		var curList []string
		if err := json.Unmarshal(curRaw, &curList); err != nil {
			continue
		}
		kept := mergeStringArray(curList, owned, nil)
		if len(kept) == 0 {
			_ = removeAtPointer(&root, ptr)
		} else {
			_ = setAtPointer(&root, ptr, kept)
		}
	}

	out, err := formatJSONCOutput(&root)
	if err != nil {
		return nil, false, err
	}
	if prev.CreatedByAgentfile {
		std, err := hujson.Standardize(out)
		if err == nil {
			var m map[string]any
			if jerr := json.Unmarshal(std, &m); jerr == nil {
				delete(m, "$schema")
				if isEffectivelyEmptyMap(m) {
					return nil, true, nil
				}
			}
		}
	}
	return out, false, nil
}

func isEffectivelyEmptyMap(m map[string]any) bool {
	for _, v := range m {
		if sub, ok := v.(map[string]any); ok {
			if !isEffectivelyEmptyMap(sub) {
				return false
			}
			continue
		}
		return false
	}
	return true
}

func mergeStringArray(current, prevOwned, desired []string) []string {
	prevSet := make(map[string]bool, len(prevOwned))
	for _, s := range prevOwned {
		prevSet[s] = true
	}
	desiredSet := make(map[string]bool, len(desired))
	for _, s := range desired {
		desiredSet[s] = true
	}

	var out []string
	seen := make(map[string]bool)
	for _, s := range current {
		if prevSet[s] && !desiredSet[s] {
			continue // Prune removed agentfile entry.
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range desired {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func getAtPointer(root *hujson.Value, ptr string) ([]byte, bool, error) {
	clone := root.Clone()
	clone.Standardize()
	std := clone.Pack()

	var doc any
	if err := json.Unmarshal(std, &doc); err != nil {
		return nil, false, err
	}
	tokens := splitPointer(ptr)
	cur := doc
	for _, tok := range tokens {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		next, exists := m[tok]
		if !exists {
			return nil, false, nil
		}
		cur = next
	}
	b, err := adapters.MarshalJSONNoEscape(cur)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func setAtPointer(root *hujson.Value, ptr string, val any) error {
	tokens := splitPointer(ptr)
	if len(tokens) == 0 {
		return fmt.Errorf("empty JSON pointer")
	}

	// Ensure intermediate parent objects exist.
	for i := 1; i < len(tokens); i++ {
		parentPtr := joinPointer(tokens[:i])
		_, exists, err := getAtPointer(root, parentPtr)
		if err != nil {
			return err
		}
		if !exists {
			patchBytes, _ := adapters.MarshalJSONNoEscape([]map[string]any{
				{"op": "add", "path": parentPtr, "value": map[string]any{}},
			})
			if err := root.Patch(patchBytes); err != nil {
				return fmt.Errorf("create parent %s: %w", parentPtr, err)
			}
		}
	}

	patchBytes, err := adapters.MarshalJSONNoEscape([]map[string]any{
		{"op": "add", "path": ptr, "value": val},
	})
	if err != nil {
		return err
	}
	if err := root.Patch(patchBytes); err != nil {
		return fmt.Errorf("patch %s: %w", ptr, err)
	}
	return nil
}

func removeAtPointer(root *hujson.Value, ptr string) error {
	_, exists, err := getAtPointer(root, ptr)
	if err != nil || !exists {
		return err
	}
	patchBytes, err := adapters.MarshalJSONNoEscape([]map[string]any{
		{"op": "remove", "path": ptr},
	})
	if err != nil {
		return err
	}
	return root.Patch(patchBytes)
}

func splitPointer(ptr string) []string {
	if ptr == "" || ptr == "/" {
		return nil
	}
	raw := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	out := make([]string, len(raw))
	for i, s := range raw {
		s = strings.ReplaceAll(s, "~1", "/")
		s = strings.ReplaceAll(s, "~0", "~")
		out[i] = s
	}
	return out
}

func joinPointer(tokens []string) string {
	var b strings.Builder
	for _, t := range tokens {
		b.WriteByte('/')
		t = strings.ReplaceAll(t, "~", "~0")
		t = strings.ReplaceAll(t, "/", "~1")
		b.WriteString(t)
	}
	return b.String()
}

func hashCanonicalJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return hashBytes(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return hashBytes(raw)
	}
	return hashBytes(b)
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func formatJSONCOutput(root *hujson.Value) ([]byte, error) {
	if !hasComments(root) {
		clone := root.Clone()
		clone.Standardize()
		var buf bytes.Buffer
		if err := json.Indent(&buf, bytes.TrimSpace(clone.Pack()), "", "  "); err != nil {
			return nil, err
		}
		buf.WriteByte('\n')
		return buf.Bytes(), nil
	}

	ensureMultilineBlocks(root)
	root.Format()
	lines := strings.Split(string(root.Pack()), "\n")
	for i, line := range lines {
		tabs := 0
		for tabs < len(line) && line[tabs] == '\t' {
			tabs++
		}
		if tabs > 0 {
			lines[i] = strings.Repeat("  ", tabs) + line[tabs:]
		}
	}
	out := []byte(strings.Join(lines, "\n"))
	if len(out) == 0 || out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, nil
}

func hasComments(v *hujson.Value) bool {
	if v == nil {
		return false
	}
	if bytes.Contains(v.BeforeExtra, []byte("//")) || bytes.Contains(v.BeforeExtra, []byte("/*")) ||
		bytes.Contains(v.AfterExtra, []byte("//")) || bytes.Contains(v.AfterExtra, []byte("/*")) {
		return true
	}
	switch node := v.Value.(type) {
	case *hujson.Object:
		if bytes.Contains(node.AfterExtra, []byte("//")) || bytes.Contains(node.AfterExtra, []byte("/*")) {
			return true
		}
		for i := range node.Members {
			if hasComments(&node.Members[i].Name) || hasComments(&node.Members[i].Value) {
				return true
			}
		}
	case *hujson.Array:
		if bytes.Contains(node.AfterExtra, []byte("//")) || bytes.Contains(node.AfterExtra, []byte("/*")) {
			return true
		}
		for i := range node.Elements {
			if hasComments(&node.Elements[i]) {
				return true
			}
		}
	}
	return false
}

func ensureMultilineBlocks(v *hujson.Value) {
	if v == nil {
		return
	}
	switch node := v.Value.(type) {
	case *hujson.Object:
		if len(node.Members) > 0 {
			for i := range node.Members {
				if !bytes.Contains(node.Members[i].Name.BeforeExtra, []byte("\n")) {
					node.Members[i].Name.BeforeExtra = append([]byte("\n"), node.Members[i].Name.BeforeExtra...)
				}
				ensureMultilineBlocks(&node.Members[i].Value)
			}
			if !bytes.Contains(node.AfterExtra, []byte("\n")) {
				node.AfterExtra = append(node.AfterExtra, '\n')
			}
		}
	case *hujson.Array:
		if len(node.Elements) > 0 {
			for i := range node.Elements {
				if !bytes.Contains(node.Elements[i].BeforeExtra, []byte("\n")) {
					node.Elements[i].BeforeExtra = append([]byte("\n"), node.Elements[i].BeforeExtra...)
				}
				ensureMultilineBlocks(&node.Elements[i])
			}
			if !bytes.Contains(node.AfterExtra, []byte("\n")) {
				node.AfterExtra = append(node.AfterExtra, '\n')
			}
		}
	}
}
