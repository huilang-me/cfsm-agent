//go:build freebsd

package cfprobe

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	gopsutilCPU "github.com/shirou/gopsutil/v4/cpu"
	gopsutilDisk "github.com/shirou/gopsutil/v4/disk"
	gopsutilHost "github.com/shirou/gopsutil/v4/host"
	gopsutilLoad "github.com/shirou/gopsutil/v4/load"
	gopsutilMem "github.com/shirou/gopsutil/v4/mem"
	gopsutilNet "github.com/shirou/gopsutil/v4/net"
)

func readCPUPercent() (float64, bool) {
	percentages, err := gopsutilCPU.Percent(0, false)
	if err != nil || len(percentages) == 0 {
		return 0, false
	}
	return percentages[0], true
}

func collectBasicStats() BasicStats {
	memTotal, memUsed := freebsdMemoryMB()
	swapTotal, swapUsed := freebsdSwapMB()
	diskTotal, diskUsed := freebsdDiskUsageMB()
	return BasicStats{
		MemTotalMB:  memTotal,
		MemUsedMB:   memUsed,
		SwapTotalMB: swapTotal,
		SwapUsedMB:  swapUsed,
		DiskTotalMB: diskTotal,
		DiskUsedMB:  diskUsed,
		LoadAvg:     freebsdLoadAvg(),
		BootTimeMS:  freebsdBootTimeMS(),
		OSName:      freebsdOSName(),
		Arch:        runtime.GOARCH,
		Kernel:      firstNonEmpty(commandOutput("uname", "-r"), "Unknown"),
		CPUInfo:     freebsdCPUInfo(),
		CPUCores:    freebsdCPUCores(),
		GPUInfo:     freebsdGPUInfo(),
		Processes:   freebsdCommandLineCount("ps", "-ax"),
		TCPConn:     freebsdConnCount("tcp"),
		UDPConn:     freebsdConnCount("udp"),
	}
}

func readMemoryStats() (MemoryStats, bool) {
	memTotal, memUsed := freebsdMemoryMB()
	swapTotal, swapUsed := freebsdSwapMB()
	if memTotal == 0 && swapTotal == 0 {
		return MemoryStats{}, false
	}
	return MemoryStats{
		MemTotalMB:  memTotal,
		MemUsedMB:   memUsed,
		SwapTotalMB: swapTotal,
		SwapUsedMB:  swapUsed,
	}, true
}

func readNetBytes(ifaces string) (NetBytes, error) {
	wanted := splitInterfaceSet(ifaces)
	counters, err := gopsutilNet.IOCounters(true)
	if err != nil {
		return NetBytes{}, err
	}
	var total NetBytes
	matched := 0
	for _, counter := range counters {
		if !shouldIncludeNetInterface(counter.Name, wanted) {
			continue
		}
		total.RX += counter.BytesRecv
		total.TX += counter.BytesSent
		matched++
	}
	if matched == 0 {
		return NetBytes{}, fmt.Errorf("no matching network interfaces")
	}
	return total, nil
}

func readDiskIOCounters(_ []DiskDeviceRef) DiskIOCounters {
	return DiskIOCounters{}
}

func freebsdMemoryMB() (uint64, uint64) {
	v, err := gopsutilMem.VirtualMemory()
	if err != nil {
		return 0, 0
	}
	total := v.Total / bytesPerMiB
	if v.Total < v.Available {
		return total, 0
	}
	return total, (v.Total - v.Available) / bytesPerMiB
}

func freebsdSwapMB() (uint64, uint64) {
	v, err := gopsutilMem.SwapMemory()
	if err != nil {
		return 0, 0
	}
	return v.Total / bytesPerMiB, v.Used / bytesPerMiB
}

type freebsdDiskUsageEntry struct {
	total uint64
	used  uint64
}

func freebsdDiskUsageMB() (uint64, uint64) {
	partitions, err := gopsutilDisk.Partitions(true)
	if err != nil {
		return 0, 0
	}
	devices := map[string]freebsdDiskUsageEntry{}
	for _, part := range partitions {
		if !includeFreeBSDMount(part) {
			continue
		}
		usage, err := gopsutilDisk.Usage(part.Mountpoint)
		if err != nil {
			continue
		}
		deviceID := part.Device
		if strings.ToLower(part.Fstype) == "zfs" {
			if idx := strings.Index(deviceID, "/"); idx != -1 {
				deviceID = deviceID[:idx]
			}
		}
		entry := freebsdDiskUsageEntry{
			total: usage.Total / bytesPerMiB,
			used:  usage.Used / bytesPerMiB,
		}
		if existing, ok := devices[deviceID]; !ok || entry.total > existing.total {
			devices[deviceID] = entry
		}
	}
	var total, used uint64
	for _, entry := range devices {
		total += entry.total
		used += entry.used
	}
	return total, used
}

