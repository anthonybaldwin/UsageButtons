//go:build windows

// Package winutil holds small Windows-only helpers shared across the
// plugin and native-host binaries.
package winutil

import (
	"os/exec"
	"syscall"
)

// createNoWindow is the Win32 CREATE_NO_WINDOW process creation flag. It
// prevents the OS from allocating a console for a console-subsystem child
// when the parent has no console attached (Stream Deck launches the plugin
// without one, so otherwise every spawn flashes a black window).
const createNoWindow = 0x08000000

// HideConsoleWindow suppresses the brief console flash that otherwise
// appears whenever a console-subsystem child is spawned from a no-console
// parent. HideWindow alone is not sufficient — CREATE_NO_WINDOW is the
// flag that actually prevents the console from being allocated.
//
// Existing fields on cmd.SysProcAttr (e.g. Token, CmdLine) are preserved.
func HideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
