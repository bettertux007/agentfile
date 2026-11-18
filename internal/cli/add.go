package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bettertux007/agentfile/internal/diff"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/bettertux007/agentfile/internal/store"
	"github.com/bettertux007/agentfile/schemas"
	"github.com/spf13/cobra"
)

func newAddCmd(ec *execContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add or replace a skill, agent, rule, or MCP server in Agentfile.yaml",
	}
	cmd.AddCommand(
		newAddSkillCmd(ec),
		newAddAgentCmd(ec),
		newAddRuleCmd(ec),
		newAddMCPCmd(ec),
	)
	return cmd
}

func newAddSkillCmd(ec *execContext) *cobra.Command {
	var replace bool
	var noSync bool
	var onConflict string
	var allowExternalLocal bool

	cmd := &cobra.Command{
		Use:   "skill <name> <source>",
		Short: "Add or replace a skill entry in Agentfile.yaml",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, source := args[0], args[1]
			manifestPath, origBytes, err := readExistingManifest(ec.workDir)
			if err != nil {
				return err
			}
			candidateBytes, err := spec.AddSkill(origBytes, spec.AddSkillInput{
				Name:    name,
				Source:  source,
				Replace: replace,
			})
			if err != nil {
				return err
			}
			return commitManifestAdd(cmd.Context(), ec, manifestPath, candidateBytes, "skill", name, noSync, onConflict, allowExternalLocal)
		},
	}

	cmd.Flags().BoolVar(&replace, "replace", false, "Overwrite an existing skill with the same name")
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "Update Agentfile.yaml only without syncing workspace or lockfile")
	cmd.Flags().StringVar(&onConflict, "on-conflict", "", "Conflict policy: ask, fail, backup, or overwrite")
	cmd.Flags().BoolVar(&allowExternalLocal, "allow-external-local", false, "Allow local dependency paths outside repository root")
	return cmd
}

func newAddAgentCmd(ec *execContext) *cobra.Command {
	var description string
	var model string
	var replace bool
	var noSync bool
	var onConflict string
	var allowExternalLocal bool

	cmd := &cobra.Command{
		Use:   "agent <name> <source>",
		Short: "Add or replace an agent entry in Agentfile.yaml",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, source := args[0], args[1]
			manifestPath, origBytes, err := readExistingManifest(ec.workDir)
			if err != nil {
				return err
			}
			candidateBytes, err := spec.AddAgent(origBytes, spec.AddAgentInput{
				Name:        name,
				Source:      source,
				Description: description,
				Model:       model,
				Replace:     replace,
			})
			if err != nil {
				return err
			}
			return commitManifestAdd(cmd.Context(), ec, manifestPath, candidateBytes, "agent", name, noSync, onConflict, allowExternalLocal)
		},
	}

	cmd.Flags().StringVar(&description, "description", "", "Agent description")
	cmd.Flags().StringVar(&model, "model", "", "Preferred model for this agent")
	cmd.Flags().BoolVar(&replace, "replace", false, "Overwrite an existing agent with the same name")
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "Update Agentfile.yaml only without syncing workspace or lockfile")
	cmd.Flags().StringVar(&onConflict, "on-conflict", "", "Conflict policy: ask, fail, backup, or overwrite")
	cmd.Flags().BoolVar(&allowExternalLocal, "allow-external-local", false, "Allow local dependency paths outside repository root")
	return cmd
}

func newAddRuleCmd(ec *execContext) *cobra.Command {
	var description string
	var globs []string
	var alwaysApply bool
	var replace bool
	var noSync bool
	var onConflict string
	var allowExternalLocal bool

	cmd := &cobra.Command{
		Use:   "rule <name> <source>",
		Short: "Add or replace a rule entry in Agentfile.yaml",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, source := args[0], args[1]
			manifestPath, origBytes, err := readExistingManifest(ec.workDir)
			if err != nil {
				return err
			}
			candidateBytes, err := spec.AddRule(origBytes, spec.AddRuleInput{
				Name:        name,
				Source:      source,
				Description: description,
				Globs:       globs,
				AlwaysApply: alwaysApply,
				Replace:     replace,
			})
			if err != nil {
				return err
			}
			return commitManifestAdd(cmd.Context(), ec, manifestPath, candidateBytes, "rule", name, noSync, onConflict, allowExternalLocal)
		},
	}

	cmd.Flags().StringVar(&description, "description", "", "Rule description")
	cmd.Flags().StringSliceVar(&globs, "globs", nil, "Comma-separated glob patterns for scoped rules")
	cmd.Flags().BoolVar(&alwaysApply, "always-apply", false, "Always apply rule across the workspace")
	cmd.Flags().BoolVar(&replace, "replace", false, "Overwrite an existing rule with the same name")
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "Update Agentfile.yaml only without syncing workspace or lockfile")
	cmd.Flags().StringVar(&onConflict, "on-conflict", "", "Conflict policy: ask, fail, backup, or overwrite")
	cmd.Flags().BoolVar(&allowExternalLocal, "allow-external-local", false, "Allow local dependency paths outside repository root")
	return cmd
}

