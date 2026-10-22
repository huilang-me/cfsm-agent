//go:build !linux && !darwin && !windows && !freebsd

package cfprobe

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func readCPUPercent() (float64, bool) {
	return 0, false
}

func collectBasicStats() BasicStats {
	return BasicStats{
		LoadAvg:    genericLoadAvg(),
		BootTimeMS: time.Now().UnixMilli(),
		OSName:     runtime.GOOS,
		Arch:       runtime.GOARCH,
		Kernel:     commandOutput("uname", "-r"),
		CPUInfo:    runtime.GOARCH,
		CPUCores:   runtime.NumCPU(),
		GPUInfo:    detectGPUInfo(),
		Processes:  commandLineCount("ps", "-e"),
		TCPConn:    genericConnCount("tcp"),
		UDPConn:    genericConnCount("udp"),
	}
}

func readMemoryStats() (MemoryStats, bool) {
	return MemoryStats{}, false
}

func readNetBytes(_ string) (NetBytes, error) {
	return NetBytes{}, errors.New("network counters are unsupported on this platform")
}

func readDiskIOCounters(_ []DiskDeviceRef) DiskIOCounters {
	return DiskIOCounters{}
}

func genericLoadAvg() string {
	out := commandOutput("uptime")
	idx := strings.LastIndex(out, "load averages:")
	if idx < 0 {
		idx = strings.LastIndex(out, "load average:")
	}
	if idx < 0 {
		return "0 0 0"
	}
	fields := strings.Fields(strings.ReplaceAll(out[idx:], ",", ""))
	if len(fields) >= 5 {
		return strings.Join(fields[len(fields)-3:], " ")
	}
	return "0 0 0"
}

func genericConnCount(kind string) int {
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

func commandLineCount(name string, args ...string) int {
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

func parseInt(raw string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n
}
