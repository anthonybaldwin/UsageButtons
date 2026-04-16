//go:build darwin

package cookies

import (
	"fmt"
	"os"
	"path/filepath"
)

// darwinBrowserDirs lists the per-browser Application Support
// subdirectories where supported browsers look for native-messaging
// host manifests. Firefox is included so a future Firefox port of
// the extension needs no plugin-side install changes.
var darwinBrowserDirs = []struct {
	name string
	dir  string
}{
	{"Google Chrome", "Google/Chrome"},
	{"Google Chrome Beta", "Google/Chrome Beta"},
	{"Google Chrome Canary", "Google/Chrome Canary"},
	{"Microsoft Edge", "Microsoft Edge"},
	{"Brave", "BraveSoftware/Brave-Browser"},
	{"Chromium", "Chromium"},
	{"Firefox", "Mozilla"},
}

func appSupportDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support"), nil
}

// RegisterHost writes one manifest file per known Chromium-based
// browser. Chrome reads these out of
// ~/Library/Application Support/<browser>/NativeMessagingHosts/.
// Missing browsers are a no-op (we create the directory). Returns the
// first error encountered, but attempts all browsers.
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
	base, err := appSupportDir()
	if err != nil {
		return err
	}
	var firstErr error
	for _, b := range darwinBrowserDirs {
		dir := filepath.Join(base, b.dir, "NativeMessagingHosts")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		path := filepath.Join(dir, hostName+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// UnregisterHost removes every per-browser manifest file for hostName.
func UnregisterHost(hostName string) error {
	base, err := appSupportDir()
	if err != nil {
		return err
	}
	var firstErr error
	for _, b := range darwinBrowserDirs {
		path := filepath.Join(base, b.dir, "NativeMessagingHosts", hostName+".json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