func newAddMCPCmd(ec *execContext) *cobra.Command {
	var transport string
	var urlStr string
	var envFlags []string
	var envRefFlags []string
	var headerFlags []string
	var headerEnvFlags []string
	var replace bool
	var noSync bool
	var onConflict string
	var allowExternalLocal bool

	cmd := &cobra.Command{
		Use:   "mcp <name> [flags] [-- <command> [args...]]",
		Short: "Add or replace an MCP server entry in Agentfile.yaml",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dashIdx := cmd.ArgsLenAtDash()
			var hasCmd bool
			var command string
			var cmdArgs []string

			if dashIdx >= 0 {
				if dashIdx != 1 {
					return spec.NewPolicyError("expected exactly one MCP server name before '--'")
				}
				tail := args[dashIdx:]
				if len(tail) == 0 || strings.TrimSpace(tail[0]) == "" {
					return spec.NewPolicyError("missing command after '--'")
				}
				hasCmd = true
				command = tail[0]
				cmdArgs = tail[1:]
			} else if len(args) != 1 {
				return spec.NewPolicyError("accepts 1 arg(s) (server name), received %d; use '-- <command> [args...]' for stdio servers", len(args))
			}

			name := args[0]
			hasURL := strings.TrimSpace(urlStr) != ""
			if hasCmd && hasURL {
				return spec.NewPolicyError("cannot specify both '-- <command>' and '--url'")
			}

			resolvedTransport := strings.TrimSpace(transport)
			if resolvedTransport == "" {
				switch {
				case hasCmd:
					resolvedTransport = "stdio"
				case hasURL:
					resolvedTransport = "http"
				default:
					return spec.NewPolicyError("cannot infer MCP transport: pass '-- <command> [args...]' for stdio or '--url <url>' for http/sse")
				}
			}

			switch resolvedTransport {
			case "stdio":
				if !hasCmd {
					return spec.NewPolicyError("stdio transport requires '-- <command> [args...]'")
				}
				if len(headerFlags) > 0 || len(headerEnvFlags) > 0 {
					return spec.NewPolicyError("stdio transport does not accept '--header' or '--header-env'")
				}
			case "http", "sse":
				if !hasURL {
					return spec.NewPolicyError("%s transport requires '--url'", resolvedTransport)
				}
				if len(envFlags) > 0 || len(envRefFlags) > 0 {
					return spec.NewPolicyError("%s transport does not accept '--env' or '--env-ref'", resolvedTransport)
				}
			default:
				return spec.NewPolicyError("unsupported transport %q (expected stdio, http, or sse)", resolvedTransport)
			}

			envMap, err := parseMCPEnvFlags(envFlags, envRefFlags)
			if err != nil {
				return err
			}
			headerMap, err := parseMCPHeaderFlags(headerFlags, headerEnvFlags)
			if err != nil {
				return err
			}

			if hasURL {
				if diff.ContainsLiteralSecret(urlStr) {
					return spec.NewPolicyError("refusing literal secret in --url (use '${VAR}' placeholder instead)")
				}
				if _, err := spec.ParseEnvString(urlStr); err != nil {
					return err
				}
			}
			if hasCmd {
				if diff.ContainsLiteralSecret(command) {
					return spec.NewPolicyError("refusing literal secret in MCP command (use '${VAR}' placeholder instead)")
				}
				if _, err := spec.ParseEnvString(command); err != nil {
					return err
				}
				for _, a := range cmdArgs {
					if diff.ContainsLiteralSecret(a) {
						return spec.NewPolicyError("refusing literal secret in MCP command args (use '--env-ref' or '${VAR}' placeholder instead)")
					}
					if _, err := spec.ParseEnvString(a); err != nil {
						return err
					}
				}
			}

			manifestPath, origBytes, err := readExistingManifest(ec.workDir)
			if err != nil {
				return err
			}
			candidateBytes, err := spec.AddMCP(origBytes, spec.AddMCPInput{
				Name:      name,
				Transport: resolvedTransport,
				Command:   command,
				URL:       urlStr,
				Args:      cmdArgs,
				Env:       envMap,
				Headers:   headerMap,
				Replace:   replace,
			})
			if err != nil {
				return err
			}
			return commitManifestAdd(cmd.Context(), ec, manifestPath, candidateBytes, "mcp", name, noSync, onConflict, allowExternalLocal)
		},
	}

	cmd.Flags().StringVar(&transport, "transport", "", "MCP transport: stdio, http, or sse (inferred from '-- <cmd>' or '--url' if omitted)")
	cmd.Flags().StringVar(&urlStr, "url", "", "MCP endpoint URL for http/sse transport")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "Environment variable KEY=VAL for stdio transport")
	cmd.Flags().StringArrayVar(&envRefFlags, "env-ref", nil, "Environment variable name VAR emitted as VAR: \"${VAR}\"")
	cmd.Flags().StringArrayVar(&headerFlags, "header", nil, "HTTP header 'Name: Value' for http/sse transport")
	cmd.Flags().StringArrayVar(&headerEnvFlags, "header-env", nil, "Header env reference VAR:HEADER (emits Bearer ${VAR} for Authorization)")
	cmd.Flags().BoolVar(&replace, "replace", false, "Overwrite an existing MCP server with the same name")
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "Update Agentfile.yaml only without syncing workspace or lockfile")
	cmd.Flags().StringVar(&onConflict, "on-conflict", "", "Conflict policy: ask, fail, backup, or overwrite")
	cmd.Flags().BoolVar(&allowExternalLocal, "allow-external-local", false, "Allow local dependency paths outside repository root")
	return cmd
}

