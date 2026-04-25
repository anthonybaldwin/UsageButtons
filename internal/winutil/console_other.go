//go:build !windows

// Package winutil holds small Windows-only helpers shared across the
// plugin and native-host binaries. The non-Windows build provides no-op
// stubs so cross-platform callers can invoke them unconditionally.
package winutil

import "os/exec"

// HideConsoleWindow is a no-op on non-Windows platforms.
func HideConsoleWindow(*exec.Cmd) {}
