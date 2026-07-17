//go:build !windows

package tsutil

import "os/exec"

func hideCmdWindow(cmd *exec.Cmd) {}
