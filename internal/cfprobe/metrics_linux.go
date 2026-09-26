//go:build linux

package cfprobe

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	procCPUTimes cpuTimes
	procCPUOK    bool
)

func readCPUPercent() (float64, bool) {
	if usage, ok := readGopsutilCPUPercent(); ok {
		return usage, true
	}
	current, ok := readProcCPUTimes()
	if !ok {
		return 0, false
	}
	if !procCPUOK {
		procCPUTimes = current
		procCPUOK = true
		return 0, false
	}
	usage, ok := cpuUsagePercent(procCPUTimes, current)
	procCPUTimes = current
	return usage, ok
}

func readProcCPUTimes() (cpuTimes, bool) {
	line := ""
	_ = scanFile("/proc/stat", func(s string) {
		if line == "" && strings.HasPrefix(s, "cpu ") {
			line = s
		}
	})
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return cpuTimes{}, false
	}
	var total uint64
	for i, f := range fields[1:] {
		if i >= 8 {
			break
		}
		n, _ := strconv.ParseUint(f, 10, 64)
		total += n
	}
	idle, _ := strconv.ParseUint(fields[4], 10, 64)
	if len(fields) > 5 {
		iowait, _ := strconv.ParseUint(fields[5], 10, 64)
		idle += iowait
	}
	return cpuTimes{Total: total, Idle: idle}, true
}

func collectBasicStats() BasicStats {
	mem := readMemInfo()
	diskTotal, diskUsed, diskDevices := diskUsageLinux()
	return BasicStats{
		MemTotalMB:  mem["MemTotal"] / 1024,
		MemUsedMB:   usedMemMB(mem),
		SwapTotalMB: mem["SwapTotal"] / 1024,
		SwapUsedMB:  usedSwapMB(mem),
		DiskTotalMB: diskTotal,
		DiskUsedMB:  diskUsed,
		DiskDevices: diskDevices,
		LoadAvg:     linuxLoadAvg(),
		BootTimeMS:  linuxBootTimeMS(),
		OSName:      linuxOSName(),
		Arch:        runtime.GOARCH,
		Kernel:      commandOutput("uname", "-r"),
		CPUInfo:     linuxCPUInfo(),
		CPUCores:    runtime.NumCPU(),
		GPUInfo:     detectGPUInfo(),
		Processes:   linuxProcessCount(),
		TCPConn:     linuxConnCount("tcp") + linuxConnCount("tcp6"),
		UDPConn:     linuxConnCount("udp") + linuxConnCount("udp6"),
	}
}

func readMemoryStats() (MemoryStats, bool) {
	mem := readMemInfo()
	if mem["MemTotal"] == 0 {
		return MemoryStats{}, false
	}
	return MemoryStats{
		MemTotalMB:  mem["MemTotal"] / 1024,
		MemUsedMB:   usedMemMB(mem),
		SwapTotalMB: mem["SwapTotal"] / 1024,
		SwapUsedMB:  usedSwapMB(mem),
	}, true
}

func readNetBytes(ifaces string) NetBytes {
	wanted := splitInterfaceSet(ifaces)
	var total NetBytes
	_ = scanFile("/proc/net/dev", func(line string) {
		if !strings.Contains(line, ":") {
			return
		}
		name, rest, _ := strings.Cut(strings.TrimSpace(line), ":")
		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			return
		}
		if !shouldIncludeNetInterface(name, wanted) {
			return
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		total.RX += rx
		total.TX += tx
	})
	return total
}

func readMemInfo() map[string]uint64 {
	out := map[string]uint64{}
	_ = scanFile("/proc/meminfo", func(line string) {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			return
		}
		out[key] = parseFirstUint(rest)
	})
	return out
}

func usedMemMB(mem map[string]uint64) uint64 {
	return memoryUsedMBFromKB(mem["MemTotal"], mem["MemAvailable"], mem["MemFree"], mem["Buffers"], mem["Cached"])
}

