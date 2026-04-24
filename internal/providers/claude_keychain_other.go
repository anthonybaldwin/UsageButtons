//go:build !darwin

package providers

// claudeKeychainCredentialFingerprint returns a stable marker off macOS.
func claudeKeychainCredentialFingerprint() string {
	return "unsupported"
}
