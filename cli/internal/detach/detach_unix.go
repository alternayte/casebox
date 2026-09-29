//go:build !windows

package detach

import "syscall"

// A new session leaves the agent's process group, so the agent's exit does not signal the child.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
