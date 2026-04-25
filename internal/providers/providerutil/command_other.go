//go:build !windows

package providerutil

import "os/exec"

// hideConsoleWindow is a no-op on non-Windows platforms.
func hideConsoleWindow(*exec.Cmd) {}
