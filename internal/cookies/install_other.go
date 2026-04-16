//go:build !windows && !darwin

package cookies

import "errors"

// errUnsupported keeps the build succeeding on Linux (where Stream
// Deck doesn't ship) without pretending the install path exists.
var errUnsupported = errors.New("cookies: native-messaging host install is only supported on Windows and macOS")

func RegisterHost(hostName, binaryPath string, allowedOrigins []string) error {
	return errUnsupported
}

func UnregisterHost(hostName string) error {
	return errUnsupported
}
