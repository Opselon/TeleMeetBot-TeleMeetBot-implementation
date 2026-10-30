//go:build !linux

package health

// readNetDevTotals is unavailable off Linux: the card hides the network line.
func readNetDevTotals() (rx, tx uint64, ok bool) {
	return 0, 0, false
}

// containerCgroupMarker has no cgroup representation off Linux.
func containerCgroupMarker() bool {
	return false
}
