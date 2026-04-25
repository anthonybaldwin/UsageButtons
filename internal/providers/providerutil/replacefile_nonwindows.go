//go:build !windows

package providerutil

import "os"

// ReplaceFileAtomic replaces path with tmpPath on platforms where os.Rename is atomic.
func ReplaceFileAtomic(tmpPath, path string) error {
	return os.Rename(tmpPath, path)
}
