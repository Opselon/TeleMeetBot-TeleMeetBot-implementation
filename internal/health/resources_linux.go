//go:build linux

package health

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// sampleLocked reads Linux system and container metrics.
func (c *resourceCollector) sampleLocked(initial bool) {
	now := time.Now()
	var res ServerResources
	res.CollectedAt = now

	// 1. Storage metrics
	dataDir := c.dataDir
	if dataDir == "" {
		dataDir = "."
	}
	if free, total, ok := diskUsage(dataDir); ok && total > 0 {
		freeBytes := free
		totalBytes := total
		var usedBytes uint64
		if totalBytes > freeBytes {
			usedBytes = totalBytes - freeBytes
		}
		res.StorageTotalGiB = float64(totalBytes) / (1 << 30)
		res.StorageUsedGiB = float64(usedBytes) / (1 << 30)
		res.StorageUsagePercent = int(math.Round(float64(usedBytes) * 100.0 / float64(totalBytes)))
		if res.StorageUsagePercent < 0 {
			res.StorageUsagePercent = 0
		} else if res.StorageUsagePercent > 100 {
			res.StorageUsagePercent = 100
		}
		res.StorageAvailable = true
	}

	// 2. RAM and Swap metrics
	hostTotalRAM, hostAvailRAM, hostTotalSwap, hostFreeSwap, memOK := readLinuxMemInfo()
	if memOK && hostTotalRAM > 0 {
		var ramUsed uint64
		var ramTotal uint64 = hostTotalRAM

		// Check if container memory limit is active and less than host RAM
		if cgUsage, cgLimit, cgOK := readCgroupMemory(); cgOK && cgLimit > 0 && cgLimit < hostTotalRAM {
			ramUsed = cgUsage
			ramTotal = cgLimit
		} else {
			if hostTotalRAM > hostAvailRAM {
				ramUsed = hostTotalRAM - hostAvailRAM
			}
		}

		res.RAMTotalGiB = float64(ramTotal) / (1 << 30)
		res.RAMUsedGiB = float64(ramUsed) / (1 << 30)
		res.RAMUsagePercent = int(math.Round(float64(ramUsed) * 100.0 / float64(ramTotal)))
		if res.RAMUsagePercent < 0 {
			res.RAMUsagePercent = 0
		} else if res.RAMUsagePercent > 100 {
			res.RAMUsagePercent = 100
		}
		res.RAMAvailable = true

		// Swap
		var swapUsed uint64
		var swapTotal uint64 = hostTotalSwap
		if cgSwapUsage, cgSwapLimit, cgSwapOK := readCgroupSwap(); cgSwapOK && cgSwapLimit > 0 && cgSwapLimit < hostTotalSwap {
			swapUsed = cgSwapUsage
			swapTotal = cgSwapLimit
		} else {
			if hostTotalSwap > hostFreeSwap {
				swapUsed = hostTotalSwap - hostFreeSwap
			}
		}

		res.SwapTotalGiB = float64(swapTotal) / (1 << 30)
		res.SwapUsedGiB = float64(swapUsed) / (1 << 30)
		if swapTotal > 0 {
			res.SwapUsagePercent = int(math.Round(float64(swapUsed) * 100.0 / float64(swapTotal)))
			if res.SwapUsagePercent < 0 {
				res.SwapUsagePercent = 0
			} else if res.SwapUsagePercent > 100 {
				res.SwapUsagePercent = 100
			}
		} else {
			res.SwapUsagePercent = 0
		}
		res.SwapAvailable = true
	}

	// 3. CPU metrics
	// Helper to sample CPU once
	sampleCPUOnce := func() {
		// A. Check cgroup CPU quota (container scope)
		if cores, cgUsec, ok := readCgroupCPUQuota(); ok && cores > 0 {
			if c.prevCgroupUs > 0 && !c.prevSampleAt.IsZero() {
				dtUs := now.Sub(c.prevSampleAt).Microseconds()
				if dtUs > 0 && cgUsec >= c.prevCgroupUs {
					dUsage := cgUsec - c.prevCgroupUs
					pct := int(math.Round(float64(dUsage) * 100.0 / (float64(dtUs) * cores)))
					if pct < 0 {
						pct = 0
					} else if pct > 100 {
						pct = 100
					}
					res.CPUUsagePercent = pct
					res.CPUScope = "container"
					res.CPUAvailable = true
				}
			}
			c.prevCgroupUs = cgUsec
		}

		// B. If no container quota, use host system-wide CPU
		if !res.CPUAvailable {
			if busy, total, ok := readLinuxProcStatCPU(); ok {
				if c.prevTotalCPU > 0 && total > c.prevTotalCPU {
					dTotal := total - c.prevTotalCPU
					var dBusy uint64
					if busy >= c.prevBusyCPU {
						dBusy = busy - c.prevBusyCPU
					}
					pct := int(math.Round(float64(dBusy) * 100.0 / float64(dTotal)))
					if pct < 0 {
						pct = 0
					} else if pct > 100 {
						pct = 100
					}
					res.CPUUsagePercent = pct
					res.CPUScope = "system-wide"
					res.CPUAvailable = true
				}
				c.prevTotalCPU = total
				c.prevBusyCPU = busy
			}
		}
		c.prevSampleAt = now
	}

	sampleCPUOnce()
	if initial && !res.CPUAvailable {
		// On initial cold start, sleep briefly to give a baseline delta
		time.Sleep(50 * time.Millisecond)
		now = time.Now()
		sampleCPUOnce()
	}

	c.cached = res
	c.lastSampled = now
}

