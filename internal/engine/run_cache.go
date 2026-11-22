package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/ir"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/bettertux007/agentfile/internal/vcs"
)

const (
	// RunCacheVersion is the schema version for .agentfile/run-cache/<target>.json.
	RunCacheVersion = 1
	// MaxFastPathFiles bounds the total input + output entries tracked by the fast path.
	MaxFastPathFiles = 2000
	racyWindowNanos  = int64(2 * time.Second)
)

// RunCacheFileStat records portable os.FileInfo attributes and content SHA-256 for one path.
type RunCacheFileStat struct {
	Path            string `json:"path"`
	IsDir           bool   `json:"is_dir,omitempty"`
	Size            int64  `json:"size,omitempty"`
	Mode            uint32 `json:"mode"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
	LinkTarget      string `json:"link_target,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
}

// RunCacheRecord stores the per-target fast-path snapshot in .agentfile/run-cache/<target>.json.
type RunCacheRecord struct {
	Version            int                   `json:"version"`
	AgentfileVersion   string                `json:"agentfile_version"`
	Target             string                `json:"target"`
	Targets            []string              `json:"targets"`
	TargetConfig       spec.TargetConfigSpec `json:"target_config"`
	MCPNames           []string              `json:"mcp_names,omitempty"`
	EnvDigest          string                `json:"env_digest"`
	ControlFiles       map[string]string     `json:"control_files"`
	ControlOrder       []string              `json:"control_order"`
	ExcludePath        string                `json:"exclude_path,omitempty"`
	ExcludeStat        *RunCacheFileStat     `json:"exclude_stat,omitempty"`
	Probes             map[string]bool       `json:"probes,omitempty"`
	LocalInputRoots    []string              `json:"local_input_roots,omitempty"`
	LocalInputs        []RunCacheFileStat    `json:"local_inputs,omitempty"`
	ExtractedDirs      []string              `json:"extracted_dirs,omitempty"`
	OutputRoots        []string              `json:"output_roots,omitempty"`
	Outputs            []RunCacheFileStat    `json:"outputs,omitempty"`
	RecordedAtUnixNano int64                 `json:"recorded_at_unix_nano"`
}

// BuildBundleForRun constructs a minimal ir.Bundle for PlanLaunch from a cached RunCacheRecord.
func (r *RunCacheRecord) BuildBundleForRun() *ir.Bundle {
	b := &ir.Bundle{
		Targets: append([]string(nil), r.Targets...),
		TargetsConfig: map[string]spec.TargetConfigSpec{
			r.Target: r.TargetConfig,
		},
	}
	for _, name := range r.MCPNames {
		b.MCP = append(b.MCP, ir.IRMCPServer{Name: name})
	}
	return b
}

// RunFastPathSaveInput bundles all parameters needed to write .agentfile/run-cache/<target>.json.
type RunFastPathSaveInput struct {
	ProjectRoot        string
	AgentfileVersion   string
	Target             string
	EffectiveSpec      *spec.ProjectSpec
	LocalManifests     []string
	EffectiveConfig    *config.EffectiveConfig
	CacheRoot          string
	Probes             adapters.ProbeResults
	ResolvedAssets     map[string]store.ResolvedAsset
	ExtraExtractedDirs []string
}

// RunFastPathCheckInput bundles parameters needed to verify .agentfile/run-cache/<target>.json.
type RunFastPathCheckInput struct {
	ProjectRoot      string
	AgentfileVersion string
	Target           string
	EffectiveConfig  *config.EffectiveConfig
	CacheRoot        string
	Probes           adapters.ProbeResults
}

// RunCachePath returns the path to .agentfile/run-cache/<target>.json.
func RunCachePath(projectRoot, target string) string {
	return filepath.Join(projectRoot, ".agentfile", "run-cache", target+".json")
}

