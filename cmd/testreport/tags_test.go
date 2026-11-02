package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A mistyped build tag hides a file from every tier without a skip or an error,
// and `go test -list` can't see it, because it reads the same tags. So a file's
// name and its tag have to agree, and the only tags are the two the tiers use.
func TestBuildTags(t *testing.T) {
	root := filepath.Join("..", "..")
	found := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		found++
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tag := ""
		for line := range strings.Lines(string(source)) {
			if strings.HasPrefix(line, "package ") {
				break
			}
			if rest, ok := strings.CutPrefix(line, "//go:build"); ok {
				tag = strings.TrimSpace(rest)
			}
		}
		integration := strings.HasSuffix(path, "_integration_test.go")
		switch {
		case integration && tag != "integration":
			t.Errorf("%s: an integration test file needs //go:build integration, has %q", path, tag)
		case !integration && tag != "" && tag != "!integration":
			t.Errorf("%s: build tag %q: a file is tagged integration only with an _integration_test.go name, and the tiers use no other tag", path, tag)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("found no test files")
	}
}
