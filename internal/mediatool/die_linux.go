package mediatool

import (
	"os/exec"
	"syscall"
)

// dieWithParent makes the kernel kill the tool if the worker is killed outright.
func dieWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
