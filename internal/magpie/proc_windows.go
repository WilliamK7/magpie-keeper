//go:build windows

package magpie

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// hideWindow configures cmd on Windows to run without creating a visible console window.
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