// SaveRunFastPath records a per-target fast-path snapshot after a successful sync.
func SaveRunFastPath(in RunFastPathSaveInput) error {
	if in.ProjectRoot == "" || in.Target == "" || in.EffectiveSpec == nil || in.EffectiveConfig == nil {
		return nil
	}
	if _, err := os.Stat(JournalPath(in.ProjectRoot)); err == nil {
		return nil
	}
	st, err := LoadState(in.ProjectRoot)
	if err != nil {
		return nil
	}
	ts := st.Targets[in.Target]
	if ts == nil {
		return nil
	}

	controlOrder := make([]string, 0, len(in.LocalManifests)+4)
	seenControl := make(map[string]bool)
	addControl := func(p string) {
		if p == "" {
			return
		}
		clean := filepath.Clean(p)
		if !seenControl[clean] {
			seenControl[clean] = true
			controlOrder = append(controlOrder, clean)
		}
	}
	if len(in.LocalManifests) == 0 {
		addControl(filepath.Join(in.ProjectRoot, "Agentfile.yaml"))
	} else {
		for _, m := range in.LocalManifests {
			addControl(m)
		}
	}
	addControl(filepath.Join(in.ProjectRoot, "Agentfile.lock"))
	if in.EffectiveConfig.LocalConfigPath != "" {
		addControl(in.EffectiveConfig.LocalConfigPath)
	} else {
		addControl(filepath.Join(in.ProjectRoot, ".agentfile", "config.yaml"))
	}
	if in.EffectiveConfig.UserConfigPath != "" {
		addControl(in.EffectiveConfig.UserConfigPath)
	}
	addControl(StatePath(in.ProjectRoot))

	controlFiles := make(map[string]string, len(controlOrder))
	for _, p := range controlOrder {
		dig, err := controlFileDigest(p)
		if err != nil {
			return nil
		}
		controlFiles[p] = dig
	}

	var localRoots []string
	seenLocal := make(map[string]bool)
	var extractedDirs []string
	seenExtracted := make(map[string]bool)

	for _, asset := range in.ResolvedAssets {
		if asset.IsRemote {
			dir := asset.SourcePath
			if info, serr := os.Stat(dir); serr == nil && !info.IsDir() {
				dir = filepath.Dir(dir)
			}
			clean := filepath.Clean(dir)
			if !seenExtracted[clean] {
				seenExtracted[clean] = true
				extractedDirs = append(extractedDirs, clean)
			}
		} else if asset.SourcePath != "" {
			clean := filepath.Clean(asset.SourcePath)
			if !seenLocal[clean] {
				seenLocal[clean] = true
				localRoots = append(localRoots, clean)
			}
		}
	}
	for _, d := range in.ExtraExtractedDirs {
		if d == "" {
			continue
		}
		clean := filepath.Clean(d)
		if !seenExtracted[clean] {
			seenExtracted[clean] = true
			extractedDirs = append(extractedDirs, clean)
		}
	}
	sort.Strings(localRoots)
	sort.Strings(extractedDirs)

	localInputs, ok, err := collectTreeStats(localRoots, MaxFastPathFiles, true)
	if err != nil || !ok {
		_ = os.Remove(RunCachePath(in.ProjectRoot, in.Target))
		return nil
	}

	outputSet := make(map[string]bool)
	for rel := range ts.Artifacts {
		outputSet[filepath.Join(in.ProjectRoot, filepath.FromSlash(rel))] = true
	}
	for rel, sar := range st.Shared {
		for _, owner := range sar.Owners {
			if owner == in.Target {
				outputSet[filepath.Join(in.ProjectRoot, filepath.FromSlash(rel))] = true
				break
			}
		}
	}
	outputRoots := make([]string, 0, len(outputSet))
	for p := range outputSet {
		outputRoots = append(outputRoots, filepath.Clean(p))
	}
	sort.Strings(outputRoots)

	remCap := MaxFastPathFiles - len(localInputs)
	if remCap <= 0 {
		_ = os.Remove(RunCachePath(in.ProjectRoot, in.Target))
		return nil
	}
	outputs, ok, err := collectTreeStats(outputRoots, remCap, true)
	if err != nil || !ok {
		_ = os.Remove(RunCachePath(in.ProjectRoot, in.Target))
		return nil
	}

	var excludePath string
	var excludeStat *RunCacheFileStat
	if in.EffectiveConfig.GitExclude {
		excludePath = vcs.ResolveExcludePath(in.ProjectRoot)
		if excludePath != "" {
			if stStat, serr := statSinglePath(excludePath, true); serr == nil {
				excludeStat = &stStat
			}
		}
	}

	mcpNames := make([]string, 0, len(in.EffectiveSpec.MCP))
	for k := range in.EffectiveSpec.MCP {
		mcpNames = append(mcpNames, k)
	}
	sort.Strings(mcpNames)

	probes := make(map[string]bool, len(in.Probes))
	for k, v := range in.Probes {
		probes[k] = v
	}

	rec := &RunCacheRecord{
		Version:            RunCacheVersion,
		AgentfileVersion:   in.AgentfileVersion,
		Target:             in.Target,
		Targets:            append([]string(nil), in.EffectiveSpec.Targets...),
		TargetConfig:       in.EffectiveSpec.TargetsConfig[in.Target],
		MCPNames:           mcpNames,
		EnvDigest:          computeRunEnvDigest(in.CacheRoot, in.EffectiveConfig),
		ControlFiles:       controlFiles,
		ControlOrder:       controlOrder,
		ExcludePath:        excludePath,
		ExcludeStat:        excludeStat,
		Probes:             probes,
		LocalInputRoots:    localRoots,
		LocalInputs:        localInputs,
		ExtractedDirs:      extractedDirs,
		OutputRoots:        outputRoots,
		Outputs:            outputs,
		RecordedAtUnixNano: time.Now().UnixNano(),
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(RunCachePath(in.ProjectRoot, in.Target), data, 0o644)
}

// CheckRunFastPath verifies whether a target's fast-path cache is warm and valid.
func CheckRunFastPath(in RunFastPathCheckInput) (*RunCacheRecord, bool, string) {
	if in.ProjectRoot == "" || in.Target == "" || in.EffectiveConfig == nil {
		return nil, false, "invalid fast-path input"
	}
	if _, err := os.Stat(JournalPath(in.ProjectRoot)); err == nil {
		return nil, false, "commit journal pending"
	}
	if _, err := os.Stat(StatePath(in.ProjectRoot)); err != nil {
		return nil, false, "state.json missing"
	}

	cacheFile := RunCachePath(in.ProjectRoot, in.Target)
	raw, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, false, "run cache absent"
	}
	var rec RunCacheRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, false, "run cache malformed"
	}
	if rec.Version != RunCacheVersion || rec.Target != in.Target {
		return nil, false, "run cache version or target mismatch"
	}
	if rec.AgentfileVersion != in.AgentfileVersion {
		return nil, false, "agentfile version changed"
	}
	if rec.EnvDigest != computeRunEnvDigest(in.CacheRoot, in.EffectiveConfig) {
		return nil, false, "effective config or environment changed"
	}

	for _, p := range rec.ControlOrder {
		dig, err := controlFileDigest(p)
		if err != nil || dig != rec.ControlFiles[p] {
			return nil, false, fmt.Sprintf("control file %s changed", filepath.Base(p))
		}
	}

	if len(rec.Probes) != len(in.Probes) {
		return nil, false, "workspace probe count changed"
	}
	for k, want := range rec.Probes {
		if got, ok := in.Probes[k]; !ok || got != want {
			return nil, false, fmt.Sprintf("workspace probe %s changed", k)
		}
	}

	for _, dir := range rec.ExtractedDirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return nil, false, fmt.Sprintf("extracted store directory %s missing", filepath.Base(dir))
		}
	}

	nowNano := time.Now().UnixNano()
	checkedRacy := false

	if in.EffectiveConfig.GitExclude {
		curExclude := vcs.ResolveExcludePath(in.ProjectRoot)
		if curExclude != rec.ExcludePath {
			return nil, false, "git exclude path changed"
		}
		if rec.ExcludeStat != nil {
			curStat, serr := statSinglePath(rec.ExcludeStat.Path, false)
			if serr != nil {
				return nil, false, "git info/exclude missing"
			}
			ok, racy, verr := verifyStatMatch(*rec.ExcludeStat, curStat, rec.RecordedAtUnixNano, nowNano)
			if verr != nil || !ok {
				return nil, false, "git info/exclude changed"
			}
			if racy {
				checkedRacy = true
			}
		}
	}

	curLocal, ok, err := collectTreeStats(rec.LocalInputRoots, MaxFastPathFiles, false)
	if err != nil || !ok {
		return nil, false, "local input tree unreadable or exceeded limit"
	}
	matchLocal, racyLocal := compareStatSlices(rec.LocalInputs, curLocal, rec.RecordedAtUnixNano, nowNano)
	if !matchLocal {
		return nil, false, "local input tree changed"
	}
	if racyLocal {
		checkedRacy = true
	}

	curOutputs, ok, err := collectTreeStats(rec.OutputRoots, MaxFastPathFiles, false)
	if err != nil || !ok {
		return nil, false, "workspace output artifacts unreadable"
	}
	matchOut, racyOut := compareStatSlices(rec.Outputs, curOutputs, rec.RecordedAtUnixNano, nowNano)
	if !matchOut {
		return nil, false, "workspace output artifact changed"
	}
	if racyOut {
		checkedRacy = true
	}

	// Promote RecordedAtUnixNano once we are past the 2-second racy window so future runs skip hashing.
	if checkedRacy && nowNano >= rec.RecordedAtUnixNano+racyWindowNanos {
		rec.RecordedAtUnixNano = nowNano
		if b, merr := json.MarshalIndent(&rec, "", "  "); merr == nil {
			b = append(b, '\n')
			_ = writeFileAtomic(cacheFile, b, 0o644)
		}
	}

	return &rec, true, ""
}

