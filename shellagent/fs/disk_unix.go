//go:build !windows

package fs

import "syscall"

func diskUsage(path string) (any, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return nil, nil
	}
	return map[string]any{
		"total": int64(stats.Bsize) * int64(stats.Blocks),
		"free":  int64(stats.Bsize) * int64(stats.Bavail),
	}, nil
}
