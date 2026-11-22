package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bettertux007/agentfile/internal/spec"
	"gopkg.in/yaml.v3"
)

// LocalConfig represents .agentfile/config.yaml or ~/.config/agentfile/config.yaml (version 1).
type LocalConfig struct {
	Version    int    `yaml:"version"`
	LinkMode   string `yaml:"link_mode,omitempty"`
	OnConflict string `yaml:"on_conflict,omitempty"`
	GitExclude *bool  `yaml:"git_exclude,omitempty"`
	LoadGlobal *bool  `yaml:"load_global,omitempty"`
}

// CLIOverrides captures CLI flags that override .agentfile/config.yaml and user-global config.
type CLIOverrides struct {
	LinkMode      string
	Copy          bool
	OnConflict    string
	Force         bool
	GitExclude    bool
	GitExcludeSet bool
}

// ResolveOptions controls user-global config discovery and CI detection during ResolveWithOptions.
type ResolveOptions struct {
	UserConfigPath    string
	DisableUserConfig bool
	IgnoreCI          bool
}

// EffectiveConfig is the resolved machine-local tool configuration.
type EffectiveConfig struct {
	LinkMode          string
	OnConflict        string
	GitExclude        bool
	LoadGlobal        *bool
	ConfigPath        string
	UserConfigPath    string
	UserConfigLoaded  bool
	LocalConfigPath   string
	LocalConfigLoaded bool
	UserLoadGlobal    *bool
	LocalLoadGlobal   *bool
	FieldSources      map[string]string
	Warnings          []string
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

// DefaultUserConfigPath returns the user-global config path (~/.config/agentfile/config.yaml)
// when user-global config discovery is active.
//
// Precedence:
//  1. AGENTFILE_NO_USER_CONFIG=1 -> ("", false, nil)
//  2. AGENTFILE_CONFIG_FILE=<path> -> fails closed with PolicyError if missing or a directory
//  3. IsCI() -> ("", false, nil)
//  4. $XDG_CONFIG_HOME/agentfile/config.yaml
//  5. $HOME/.config/agentfile/config.yaml (Unix/macOS)
//  6. os.UserConfigDir()/agentfile/config.yaml fallback
func DefaultUserConfigPath() (path string, ok bool, err error) {
	return defaultUserConfigPath(false)
}

func defaultUserConfigPath(ignoreCI bool) (path string, ok bool, err error) {
	if strings.TrimSpace(os.Getenv("AGENTFILE_NO_USER_CONFIG")) == "1" {
		return "", false, nil
	}
	if explicit := strings.TrimSpace(os.Getenv("AGENTFILE_CONFIG_FILE")); explicit != "" {
		info, statErr := os.Stat(explicit)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return "", false, spec.NewPolicyError("user config file %s does not exist (AGENTFILE_CONFIG_FILE)", explicit)
			}
			return "", false, &spec.PolicyError{
				Message: fmt.Sprintf("cannot access user config file %s (AGENTFILE_CONFIG_FILE)", explicit),
				Err:     statErr,
			}
		}
		if info.IsDir() {
			return "", false, spec.NewPolicyError("user config path %s is a directory (AGENTFILE_CONFIG_FILE)", explicit)
		}
		return explicit, true, nil
	}
	if !ignoreCI && IsCI() {
		return "", false, nil
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "agentfile", "config.yaml"), true, nil
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return filepath.Join(home, ".config", "agentfile", "config.yaml"), true, nil
	}
	cfgDir, uerr := os.UserConfigDir()
	if uerr != nil || strings.TrimSpace(cfgDir) == "" {
		return "", false, nil
	}
	return filepath.Join(cfgDir, "agentfile", "config.yaml"), true, nil
}

