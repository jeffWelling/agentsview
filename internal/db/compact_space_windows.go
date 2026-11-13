//go:build windows

package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func filesystemFreeBytes(path string) (uint64, bool, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false, err
	}
	var available, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &available, &total, &totalFree); err != nil {
		return 0, false, err
	}
	return available, true, nil
}

func sameFilesystem(first, second string) (bool, error) {
	firstVolume, err := compactVolumeName(first)
	if err != nil {
		return false, fmt.Errorf("resolve first volume: %w", err)
	}
	secondVolume, err := compactVolumeName(second)
	if err != nil {
		return false, fmt.Errorf("resolve second volume: %w", err)
	}
	return strings.EqualFold(firstVolume, secondVolume), nil
}

func compactVolumeName(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	pathPtr, err := windows.UTF16PtrFromString(absPath)
	if err != nil {
		return "", err
	}
	volumePath := make([]uint16, 32768)
	if err := windows.GetVolumePathName(
		pathPtr, &volumePath[0], uint32(len(volumePath)),
	); err != nil {
		return "", err
	}
	volumeName := make([]uint16, 32768)
	if err := windows.GetVolumeNameForVolumeMountPoint(
		&volumePath[0], &volumeName[0], uint32(len(volumeName)),
	); err != nil {
		return "", err
	}
	return windows.UTF16ToString(volumeName), nil
}

func syncDirectory(string) error { return nil }

// The pinned SQLite Windows VFS uses its explicit global directory or
// GetTempPathW. Do not change the process-global temp directory for maintenance.
func messageIndexTempDirectory(ctx context.Context, configured string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if configured != "" {
		return filepath.Abs(configured)
	}
	path := make([]uint16, 32768)
	n, err := windows.GetTempPath(uint32(len(path)), &path[0])
	if err != nil {
		return "", err
	}
	if n == 0 || n >= uint32(len(path)) {
		return "", fmt.Errorf("cannot determine SQLite temporary directory")
	}
	return filepath.Abs(windows.UTF16ToString(path[:n]))
}