// readLinuxMemInfo reads MemTotal, MemAvailable, SwapTotal, SwapFree from /proc/meminfo.
func readLinuxMemInfo() (totalRAM, availRAM, totalSwap, freeSwap uint64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var freeRAM, buffers, cached uint64
	var hasTotalRAM, hasAvailRAM, hasTotalSwap, hasFreeSwap bool

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSuffix(parts[0], ":")
		val, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			continue
		}
		// Values in /proc/meminfo are in kB
		valBytes := val * 1024
		switch key {
		case "MemTotal":
			totalRAM = valBytes
			hasTotalRAM = true
		case "MemAvailable":
			availRAM = valBytes
			hasAvailRAM = true
		case "MemFree":
			freeRAM = valBytes
		case "Buffers":
			buffers = valBytes
		case "Cached":
			cached = valBytes
		case "SwapTotal":
			totalSwap = valBytes
			hasTotalSwap = true
		case "SwapFree":
			freeSwap = valBytes
			hasFreeSwap = true
		}
	}

	if !hasTotalRAM {
		return 0, 0, 0, 0, false
	}
	if !hasAvailRAM {
		availRAM = freeRAM + buffers + cached
	}
	if !hasTotalSwap {
		totalSwap = 0
	}
	if !hasFreeSwap {
		freeSwap = 0
	}
	return totalRAM, availRAM, totalSwap, freeSwap, true
}

// readCgroupMemory checks cgroup v2 and v1 for container-specific memory limits and usage.
func readCgroupMemory() (usageBytes, limitBytes uint64, ok bool) {
	// cgroup v2
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		s := strings.TrimSpace(string(data))
		if s != "" && s != "max" {
			if lim, err := strconv.ParseUint(s, 10, 64); err == nil && lim > 0 {
				if curData, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
					if cur, err := strconv.ParseUint(strings.TrimSpace(string(curData)), 10, 64); err == nil {
						return cur, lim, true
					}
				}
			}
		}
	}

	// cgroup v1
	if data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
		s := strings.TrimSpace(string(data))
		if lim, err := strconv.ParseUint(s, 10, 64); err == nil && lim > 0 && lim < (1<<62) {
			if curData, err := os.ReadFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); err == nil {
				if cur, err := strconv.ParseUint(strings.TrimSpace(string(curData)), 10, 64); err == nil {
					return cur, lim, true
				}
			}
		}
	}

	return 0, 0, false
}

