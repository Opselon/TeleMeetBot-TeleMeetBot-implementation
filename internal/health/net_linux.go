//go:build linux

package health

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// readNetDevTotals sums the RX/TX byte counters over every interface in the
// process's network namespace except the loopback. Inside Docker this is the
// container's own traffic; on bare metal it is the host's (the card labels
// the scope).
func readNetDevTotals() (rx, tx uint64, ok bool) {
	f, err := os.Open("/proc/self/net/dev")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		// Format:
		//   Inter-|   Receive                            ... Transmit ...
		//    face |bytes    packets errs drop fifo frame ... bytes packets ...
		if lineNo <= 2 {
			continue // two header lines
		}
		parts := strings.SplitN(scanner.Text(), ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "" || iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(fields[0], 10, 64)
		t, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += r
		tx += t
	}
	return rx, tx, true
}

// containerCgroupMarker inspects the cgroup path for container runtime
// fingerprints (Docker/containerd/Kubernetes/LXC), covering environments
// where the marker files are absent.
func containerCgroupMarker() bool {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false
	}
	for _, marker := range []string{"docker", "containerd", "kubepods", "lxc"} {
		if strings.Contains(string(data), marker) {
			return true
		}
	}
	return false
}
