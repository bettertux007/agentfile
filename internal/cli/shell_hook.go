package cli

import (
	"fmt"
	"strings"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/adapters/registry"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/spf13/cobra"
)

func newShellHookCmd(ec *execContext) *cobra.Command {
	var targets []string
	var onlyIsolated bool

	cmd := &cobra.Command{
		Use:   "shell-hook <zsh|bash|fish>",
		Short: "Emit opt-in shell wrapper functions routing bare target CLIs through agentfile run",
		Long: `Emit opt-in shell wrapper functions for AI coding agent CLIs.

Add to your shell startup file:
  zsh (~/.zshrc):                 eval "$(agentfile shell-hook zsh)"
  bash (~/.bashrc):               eval "$(agentfile shell-hook bash)"
  fish (~/.config/fish/config.fish): agentfile shell-hook fish | source`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			shellName := strings.ToLower(strings.TrimSpace(args[0]))
			switch shellName {
			case "zsh", "bash", "fish":
			default:
				return spec.NewPolicyError("unsupported shell %q (expected zsh, bash, or fish)", args[0])
			}

			reg := registry.Default()
			var selected []adapters.Adapter
			if len(targets) > 0 {
				seen := make(map[string]bool)
				for _, t := range targets {
					t = strings.TrimSpace(t)
					adp, err := registry.Lookup(reg, t)
					if err != nil {
						return spec.NewPolicyError("target %q is not supported (available: %v)", t, registry.Names(reg))
					}
					if !seen[adp.Name()] {
						seen[adp.Name()] = true
						selected = append(selected, adp)
					}
				}
			} else {
				for _, name := range registry.Names(reg) {
					adp, _ := registry.Lookup(reg, name)
					if adp == nil || adp.SchemaContract().BinaryOptional {
						continue
					}
					selected = append(selected, adp)
				}
			}

			script := renderShellHook(shellName, selected, onlyIsolated)
			_, err := fmt.Fprint(ec.out, script)
			return err
		},
	}

	cmd.Flags().StringSliceVar(&targets, "targets", nil, "Comma-separated subset of targets to wrap (defaults to non-IDE CLI targets)")
	cmd.Flags().BoolVar(&onlyIsolated, "only-isolated", false, "Only route through agentfile run when target resolves load_global=false")
	return cmd
}

