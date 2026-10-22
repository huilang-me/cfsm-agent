//go:build darwin

package cfprobe

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	darwinGPUNameOnce sync.Once
	darwinGPUNames    []string
	darwinHWOnce      sync.Once
	darwinHW          map[string]string
)

func readCPUPercent() (float64, bool) {
	if usage, ok := darwinTopCPUPercent(); ok {
		return usage, true
	}
	if usage, ok := darwinPSCPUPercent(); ok {
		return usage, true
	}
	return 0, false
}

func collectBasicStats() BasicStats {
	total, used := darwinMemoryMB()
	diskTotal, diskUsed := darwinDiskUsage("/")
	return BasicStats{
		MemTotalMB:  total,
		MemUsedMB:   used,
		SwapTotalMB: 0,
		SwapUsedMB:  0,
		DiskTotalMB: diskTotal,
		DiskUsedMB:  diskUsed,
		LoadAvg:     darwinLoadAvg(),
		BootTimeMS:  darwinBootTimeMS(),
		OSName:      darwinOSName(),
		Arch:        runtime.GOARCH,
		Kernel:      commandOutput("uname", "-r"),
		CPUInfo:     darwinCPUInfo(),
		CPUCores:    runtime.NumCPU(),
		GPUInfo:     darwinGPUInfo(),
		Processes:   commandLineCount("ps", "-e"),
		TCPConn:     darwinConnCount("tcp"),
		UDPConn:     darwinConnCount("udp"),
	}
}

func readMemoryStats() (MemoryStats, bool) {
	total, used := darwinMemoryMB()
	if total == 0 {
		return MemoryStats{}, false
	}
	return MemoryStats{MemTotalMB: total, MemUsedMB: used}, true
}

func darwinTopCPUPercent() (float64, bool) {
	out := commandOutput("top", "-l", "1", "-n", "0", "-s", "0")
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "CPU usage:") {
			continue
		}
		parts := strings.Split(line, ",")
		for _, part := range parts {
			if !strings.Contains(part, "idle") {
				continue
			}
			idle, ok := parsePercentBeforeMark(part)
			if !ok {
				return 0, false
			}
			return 100 - idle, true
		}
	}
	return 0, false
}

func darwinPSCPUPercent() (float64, bool) {
	out := commandOutput("ps", "-A", "-o", "%cpu=")
	var total float64
	var found bool
	for _, field := range strings.Fields(out) {
		value, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
		if err != nil {
			continue
		}
		total += value
		found = true
	}
	if !found {
		return 0, false
	}
	cores := runtime.NumCPU()
	if cores <= 0 {
		cores = 1
	}
	return total / float64(cores), true
}

func parsePercentBeforeMark(raw string) (float64, bool) {
	idx := strings.Index(raw, "%")
	if idx < 0 {
		return 0, false
	}
	fields := strings.Fields(raw[:idx])
	if len(fields) == 0 {
		return 0, false
	}
	value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	return value, err == nil
}

func darwinCPUInfo() string {
	if brand := strings.TrimSpace(commandOutput("sysctl", "-n", "machdep.cpu.brand_string")); brand != "" {
		return brand
	}
	if chip := darwinHardwareValue("chip_type"); chip != "" {
		return chip
	}
	return runtime.GOARCH
}

func darwinHardwareValue(key string) string {
	darwinHWOnce.Do(func() {
		darwinHW = map[string]string{}
		out := commandOutput("system_profiler", "SPHardwareDataType", "-json")
		var report struct {
			Hardware []map[string]any `json:"SPHardwareDataType"`
		}
		if json.Unmarshal([]byte(out), &report) != nil || len(report.Hardware) == 0 {
			return
		}
		for k, v := range report.Hardware[0] {
			if value := stringFromAny(v); value != "" {
				darwinHW[k] = value
			}
		}
	})
	return darwinHW[key]
}

func darwinGPUInfo() any {
	if gpu := detectGPUInfo(); gpu != nil {
		return gpu
	}
	names := darwinGetGPUNames()
	if len(names) == 0 {
		return nil
	}
	usages := darwinGPUUsagesFromIOReg()
	gpus := make([]gpuMetric, 0, len(names))
	for i, name := range names {
		info := any(0)
		if i < len(usages) {
			info = usages[i]
		}
		gpus = append(gpus, gpuMetric{Name: name, Info: info, ID: strconv.Itoa(i)})
	}
	return gpus
}

func darwinGetGPUNames() []string {
	darwinGPUNameOnce.Do(func() {
		darwinGPUNames = darwinGPUNamesFromSystemProfilerJSON()
		if len(darwinGPUNames) == 0 {
			darwinGPUNames = darwinGPUNamesFromSystemProfilerText()
		}
		if len(darwinGPUNames) == 0 {
			if name := darwinGPUModelFromIOReg(); name != "" {
				darwinGPUNames = []string{name}
			}
		}
	})
	return append([]string(nil), darwinGPUNames...)
}

