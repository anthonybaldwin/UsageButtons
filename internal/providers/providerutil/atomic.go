package providerutil

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// WriteJSONAtomic writes data to path using atomic replace semantics.
func WriteJSONAtomic(path string, data any) error {
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(path, append(payload, '\n'))
}

// WriteAtomic writes data to path using atomic replace semantics.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := ReplaceFileAtomic(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
