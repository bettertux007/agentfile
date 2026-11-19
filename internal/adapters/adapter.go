package adapters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/bettertux007/agentfile/internal/ir"
)

// ArtifactOpKind enumerates how an adapter artifact is materialized on disk.
type ArtifactOpKind string

const (
	OpWriteOwned      ArtifactOpKind = "write_owned"
	OpMergeJSONC      ArtifactOpKind = "merge_jsonc"
	OpMergeTOML       ArtifactOpKind = "merge_toml"
	OpWriteDropIn     ArtifactOpKind = "write_dropin"
	OpManagedBlock    ArtifactOpKind = "managed_block"
	OpMaterializeTree ArtifactOpKind = "materialize_tree"
)

// JSONMergeSpec describes structured object-key and string-array mutations inside a JSON/JSONC file.
type JSONMergeSpec struct {
	SchemaURL    string              // Optional $schema value set if absent
	MapEntries   map[string]any      // RFC 6901 pointer (e.g. "/mcpServers/github") -> value
	ArrayEntries map[string][]string // RFC 6901 pointer (e.g. "/permissions/allow") -> managed strings
}

// TOMLMergeSpec describes structured table-key mutations inside a TOML file.
type TOMLMergeSpec struct {
	TableEntries map[string]any // Dotted table path (e.g. "mcp_servers.github") -> value
}

// ArtifactOp is a pure compiled output operation produced by an adapter.
type ArtifactOp struct {
	Target     string
	RelPath    string
	Kind       ArtifactOpKind
	Shared     bool           // True when multiple targets co-own the same RelPath (e.g. AGENTS.md)
	Content    []byte         // Used for OpWriteOwned, OpWriteDropIn, OpManagedBlock
	SourcePath string         // Used for OpMaterializeTree
	IsRemote   bool           // Provenance for OpMaterializeTree
	JSONMerge  *JSONMergeSpec // Used for OpMergeJSONC
	TOMLMerge  *TOMLMergeSpec // Used for OpMergeTOML
}

// ProbeRequest asks engine.Plan to check a workspace file's existence without giving adapters filesystem access.
type ProbeRequest struct {
	Key     string
	RelPath string
}

// ProbeResults maps ProbeRequest.Key to whether the workspace path exists.
type ProbeResults map[string]bool

// SchemaContract documents an adapter's supported upstream CLI version range.
type SchemaContract struct {
	MinVersion          string
	MinIsolationVersion string
	MaxTested           string
	BinaryOptional      bool
}

// GlobalConfigSpec declares a user-global config, MCP, skill, rule, or agent path.
type GlobalConfigSpec struct {
	RelPath      string
	Kind         string // mcp | skills | rules | agents
	JSONMCPKey   string // JSON object key containing MCP servers when Kind == "mcp"
	UseXDGConfig bool   // If true, RelPath is relative to XDG_CONFIG_HOME (~/.config)
}

// IsolationMode classifies how runtime global isolation is enforced for a launch.
type IsolationMode string

const (
	IsolationNone        IsolationMode = "none"
	IsolationNativeFlags IsolationMode = "native_flags"
	IsolationPartialWarn IsolationMode = "partial_warn"
	IsolationUnsupported IsolationMode = "unsupported"
)

// LaunchOptions configures pure launch planning for a target.
type LaunchOptions struct {
	LoadGlobal bool
	UserArgs   []string
}

// LaunchFacts supplies read-only workspace, version, and global-hit facts gathered by runner.
type LaunchFacts struct {
	ProjectRoot   string
	BinaryVersion string
	Probes        ProbeResults
	GlobalHits    map[string]bool
	CacheRoot     string
}

// LaunchPlan is the pure runtime command modification plan emitted by PlanLaunch.
type LaunchPlan struct {
	Target        string
	Mode          IsolationMode
	PrependedArgs []string
	EnvOverrides  map[string]string
	EnsureDirs    []string
	NeedEmptyMCP  bool
	Diagnostics   []Diagnostic
}

// DiagLevel classifies adapter compilation or policy diagnostics.
type DiagLevel string

const (
	DiagError   DiagLevel = "error"
	DiagWarning DiagLevel = "warning"
	DiagInfo    DiagLevel = "info"
)

// Diagnostic reports a capability gap, fail-closed policy error, or warning.
type Diagnostic struct {
	Level   DiagLevel
	Target  string
	Code    string
	Message string
}

// CompileResult holds compiled artifact operations and diagnostics for one target.
type CompileResult struct {
	Target      string
	Files       []ArtifactOp
	Diagnostics []Diagnostic
}

// Adapter compiles an IR Bundle into target-native operations without performing I/O.
type Adapter interface {
	Name() string
	BinaryName() string
	SchemaContract() SchemaContract
	RequiredProbes() []ProbeRequest
	Compile(bundle *ir.Bundle, facts ProbeResults) (*CompileResult, error)
	CheckPolicy(bundle *ir.Bundle, mergedFiles map[string][]byte) []Diagnostic
	GlobalConfigSpecs() []GlobalConfigSpec
	IsSessionArgs(userArgs []string) bool
	PlanLaunch(bundle *ir.Bundle, opts LaunchOptions, facts LaunchFacts) (*LaunchPlan, error)
}

var semverRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// CompareSemver extracts the first X.Y[.Z] version from versionStr and returns true if >= minStr.
func CompareSemver(versionStr, minStr string) (bool, error) {
	vMaj, vMin, vPat, ok := parseSemverTuple(versionStr)
	if !ok {
		return false, fmt.Errorf("could not parse version from %q", versionStr)
	}
	mMaj, mMin, mPat, ok := parseSemverTuple(minStr)
	if !ok {
		return false, fmt.Errorf("invalid minimum version %q", minStr)
	}
	if vMaj != mMaj {
		return vMaj > mMaj, nil
	}
	if vMin != mMin {
		return vMin > mMin, nil
	}
	return vPat >= mPat, nil
}

func parseSemverTuple(s string) (int, int, int, bool) {
	m := semverRe.FindStringSubmatch(s)
	if len(m) < 3 {
		return 0, 0, 0, false
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	pat := 0
	if len(m) >= 4 && m[3] != "" {
		pat, _ = strconv.Atoi(m[3])
	}
	return maj, min, pat, true
}

// ValidateForbiddenFlags scans userArgs before "--" (skipping values after valueFlags)
// and emits DiagError diagnostics if any forbidden flag is supplied.
func ValidateForbiddenFlags(target string, userArgs, forbidden, valueFlags []string) []Diagnostic {
	valSet := make(map[string]bool, len(valueFlags))
	for _, vf := range valueFlags {
		valSet[vf] = true
	}

	var diags []Diagnostic
	skipNext := false
	for _, tok := range userArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if tok == "--" {
			break
		}
		for _, f := range forbidden {
			if tok == f || strings.HasPrefix(tok, f+"=") {
				diags = append(diags, Diagnostic{
					Level:   DiagError,
					Target:  target,
					Code:    "FORBIDDEN_ISOLATION_FLAG",
					Message: fmt.Sprintf("flag %q conflicts with load_global=false isolation on %s", tok, target),
				})
				break
			}
		}
		if valSet[tok] {
			skipNext = true
		}
	}
	return diags
}

// MarshalJSONNoEscape encodes v with SetEscapeHTML(false) and 2-space indentation.
func MarshalJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
