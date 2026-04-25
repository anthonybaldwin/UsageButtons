//go:build windows

package cookies

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// createNoWindow is the Win32 CREATE_NO_WINDOW process creation flag. The
// plugin runs without a console attached, so spawning console-subsystem
// children (reg.exe here) without this flag flashes a black console
// window. HideWindow alone is not sufficient.
const createNoWindow = 0x08000000

// windowsBrowserKeys lists the HKCU registry roots under which each
// supported browser reads native-messaging host manifests. We install
// into all of them so the extension works no matter which browser the
// user runs. Firefox is included here so a future Firefox port of the
// extension requires no plugin changes on the install side.
var windowsBrowserKeys = []struct {
	name    string
	regRoot string
}{
	{"Google Chrome", `Software\Google\Chrome\NativeMessagingHosts`},
	{"Google Chrome Beta", `Software\Google\Chrome Beta\NativeMessagingHosts`},
	{"Google Chrome Canary", `Software\Google\Chrome SxS\NativeMessagingHosts`},
	{"Microsoft Edge", `Software\Microsoft\Edge\NativeMessagingHosts`},
	{"Brave", `Software\BraveSoftware\Brave-Browser\NativeMessagingHosts`},
	{"Chromium", `Software\Chromium\NativeMessagingHosts`},
	{"Firefox", `Software\Mozilla\NativeMessagingHosts`},
}

// manifestFilePath returns the on-disk location of the host manifest file
// for the given host name under %LOCALAPPDATA%\UsageButtons\.
func manifestFilePath(hostName string) (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", fmt.Errorf("cookies: LOCALAPPDATA not set")
	}
	return filepath.Join(base, "UsageButtons", hostName+".json"), nil
}

// RegisterHost writes the native-messaging manifest to
// %LOCALAPPDATA%\UsageButtons\<hostName>.json and adds an HKCU
// registry key for each known Chromium-based browser pointing at it.
// Browsers not installed on this machine will fail individually; the
// first such failure is returned, but all browsers are attempted.
func RegisterHost(hostName, binaryPath string, allowedOrigins []string) error {
	abs, err := filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("cookies: resolve binary path: %w", err)
	}
	mf := HostManifest{
		Name:           hostName,
		Description:    "Usage Buttons cookie bridge",
		Path:           abs,
		Type:           "stdio",
		AllowedOrigins: allowedOrigins,
	}
	data, err := MarshalHostManifest(mf)
	if err != nil {
		return err
	}
	path, err := manifestFilePath(hostName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}

	var firstErr error
	for _, b := range windowsBrowserKeys {
		key := fmt.Sprintf(`HKCU\%s\%s`, b.regRoot, hostName)
		cmd := exec.Command("reg", "add", key, "/ve", "/t", "REG_SZ", "/d", path, "/f")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		if out, err := cmd.CombinedOutput(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("reg add %s: %w (%s)", b.name, err, string(out))
			}
		}
	}
	return firstErr
}

// IsHostRegistered reports whether the native-messaging manifest file
// exists on disk.
func IsHostRegistered(hostName string) bool {
	path, err := manifestFilePath(hostName)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// UnregisterHost removes the registry keys for every browser and the
// manifest file. Missing keys are treated as success (already gone is
// the desired end state).
func UnregisterHost(hostName string) error {
	for _, b := range windowsBrowserKeys {
		key := fmt.Sprintf(`HKCU\%s\%s`, b.regRoot, hostName)
		cmd := exec.Command("reg", "delete", key, "/f")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		_, _ = cmd.CombinedOutput()
	}
	var firstErr error
	if path, err := manifestFilePath(hostName); err == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			firstErr = err
		}
	}
	return firstErr
}
