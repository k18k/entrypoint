//go:build windows

package connector

import "os/exec"

const isWindows = true

func setParentDeath(cmd *exec.Cmd) {}

func terminate(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
