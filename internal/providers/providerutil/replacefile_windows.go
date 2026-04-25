//go:build windows

package providerutil

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const (
	replaceFileWriteThrough       = 0x00000001
	replaceFileIgnoreMergeErrors  = 0x00000002
	replaceFileIgnoreACLMergeErrs = 0x00000004
)

var replaceFileW = syscall.NewLazyDLL("kernel32.dll").NewProc("ReplaceFileW")

// ReplaceFileAtomic replaces path with tmpPath using ReplaceFileW when a target exists.
func ReplaceFileAtomic(tmpPath, path string) error {
	target, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	replacement, err := syscall.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	flags := uintptr(replaceFileWriteThrough | replaceFileIgnoreMergeErrors | replaceFileIgnoreACLMergeErrs)
	ok, _, callErr := replaceFileW.Call(
		uintptr(unsafe.Pointer(target)),
		uintptr(unsafe.Pointer(replacement)),
		0,
		flags,
		0,
		0,
	)
	if ok != 0 {
		return nil
	}
	if errors.Is(callErr, syscall.ERROR_FILE_NOT_FOUND) {
		return os.Rename(tmpPath, path)
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return syscall.EINVAL
}
