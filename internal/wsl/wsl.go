// Package wsl discovers running WSL distributions on Windows so that
// providers which scan local CLI state files (~/.claude/projects,
// ~/.codex/sessions) can also surface usage from inside WSL distros as
// separate "machines."
//
// The whole feature is Windows-only by design — on non-Windows builds
// Sources returns nil, so callers can compose unconditionally.
package wsl

// Source describes one WSL distribution surface that providers can scan.
//
// Native Windows is NOT represented here; callers continue to use
// os.UserHomeDir() for that. Sources returns ONLY the additional WSL
// distros so the call site reads as "scan native + each WSL source."
type Source struct {
	// Key is the metric-ID-safe identifier for this distro (alnum + "_").
	// Distro names like "Ubuntu-22.04" become "Ubuntu_22_04" so they can
	// be appended to existing metric IDs (e.g. "cost-today-wsl-Ubuntu_22_04")
	// without breaking the codebase's hyphen-delimited convention.
	Key string
	// Label is the human-friendly distro name as reported by wsl.exe
	// (e.g. "Ubuntu-22.04"). Used verbatim in PI dropdown labels.
	Label string
	// Home is the UNC path to the distro user's home directory
	// (e.g. \\wsl.localhost\Debian\home\anthony). Suitable for direct
	// filepath.Join with relative subpaths like ".claude/projects".
	Home string
}
