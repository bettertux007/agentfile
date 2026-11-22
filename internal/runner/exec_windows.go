//go:build windows

package runner

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// Run spawns the target child process, ignores os.Interrupt while waiting so interactive
// TUIs receive CTRL_C_EVENT cleanly, and propagates non-zero exit codes via ExitCodeError.
func (OSProcessRunner) Run(bin string, argv []string, env []string, dir string) error {
	cmd := exec.Command(bin, argv...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &ExitCodeError{Code: exitErr.ExitCode(), Err: err}
		}
		return err
	}
	return nil
}
