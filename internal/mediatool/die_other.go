//go:build !linux

package mediatool

import "os/exec"

// dieWithParent has no equivalent here; only a clean shutdown kills the tool.
func dieWithParent(*exec.Cmd) {}
