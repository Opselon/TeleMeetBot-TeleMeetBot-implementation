//go:build windows

package health

import "golang.org/x/sys/windows"

// diskUsage returns the free and total byte counts of the volume holding
// path. Free is the count available to the calling user: GetDiskFreeSpaceEx
// reports the caller-visible quota, not the volume-wide free space, which is
// the honest number for a "can I write a 2 GiB download here" question.
func diskUsage(path string) (free, total uint64, ok bool) {
	if path == "" {
		path = "."
	}
	var freeCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(path), &freeCaller, &totalBytes, &totalFree); err != nil {
		return 0, 0, false
	}
	return freeCaller, totalBytes, true
}
