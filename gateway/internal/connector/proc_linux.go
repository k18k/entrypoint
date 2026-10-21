//go:build linux

package connector

import (
	"os/exec"
	"syscall"
)

const isWindows = false

// setParentDeath: the kernel kills frpc when the connector dies (including kill -9).
func setParentDeath(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}

func terminate(cmd *exec.Cmd) { _ = cmd.Process.Signal(syscall.SIGTERM) }
