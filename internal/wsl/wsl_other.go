//go:build !windows

package wsl

// Sources returns nil on non-Windows platforms. WSL only exists on
// Windows, so providers compiled for macOS/Linux see an empty source
// list and never try to spawn wsl.exe or read \\wsl.localhost paths.
func Sources() []Source { return nil }
