//go:build windows

package media

import "golang.org/x/sys/windows"

// diskUsageForPath reports the caller-visible free bytes and the total bytes
// of the volume holding path (GetDiskFreeSpaceEx).
func diskUsageForPath(path string) (free, total uint64, ok bool) {
	if path == "" {
		path = "."
	}
	var freeCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(path), &freeCaller, &totalBytes, &totalFree); err != nil {
		return 0, 0, false
	}
	return freeCaller, totalBytes, true
}