func usedSwapMB(mem map[string]uint64) uint64 {
	return swapUsedMBFromKB(mem["SwapTotal"], mem["SwapFree"])
}

func linuxLoadAvg() string {
	fields := strings.Fields(readSmallFile("/proc/loadavg"))
	if len(fields) >= 3 {
		return strings.Join(fields[:3], " ")
	}
	return "0 0 0"
}

func linuxBootTimeMS() int64 {
	var boot int64
	_ = scanFile("/proc/stat", func(line string) {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			boot, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	})
	if boot > 0 {
		return boot * 1000
	}
	uptime := strings.Fields(readSmallFile("/proc/uptime"))
	if len(uptime) > 0 {
		f, _ := strconv.ParseFloat(uptime[0], 64)
		if f > 0 {
			return time.Now().Add(-time.Duration(f) * time.Second).UnixMilli()
		}
	}
	return 0
}

func linuxOSName() string {
	if isSynology() {
		if name := synologyOSName(); name != "" {
			return name
		}
	}
	values, err := parseKVFile("/etc/os-release")
	if err == nil {
		if pretty := values["PRETTY_NAME"]; pretty != "" {
			return pretty
		}
		if id := values["ID"]; id != "" {
			return id
		}
	}
	return "Linux"
}

func synologyOSName() string {
	values, err := parseKVFile("/etc.defaults/VERSION")
	if err != nil {
		values, err = parseKVFile("/etc/VERSION")
	}
	if err != nil {
		return "Synology DSM"
	}
	product := values["productversion"]
	build := values["buildnumber"]
	if product == "" {
		return "Synology DSM"
	}
	if build != "" {
		return "Synology DSM " + product + "-" + build
	}
	return "Synology DSM " + product
}

func linuxCPUInfo() string {
	info := ""
	_ = scanFile("/proc/cpuinfo", func(line string) {
		if info != "" {
			return
		}
		if strings.HasPrefix(line, "model name") || strings.HasPrefix(line, "Hardware") || strings.HasPrefix(line, "Processor") {
			_, v, ok := strings.Cut(line, ":")
			if ok {
				info = strings.TrimSpace(v)
			}
		}
	})
	if info == "" {
		info = runtime.GOARCH
	}
	return info
}

func linuxProcessCount() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err == nil {
			count++
		}
	}
	return count
}

func linuxConnCount(name string) int {
	count := 0
	_ = scanFile("/proc/net/"+name, func(line string) {
		if strings.HasPrefix(strings.TrimSpace(line), "sl") {
			return
		}
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			if strings.HasPrefix(name, "tcp") {
				if fields[3] == "01" {
					count++
				}
			} else {
				count++
			}
		}
	})
	return count
}

type diskUsageEntry struct {
	total     uint64
	used      uint64
	device    DiskDeviceRef
	hasDevice bool
}

