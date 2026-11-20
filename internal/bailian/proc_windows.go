//go:build windows

package bailian

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// hideWindow prevents console window flashes when launching Node subprocesses on Windows.
func hideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
