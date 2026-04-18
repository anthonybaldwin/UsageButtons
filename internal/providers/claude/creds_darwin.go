//go:build darwin

package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// claudeKeychainService is the generic-password service name Claude
// Code writes on macOS. Same constant as CodexBar's
// ClaudeOAuthCredentialsStore.claudeKeychainService — must match
// verbatim or the lookup returns errSecItemNotFound.
const claudeKeychainService = "Claude Code-credentials"

// securityCLITimeout bounds the `/usr/bin/security` subprocess. The
// command returns in milliseconds normally; a slow response usually
// means a keychain-unlock prompt is stuck behind another app. Matches
// CodexBar's 1.5s.
const securityCLITimeout = 1500 * time.Millisecond

// readKeychainBlob reads the Claude Code credentials JSON from the
// macOS login keychain by shelling out to `/usr/bin/security`.
//
// Why shell out instead of linking Security.framework: avoids CGO,
// runs in a child process so a stuck auth prompt can't block the
// plugin's event loop, and matches CodexBar's default strategy
// (securityCLIExperimental) for avoiding keychain-unlock prompts.
func readKeychainBlob() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), securityCLITimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/bin/security",
		"find-generic-password", "-s", claudeKeychainService, "-w")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("macOS keychain read timed out after %s", securityCLITimeout)
	}
	if err != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) == 0 {
			return nil, fmt.Errorf("macOS keychain lookup failed: %w", err)
		}
		return nil, fmt.Errorf("macOS keychain lookup failed: %s", msg)
	}

	out := bytes.TrimRight(stdout.Bytes(), "\r\n")
	if len(out) == 0 {
		return nil, fmt.Errorf("macOS keychain returned empty payload for service %q", claudeKeychainService)
	}
	return out, nil
}

// credsSourceHint describes where loadCreds looks, for user-facing
// errors when nothing was found.
func credsSourceHint() string {
	return fmt.Sprintf("macOS keychain (service %q) or %s", claudeKeychainService, credPath())
}
