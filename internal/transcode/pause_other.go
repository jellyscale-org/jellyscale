//go:build !linux

package transcode

import (
	"errors"
	"os"
)

// pauseProcess has no equivalent here; a job then runs without a lead limit.
func pauseProcess(*os.Process, bool) error {
	return errors.New("pausing ffmpeg is not supported on this system")
}