func diskUsageLinux() (uint64, uint64, []DiskDeviceRef) {
	devices := map[string]diskUsageEntry{}
	_ = scanFile("/proc/mounts", func(line string) {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return
		}
		dev, mountPoint, fsType := fields[0], fields[1], fields[2]
		opts := ""
		if len(fields) >= 4 {
			opts = fields[3]
		}
		mountPoint = unescapeLinuxMountPoint(mountPoint)
		if !includeLinuxMount(dev, mountPoint, fsType, opts) {
			return
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mountPoint, &st); err != nil {
			return
		}
		size, used, ok := diskUsageMBFromBlocks(st.Blocks, st.Bfree, int64(st.Bsize))
		if !ok {
			return
		}
		deviceID := dev
		if strings.ToLower(fsType) == "zfs" {
			if idx := strings.Index(deviceID, "/"); idx != -1 {
				deviceID = deviceID[:idx]
			}
		}
		device, hasDevice := linuxDiskDeviceRef(dev, mountPoint, deviceID)
		entry := diskUsageEntry{total: size, used: used, device: device, hasDevice: hasDevice}
		// Dedup by the underlying filesystem id (statfs f_fsid) rather than the
		// device name, so a single physical disk is counted once even when it
		// appears twice inside a container: once as the overlay root ("/") and
		// again as a bind-mounted volume (e.g. "/data"), which carry different
		// device strings but share f_fsid. Fall back to the device string when
		// f_fsid is unset so distinct real disks are never wrongly merged.
		dedupKey := linuxMountFsidKey(&st)
		if existing, ok := devices[dedupKey]; dedupKey == "" {
			dedupKey = deviceID
		} else if ok && distinctBlockDevices(deviceID, existing.device.Key) {
			// Two different real block devices reporting the same f_fsid (e.g.
			// cloned filesystems sharing a UUID): keep them counted separately.
			dedupKey = deviceID
		}
		if existing, ok := devices[dedupKey]; !ok || diskEntryPreferred(entry, existing) {
			devices[dedupKey] = entry
		}
	})
	var total, used uint64
	diskDevices := make([]DiskDeviceRef, 0, len(devices))
	for _, entry := range devices {
		total += entry.total
		used += entry.used
		if entry.hasDevice {
			diskDevices = append(diskDevices, entry.device)
		}
	}
	sort.Slice(diskDevices, func(i, j int) bool {
		return diskDevices[i].Key < diskDevices[j].Key
	})
	return total, used, diskDevices
}

// linuxMountFsidKey builds a dedup key from the statfs f_fsid, which identifies
// the underlying filesystem. Returns "" when f_fsid is unset, so callers fall
// back to the device string instead of collapsing unrelated mounts.
func linuxMountFsidKey(st *syscall.Statfs_t) string {
	if st.Fsid.X__val[0] == 0 && st.Fsid.X__val[1] == 0 {
		return ""
	}
	return fmt.Sprintf("fsid:%d:%d", int64(st.Fsid.X__val[0]), int64(st.Fsid.X__val[1]))
}

// distinctBlockDevices reports whether two mount sources are two *different*
// real block devices (both under /dev/). Used to avoid ever merging two distinct
// physical disks that happen to report the same f_fsid (e.g. cloned filesystems
// sharing a UUID), while still allowing a non-/dev source such as an overlay
// root to fold into its backing device.
func distinctBlockDevices(a, b string) bool {
	return strings.HasPrefix(a, "/dev/") && strings.HasPrefix(b, "/dev/") && a != b
}

// diskEntryPreferred reports whether entry should replace existing for the same
// filesystem. Larger capacity wins (handles quotas); on a tie, prefer the entry
// backed by a real block device (non-zero major) so the reported device list
// keeps a usable major/minor for IO mapping rather than an anonymous overlay
// device (major 0).
func diskEntryPreferred(entry, existing diskUsageEntry) bool {
	if entry.total != existing.total {
		return entry.total > existing.total
	}
	return entry.device.Major != 0 && existing.device.Major == 0
}

func linuxDiskDeviceRef(dev, mountPoint, key string) (DiskDeviceRef, bool) {
	devPath := unescapeLinuxMountPoint(dev)
	if strings.HasPrefix(devPath, "/dev/") {
		var st syscall.Stat_t
		if err := syscall.Stat(devPath, &st); err == nil && st.Rdev != 0 {
			if device, ok := linuxDiskDeviceRefFromDev(key, uint64(st.Rdev)); ok {
				return device, true
			}
		}
	}

	var st syscall.Stat_t
	if err := syscall.Stat(mountPoint, &st); err == nil && st.Dev != 0 {
		if device, ok := linuxDiskDeviceRefFromDev(key, uint64(st.Dev)); ok {
			return device, true
		}
	}
	return DiskDeviceRef{}, false
}

func linuxDiskDeviceRefFromDev(key string, dev uint64) (DiskDeviceRef, bool) {
	major, minor := linuxDeviceMajorMinor(dev)
	if major == 0 && minor == 0 {
		return DiskDeviceRef{}, false
	}
	return DiskDeviceRef{
		Key:   key,
		Major: major,
		Minor: minor,
	}, true
}

