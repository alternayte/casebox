// Package detach starts this binary again as a background process that outlives its parent,
// so a hook returns at once and an agent that exits does not kill the work.
package detach

import (
	"os"
	"os/exec"
)

// Start runs the current executable with args, detached, with no standard streams.
func Start(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
