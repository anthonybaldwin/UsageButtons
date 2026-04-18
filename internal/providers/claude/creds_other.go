//go:build !darwin

package claude

import "errors"

var errKeychainUnsupported = errors.New("keychain lookup is macOS-only")

func readKeychainBlob() ([]byte, error) { return nil, errKeychainUnsupported }

func credsSourceHint() string { return credPath() }
