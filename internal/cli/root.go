package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bettertux007/agentfile/internal/runner"
	"github.com/bettertux007/agentfile/internal/spec"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Build metadata injected via -ldflags.
var (
	Version = "0.1.0-dev"
	Commit  = "none"
	Date    = "unknown"
)

type execContext struct {
	workDir    string
	in         io.Reader
	out        io.Writer
	errOut     io.Writer
	isTTY      func() bool
	verbose    bool
	procRunner runner.ProcessRunner
}

// Execute runs the agentfile CLI and returns the process exit code (0, 1, or 2).
func Execute(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	isTTY := func() bool {
		return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
	}
	return ExecuteWithRunner(args, cwd, os.Stdin, os.Stdout, os.Stderr, isTTY, runner.OSProcessRunner{})
}

// ExecuteWithIO runs the CLI with injected streams and working directory for testing.
func ExecuteWithIO(args []string, workDir string, in io.Reader, out, errOut io.Writer, isTTY func() bool) int {
	return ExecuteWithRunner(args, workDir, in, out, errOut, isTTY, runner.OSProcessRunner{})
}

// ExecuteWithRunner runs the CLI with injected streams, working directory, and ProcessRunner for testing.
func ExecuteWithRunner(args []string, workDir string, in io.Reader, out, errOut io.Writer, isTTY func() bool, pr runner.ProcessRunner) int {
	if pr == nil {
		pr = runner.OSProcessRunner{}
	}
	ec := &execContext{
		workDir:    workDir,
		in:         in,
		out:        out,
		errOut:     errOut,
		isTTY:      isTTY,
		procRunner: pr,
	}

	rootCmd := newRootCmd(ec)
	rootCmd.SetArgs(args)
	rootCmd.SetIn(in)
	rootCmd.SetOut(out)
	rootCmd.SetErr(errOut)

	if err := rootCmd.Execute(); err != nil {
		var exitErr *runner.ExitCodeError
		if errors.As(err, &exitErr) {
			return exitErr.Code
		}
		fmt.Fprintf(errOut, "error: %v\n", err)
		var pe *spec.PolicyError
		if errors.As(err, &pe) || isCobraUsageError(rootCmd, err) {
			return 2
		}
		return 1
	}
	return 0
}

func newRootCmd(ec *execContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "agentfile",
		Short:         "Declarative compiler and dependency hydrator for AI coding agents",
		Version:       fmt.Sprintf("%s (commit=%s date=%s)", Version, Commit, Date),
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.PersistentFlags().BoolVarP(&ec.verbose, "verbose", "v", false, "Enable verbose diagnostic output")

	cmd.AddCommand(
		newInitCmd(ec),
		newSyncCmd(ec),
		newStatusCmd(ec),
		newDiffCmd(ec),
		newCleanCmd(ec),
		newDoctorCmd(ec),
		newRunCmd(ec),
	)
	return cmd
}

func isCobraUsageError(root *cobra.Command, err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, prefix := range []string{
		"unknown flag:",
		"unknown shorthand flag:",
		"unknown command ",
		"flag needs an argument:",
		"invalid argument ",
		"accepts ",
		"requires at least ",
		"requires at most ",
	} {
		if len(msg) >= len(prefix) && msg[:len(prefix)] == prefix {
			return true
		}
	}
	_ = root
	return false
}
