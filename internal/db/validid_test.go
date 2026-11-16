package db

import "testing"

func TestValidIDRefusesStrayDashes(t *testing.T) {
	const compact = "1a2b3c4d5e6f40718293a4b5c6d7e8f4"
	const dashed = "1a2b3c4d-5e6f-4071-8293-a4b5c6d7e8f4"
	for id, want := range map[string]bool{
		compact: true, dashed: true, "1A2B3C4D5E6F40718293A4B5C6D7E8F4": true,
		compact + "--": false, "-" + compact: false, "--" + compact: false,
		"1a2b3c4d5e6f-4071-8293-a4b5c6d7e8f4--": false, "1a2b-3c4d5e6f-4071-8293-a4b5c6d7e8f4": false,
		"{" + dashed + "}": false, "not-a-guid": false, "": false,
	} {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, got, want)
		}
	}
}
