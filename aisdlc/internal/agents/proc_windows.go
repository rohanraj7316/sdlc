//go:build windows

package agents

import "os/exec"

// setProcessGroup is a no-op on Windows; the default Kill terminates only the direct child.
func setProcessGroup(cmd *exec.Cmd) {}
