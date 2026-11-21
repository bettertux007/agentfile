package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bettertux007/agentfile/internal/config"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/bettertux007/agentfile/schemas"
)

type workspaceInspection struct {
	ManifestValid bool
	SchemaErrors  []string
	Warnings      []string
	RawSpec       *spec.ProjectSpec
	Composed      *spec.ComposedManifest
	ProjectSpec   *spec.ProjectSpec
	Lockfile      *spec.Lockfile
	LockStatus    string // "ok" | "not_required" | "missing" | "out_of_sync" | "invalid" | "not_checked"
	LockError     error
	EffectiveCfg  *config.EffectiveConfig
	ConfigSummary *DoctorConfigSummary
	Deps          *store.InspectReport
}

func inspectWorkspace(workDir string, isTTY func() bool, parseOpts store.ParseOptions) (*workspaceInspection, error) {
	ins := &workspaceInspection{
		LockStatus: "not_checked",
	}

	manifestPath := filepath.Join(workDir, "Agentfile.yaml")
	rawManifest, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			ins.SchemaErrors = append(ins.SchemaErrors, "Agentfile.yaml not found")
		} else {
			ins.SchemaErrors = append(ins.SchemaErrors, readErr.Error())
		}
	} else {
		if err := schemas.ValidateAgentfileV1(rawManifest); err != nil {
			ins.SchemaErrors = append(ins.SchemaErrors, fmt.Sprintf("JSON schema: %v", err))
		}
		parsed, perr := spec.ParseRawFile(manifestPath)
		if perr != nil {
			ins.SchemaErrors = append(ins.SchemaErrors, fmt.Sprintf("spec parser: %v", perr))
		} else {
			ins.RawSpec = parsed
			ins.ProjectSpec = parsed
		}
		ins.ManifestValid = len(ins.SchemaErrors) == 0
	}

	effCfg, cerr := config.Resolve(workDir, config.CLIOverrides{}, isTTY)
	if cerr != nil {
		ins.SchemaErrors = append(ins.SchemaErrors, fmt.Sprintf("config: %v", cerr))
	} else {
		ins.EffectiveCfg = effCfg
		ins.Warnings = append(ins.Warnings, effCfg.Warnings...)
		ins.ConfigSummary = &DoctorConfigSummary{
			LinkMode:        effCfg.LinkMode,
			OnConflict:      effCfg.OnConflict,
			GitExclude:      effCfg.GitExclude,
			LoadGlobal:      effCfg.LoadGlobal,
			ConfigPath:      effCfg.ConfigPath,
			LocalConfigPath: effCfg.LocalConfigPath,
			UserConfigPath:  effCfg.UserConfigPath,
			Sources:         effCfg.FieldSources,
		}
	}

	if ins.RawSpec != nil {
		lockPath := filepath.Join(workDir, "Agentfile.lock")
		lf, lerr := spec.LoadLock(lockPath)
		if lerr != nil && !errors.Is(lerr, os.ErrNotExist) {
			ins.LockStatus = "invalid"
			ins.LockError = lerr
			ins.SchemaErrors = append(ins.SchemaErrors, lerr.Error())
		} else {
			if errors.Is(lerr, os.ErrNotExist) {
				lf = nil
			}
			ins.Lockfile = lf

			st, err := store.NewDefaultStore()
			if err != nil {
				return nil, err
			}
			composed, depRep, derr := st.InspectComposedManifest(workDir, ins.RawSpec, lf, parseOpts)
			if derr != nil {
				ins.SchemaErrors = append(ins.SchemaErrors, derr.Error())
				ins.ManifestValid = false
			} else {
				ins.Composed = composed
				ins.ProjectSpec = composed.Effective
				ins.Deps = depRep
				ins.LockStatus = depRep.LockStatus
				ins.LockError = depRep.LockError
			}
		}
	}

	return ins, nil
}

func appendUniqueString(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}
