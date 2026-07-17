//go:build windows

package tsutil

import (
	"os/exec"
	"syscall"
)

// hideCmdWindow prevents a console window from flashing when the GUI (built
// with -H=windowsgui) shells out to the tailscale CLI.
func hideCmdWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