func renderShellHook(shellName string, selected []adapters.Adapter, onlyIsolated bool) string {
	var b strings.Builder
	runFlags := "--if-configured"
	if onlyIsolated {
		runFlags += " --only-isolated"
	}

	if shellName == "fish" {
		b.WriteString("# agentfile shell-hook (fish)\n")
		b.WriteString("function _agentfile_find_root\n")
		b.WriteString("  set -l dir $PWD\n")
		b.WriteString("  while test -n \"$dir\" -a \"$dir\" != \"/\" -a \"$dir\" != \"$HOME\"\n")
		b.WriteString("    if test -f \"$dir/Agentfile.yaml\"\n")
		b.WriteString("      printf '%s\\n' \"$dir\"\n")
		b.WriteString("      return 0\n")
		b.WriteString("    end\n")
		b.WriteString("    if test -e \"$dir/.git\"\n")
		b.WriteString("      return 1\n")
		b.WriteString("    end\n")
		b.WriteString("    set dir (dirname \"$dir\")\n")
		b.WriteString("  end\n")
		b.WriteString("  return 1\n")
		b.WriteString("end\n")

		for _, adp := range selected {
			bin := adp.BinaryName()
			target := adp.Name()
			b.WriteString("\n")
			fmt.Fprintf(&b, "functions -e %s 2>/dev/null; or true\n", bin)
			fmt.Fprintf(&b, "function %s --wraps=%s\n", bin, bin)
			fmt.Fprintf(&b, "  if test \"$AGENTFILE_NO_HOOK\" = \"1\" -o -n \"$CI\"\n")
			fmt.Fprintf(&b, "    command %s $argv\n", bin)
			fmt.Fprintf(&b, "    return $status\n")
			fmt.Fprintf(&b, "  end\n")
			fmt.Fprintf(&b, "  if not command -sq agentfile\n")
			fmt.Fprintf(&b, "    command %s $argv\n", bin)
			fmt.Fprintf(&b, "    return $status\n")
			fmt.Fprintf(&b, "  end\n")
			fmt.Fprintf(&b, "  set -l _af_root (_agentfile_find_root)\n")
			fmt.Fprintf(&b, "  if test $status -ne 0 -o -z \"$_af_root\"\n")
			fmt.Fprintf(&b, "    command %s $argv\n", bin)
			fmt.Fprintf(&b, "    return $status\n")
			fmt.Fprintf(&b, "  end\n")
			fmt.Fprintf(&b, "  if test \"$AGENTFILE_ACTIVE\" = \"1\" -a \"$AGENTFILE_ROOT\" = \"$_af_root\"\n")
			fmt.Fprintf(&b, "    command %s $argv\n", bin)
			fmt.Fprintf(&b, "    return $status\n")
			fmt.Fprintf(&b, "  end\n")
			fmt.Fprintf(&b, "  command agentfile run %s --project-root \"$_af_root\" %s $argv\n", runFlags, target)
			fmt.Fprintf(&b, "end\n")
		}
		return b.String()
	}

	// zsh or bash
	fmt.Fprintf(&b, "# agentfile shell-hook (%s)\n", shellName)
	b.WriteString("_agentfile_find_root() {\n")
	b.WriteString("  local dir=\"$PWD\"\n")
	b.WriteString("  while [ -n \"$dir\" ] && [ \"$dir\" != \"/\" ] && [ \"$dir\" != \"${HOME:-}\" ]; do\n")
	b.WriteString("    if [ -f \"$dir/Agentfile.yaml\" ]; then\n")
	b.WriteString("      printf '%s\\n' \"$dir\"\n")
	b.WriteString("      return 0\n")
	b.WriteString("    fi\n")
	b.WriteString("    if [ -e \"$dir/.git\" ]; then\n")
	b.WriteString("      return 1\n")
	b.WriteString("    fi\n")
	b.WriteString("    dir=$(dirname \"$dir\")\n")
	b.WriteString("  done\n")
	b.WriteString("  return 1\n")
	b.WriteString("}\n")

	for _, adp := range selected {
		bin := adp.BinaryName()
		target := adp.Name()
		b.WriteString("\n")
		fmt.Fprintf(&b, "unalias %s 2>/dev/null || true\n", bin)
		fmt.Fprintf(&b, "%s() {\n", bin)
		fmt.Fprintf(&b, "  if [ \"${AGENTFILE_NO_HOOK:-}\" = \"1\" ] || [ -n \"${CI:-}\" ]; then\n")
		fmt.Fprintf(&b, "    command %s \"$@\"\n", bin)
		fmt.Fprintf(&b, "    return $?\n")
		fmt.Fprintf(&b, "  fi\n")
		fmt.Fprintf(&b, "  if ! command -v agentfile >/dev/null 2>&1; then\n")
		fmt.Fprintf(&b, "    command %s \"$@\"\n", bin)
		fmt.Fprintf(&b, "    return $?\n")
		fmt.Fprintf(&b, "  fi\n")
		fmt.Fprintf(&b, "  local _af_root\n")
		fmt.Fprintf(&b, "  if ! _af_root=$(_agentfile_find_root); then\n")
		fmt.Fprintf(&b, "    command %s \"$@\"\n", bin)
		fmt.Fprintf(&b, "    return $?\n")
		fmt.Fprintf(&b, "  fi\n")
		fmt.Fprintf(&b, "  if [ \"${AGENTFILE_ACTIVE:-}\" = \"1\" ] && [ \"${AGENTFILE_ROOT:-}\" = \"$_af_root\" ]; then\n")
		fmt.Fprintf(&b, "    command %s \"$@\"\n", bin)
		fmt.Fprintf(&b, "    return $?\n")
		fmt.Fprintf(&b, "  fi\n")
		fmt.Fprintf(&b, "  command agentfile run %s --project-root \"$_af_root\" %s \"$@\"\n", runFlags, target)
		fmt.Fprintf(&b, "}\n")
	}
	return b.String()
}
