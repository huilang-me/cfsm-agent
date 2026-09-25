//go:build windows

package cfprobe

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	gopsutilCPU "github.com/shirou/gopsutil/v4/cpu"
	"golang.org/x/sys/windows/registry"
)

func readCPUPercent() (float64, bool) {
	percentages, err := gopsutilCPU.Percent(0, false)
	if err != nil || len(percentages) == 0 {
		return 0, false
	}
	return percentages[0], true
}

func collectBasicStats() BasicStats {
	total, used := windowsMemoryMB()
	diskTotal, diskUsed := windowsDiskUsage()
	return BasicStats{
		MemTotalMB:  total,
		MemUsedMB:   used,
		SwapTotalMB: 0,
		SwapUsedMB:  0,
		DiskTotalMB: diskTotal,
		DiskUsedMB:  diskUsed,
		LoadAvg:     "0 0 0",
		BootTimeMS:  windowsBootTimeMS(),
		OSName:      windowsOSName(),
		Arch:        nativeArch(),
		Kernel:      windowsKernelVersion(),
		CPUInfo:     windowsCPUInfo(),
		CPUCores:    runtime.NumCPU(),
		GPUInfo:     windowsGPUInfo(),
		Processes:   windowsProcessCount(),
		TCPConn:     windowsConnCount("TCP"),
		UDPConn:     windowsConnCount("UDP"),
	}
}

func readMemoryStats() (MemoryStats, bool) {
	total, used := windowsMemoryMB()
	if total == 0 {
		return MemoryStats{}, false
	}
	return MemoryStats{MemTotalMB: total, MemUsedMB: used}, true
}

func windowsPowerShellOutput(script string) string {
	prefix := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8; "
	return commandOutput("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", prefix+script)
}

func readNetBytes(ifaces string) NetBytes {
	script := "Get-NetAdapterStatistics | ForEach-Object { \"$($_.Name),$($_.ReceivedBytes),$($_.SentBytes)\" }"
	out := windowsPowerShellOutput(script)
	wanted := splitInterfaceSet(ifaces)
	var total NetBytes
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) != 3 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if len(wanted) > 0 && !wanted[name] {
			continue
		}
		rx, _ := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		tx, _ := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 64)
		total.RX += rx
		total.TX += tx
	}
	return total
}

func readDiskIOCounters(_ []DiskDeviceRef) DiskIOCounters {
	return DiskIOCounters{}
}

func windowsOSName() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return firstNonEmpty(windowsPowerShellOutput("(Get-CimInstance Win32_OperatingSystem).Caption"), "Microsoft Windows")
	}
	defer key.Close()

	productName, _, err := key.GetStringValue("ProductName")
	if err != nil || strings.TrimSpace(productName) == "" {
		return firstNonEmpty(windowsPowerShellOutput("(Get-CimInstance Win32_OperatingSystem).Caption"), "Microsoft Windows")
	}
	productName = strings.TrimSpace(productName)
	if strings.Contains(productName, "Server") || strings.Contains(productName, "Windows 11") {
		return productName
	}
	if build, ok := windowsCurrentBuild(key); ok && build >= 22000 && strings.Contains(productName, "Windows 10") {
		return strings.Replace(productName, "Windows 10", "Windows 11", 1)
	}
	return productName
}

func windowsKernelVersion() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return firstNonEmpty(windowsPowerShellOutput("(Get-CimInstance Win32_OperatingSystem).Version"), "Windows")
	}
	defer key.Close()

	build, _ := windowsCurrentBuildString(key)
	major, _, majorErr := key.GetIntegerValue("CurrentMajorVersionNumber")
	minor, _, minorErr := key.GetIntegerValue("CurrentMinorVersionNumber")
	version := ""
	if majorErr == nil && minorErr == nil && build != "" {
		version = fmt.Sprintf("%d.%d.%s", major, minor, build)
	} else {
		version, _, _ = key.GetStringValue("CurrentVersion")
		if version != "" && build != "" && !strings.HasSuffix(version, "."+build) {
			version += "." + build
		}
	}
	if version == "" {
		version = firstNonEmpty(windowsPowerShellOutput("(Get-CimInstance Win32_OperatingSystem).Version"), build, "Windows")
	}
	if ubr, _, err := key.GetIntegerValue("UBR"); err == nil {
		return version + "." + strconv.FormatUint(ubr, 10)
	}
	return version
}

func windowsCPUInfo() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err == nil {
		defer key.Close()
		name, _, err := key.GetStringValue("ProcessorNameString")
		if err == nil && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
	}
	return firstNonEmpty(windowsPowerShellOutput("(Get-CimInstance Win32_Processor | Select-Object -First 1 -ExpandProperty Name)"), nativeArch())
}

func windowsCurrentBuild(key registry.Key) (int, bool) {
	raw, ok := windowsCurrentBuildString(key)
	if !ok {
		return 0, false
	}
	build, err := strconv.Atoi(raw)
	return build, err == nil
}

func windowsCurrentBuildString(key registry.Key) (string, bool) {
	for _, valueName := range []string{"CurrentBuildNumber", "CurrentBuild"} {
		value, _, err := key.GetStringValue(valueName)
		if err == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

func windowsMemoryMB() (uint64, uint64) {
	out := windowsPowerShellOutput("$o=Get-CimInstance Win32_OperatingSystem; \"$($o.TotalVisibleMemorySize),$($o.FreePhysicalMemory)\"")
	parts := strings.Split(out, ",")
	if len(parts) != 2 {
		return 0, 0
	}
	totalKB := parseFirstUint(parts[0])
	freeKB := parseFirstUint(parts[1])
	if totalKB < freeKB {
		return totalKB / 1024, 0
	}
	return totalKB / 1024, (totalKB - freeKB) / 1024
}

func windowsDiskUsage() (uint64, uint64) {
	out := windowsPowerShellOutput("Get-CimInstance Win32_LogicalDisk -Filter \"DriveType=3\" | ForEach-Object { \"$($_.Size),$($_.FreeSpace)\" }")
	var total, used uint64
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) != 2 {
			continue
		}
		size := parseFirstUint(parts[0]) / 1024 / 1024
		free := parseFirstUint(parts[1]) / 1024 / 1024
		if size >= free {
			total += size
			used += size - free
		}
	}
	return total, used
}

func windowsBootTimeMS() int64 {
	out := windowsPowerShellOutput("([DateTimeOffset](Get-CimInstance Win32_OperatingSystem).LastBootUpTime).ToUnixTimeMilliseconds()")
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Now().UnixMilli()
	}
	return n
}

func windowsProcessCount() int {
	out := windowsPowerShellOutput("(Get-Process).Count")
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

func windowsConnCount(kind string) int {
	cmdlet := "Get-NetTCPConnection"
	if kind == "UDP" {
		cmdlet = "Get-NetUDPEndpoint"
	}
	out := windowsPowerShellOutput("(" + cmdlet + ").Count")
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}
