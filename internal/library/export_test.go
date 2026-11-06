package library

import "time"

// SetChapterFrameTimeout shortens the time a chapter's frame may take, for a
// test of a hung ffmpeg, and returns what puts it back.
func SetChapterFrameTimeout(d time.Duration) func() {
	old := chapterFrameTimeout
	chapterFrameTimeout = d
	return func() { chapterFrameTimeout = old }
}

// SetReadableTimeout shortens the check that a file can still be read, and
// returns what puts it back.
func SetReadableTimeout(d time.Duration) func() {
	old := readableTimeout
	readableTimeout = d
	return func() { readableTimeout = old }
}
