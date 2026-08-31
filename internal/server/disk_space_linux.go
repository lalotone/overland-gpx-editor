//go:build linux

package server

import (
	"math"
	"syscall"
)

func availableDiskBytes(path string) (uint64, bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, true, err
	}
	blockSize := uint64(stat.Bsize)
	if blockSize != 0 && stat.Bavail > math.MaxUint64/blockSize {
		return math.MaxUint64, true, nil
	}
	return stat.Bavail * blockSize, true, nil
}
