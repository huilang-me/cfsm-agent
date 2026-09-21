package cfprobe

import (
	"encoding/json"
	"testing"
)

func TestCPUUsagePercentFromZeroPrevious(t *testing.T) {
	got, ok := cpuUsagePercent(cpuTimes{}, cpuTimes{Total: 100, Idle: 75})
	if !ok {
		t.Fatal("expected cpuUsagePercent to calculate from zero previous sample")
	}
	if got != 25 {
		t.Fatalf("got %.2f, want 25", got)
	}
}

func TestCPUPercentStringReportsZeroAsZero(t *testing.T) {
	if got := cpuPercentString(0); got != "0.00" {
		t.Fatalf("got %q, want 0.00", got)
	}
}

func TestCPUPercentStringReportsPercentUnits(t *testing.T) {
	if got := cpuPercentString(5); got != "5.00" {
		t.Fatalf("got %q, want 5.00", got)
	}
}

func TestDiskUsageMBFromBlocksUsesFreeBlocksForUsedValue(t *testing.T) {
	total, used, ok := diskUsageMBFromBlocks(100, 65, int64(bytesPerMiB))
	if !ok {
		t.Fatal("expected disk usage calculation to succeed")
	}
	if total != 100 {
		t.Fatalf("total = %d, want 100", total)
	}
	if used != 35 {
		t.Fatalf("used = %d, want 35", used)
	}
}

func TestDiskIOStatsFromCounters(t *testing.T) {
	prev := DiskIOCounters{
		ReadBytes:   1024,
		WriteBytes:  2048,
		ReadOps:     10,
		WriteOps:    20,
		ReadTimeMS:  100,
		WriteTimeMS: 300,
		IOTicksMS:   1000,
		DeviceCount: 2,
		Fingerprint: "8:1,8:2",
	}
	current := DiskIOCounters{
		ReadBytes:   3072,
		WriteBytes:  6144,
		ReadOps:     14,
		WriteOps:    28,
		ReadTimeMS:  180,
		WriteTimeMS: 480,
		IOTicksMS:   1600,
		DeviceCount: 2,
		Fingerprint: "8:1,8:2",
	}

	got := diskIOStatsFromCounters(prev, current, 2)
	if got.ReadBps != 1024 {
		t.Fatalf("read_bps = %d, want 1024", got.ReadBps)
	}
	if got.WriteBps != 2048 {
		t.Fatalf("write_bps = %d, want 2048", got.WriteBps)
	}
	if got.ReadIOPS != 2 {
		t.Fatalf("read_iops = %.2f, want 2", got.ReadIOPS)
	}
	if got.WriteIOPS != 4 {
		t.Fatalf("write_iops = %.2f, want 4", got.WriteIOPS)
	}
	if got.AwaitMS != 21.67 {
		t.Fatalf("await_ms = %.2f, want 21.67", got.AwaitMS)
	}
	if got.Util != 15 {
		t.Fatalf("util = %.2f, want 15", got.Util)
	}
}

func TestDiskIOStatsFromCountersRejectsChangedDeviceSet(t *testing.T) {
	prev := DiskIOCounters{ReadBytes: 1024, DeviceCount: 1, Fingerprint: "8:1"}
	current := DiskIOCounters{ReadBytes: 4096, DeviceCount: 1, Fingerprint: "8:2"}

	got := diskIOStatsFromCounters(prev, current, 30)
	if got != (DiskIOStats{}) {
		t.Fatalf("got %+v, want zero stats", got)
	}
}

func TestShouldIncludeNetInterfaceExcludesTunnelInterfaces(t *testing.T) {
	for _, name := range []string{"tailscale0", "tun0", "wg0", "wireguard0", "ipsec0", "gre0", "gretap0", "ipip0", "sit0", "ip6tnl0", "zerotier0"} {
		if shouldIncludeNetInterface(name, nil) {
			t.Fatalf("%s should be excluded", name)
		}
	}
}

func TestShouldIncludeNetInterfaceAllowsPhysicalInterface(t *testing.T) {
	if !shouldIncludeNetInterface("eth0", nil) {
		t.Fatal("eth0 should be included")
	}
}

func TestMemoryUsedMBFromKBUsesMemAvailable(t *testing.T) {
	if got := memoryUsedMBFromKB(8*1024, 3*1024, 0, 0, 0); got != 5 {
		t.Fatalf("got %d, want 5", got)
	}
}

func TestMemoryUsedMBFromKBFallsBackToFreeBuffersCached(t *testing.T) {
	if got := memoryUsedMBFromKB(8*1024, 0, 1*1024, 2*1024, 3*1024); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
}

