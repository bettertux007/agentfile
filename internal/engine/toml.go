package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/pelletier/go-toml/v2"
)

// MergeTOML applies a 3-way TOML table-key merge for dotted table paths.
func MergeTOML(
	existingBytes []byte,
	exists bool,
	mergeSpec *adapters.TOMLMergeSpec,
	prev *ArtifactRecord,
	resolveConflict ConflictResolver,
) ([]byte, ArtifactRecord, bool, error) {
	created := !exists || len(bytes.TrimSpace(existingBytes)) == 0
	var doc map[string]any
	if created {
		doc = make(map[string]any)
	} else {
		if err := toml.Unmarshal(existingBytes, &doc); err != nil {
			return nil, ArtifactRecord{}, false, spec.NewPolicyError("invalid TOML syntax: %v", err)
		}
		if doc == nil {
			doc = make(map[string]any)
		}
	}

	var tableEntries map[string]any
	if mergeSpec != nil {
		tableEntries = mergeSpec.TableEntries
	}

	needsBackup := false
	mutated := created
	nextRec := ArtifactRecord{
		ArtifactType:       TypeTOMLPatch,
		TOMLKeyHashes:      make(map[string]string),
		CreatedByAgentfile: created || (prev != nil && prev.CreatedByAgentfile),
	}

	// 1. Prune previously owned dotted keys no longer desired.
	if prev != nil {
		oldKeys := append([]string(nil), prev.TOMLKeys...)
		sort.Strings(oldKeys)
		for _, oldKey := range oldKeys {
			if _, stillWanted := tableEntries[oldKey]; stillWanted {
				continue
			}
			curVal, ok, err := getAtTOMLPath(doc, oldKey)
			if err != nil {
				return nil, ArtifactRecord{}, false, err
			}
			if !ok {
				continue
			}
			curHash, err := hashCanonicalTOMLValue(curVal)
			if err != nil {
				return nil, ArtifactRecord{}, false, err
			}
			if prev.TOMLKeyHashes[oldKey] != "" && curHash != prev.TOMLKeyHashes[oldKey] {
				overwrite, backup, cerr := resolveTOMLConflict(resolveConflict, fmt.Sprintf("owned TOML key %s was modified on disk", oldKey))
				if cerr != nil {
					return nil, ArtifactRecord{}, false, cerr
				}
				if backup {
					needsBackup = true
				}
				if !overwrite {
					// Preserve user-modified value by omitting from removal.
					continue
				}
			}
			if removed, err := removeAtTOMLPath(doc, oldKey); err != nil {
				return nil, ArtifactRecord{}, false, err
			} else if removed {
				mutated = true
			}
		}
	}

	// 2. Apply desired dotted keys in deterministic order.
	keys := make([]string, 0, len(tableEntries))
	for k := range tableEntries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		desiredVal := tableEntries[key]
		desiredHash, err := hashCanonicalTOMLValue(desiredVal)
		if err != nil {
			return nil, ArtifactRecord{}, false, err
		}

		curVal, existsOnDisk, err := getAtTOMLPath(doc, key)
		if err != nil {
			return nil, ArtifactRecord{}, false, err
		}
		if existsOnDisk {
			curHash, err := hashCanonicalTOMLValue(curVal)
			if err != nil {
				return nil, ArtifactRecord{}, false, err
			}
			if curHash == desiredHash {
				nextRec.TOMLKeys = append(nextRec.TOMLKeys, key)
				nextRec.TOMLKeyHashes[key] = desiredHash
				continue
			}
			var prevHash string
			if prev != nil {
				prevHash = prev.TOMLKeyHashes[key]
			}
			// Conflict if unowned (prevHash == "") or user edited since last write (curHash != prevHash).
			if prevHash == "" || curHash != prevHash {
				reason := fmt.Sprintf("TOML key %s already exists with different value", key)
				if prevHash != "" {
					reason = fmt.Sprintf("owned TOML key %s was modified on disk", key)
				}
				overwrite, backup, cerr := resolveTOMLConflict(resolveConflict, reason)
				if cerr != nil {
					return nil, ArtifactRecord{}, false, cerr
				}
				if backup {
					needsBackup = true
				}
				if !overwrite {
					continue
				}
			}
		}

		if err := setAtTOMLPath(doc, key, desiredVal); err != nil {
			return nil, ArtifactRecord{}, false, err
		}
		mutated = true
		nextRec.TOMLKeys = append(nextRec.TOMLKeys, key)
		nextRec.TOMLKeyHashes[key] = desiredHash
	}

	sort.Strings(nextRec.TOMLKeys)

	if exists && !created && !mutated {
		out := bytes.Clone(existingBytes)
		nextRec.SHA256 = hashBytes(out)
		return out, nextRec, needsBackup, nil
	}

	out, err := formatTOMLOutput(doc)
	if err != nil {
		return nil, ArtifactRecord{}, false, err
	}
	nextRec.SHA256 = hashBytes(out)
	return out, nextRec, needsBackup, nil
}