func loadLocalConfigFile(path string, required bool) (*LocalConfig, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if required {
				return nil, false, spec.NewPolicyError("config file %s does not exist", path)
			}
			return nil, false, nil
		}
		if required {
			return nil, false, &spec.PolicyError{
				Message: fmt.Sprintf("read config file %s", path),
				Err:     err,
			}
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg LocalConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, false, spec.NewPolicyError("%s is empty", path)
		}
		return nil, false, spec.NewPolicyError("invalid %s: %v", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, false, spec.NewPolicyError("%s must contain a single YAML document", path)
	}
	if cfg.Version != 1 {
		return nil, false, spec.NewPolicyError("unsupported %s version %d (expected 1)", path, cfg.Version)
	}
	if cfg.LinkMode != "" && cfg.LinkMode != "copy" && cfg.LinkMode != "symlink" {
		return nil, false, spec.NewPolicyError("invalid link_mode %q in %s (expected copy or symlink)", cfg.LinkMode, path)
	}
	if cfg.OnConflict != "" {
		switch cfg.OnConflict {
		case "ask", "fail", "backup", "overwrite":
		default:
			return nil, false, spec.NewPolicyError("invalid on_conflict %q in %s (expected ask, fail, backup, or overwrite)", cfg.OnConflict, path)
		}
	}
	return &cfg, true, nil
}

// Resolve loads optional user-global and repo-local config.yaml files and applies CLI overrides and defaults.
func Resolve(projectRoot string, cli CLIOverrides, isTTY func() bool) (*EffectiveConfig, error) {
	return ResolveWithOptions(projectRoot, cli, isTTY, ResolveOptions{})
}

