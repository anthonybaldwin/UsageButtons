//go:build !windows

package codex

import "os"

// replaceFileAtomic replaces path with tmpPath on platforms where os.Rename is atomic.
func replaceFileAtomic(tmpPath, path string) error {
	return os.Rename(tmpPath, path)
}
