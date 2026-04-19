//go:build !darwin

package claude

import "errors"

// errKeychainUnsupported is returned on platforms without a macOS keychain.
var errKeychainUnsupported = errors.New("keychain lookup is macOS-only")

// readKeychainBlob is a stub on non-darwin builds; see the darwin variant.
func readKeychainBlob() ([]byte, error) { return nil, errKeychainUnsupported }

// credsSourceHint describes where loadCreds looks on non-darwin builds.
func credsSourceHint() string { return credPath() }
