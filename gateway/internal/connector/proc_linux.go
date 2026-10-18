//go:build linux

package connector

import (
	"os/exec"
	"syscall"
)

const isWindows = false

// setParentDeath: jądro zabija frpc, gdy konektor zginie (także przez kill -9).
func setParentDeath(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}

func terminate(cmd *exec.Cmd) { _ = cmd.Process.Signal(syscall.SIGTERM) }
