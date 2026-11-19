package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newInitCmd(ec *execContext) *cobra.Command {
	var minimal bool
	var full bool
	var force bool
	var importMode bool
	var targets []string
	var dryRun bool
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a starter Agentfile.yaml in the current directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if minimal && full {
				return spec.NewPolicyError("--minimal and --full are mutually exclusive")
			}
			if importMode && (minimal || full) {
				return spec.NewPolicyError("--import cannot be combined with --minimal or --full")
			}
			if !importMode && (dryRun || jsonOut || cmd.Flags().Changed("targets")) {
				return spec.NewPolicyError("--dry-run, --json, and --targets require --import")
			}

			if importMode {
				return runInitImport(ec, targets, force, dryRun, jsonOut)
			}

			targetPath := filepath.Join(ec.workDir, "Agentfile.yaml")
			if _, err := os.Stat(targetPath); err == nil && !force {
				return spec.NewPolicyError("Agentfile.yaml already exists (pass --force to overwrite)")
			}

			detected := detectTargetsOnPath()
			var content string
			if full {
				content = renderFullTemplate(detected)
			} else {
				content = renderMinimalTemplate(detected)
			}

			if err := os.WriteFile(targetPath, []byte(content), 0o644); err != nil {
				return fmt.Errorf("write Agentfile.yaml: %w", err)
			}
			fmt.Fprintf(ec.out, "Created %s (targets: %v)\n", targetPath, detected)
			return nil
		},
	}

	cmd.Flags().BoolVar(&minimal, "minimal", false, "Generate minimal Agentfile.yaml (default)")
	cmd.Flags().BoolVar(&full, "full", false, "Generate full Agentfile.yaml with example sections")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing Agentfile.yaml")
	cmd.Flags().BoolVar(&importMode, "import", false, "Import existing repo-local agent configs into Agentfile.yaml")
	cmd.Flags().StringSliceVar(&targets, "targets", nil, "Comma-separated list of targets for generated Agentfile.yaml (with --import)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview import without writing files (requires --import)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit import summary as JSON (requires --import)")
	return cmd
}

func detectTargetsOnPath() []string {
	candidates := []struct {
		name string
		bin  string
	}{
		{"claude", "claude"},
		{"opencode", "opencode"},
		{"gemini", "gemini"},
		{"cursor", "cursor"},
		{"codex", "codex"},
		{"copilot", "copilot"},
		{"windsurf", "windsurf"},
	}
	var found []string
	for _, c := range candidates {
		if _, err := exec.LookPath(c.bin); err == nil {
			found = append(found, c.name)
		}
	}
	if len(found) == 0 {
		return []string{"claude", "opencode"}
	}
	return found
}

func renderMinimalTemplate(targets []string) string {
	doc := struct {
		Version int      `yaml:"version"`
		Targets []string `yaml:"targets"`
	}{
		Version: 1,
		Targets: targets,
	}
	b, _ := yaml.Marshal(doc)
	return string(b) + "\n# Add mcp, skills, agents, rules, or settings below.\n"
}

func renderFullTemplate(targets []string) string {
	targetsYAML, _ := yaml.Marshal(map[string]any{"targets": targets})
	return "version: 1\n" + string(targetsYAML) + `
mcp:
  filesystem:
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "."]

# skills:
#   - name: my-skill
#     source: ./skills/my-skill
#   - name: remote-skill
#     source: https://github.com/org/agent-skills.git#v1.0.0//skills/remote-skill

# agents:
#   - name: reviewer
#     description: "Reviews code changes"
#     source: ./agents/reviewer.md

# rules:
#   - name: standards
#     source: ./docs/AGENT_RULES.md
#     globs: ["**/*.go"]

settings:
  permissions:
    allow_commands:
      - "go test ./..."
    deny_commands:
      - "rm -rf *"
`
}
