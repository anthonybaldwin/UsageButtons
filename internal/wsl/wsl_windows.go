//go:build windows

package wsl

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/anthonybaldwin/UsageButtons/internal/winutil"
)

// sourcesCacheTTL bounds how often we shell out to wsl.exe and walk
// \\wsl.localhost. Distro state is stable on a human timescale; rescanning
// on every Fetch would slow down the cost-tile path needlessly.
const sourcesCacheTTL = 30 * time.Second

var (
	sourcesMu     sync.Mutex
	sourcesCache  []Source
	sourcesCacheT time.Time
)

// Sources returns the running, user-bearing WSL distributions on this
// machine. Stopped distros are deliberately omitted — accessing
// \\wsl.localhost\<distro>\... cold-starts the VM, which is far too
// expensive to do on a 5-minute scan loop.
//
// Returns nil (not error) if WSL is not installed, no distros are
// running, or any probe fails. Callers should treat nil as "no extra
// sources" rather than as an error condition.
func Sources() []Source {
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	if sourcesCache != nil && time.Since(sourcesCacheT) < sourcesCacheTTL {
		return sourcesCache
	}
	sourcesCache = discover()
	sourcesCacheT = time.Now()
	return sourcesCache
}

// discover does the actual probe: enumerate running distros via wsl.exe,
// then resolve each distro's home directory under \\wsl.localhost.
func discover() []Source {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return nil
	}

	distros := listRunningDistros()
	if len(distros) == 0 {
		return nil
	}

	var out []Source
	for _, name := range distros {
		// docker-desktop is a Docker-internal distro with no real user
		// home; skip even if it's running.
		if strings.EqualFold(name, "docker-desktop") || strings.EqualFold(name, "docker-desktop-data") {
			continue
		}
		home := resolveHome(name)
		if home == "" {
			continue
		}
		out = append(out, Source{
			Key:   sanitizeKey(name),
			Label: name,
			Home:  home,
		})
	}
	return out
}

// listRunningDistros runs `wsl.exe -l -q --running` and returns the
// distro names it prints. The --running flag is critical: bare `-l -q`
// would also list stopped distros, but querying their FS via
// \\wsl.localhost would silently boot them.
//
// wsl.exe writes UTF-16LE to stdout, with each distro on its own line.
// We decode and trim before returning.
func listRunningDistros() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "wsl.exe", "-l", "-q", "--running")
	winutil.HideConsoleWindow(cmd)
	raw, err := cmd.Output()
	if err != nil {
		return nil
	}

	text := decodeUTF16LE(raw)
	var names []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r\x00"))
		if line == "" {
			continue
		}
		names = append(names, line)
	}
	sort.Strings(names)
	return names
}

// decodeUTF16LE converts UTF-16LE bytes (with optional BOM) into a Go
// string. wsl.exe always writes its list output in UTF-16LE on Windows,
// regardless of console code page.
func decodeUTF16LE(raw []byte) string {
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		raw = raw[2:]
	}
	if len(raw)%2 != 0 {
		raw = raw[:len(raw)-1]
	}
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	return string(utf16.Decode(u16))
}

// resolveHome returns the UNC path to the default user's home directory
// inside the given distro, or "" if it can't be determined.
//
// Strategy: enumerate \\wsl.localhost\<distro>\home\ and pick the first
// (and almost always only) user directory. Shelling out via `wsl -d
// <distro> -u root -e cat /etc/passwd` would be more correct on
// multi-user distros but adds a per-distro process spawn for a case
// that doesn't exist in practice.
func resolveHome(distro string) string {
	root := `\\wsl.localhost\` + distro + `\home`
	entries, err := os.ReadDir(root)
	if err != nil {
		// Older Windows builds expose the legacy \\wsl$\... share instead
		// of \\wsl.localhost\... — try that as a fallback before giving up.
		root = `\\wsl$\` + distro + `\home`
		entries, err = os.ReadDir(root)
		if err != nil {
			return ""
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(root, e.Name())
		}
	}
	return ""
}

// sanitizeKey converts a distro name to a metric-ID-safe identifier by
// replacing every non-alphanumeric character with underscore. "Ubuntu-22.04"
// becomes "Ubuntu_22_04" so it can be appended to hyphenated metric IDs
// without ambiguity.
func sanitizeKey(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
