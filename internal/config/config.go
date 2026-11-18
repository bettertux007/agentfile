package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bettertux007/agentfile/internal/spec"
	"gopkg.in/yaml.v3"
)

// LocalConfig represents .agentfile/config.yaml (version 1).
type LocalConfig struct {
	Version    int    `yaml:"version"`
	LinkMode   string `yaml:"link_mode,omitempty"`
	OnConflict string `yaml:"on_conflict,omitempty"`
	GitExclude *bool  `yaml:"git_exclude,omitempty"`
	LoadGlobal *bool  `yaml:"load_global,omitempty"`
}

// CLIOverrides captures CLI flags that override .agentfile/config.yaml.
type CLIOverrides struct {
	LinkMode      string
	Copy          bool
	OnConflict    string
	Force         bool
	GitExclude    bool
	GitExcludeSet bool
}

// EffectiveConfig is the resolved machine-local tool configuration.
type EffectiveConfig struct {
	LinkMode   string
	OnConflict string
	GitExclude bool
	LoadGlobal *bool
	ConfigPath string
}

// IsCI reports whether standard CI environment variables are set.
func IsCI() bool {
	for _, k := range []string{"CI", "GITHUB_ACTIONS", "BUILDKITE", "GITLAB_CI", "CIRCLECI"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// Resolve loads optional .agentfile/config.yaml and applies CLI overrides and defaults.
func Resolve(projectRoot string, cli CLIOverrides, isTTY func() bool) (*EffectiveConfig, error) {
	if cli.Copy && cli.LinkMode != "" && cli.LinkMode != "copy" {
		return nil, spec.NewPolicyError("conflicting flags: --copy cannot be combined with --link-mode=%s", cli.LinkMode)
	}
	if cli.Force && cli.OnConflict != "" && cli.OnConflict != "overwrite" {
		return nil, spec.NewPolicyError("conflicting flags: --force cannot be combined with --on-conflict=%s", cli.OnConflict)
	}

	cfgPath := filepath.Join(projectRoot, ".agentfile", "config.yaml")
	var fileCfg LocalConfig
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", cfgPath, err)
		}
	} else {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&fileCfg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, spec.NewPolicyError("%s is empty", cfgPath)
			}
			return nil, spec.NewPolicyError("invalid %s: %v", cfgPath, err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return nil, spec.NewPolicyError("%s must contain a single YAML document", cfgPath)
		}
		if fileCfg.Version != 1 {
			return nil, spec.NewPolicyError("unsupported %s version %d (expected 1)", cfgPath, fileCfg.Version)
		}
	}

	eff := &EffectiveConfig{
		LinkMode:   "copy",
		GitExclude: true,
		LoadGlobal: fileCfg.LoadGlobal,
		ConfigPath: cfgPath,
	}

	if fileCfg.LinkMode != "" {
		eff.LinkMode = fileCfg.LinkMode
	}
	if fileCfg.OnConflict != "" {
		eff.OnConflict = fileCfg.OnConflict
	}
	if fileCfg.GitExclude != nil {
		eff.GitExclude = *fileCfg.GitExclude
	}

	if cli.Copy {
		eff.LinkMode = "copy"
	} else if cli.LinkMode != "" {
		eff.LinkMode = cli.LinkMode
	}

	if cli.Force {
		eff.OnConflict = "overwrite"
	} else if cli.OnConflict != "" {
		eff.OnConflict = cli.OnConflict
	}

	if cli.GitExcludeSet {
		eff.GitExclude = cli.GitExclude
	}

	if eff.OnConflict == "" {
		if isTTY != nil && isTTY() && !IsCI() {
			eff.OnConflict = "ask"
		} else {
			eff.OnConflict = "fail"
		}
	}

	switch eff.LinkMode {
	case "copy", "symlink":
	default:
		return nil, spec.NewPolicyError("invalid link_mode %q (expected copy or symlink)", eff.LinkMode)
	}
	switch eff.OnConflict {
	case "ask", "fail", "backup", "overwrite":
	default:
		return nil, spec.NewPolicyError("invalid on_conflict %q (expected ask, fail, backup, or overwrite)", eff.OnConflict)
	}

	return eff, nil
}

// Canonical source values returned by ResolveTargetLoadGlobal.
const (
	SourceCLI         = "cli"
	SourceAgentfile   = "agentfile"
	SourceLocalConfig = "local_config"
	SourceDefault     = "default"
)

// ResolveTargetLoadGlobal resolves effective load_global for a target using the
// precedence and non-downgrade rules across CLI flags, .agentfile/config.yaml, and Agentfile.yaml.
func ResolveTargetLoadGlobal(
	specCfg spec.TargetConfigSpec,
	eff *EffectiveConfig,
	cliLoadGlobal bool,
	cliLoadGlobalSet bool,
	cliNoGlobal bool,
) (loadGlobal bool, source string, overrideWarn string, err error) {
	if cliLoadGlobalSet && cliLoadGlobal && cliNoGlobal {
		return false, "", "", spec.NewPolicyError("conflicting flags: --load-global and --no-global cannot be combined")
	}

	if cliNoGlobal {
		return false, SourceCLI, "", nil
	}
	if cliLoadGlobalSet {
		if !cliLoadGlobal {
			return false, SourceCLI, "", nil
		}
		var warn string
		if specCfg.LoadGlobal != nil && !*specCfg.LoadGlobal {
			warn = "overriding Agentfile.yaml load_global=false via CLI --load-global"
		}
		return true, SourceCLI, warn, nil
	}

	var localVal *bool
	if eff != nil {
		localVal = eff.LoadGlobal
	}

	if specCfg.LoadGlobal != nil && !*specCfg.LoadGlobal {
		var warn string
		if localVal != nil && *localVal {
			warn = "Agentfile.yaml enforces load_global=false, overriding .agentfile/config.yaml load_global=true"
		}
		return false, SourceAgentfile, warn, nil
	}

	if localVal != nil && !*localVal {
		return false, SourceLocalConfig, "", nil
	}
	if specCfg.LoadGlobal != nil {
		return *specCfg.LoadGlobal, SourceAgentfile, "", nil
	}
	if localVal != nil {
		return *localVal, SourceLocalConfig, "", nil
	}
	return true, SourceDefault, "", nil
}