func darwinGPUNamesFromSystemProfilerJSON() []string {
	out := commandOutput("system_profiler", "SPDisplaysDataType", "-json")
	var report struct {
		Displays []map[string]any `json:"SPDisplaysDataType"`
	}
	if json.Unmarshal([]byte(out), &report) != nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, item := range report.Displays {
		name := firstNonEmpty(
			stringFromAny(item["sppci_model"]),
			stringFromAny(item["_name"]),
			stringFromAny(item["spdisplays_chipset-model"]),
		)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func darwinGPUNamesFromSystemProfilerText() []string {
	out := commandOutput("system_profiler", "SPDisplaysDataType")
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		name := strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
		if name == line || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func darwinGPUModelFromIOReg() string {
	out := commandOutput("ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")
	return parseIORegQuotedValue(out, "model")
}

func darwinGPUUsagesFromIOReg() []float64 {
	out := commandOutput("ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")
	usages := parseIORegNumberValues(out, "Device Utilization %")
	if len(usages) == 0 {
		usages = parseIORegNumberValues(out, "Renderer Utilization %")
	}
	for i, usage := range usages {
		if usage < 0 {
			usages[i] = 0
		}
		if usage > 100 {
			usages[i] = 100
		}
	}
	return usages
}

func parseIORegQuotedValue(out, key string) string {
	pattern := `"` + key + `"`
	idx := strings.Index(out, pattern)
	if idx < 0 {
		return ""
	}
	rest := out[idx+len(pattern):]
	eq := strings.Index(rest, "=")
	if eq < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[eq+1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func parseIORegNumberValues(out, key string) []float64 {
	pattern := `"` + key + `"=`
	var values []float64
	for {
		idx := strings.Index(out, pattern)
		if idx < 0 {
			return values
		}
		rest := out[idx+len(pattern):]
		end := 0
		for end < len(rest) {
			ch := rest[end]
			if (ch < '0' || ch > '9') && ch != '.' {
				break
			}
			end++
		}
		if end > 0 {
			if value, err := strconv.ParseFloat(rest[:end], 64); err == nil {
				values = append(values, value)
			}
		}
		out = rest[end:]
	}
}

func stringFromAny(v any) string {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	default:
		return ""
	}
}

func readNetBytes(ifaces string) (NetBytes, error) {
	wanted := splitInterfaceSet(ifaces)
	seen := map[string]bool{}
	var total NetBytes
	out, err := commandOutputChecked("netstat", "-ibn")
	if err != nil {
		return NetBytes{}, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[0] == "Name" || !strings.HasPrefix(fields[2], "<Link#") {
			continue
		}
		name := fields[0]
		if seen[name] {
			continue
		}
		if len(wanted) > 0 {
			if !wanted[name] {
				continue
			}
		} else if name == "lo0" || strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "awdl") {
			continue
		}
		rx, rxErr := strconv.ParseUint(fields[6], 10, 64)
		tx, txErr := strconv.ParseUint(fields[9], 10, 64)
		if rxErr != nil || txErr != nil {
			return NetBytes{}, fmt.Errorf("invalid network counters for %s", name)
		}
		total.RX += rx
		total.TX += tx
		seen[name] = true
	}
	if len(seen) == 0 {
		return NetBytes{}, fmt.Errorf("no matching network interfaces")
	}
	return total, nil
}

func readDiskIOCounters(_ []DiskDeviceRef) DiskIOCounters {
	return DiskIOCounters{}
}

func darwinMemoryMB() (uint64, uint64) {
	total := parseFirstUint(commandOutput("sysctl", "-n", "hw.memsize")) / 1024 / 1024
	out := commandOutput("vm_stat")
	pageSize := uint64(0)
	var freePages, inactivePages, speculativePages uint64
	for _, line := range strings.Split(out, "\n") {
		if pageSize == 0 {
			if _, value, ok := strings.Cut(line, "page size of "); ok {
				pageSize = parseFirstUint(value)
			}
		}
		clean := strings.ReplaceAll(line, ".", "")
		label, v, ok := strings.Cut(clean, ":")
		if !ok {
			continue
		}
		pages := parseFirstUint(v)
		switch strings.TrimSpace(label) {
		case "Pages free":
			freePages = pages
		case "Pages inactive":
			inactivePages = pages
		case "Pages speculative":
			speculativePages = pages
		}
	}
	if pageSize == 0 {
		pageSize = parseFirstUint(commandOutput("sysctl", "-n", "hw.pagesize"))
	}
	if pageSize == 0 {
		pageSize = 4096
	}

	// Preserve the existing available-memory estimate while using the actual VM page size.
	availablePages := freePages + inactivePages + speculativePages
	availableMB := availablePages * pageSize / 1024 / 1024
	if total < availableMB {
		return total, 0
	}
	return total, total - availableMB
}

func darwinDiskUsage(path string) (uint64, uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	total := uint64(st.Blocks) * uint64(st.Bsize) / 1024 / 1024
	free := uint64(st.Bavail) * uint64(st.Bsize) / 1024 / 1024
	if total < free {
		return total, 0
	}
	return total, total - free
}

func darwinLoadAvg() string {
	out := strings.NewReplacer("{", "", "}", "").Replace(commandOutput("sysctl", "-n", "vm.loadavg"))
	fields := strings.Fields(out)
	if len(fields) >= 3 {
		return strings.Join(fields[:3], " ")
	}
	return "0 0 0"
}

func darwinBootTimeMS() int64 {
	out := commandOutput("sysctl", "-n", "kern.boottime")
	if _, rest, ok := strings.Cut(out, "sec = "); ok {
		secRaw := strings.Split(rest, ",")[0]
		sec, _ := strconv.ParseInt(strings.TrimSpace(secRaw), 10, 64)
		return sec * 1000
	}
	return time.Now().UnixMilli()
}

func darwinOSName() string {
	product := commandOutput("sw_vers", "-productName")
	version := commandOutput("sw_vers", "-productVersion")
	if product == "" {
		product = "macOS"
	}
	if version != "" {
		return product + " " + version
	}
	return product
}

func darwinConnCount(kind string) int {
	out := commandOutput("netstat", "-an", "-p", kind)
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if kind == "tcp" && strings.Contains(line, "ESTABLISHED") {
			count++
		}
		if kind == "udp" && strings.HasPrefix(strings.TrimSpace(line), "udp") {
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
