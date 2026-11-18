//go:build unix

package runner

import (
	"os"

	"golang.org/x/sys/unix"
)

// Run replaces the current process image via unix.Exec after changing into dir if non-empty.
func (OSProcessRunner) Run(bin string, argv []string, env []string, dir string) error {
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			return err
		}
	}
	fullArgv := make([]string, 0, 1+len(argv))
	fullArgv = append(fullArgv, bin)
	fullArgv = append(fullArgv, argv...)
	return unix.Exec(bin, fullArgv, env)
}