// readCgroupSwap checks cgroup v2 and v1 for container-specific swap limits and usage.
func readCgroupSwap() (usageBytes, limitBytes uint64, ok bool) {
	// cgroup v2
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.swap.max"); err == nil {
		s := strings.TrimSpace(string(data))
		if s != "" && s != "max" {
			if lim, err := strconv.ParseUint(s, 10, 64); err == nil && lim > 0 {
				if curData, err := os.ReadFile("/sys/fs/cgroup/memory.swap.current"); err == nil {
					if cur, err := strconv.ParseUint(strings.TrimSpace(string(curData)), 10, 64); err == nil {
						return cur, lim, true
					}
				}
			}
		}
	}

	// cgroup v1
	if data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.memsw.limit_in_bytes"); err == nil {
		s := strings.TrimSpace(string(data))
		if lim, err := strconv.ParseUint(s, 10, 64); err == nil && lim > 0 && lim < (1<<62) {
			if curData, err := os.ReadFile("/sys/fs/cgroup/memory/memory.memsw.usage_in_bytes"); err == nil {
				if cur, err := strconv.ParseUint(strings.TrimSpace(string(curData)), 10, 64); err == nil {
					return cur, lim, true
				}
			}
		}
	}

	return 0, 0, false
}

// readCgroupCPUQuota checks if a container CPU quota is set, returning cores and total usage usec.
func readCgroupCPUQuota() (cores float64, usageUsec uint64, ok bool) {
	// cgroup v2
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 && fields[0] != "max" {
			quota, err1 := strconv.ParseFloat(fields[0], 64)
			period, err2 := strconv.ParseFloat(fields[1], 64)
			if err1 == nil && err2 == nil && period > 0 && quota > 0 {
				cores = quota / period
				// read usage_usec from cpu.stat
				if statData, err := os.ReadFile("/sys/fs/cgroup/cpu.stat"); err == nil {
					scanner := bufio.NewScanner(strings.NewReader(string(statData)))
					for scanner.Scan() {
						line := scanner.Text()
						if strings.HasPrefix(line, "usage_usec") {
							f := strings.Fields(line)
							if len(f) >= 2 {
								if u, err := strconv.ParseUint(f[1], 10, 64); err == nil {
									return cores, u, true
								}
							}
						}
					}
				}
			}
		}
	}

	// cgroup v1
	if quotaData, err := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us"); err == nil {
		quota, err := strconv.ParseFloat(strings.TrimSpace(string(quotaData)), 64)
		if err == nil && quota > 0 {
			if periodData, err := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us"); err == nil {
				period, err := strconv.ParseFloat(strings.TrimSpace(string(periodData)), 64)
				if err == nil && period > 0 {
					cores = quota / period
					if usageData, err := os.ReadFile("/sys/fs/cgroup/cpu/cpuacct.usage"); err == nil {
						if ns, err := strconv.ParseUint(strings.TrimSpace(string(usageData)), 10, 64); err == nil {
							return cores, ns / 1000, true
						}
					}
				}
			}
		}
	}

	return 0, 0, false
}

// readLinuxProcStatCPU reads total busy and total time from /proc/stat.
func readLinuxProcStatCPU() (busy, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				return 0, 0, false
			}
			var user, nice, system, idle, iowait, irq, softirq, steal uint64
			user, _ = strconv.ParseUint(fields[1], 10, 64)
			nice, _ = strconv.ParseUint(fields[2], 10, 64)
			system, _ = strconv.ParseUint(fields[3], 10, 64)
			idle, _ = strconv.ParseUint(fields[4], 10, 64)
			if len(fields) > 5 {
				iowait, _ = strconv.ParseUint(fields[5], 10, 64)
			}
			if len(fields) > 6 {
				irq, _ = strconv.ParseUint(fields[6], 10, 64)
			}
			if len(fields) > 7 {
				softirq, _ = strconv.ParseUint(fields[7], 10, 64)
			}
			if len(fields) > 8 {
				steal, _ = strconv.ParseUint(fields[8], 10, 64)
			}

			total = user + nice + system + idle + iowait + irq + softirq + steal
			busy = user + nice + system + irq + softirq + steal
			return busy, total, true
		}
	}

	return 0, 0, false
}
