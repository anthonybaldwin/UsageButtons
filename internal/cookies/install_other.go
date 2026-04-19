//go:build !windows && !darwin

package cookies

import "errors"

// errUnsupported keeps the build succeeding on Linux (where Stream
// Deck doesn't ship) without pretending the install path exists.
var errUnsupported = errors.New("cookies: native-messaging host install is only supported on Windows and macOS")

// RegisterHost is a no-op on unsupported platforms.
func RegisterHost(hostName, binaryPath string, allowedOrigins []string) error {
	return errUnsupported
}

// UnregisterHost is a no-op on unsupported platforms.
func UnregisterHost(hostName string) error {
	return errUnsupported
}

// IsHostRegistered reports false on unsupported platforms.
func IsHostRegistered(hostName string) bool {
	return false
}
