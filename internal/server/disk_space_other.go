//go:build !linux

package server

func availableDiskBytes(string) (uint64, bool, error) {
	return 0, false, nil
}