func computeRunEnvDigest(cacheRoot string, eff *config.EffectiveConfig) string {
	var ulg, llg string
	if eff.UserLoadGlobal != nil {
		ulg = fmt.Sprintf("%t", *eff.UserLoadGlobal)
	}
	if eff.LocalLoadGlobal != nil {
		llg = fmt.Sprintf("%t", *eff.LocalLoadGlobal)
	}
	raw := fmt.Sprintf(
		"cache=%s\nuser_cfg=%s(%t)\nlocal_cfg=%s(%t)\nlink=%s\nconflict=%s\nexclude=%t\nulg=%s\nllg=%s\nci=%t\nno_user=%s\nallow_file=%s\n",
		cacheRoot,
		eff.UserConfigPath, eff.UserConfigLoaded,
		eff.LocalConfigPath, eff.LocalConfigLoaded,
		eff.LinkMode,
		eff.OnConflict,
		eff.GitExclude,
		ulg,
		llg,
		config.IsCI(),
		os.Getenv("AGENTFILE_NO_USER_CONFIG"),
		os.Getenv("AGENTFILE_ALLOW_FILE_GIT"),
	)
	return hashBytes([]byte(raw))
}

func controlFileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "absent", nil
		}
		return "", err
	}
	return hashBytes(data), nil
}

func hashFileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func statSinglePath(absPath string, includeHash bool) (RunCacheFileStat, error) {
	info, err := os.Lstat(absPath)
	if err != nil {
		return RunCacheFileStat{}, err
	}
	st := RunCacheFileStat{
		Path:            filepath.ToSlash(absPath),
		IsDir:           info.IsDir(),
		Mode:            uint32(info.Mode()),
		ModTimeUnixNano: info.ModTime().UnixNano(),
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, lerr := os.Readlink(absPath)
		if lerr != nil {
			return RunCacheFileStat{}, lerr
		}
		st.LinkTarget = target
		return st, nil
	}
	if !info.IsDir() {
		st.Size = info.Size()
		if includeHash {
			h, herr := hashFileSHA256(absPath)
			if herr != nil {
				return RunCacheFileStat{}, herr
			}
			st.SHA256 = h
		}
	}
	return st, nil
}

func collectTreeStats(roots []string, maxEntries int, includeHash bool) ([]RunCacheFileStat, bool, error) {
	var out []RunCacheFileStat
	for _, root := range roots {
		info, err := os.Lstat(root)
		if err != nil {
			return nil, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			st, err := statSinglePath(root, includeHash)
			if err != nil {
				return nil, false, err
			}
			out = append(out, st)
			if len(out) > maxEntries {
				return nil, false, nil
			}
			continue
		}

		walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			st, serr := statSinglePath(p, includeHash)
			if serr != nil {
				return serr
			}
			out = append(out, st)
			if len(out) > maxEntries {
				return errFastPathCapExceeded
			}
			return nil
		})
		if errors.Is(walkErr, errFastPathCapExceeded) {
			return nil, false, nil
		}
		if walkErr != nil {
			return nil, false, walkErr
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out, true, nil
}

var errFastPathCapExceeded = errors.New("fast path file cap exceeded")

func compareStatSlices(expected, current []RunCacheFileStat, recordedAtNano, nowNano int64) (bool, bool) {
	if len(expected) != len(current) {
		return false, false
	}
	checkedRacy := false
	for i := range expected {
		ok, racy, err := verifyStatMatch(expected[i], current[i], recordedAtNano, nowNano)
		if err != nil || !ok {
			return false, false
		}
		if racy {
			checkedRacy = true
		}
	}
	return true, checkedRacy
}

func verifyStatMatch(exp, cur RunCacheFileStat, recordedAtNano, nowNano int64) (bool, bool, error) {
	if exp.Path != cur.Path ||
		exp.IsDir != cur.IsDir ||
		exp.Size != cur.Size ||
		exp.Mode != cur.Mode ||
		exp.ModTimeUnixNano != cur.ModTimeUnixNano ||
		exp.LinkTarget != cur.LinkTarget {
		return false, false, nil
	}
	if exp.IsDir || exp.LinkTarget != "" {
		return true, false, nil
	}
	isRacy := cur.ModTimeUnixNano >= recordedAtNano-racyWindowNanos || nowNano-cur.ModTimeUnixNano < racyWindowNanos
	if !isRacy {
		return true, false, nil
	}
	h, err := hashFileSHA256(filepath.FromSlash(cur.Path))
	if err != nil {
		return false, true, err
	}
	return strings.EqualFold(h, exp.SHA256), true, nil
}