func parseMCPEnvFlags(envFlags, envRefFlags []string) (map[string]string, error) {
	if len(envFlags) == 0 && len(envRefFlags) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(envFlags)+len(envRefFlags))
	for _, raw := range envFlags {
		key, val, ok := strings.Cut(raw, "=")
		if !ok {
			return nil, spec.NewPolicyError("invalid --env %q (expected KEY=VAL)", raw)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, spec.NewPolicyError("invalid --env %q: empty key", raw)
		}
		if strings.TrimSpace(val) == "" {
			return nil, spec.NewPolicyError("refusing empty --env value for %s (use --env-ref %s or single-quoted '${%s}' to avoid shell expansion)", key, key, key)
		}
		if diff.ContainsLiteralSecret(key + "=" + val) {
			return nil, spec.NewPolicyError("refusing literal secret in --env %s (use --env-ref %s or single-quoted '${%s}' placeholder)", key, key, key)
		}
		if _, err := spec.ParseEnvString(val); err != nil {
			return nil, err
		}
		if _, exists := out[key]; exists {
			return nil, spec.NewPolicyError("duplicate --env key %q", key)
		}
		out[key] = val
	}
	for _, raw := range envRefFlags {
		varName := strings.TrimSpace(raw)
		if varName == "" {
			return nil, spec.NewPolicyError("invalid --env-ref: variable name cannot be empty")
		}
		ph := "${" + varName + "}"
		if _, err := spec.ParseEnvString(ph); err != nil {
			return nil, spec.NewPolicyError("invalid --env-ref %q: %v", raw, err)
		}
		if _, exists := out[varName]; exists {
			return nil, spec.NewPolicyError("duplicate env key %q", varName)
		}
		out[varName] = ph
	}
	return out, nil
}

