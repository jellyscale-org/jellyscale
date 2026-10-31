package subtitles

import (
	"os/exec"
	"syscall"
)

// DieWithParent makes the kernel kill ffmpeg if the worker is killed outright,
// which would otherwise leave it running to the end of the file.
func DieWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
