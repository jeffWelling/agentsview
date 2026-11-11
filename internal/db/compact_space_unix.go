//go:build !windows

package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func filesystemFreeBytes(path string) (uint64, bool, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, false, err
	}
	return stat.Bavail * uint64(stat.Bsize), true, nil
}

func sameFilesystem(first, second string) (bool, error) {
	var firstStat, secondStat unix.Stat_t
	if err := unix.Stat(first, &firstStat); err != nil {
		return false, err
	}
	if err := unix.Stat(second, &secondStat); err != nil {
		return false, err
	}
	return firstStat.Dev == secondStat.Dev, nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Match the pinned SQLite unix VFS search order without changing its global
// configuration. The maintenance CLI does not alter temp environment variables.
func messageIndexTempDirectory(ctx context.Context, configured string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	for _, dir := range []string{configured, os.Getenv("SQLITE_TMPDIR"), os.Getenv("TMPDIR"), "/var/tmp", "/usr/tmp", "/tmp", "."} {
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if err == nil && info.IsDir() && unix.Access(dir, unix.W_OK|unix.X_OK) == nil {
			return filepath.Abs(dir)
		}
	}
	return "", fmt.Errorf("cannot determine a writable SQLite temporary directory")
}
