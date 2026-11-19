//go:build !linux

package transcode

import "os"

func shrinkPipe(*os.File) {}
