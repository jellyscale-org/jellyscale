//go:build !linux

package subtitles

import "os/exec"

// DieWithParent has no equivalent here; only a clean shutdown kills ffmpeg.
func DieWithParent(*exec.Cmd) {}