// RemoveTOML strips agentfile-owned dotted keys from an existing TOML file and cleans up empty parent tables.
// Returns deleteFile=true if the file was created by agentfile and becomes effectively empty.
func RemoveTOML(existingBytes []byte, prev ArtifactRecord) ([]byte, bool, error) {
	if len(bytes.TrimSpace(existingBytes)) == 0 {
		return nil, prev.CreatedByAgentfile, nil
	}
	var doc map[string]any
	if err := toml.Unmarshal(existingBytes, &doc); err != nil {
		return nil, false, spec.NewPolicyError("invalid TOML syntax: %v", err)
	}
	if doc == nil {
		doc = make(map[string]any)
	}

	mutated := false
	for _, key := range prev.TOMLKeys {
		removed, err := removeAtTOMLPath(doc, key)
		if err != nil {
			return nil, false, err
		}
		if removed {
			mutated = true
		}
	}

	if prev.CreatedByAgentfile && isEffectivelyEmptyMap(doc) {
		return nil, true, nil
	}
	if !mutated {
		return bytes.Clone(existingBytes), false, nil
	}
	out, err := formatTOMLOutput(doc)
	if err != nil {
		return nil, false, err
	}
	return out, false, nil
}

func resolveTOMLConflict(resolveConflict ConflictResolver, detail string) (bool, bool, error) {
	if resolveConflict == nil {
		return false, false, spec.NewPolicyError("TOML conflict: %s", detail)
	}
	return resolveConflict(detail)
}

func parseTOMLPath(path string) ([]string, error) {
	if path == "" {
		return nil, spec.NewPolicyError("empty TOML key path")
	}
	var parts []string
	i := 0
	for i < len(path) {
		if path[i] == '"' {
			i++
			var seg strings.Builder
			closed := false
			for i < len(path) {
				ch := path[i]
				if ch == '\\' {
					i++
					if i >= len(path) {
						return nil, spec.NewPolicyError("invalid TOML key path %q", path)
					}
					switch path[i] {
					case '"', '\\':
						seg.WriteByte(path[i])
					default:
						return nil, spec.NewPolicyError("invalid TOML key path %q", path)
					}
					i++
					continue
				}
				if ch == '"' {
					closed = true
					i++
					break
				}
				seg.WriteByte(ch)
				i++
			}
			if !closed {
				return nil, spec.NewPolicyError("invalid TOML key path %q", path)
			}
			parts = append(parts, seg.String())
			if i < len(path) {
				if path[i] != '.' {
					return nil, spec.NewPolicyError("invalid TOML key path %q", path)
				}
				i++
				if i == len(path) {
					return nil, spec.NewPolicyError("invalid TOML key path %q", path)
				}
			}
			continue
		}

		start := i
		for i < len(path) && path[i] != '.' {
			if path[i] == '"' {
				return nil, spec.NewPolicyError("invalid TOML key path %q", path)
			}
			i++
		}
		seg := path[start:i]
		if seg == "" {
			return nil, spec.NewPolicyError("invalid TOML key path %q", path)
		}
		parts = append(parts, seg)
		if i < len(path) {
			i++
			if i == len(path) {
				return nil, spec.NewPolicyError("invalid TOML key path %q", path)
			}
		}
	}
	return parts, nil
}

func getAtTOMLPath(doc map[string]any, dottedKey string) (any, bool, error) {
	parts, err := parseTOMLPath(dottedKey)
	if err != nil {
		return nil, false, err
	}
	var cur any = doc
	for i, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, spec.NewPolicyError("TOML key %q is not a table while traversing %q", strings.Join(parts[:i], "."), dottedKey)
		}
		next, exists := m[part]
		if !exists {
			return nil, false, nil
		}
		cur = next
	}
	return cur, true, nil
}

func setAtTOMLPath(doc map[string]any, dottedKey string, val any) error {
	parts, err := parseTOMLPath(dottedKey)
	if err != nil {
		return err
	}
	cur := doc
	for i := 0; i < len(parts)-1; i++ {
		seg := parts[i]
		existing, exists := cur[seg]
		if !exists {
			child := make(map[string]any)
			cur[seg] = child
			cur = child
			continue
		}
		child, ok := existing.(map[string]any)
		if !ok {
			return spec.NewPolicyError("TOML key %q is not a table while setting %q", strings.Join(parts[:i+1], "."), dottedKey)
		}
		cur = child
	}
	cur[parts[len(parts)-1]] = val
	return nil
}

func removeAtTOMLPath(doc map[string]any, dottedKey string) (bool, error) {
	parts, err := parseTOMLPath(dottedKey)
	if err != nil {
		return false, err
	}
	return removeTOMLSegments(doc, parts), nil
}

func removeTOMLSegments(m map[string]any, parts []string) bool {
	if len(parts) == 0 || m == nil {
		return false
	}
	if len(parts) == 1 {
		if _, exists := m[parts[0]]; !exists {
			return false
		}
		delete(m, parts[0])
		return true
	}
	child, ok := m[parts[0]].(map[string]any)
	if !ok {
		return false
	}
	removed := removeTOMLSegments(child, parts[1:])
	if removed && len(child) == 0 {
		delete(m, parts[0])
	}
	return removed
}

func hashCanonicalTOMLValue(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var norm any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return hashBytes(raw), nil
	}
	canon, err := json.Marshal(norm)
	if err != nil {
		return hashBytes(raw), nil
	}
	return hashBytes(canon), nil
}

func formatTOMLOutput(doc map[string]any) ([]byte, error) {
	out, err := toml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, nil
}
