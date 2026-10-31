//go:build !linux

package transcode

import "os/exec"

// dieWithParent has no equivalent here; only a clean shutdown kills ffmpeg.
func dieWithParent(*exec.Cmd) {}
