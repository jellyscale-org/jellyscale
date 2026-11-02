// Package testenv says what an integration test needs and fails the test
// naming it when the machine lacks it. Tests of the integration tier (build tag
// integration, run by test/integration.sh) call it instead of skipping, so a
// missing tool is a failure and never a test that quietly didn't run.
package testenv

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// FFmpeg fails the test unless ffmpeg and ffprobe are installed, and returns
// the path of ffmpeg.
func FFmpeg(t *testing.T) string {
	t.Helper()
	var ffmpeg string
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("this test needs %s on PATH (the integration tier needs ffmpeg with libx264, libx265 and libass)", name)
		}
		if name == "ffmpeg" {
			ffmpeg = path
		}
	}
	return ffmpeg
}

// Font fails the test unless fontconfig finds a font, which burning in a
// subtitle needs to draw any text.
func Font(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("fc-list").Output(); err != nil || len(bytes.TrimSpace(out)) == 0 {
		t.Fatal("this test needs a font that fontconfig finds (fc-list lists none); install fonts-dejavu-core or font-dejavu")
	}
}

// DatabaseURL returns the Postgres test/integration.sh started.
func DatabaseURL(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("JELLYSCALE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("this test needs Postgres: JELLYSCALE_TEST_DATABASE_URL is not set (run test/integration.sh)")
	}
	return dsn
}

// S3 returns the S3 targets test/integration.sh started, each "name=store URI".
func S3(t *testing.T) []string {
	t.Helper()
	var entries []string
	for _, entry := range strings.Split(os.Getenv("JELLYSCALE_TEST_S3"), ",") {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		t.Fatal("this test needs S3: JELLYSCALE_TEST_S3 is not set (run test/integration.sh)")
	}
	return entries
}
