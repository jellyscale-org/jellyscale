package conform_test

import (
	"encoding/json"
	"os"
	"testing"
)

func writeFile(path, text string) error { return os.WriteFile(path, []byte(text), 0o644) }

// removeKey drops a top-level key from a JSON object, keeping the rest.
func removeKey(t *testing.T, body, key string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, key)
	out, _ := json.Marshal(m)
	return string(out)
}
