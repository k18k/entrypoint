//go:build darwin || freebsd

package connector

import (
	"os/exec"
	"syscall"
)

const isWindows = false

func setParentDeath(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(cmd *exec.Cmd) { _ = cmd.Process.Signal(syscall.SIGTERM) }
