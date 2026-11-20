//go:build !windows

package bailian

import "os/exec"

func hideWindow(_ *exec.Cmd) {}
