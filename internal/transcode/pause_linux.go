package transcode

import (
	"os"
	"syscall"
)

// pauseProcess stops ffmpeg where it is, or lets it go on. ffmpeg has no
// control for this that works the same in 6.1, 8.1 and 9, and a stopped
// process keeps its input and its place, so a resume is exact.
func pauseProcess(p *os.Process, pause bool) error {
	if pause {
		return p.Signal(syscall.SIGSTOP)
	}
	return p.Signal(syscall.SIGCONT)
}
