//go:build darwin

package providers

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

const (
	// claudeKeychainService is the generic-password service name Claude Code
	// writes on macOS. It must match internal/providers/claude.
	claudeKeychainService = "Claude Code-credentials"
	// claudeKeychainTimeout bounds the macOS security subprocess.
	claudeKeychainTimeout = 1500 * time.Millisecond
)

// claudeKeychainCredentialFingerprint hashes Claude's macOS keychain payload.
func claudeKeychainCredentialFingerprint() string {
	ctx, cancel := context.WithTimeout(context.Background(), claudeKeychainTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/bin/security",
		"find-generic-password", "-s", claudeKeychainService, "-w")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if err != nil {
		return "missing"
	}

	out := bytes.TrimRight(stdout.Bytes(), "\r\n")
	if len(out) == 0 {
		return "empty"
	}
	return contentFingerprint(out)
}