// ResolveWithOptions merges 4 configuration layers (default -> user_config -> local_config -> CLI)
// and records field-level provenance and non-fatal warnings.
func ResolveWithOptions(projectRoot string, cli CLIOverrides, isTTY func() bool, opts ResolveOptions) (*EffectiveConfig, error) {
	if cli.Copy && cli.LinkMode != "" && cli.LinkMode != "copy" {
		return nil, spec.NewPolicyError("conflicting flags: --copy cannot be combined with --link-mode=%s", cli.LinkMode)
	}
	if cli.Force && cli.OnConflict != "" && cli.OnConflict != "overwrite" {
		return nil, spec.NewPolicyError("conflicting flags: --force cannot be combined with --on-conflict=%s", cli.OnConflict)
	}

	var (
		userPath   string
		userCfg    *LocalConfig
		userLoaded bool
		err        error
	)
	if !opts.DisableUserConfig {
		if opts.UserConfigPath != "" {
			userPath = opts.UserConfigPath
			userCfg, userLoaded, err = loadLocalConfigFile(userPath, true)
			if err != nil {
				return nil, err
			}
		} else {
			p, ok, derr := defaultUserConfigPath(opts.IgnoreCI)
			if derr != nil {
				return nil, derr
			}
			if ok {
				userPath = p
				required := strings.TrimSpace(os.Getenv("AGENTFILE_CONFIG_FILE")) != ""
				userCfg, userLoaded, err = loadLocalConfigFile(userPath, required)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	localPath := filepath.Join(projectRoot, ".agentfile", "config.yaml")
	localCfg, localLoaded, err := loadLocalConfigFile(localPath, false)
	if err != nil {
		return nil, err
	}

	fieldSources := map[string]string{
		"link_mode":   SourceDefault,
		"on_conflict": SourceDefault,
		"git_exclude": SourceDefault,
		"load_global": SourceDefault,
	}

	eff := &EffectiveConfig{
		LinkMode:          "copy",
		GitExclude:        true,
		ConfigPath:        localPath,
		UserConfigPath:    userPath,
		UserConfigLoaded:  userLoaded,
		LocalConfigPath:   localPath,
		LocalConfigLoaded: localLoaded,
		FieldSources:      fieldSources,
	}

	if userLoaded && userCfg != nil {
		if userCfg.LinkMode != "" {
			eff.LinkMode = userCfg.LinkMode
			eff.FieldSources["link_mode"] = SourceUserConfig
		}
		if userCfg.OnConflict != "" {
			eff.OnConflict = userCfg.OnConflict
			eff.FieldSources["on_conflict"] = SourceUserConfig
		}
		if userCfg.GitExclude != nil {
			eff.GitExclude = *userCfg.GitExclude
			eff.FieldSources["git_exclude"] = SourceUserConfig
		}
		if userCfg.LoadGlobal != nil {
			eff.UserLoadGlobal = userCfg.LoadGlobal
			eff.LoadGlobal = userCfg.LoadGlobal
			eff.FieldSources["load_global"] = SourceUserConfig
		}
	}

	if localLoaded && localCfg != nil {
		if localCfg.LinkMode != "" {
			eff.LinkMode = localCfg.LinkMode
			eff.FieldSources["link_mode"] = SourceLocalConfig
		}
		if localCfg.OnConflict != "" {
			eff.OnConflict = localCfg.OnConflict
			eff.FieldSources["on_conflict"] = SourceLocalConfig
		}
		if localCfg.GitExclude != nil {
			eff.GitExclude = *localCfg.GitExclude
			eff.FieldSources["git_exclude"] = SourceLocalConfig
		}
		if localCfg.LoadGlobal != nil {
			eff.LocalLoadGlobal = localCfg.LoadGlobal
			eff.LoadGlobal = localCfg.LoadGlobal
			eff.FieldSources["load_global"] = SourceLocalConfig
		}
	}

	if cli.Copy {
		eff.LinkMode = "copy"
		eff.FieldSources["link_mode"] = SourceCLI
	} else if cli.LinkMode != "" {
		eff.LinkMode = cli.LinkMode
		eff.FieldSources["link_mode"] = SourceCLI
	}

	if cli.Force {
		eff.OnConflict = "overwrite"
		eff.FieldSources["on_conflict"] = SourceCLI
	} else if cli.OnConflict != "" {
		eff.OnConflict = cli.OnConflict
		eff.FieldSources["on_conflict"] = SourceCLI
	}

	if cli.GitExcludeSet {
		eff.GitExclude = cli.GitExclude
		eff.FieldSources["git_exclude"] = SourceCLI
	}

	if eff.OnConflict == "" {
		ci := !opts.IgnoreCI && IsCI()
		if isTTY != nil && isTTY() && !ci {
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

	if eff.OnConflict == "overwrite" && eff.FieldSources["on_conflict"] == SourceUserConfig {
		eff.Warnings = append(eff.Warnings, fmt.Sprintf("user config (%s) sets on_conflict=overwrite; unmanaged local edits may be overwritten without prompting", eff.UserConfigPath))
	}

	return eff, nil
}

// Canonical source values returned by ResolveTargetLoadGlobal and EffectiveConfig.FieldSources.
const (
	SourceCLI         = "cli"
	SourceAgentfile   = "agentfile"
	SourceLocalConfig = "local_config"
	SourceUserConfig  = "user_config"
	SourceDefault     = "default"
)

// ResolveTargetLoadGlobal resolves effective load_global for a target using the
// precedence and non-downgrade rules:
// CLI > Agentfile(false) > local_config > Agentfile(true) > user_config > default(true).
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

	var (
		localVal *bool
		userVal  *bool
	)
	if eff != nil {
		userVal = eff.UserLoadGlobal
		localVal = eff.LocalLoadGlobal
		if localVal == nil && eff.LoadGlobal != nil && userVal == nil {
			localVal = eff.LoadGlobal
		}
	}

	if specCfg.LoadGlobal != nil && !*specCfg.LoadGlobal {
		var overridden []string
		if localVal != nil && *localVal {
			overridden = append(overridden, "local_config (.agentfile/config.yaml)")
		}
		if userVal != nil && *userVal && (localVal == nil || *localVal) {
			overridden = append(overridden, "user_config")
		}
		var warn string
		if len(overridden) > 0 {
			warn = fmt.Sprintf("Agentfile.yaml enforces load_global=false, overriding %s load_global=true", strings.Join(overridden, " and "))
		}
		return false, SourceAgentfile, warn, nil
	}

	if localVal != nil {
		return *localVal, SourceLocalConfig, "", nil
	}
	if specCfg.LoadGlobal != nil {
		return *specCfg.LoadGlobal, SourceAgentfile, "", nil
	}
	if userVal != nil {
		return *userVal, SourceUserConfig, "", nil
	}
	return true, SourceDefault, "", nil
}
