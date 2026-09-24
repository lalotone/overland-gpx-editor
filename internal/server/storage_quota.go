package server

import "math"

const offlineDiskSafetyMargin = uint64(64 << 20)

// Both caches share the filesystem's available space; these are opportunistic
// capacities, not two independent allocations. Writes check space again.
func availableStorageQuota(path string, used int64) int64 {
	available, supported, err := availableDiskBytes(path)
	if err != nil || !supported {
		return used
	}
	return quotaWithFreeSpace(used, available)
}

func quotaWithFreeSpace(used int64, available uint64) int64 {
	if available <= offlineDiskSafetyMargin {
		return used
	}
	remaining := available - offlineDiskSafetyMargin
	if remaining > uint64(math.MaxInt64-used) {
		return math.MaxInt64
	}
	return used + int64(remaining)
}