func linuxDeviceMajorMinor(dev uint64) (uint64, uint64) {
	major := (dev & 0x00000000000fff00) >> 8
	major |= (dev & 0xfffff00000000000) >> 32
	minor := dev & 0x00000000000000ff
	minor |= (dev & 0x00000ffffff00000) >> 12
	return major, minor
}

func readDiskIOCounters(devices []DiskDeviceRef) DiskIOCounters {
	targets := linuxDiskIOTargets(devices)
	if len(targets) == 0 {
		return DiskIOCounters{}
	}
	var total DiskIOCounters
	matched := map[string]bool{}
	_ = scanFile("/proc/diskstats", func(line string) {
		fields := strings.Fields(line)
		if len(fields) < 14 {
			return
		}
		major, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return
		}
		minor, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return
		}
		id := linuxDiskID(major, minor)
		if !targets[id] {
			return
		}
		readOps, _ := strconv.ParseUint(fields[3], 10, 64)
		readSectors, _ := strconv.ParseUint(fields[5], 10, 64)
		readTime, _ := strconv.ParseUint(fields[6], 10, 64)
		writeOps, _ := strconv.ParseUint(fields[7], 10, 64)
		writeSectors, _ := strconv.ParseUint(fields[9], 10, 64)
		writeTime, _ := strconv.ParseUint(fields[10], 10, 64)
		ioTicks, _ := strconv.ParseUint(fields[12], 10, 64)

		total.ReadOps += readOps
		total.WriteOps += writeOps
		total.ReadBytes += readSectors * 512
		total.WriteBytes += writeSectors * 512
		total.ReadTimeMS += readTime
		total.WriteTimeMS += writeTime
		total.IOTicksMS += ioTicks
		matched[id] = true
	})
	keys := make([]string, 0, len(matched))
	for key := range matched {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	total.DeviceCount = len(keys)
	total.Fingerprint = strings.Join(keys, ",")
	return total
}

func linuxDiskIOTargets(devices []DiskDeviceRef) map[string]bool {
	targets := map[string]bool{}
	for _, device := range devices {
		if device.Major == 0 && device.Minor == 0 {
			continue
		}
		targets[linuxDiskID(device.Major, device.Minor)] = true
	}
	return targets
}

func linuxDiskID(major, minor uint64) string {
	return strconv.FormatUint(major, 10) + ":" + strconv.FormatUint(minor, 10)
}

func includeLinuxMount(dev, mountPoint, fsType, opts string) bool {
	if mountPoint == "/" {
		return true
	}
	mountPointLower := strings.ToLower(mountPoint)
	for _, prefix := range []string{"/tmp", "/var/tmp", "/dev", "/run", "/var/lib/containers", "/var/lib/docker", "/proc", "/sys", "/sys/fs/cgroup", "/etc/resolv.conf", "/etc/host", "/nix/store"} {
		if mountPointLower == prefix || strings.HasPrefix(mountPointLower, prefix) {
			return false
		}
	}
	fsTypeLower := strings.ToLower(fsType)
	if fsTypeLower == "autofs" && !strings.HasPrefix(dev, "/dev/") {
		return false
	}
	if fsTypeLower == "fuseblk" {
		return true
	}
	for _, excluded := range []string{"tmpfs", "devtmpfs", "udev", "nfs", "cifs", "smb", "vboxsf", "9p", "fuse", "overlay", "proc", "devpts", "sysfs", "cgroup", "mqueue", "hugetlbfs", "debugfs", "binfmt_misc", "securityfs"} {
		if fsTypeLower == excluded || strings.HasPrefix(fsTypeLower, excluded) {
			return false
		}
	}
	optsLower := strings.ToLower(opts)
	if strings.Contains(optsLower, "remote") || strings.Contains(optsLower, "network") {
		return false
	}
	if strings.HasPrefix(dev, "/dev/loop") {
		return false
	}
	return true
}

func unescapeLinuxMountPoint(path string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(path)
}
