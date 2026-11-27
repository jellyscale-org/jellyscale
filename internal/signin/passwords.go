package signin

import (
	_ "embed"
	"strings"
)

// commonPasswords is SecLists' Passwords/Common-Credentials/10k-most-common.txt (MIT), as of its commit
// 913b327317496d062bcc7cace524aaad8a693be2 (docs/access.md). It is part of the binary, so no password is sent anywhere.
//
//go:embed common_passwords.txt
var commonPasswords string

var common = func() map[string]bool {
	set := map[string]bool{}
	for _, password := range strings.Fields(commonPasswords) {
		set[strings.ToLower(password)] = true
	}
	return set
}()

// maxPasswordBytes is what bcrypt reads of a password.
const maxPasswordBytes = 72

// CommonlyGuessed says whether the password is on the shipped list, in any letter case.
func CommonlyGuessed(password string) bool { return common[strings.ToLower(password)] }

// Unacceptable says why a new password may not be set, or "" if it may. An empty one is answered as a guessed one
// (D-100).
func Unacceptable(password string) string {
	switch {
	case password == "" || CommonlyGuessed(password):
		return "this password is empty or commonly guessed; choose another"
	case len(password) > maxPasswordBytes:
		return "a password may be at most 72 bytes long"
	}
	return ""
}