func parseMCPHeaderFlags(headerFlags, headerEnvFlags []string) (map[string]string, error) {
	if len(headerFlags) == 0 && len(headerEnvFlags) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(headerFlags)+len(headerEnvFlags))
	seenLower := make(map[string]string, len(headerFlags)+len(headerEnvFlags))

	addHeader := func(name, val string) error {
		lower := strings.ToLower(name)
		if prev, exists := seenLower[lower]; exists {
			return spec.NewPolicyError("duplicate HTTP header %q (conflicts with %q)", name, prev)
		}
		seenLower[lower] = name
		out[name] = val
		return nil
	}

	for _, raw := range headerFlags {
		name, val, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, spec.NewPolicyError("invalid --header %q (expected 'Name: Value')", raw)
		}
		name = strings.TrimSpace(name)
		val = strings.TrimSpace(val)
		if name == "" {
			return nil, spec.NewPolicyError("invalid --header %q: empty header name", raw)
		}
		if val == "" {
			return nil, spec.NewPolicyError("refusing empty --header value for %s (use --header-env VAR:%s or single-quoted '${VAR}' to avoid shell expansion)", name, name)
		}
		if diff.ContainsLiteralSecret(name + ": " + val) {
			return nil, spec.NewPolicyError("refusing literal secret in --header %s (use --header-env VAR:%s or single-quoted '${VAR}' placeholder)", name, name)
		}
		if _, err := spec.ParseEnvString(val); err != nil {
			return nil, err
		}
		if err := addHeader(name, val); err != nil {
			return nil, err
		}
	}

	for _, raw := range headerEnvFlags {
		varPart, hdrPart, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, spec.NewPolicyError("invalid --header-env %q (expected VAR:HEADER)", raw)
		}
		varName := strings.TrimSpace(varPart)
		hdrName := strings.TrimSpace(hdrPart)
		if varName == "" || hdrName == "" {
			return nil, spec.NewPolicyError("invalid --header-env %q (expected VAR:HEADER)", raw)
		}
		ph := "${" + varName + "}"
		if _, err := spec.ParseEnvString(ph); err != nil {
			return nil, spec.NewPolicyError("invalid --header-env %q: %v", raw, err)
		}
		hdrVal := ph
		if strings.EqualFold(hdrName, "Authorization") {
			hdrName = "Authorization"
			hdrVal = "Bearer " + ph
		}
		if err := addHeader(hdrName, hdrVal); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func readExistingManifest(workDir string) (string, []byte, error) {
	manifestPath := filepath.Join(workDir, "Agentfile.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, spec.NewPolicyError("Agentfile.yaml not found in %s (run 'agentfile init' first)", workDir)
		}
		return "", nil, err
	}
	return manifestPath, data, nil
}

func commitManifestAdd(
	ctx context.Context,
	ec *execContext,
	manifestPath string,
	candidateBytes []byte,
	kind, name string,
	noSync bool,
	onConflict string,
	allowExternalLocal bool,
) error {
	if err := schemas.ValidateAgentfileV1(candidateBytes); err != nil {
		return spec.NewPolicyError("invalid Agentfile.yaml: %v", err)
	}
	candidateSpec, err := spec.Parse(bytes.NewReader(candidateBytes))
	if err != nil {
		return err
	}

	parseOpts := store.ParseOptions{
		AllowExternalLocal: allowExternalLocal,
		AllowFileScheme:    os.Getenv("AGENTFILE_ALLOW_FILE_GIT") == "1",
	}

	if src, ok := findSourceInSpec(candidateSpec, kind, name); ok {
		if _, err := store.ParseSource(src, ec.workDir, parseOpts); err != nil {
			return err
		}
	}

	if noSync {
		if err := writeFileAtomic(manifestPath, candidateBytes, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(ec.out, "Updated %s (%s %s; --no-sync)\n", manifestPath, kind, name)
		return nil
	}

	_, err = executeSyncWithSpec(ctx, ec, syncOptions{
		OnConflict:         onConflict,
		AllowExternalLocal: allowExternalLocal,
		ManifestBytes:      candidateBytes,
	}, candidateSpec, store.ResolveOptions{
		ReconcileKey: spec.LockKey(kind, name),
		ParseOpts:    parseOpts,
	})
	return err
}

func findSourceInSpec(ps *spec.ProjectSpec, kind, name string) (spec.SourceSpec, bool) {
	switch kind {
	case "skill":
		for _, sk := range ps.Skills {
			if sk.Name == name {
				return sk.Source, true
			}
		}
	case "agent":
		for _, ag := range ps.Agents {
			if ag.Name == name {
				return ag.Source, true
			}
		}
	case "rule":
		for _, rl := range ps.Rules {
			if rl.Name == name {
				return rl.Source, true
			}
		}
	}
	return spec.SourceSpec{}, false
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agentfile-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
