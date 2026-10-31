package transcode

import (
	"os/exec"
	"syscall"
)

// dieWithParent makes the kernel kill ffmpeg if the worker is killed outright,
// which would otherwise leave it running to the end of the file.
func dieWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