func includeFreeBSDMount(part gopsutilDisk.PartitionStat) bool {
	if part.Mountpoint == "/" {
		return true
	}
	mountPoint := strings.ToLower(part.Mountpoint)
	for _, prefix := range []string{"/tmp", "/var/tmp", "/dev", "/run", "/var/lib/containers", "/var/lib/docker", "/proc", "/sys", "/sys/fs/cgroup", "/etc/resolv.conf", "/etc/host", "/nix/store"} {
		if mountPoint == prefix || strings.HasPrefix(mountPoint, prefix) {
			return false
		}
	}
	fsType := strings.ToLower(part.Fstype)
	if fsType == "fuseblk" {
		return true
	}
	for _, excluded := range []string{"tmpfs", "devtmpfs", "udev", "nfs", "cifs", "smb", "vboxsf", "9p", "fuse", "overlay", "proc", "devpts", "sysfs", "cgroup", "mqueue", "hugetlbfs", "debugfs", "binfmt_misc", "securityfs"} {
		if fsType == excluded || strings.HasPrefix(fsType, excluded) {
			return false
		}
	}
	opts := strings.ToLower(strings.Join(part.Opts, ","))
	if strings.Contains(opts, "remote") || strings.Contains(opts, "network") {
		return false
	}
	if strings.HasPrefix(part.Device, "/dev/loop") {
		return false
	}
	return true
}

func freebsdLoadAvg() string {
	avg, err := gopsutilLoad.Avg()
	if err != nil {
		return "0 0 0"
	}
	return fmt.Sprintf("%.2f %.2f %.2f", avg.Load1, avg.Load5, avg.Load15)
}

func freebsdBootTimeMS() int64 {
	bootTime, err := gopsutilHost.BootTime()
	if err != nil || bootTime == 0 {
		return time.Now().UnixMilli()
	}
	return int64(bootTime) * 1000
}

func freebsdOSName() string {
	return firstNonEmpty(commandOutput("uname", "-sr"), "FreeBSD")
}

func freebsdCPUInfo() string {
	info, err := gopsutilCPU.Info()
	if err == nil && len(info) > 0 {
		name := strings.TrimSpace(info[0].ModelName)
		if name != "" {
			return name
		}
		if info[0].VendorID != "" || info[0].Family != "" {
			name = strings.TrimSpace(info[0].VendorID + " " + info[0].Family)
			if name != "" {
				return name
			}
		}
	}
	return runtime.GOARCH
}

func freebsdCPUCores() int {
	cores, err := gopsutilCPU.Counts(true)
	if err == nil && cores > 0 {
		return cores
	}
	return runtime.NumCPU()
}

func freebsdGPUInfo() any {
	out := commandOutput("pciconf", "-lv")
	if strings.TrimSpace(out) == "" {
		return nil
	}
	seen := map[string]bool{}
	var gpus []gpuMetric
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || (!strings.Contains(line, "VGA") && !strings.Contains(line, "Display")) {
			continue
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		gpus = append(gpus, gpuMetric{Name: line, Info: 0, ID: strconv.Itoa(len(gpus))})
	}
	if len(gpus) == 0 {
		return nil
	}
	return gpus
}

func freebsdConnCount(kind string) int {
	connections, err := gopsutilNet.Connections(kind)
	if err == nil {
		return len(connections)
	}
	return freebsdGenericConnCount(kind)
}

func freebsdGenericConnCount(kind string) int {
	out := commandOutput("netstat", "-an")
	count := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if kind == "tcp" && strings.HasPrefix(line, "tcp") && strings.Contains(line, "established") {
			count++
		}
		if kind == "udp" && strings.HasPrefix(line, "udp") {
			count++
		}
	}
	return count
}

func freebsdCommandLineCount(name string, args ...string) int {
	out := commandOutput(name, args...)
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	if count > 0 {
		count--
	}
	return count
}
