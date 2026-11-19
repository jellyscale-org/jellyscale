package transcode

import (
	"os"

	"golang.org/x/sys/unix"
)

// shrinkPipe makes the pipe hold one page, so that what ffmpeg has not taken is
// little, and so is what has been read from the store for it. A pipe that stays at
// its default of 64 KiB reads that much of the stored track ahead.
func shrinkPipe(f *os.File) {
	// Through SyscallConn, not Fd, which would make the file block for good.
	if raw, err := f.SyscallConn(); err == nil {
		raw.Control(func(fd uintptr) { //nolint:errcheck // Only a limit on read-ahead.
			unix.FcntlInt(fd, unix.F_SETPIPE_SZ, os.Getpagesize()) //nolint:errcheck
		})
	}
}
