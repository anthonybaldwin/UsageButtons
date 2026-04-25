//go:build windows

package providerutil

import (
	"os/exec"
	"syscall"
)

// createNoWindow is the Win32 CREATE_NO_WINDOW process creation flag. It
// prevents the OS from allocating a console for a console-subsystem child
// when the parent has no console attached (Stream Deck launches the plugin
// without one, so otherwise every spawn flashes a black window).
const createNoWindow = 0x08000000

// hideConsoleWindow suppresses the brief console flash that otherwise
// appears whenever a console-subsystem child is spawned from a no-console
// parent. HideWindow alone is not sufficient — CREATE_NO_WINDOW is the
// flag that actually prevents the console from being allocated.
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
