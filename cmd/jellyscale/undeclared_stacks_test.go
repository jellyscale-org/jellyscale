package main

import (
	"os"
	"strings"
	"testing"
)

// JS-161 (D-062): the stacks the test scripts start run workers that declare nothing, as before JS-161, so that the
// default is what the suites exercise; a script that declares transcodeCores or transcodeSlots hides what an undeclared
// worker does. A declaration kept is added here with the check that needs it, named.
func TestTheRolesE2EAndDifferentialStacksDeclareNoCapacity(t *testing.T) {
	for _, script := range []string{"test/roles.sh", "test/e2e/run.sh", "test/fixtures/posix.sh", "test/differential/run.sh"} {
		raw, err := os.ReadFile("../../" + script)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"transcodeCores", "transcodeSlots"} {
			if strings.Contains(string(raw), key) {
				t.Errorf("%s declares %s: its workers must declare nothing (D-062)", script, key)
			}
		}
	}
}