func TestMemoryUsedMBFromKBClampsNegativeUsage(t *testing.T) {
	if got := memoryUsedMBFromKB(3*1024, 8*1024, 0, 0, 0); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

func TestSwapUsedMBFromKB(t *testing.T) {
	if got := swapUsedMBFromKB(4*1024, 1*1024); got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
}

func TestSwapUsedMBFromKBClampsNegativeUsage(t *testing.T) {
	if got := swapUsedMBFromKB(1*1024, 4*1024); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

func TestParseNvidiaSMILegacyOutput(t *testing.T) {
	gpus := parseNvidiaSMI("0, NVIDIA GeForce RTX 3060, 12\n1, NVIDIA GeForce RTX 3060, 87\n", nvidiaSMILegacyTail)
	if len(gpus) != 2 {
		t.Fatalf("got %d gpus, want 2", len(gpus))
	}
	if gpus[0].ID != "0" || gpus[0].Name != "NVIDIA GeForce RTX 3060" || gpus[0].Info != float64(12) {
		t.Fatalf("got %+v, want id 0 name NVIDIA GeForce RTX 3060 info 12", gpus[0])
	}
	if gpus[1].Info != float64(87) {
		t.Fatalf("got info %v, want 87", gpus[1].Info)
	}
	if gpus[0].MemUsed != nil || gpus[0].MemTotal != nil || gpus[0].SMClock != nil || gpus[0].Power != nil {
		t.Fatalf("legacy parse should leave detail fields nil, got %+v", gpus[0])
	}
}

func TestParseNvidiaSMILegacyOutputWithCommaInName(t *testing.T) {
	gpus := parseNvidiaSMI("0, NVIDIA Tesla, T4, 5\n", nvidiaSMILegacyTail)
	if len(gpus) != 1 {
		t.Fatalf("got %d gpus, want 1", len(gpus))
	}
	if gpus[0].Name != "NVIDIA Tesla, T4" {
		t.Fatalf("got name %q, want NVIDIA Tesla, T4", gpus[0].Name)
	}
	if gpus[0].Info != float64(5) {
		t.Fatalf("got info %v, want 5", gpus[0].Info)
	}
}

func TestParseNvidiaSMIDetailOutput(t *testing.T) {
	gpus := parseNvidiaSMI("0, NVIDIA GeForce RTX 4090, 55, 9216, 24564, 2520, 328.5\n1, NVIDIA A100, 0, 1024, 81920, [N/A], N/A\n", nvidiaSMIDetailTail)
	if len(gpus) != 2 {
		t.Fatalf("got %d gpus, want 2", len(gpus))
	}
	first := gpus[0]
	if first.ID != "0" || first.Name != "NVIDIA GeForce RTX 4090" || first.Info != float64(55) {
		t.Fatalf("got %+v, want id 0 name NVIDIA GeForce RTX 4090 info 55", first)
	}
	if first.MemUsed != float64(9216) || first.MemTotal != float64(24564) || first.SMClock != float64(2520) || first.Power != 328.5 {
		t.Fatalf("got detail %+v, want 9216/24564/2520/328.5", first)
	}
	second := gpus[1]
	if second.MemUsed != float64(1024) || second.MemTotal != float64(81920) {
		t.Fatalf("got detail %+v, want mem 1024/81920", second)
	}
	if second.SMClock != nil || second.Power != nil {
		t.Fatalf("unsupported values should parse to nil, got %+v", second)
	}
}

func TestParseNvidiaSMIDetailOutputWithCommaInName(t *testing.T) {
	gpus := parseNvidiaSMI("0, NVIDIA Tesla, T4, 10, 100, 1000, 100, 50\n", nvidiaSMIDetailTail)
	if len(gpus) != 1 {
		t.Fatalf("got %d gpus, want 1", len(gpus))
	}
	if gpus[0].Name != "NVIDIA Tesla, T4" {
		t.Fatalf("got name %q, want NVIDIA Tesla, T4", gpus[0].Name)
	}
	if gpus[0].Info != float64(10) || gpus[0].MemUsed != float64(100) || gpus[0].MemTotal != float64(1000) || gpus[0].SMClock != float64(100) || gpus[0].Power != float64(50) {
		t.Fatalf("got %+v, want 10/100/1000/100/50", gpus[0])
	}
}

func TestParseNvidiaSMISkipsShortLines(t *testing.T) {
	if gpus := parseNvidiaSMI("0, 12\n\n1, NVIDIA GPU, 3\n", nvidiaSMILegacyTail); len(gpus) != 1 {
		t.Fatalf("got %d gpus, want 1", len(gpus))
	}
	if gpus := parseNvidiaSMI("0, NVIDIA GPU, 12, 100, 1000, 2520\n", nvidiaSMIDetailTail); len(gpus) != 0 {
		t.Fatalf("got %d gpus, want 0", len(gpus))
	}
}

func TestParseNvidiaFloat(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{" 12.5 ", 12.5},
		{"2520", 2520.0},
		{"[N/A]", nil},
		{"N/A", nil},
		{"n/a", nil},
		{"NOT SUPPORTED", nil},
		{"", nil},
		{"abc", nil},
	}
	for _, c := range cases {
		if got := parseNvidiaFloat(c.in); got != c.want {
			t.Fatalf("parseNvidiaFloat(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestGPUMetricJSONOmitsNilDetailFields(t *testing.T) {
	data, err := json.Marshal([]gpuMetric{{Name: "NVIDIA GPU", Info: 12.5, ID: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"NVIDIA GPU","info":12.5,"id":"0"}]`
	if string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
}

func TestGPUMetricJSONIncludesDetailFields(t *testing.T) {
	data, err := json.Marshal([]gpuMetric{{Name: "NVIDIA GPU", Info: 12.5, ID: "0", MemUsed: 100, MemTotal: 24564, SMClock: 2520, Power: 328.5}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"NVIDIA GPU","info":12.5,"id":"0","mem_used":100,"mem_total":24564,"sm_clock":2520,"power":328.5}]`
	if string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
}
