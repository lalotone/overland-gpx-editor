//go:build !linux && !darwin

package server

func availableDiskBytes(string) (uint64, bool, error) {
	return 0, false, nil
}
