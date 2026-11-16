package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// JS-117 adds no migration: the original language is made when an item is answered, from rows
// the scan already writes. A migration of it would be named for it or hold the word in its text.
// (A migration of another task may come in beside this one, so the count is not pinned.)
func TestNoMigrationHoldsTheOriginalLanguage(t *testing.T) {
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v, %d files", err, len(files))
	}
	for _, file := range files {
		text, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if lower := strings.ToLower(file + string(text)); strings.Contains(lower, "original_language") || strings.Contains(lower, "originallanguage") || strings.Contains(lower, "isoriginal") || strings.Contains(lower, "is_original") {
			t.Errorf("%s is a migration of the original language, which the task stores nowhere", file)
		}
	}
}
