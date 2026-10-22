package cfprobe

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareInstallTrafficCorrection(t *testing.T) {
	readErr := errors.New("network counters unavailable")
	baseline := NetBytes{RX: 123, TX: 456}
	for _, tt := range []struct {
		name      string
		rx, tx    string
		readErr   error
		wantReads int
		wantError bool
	}{
		{name: "no correction"},
		{name: "zero correction", rx: "0", wantReads: 1},
		{name: "receive correction", rx: "1.5", wantReads: 1},
		{name: "transmit correction", tx: "2", wantReads: 1},
		{name: "invalid receive correction", rx: "-1", wantError: true},
		{name: "invalid transmit correction", rx: "1", tx: "invalid", wantError: true},
		{name: "failed network read", rx: "1", readErr: readErr, wantReads: 1, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := InstallOptions{Config: Config{Interface: "eth0,eth1"}, RXCorrectionGB: tt.rx, TXCorrectionGB: tt.tx}
			reads := 0
			got, err := prepareInstallTrafficCorrection(opts, func(iface string) (NetBytes, error) {
				reads++
				if iface != opts.Interface {
					t.Fatalf("sampled interface %q, want %q", iface, opts.Interface)
				}
				return baseline, tt.readErr
			})
			if (err != nil) != tt.wantError || reads != tt.wantReads {
				t.Fatalf("error=%v reads=%d, want error=%v reads=%d", err, reads, tt.wantError, tt.wantReads)
			}
			if tt.readErr != nil && !errors.Is(err, tt.readErr) {
				t.Fatalf("network read error not preserved: %v", err)
			}
			want := NetBytes{}
			if !tt.wantError && tt.wantReads > 0 {
				want = baseline
			}
			if got != want {
				t.Fatalf("baseline=%+v, want %+v", got, want)
			}
		})
	}
}

func TestMergeExplicitInstallConfig(t *testing.T) {
	existing := Config{
		ServerID:       "sid",
		Secret:         "secret",
		WorkerURL:      "https://worker.example.com/report",
		ReportInterval: 120,
		ResetDay:       5,
		ConnectionMode: connectionModeAuto,
		PingMode:       pingModeTCP,
		AutoUpdate:     false,
	}
	flagConfig := Config{
		ReportInterval: defaultReportIntervalSec,
		ResetDay:       1,
		ConnectionMode: connectionModeHTTP,
		PingMode:       pingModeICMP,
		AutoUpdate:     true,
		UpdateProxy:    "https://gh-proxy.example.com",
	}

	merged := existing
	mergeExplicitInstallConfig(&merged, flagConfig, map[string]bool{"auto_update": true})
	if !merged.AutoUpdate {
		t.Fatal("AutoUpdate = false, want true when -auto_update=1 is explicit")
	}
	if merged.ReportInterval != existing.ReportInterval || merged.ResetDay != existing.ResetDay {
		t.Fatalf("non-explicit fields changed: %+v", merged)
	}
	if merged.UpdateProxy != "" {
		t.Fatalf("UpdateProxy = %q, want preserved empty", merged.UpdateProxy)
	}
	if merged.ConnectionMode != connectionModeAuto {
		t.Fatalf("ConnectionMode = %q, want preserved %q", merged.ConnectionMode, connectionModeAuto)
	}
	if merged.PingMode != pingModeTCP {
		t.Fatalf("PingMode = %q, want preserved %q", merged.PingMode, pingModeTCP)
	}

	merged = existing
	mergeExplicitInstallConfig(&merged, flagConfig, map[string]bool{"connection_mode": true})
	if merged.ConnectionMode != connectionModeHTTP {
		t.Fatalf("ConnectionMode = %q, want %q", merged.ConnectionMode, connectionModeHTTP)
	}

	merged = existing
	mergeExplicitInstallConfig(&merged, flagConfig, map[string]bool{"ping_mode": true})
	if merged.PingMode != pingModeICMP {
		t.Fatalf("PingMode = %q, want %q", merged.PingMode, pingModeICMP)
	}

	merged = existing
	mergeExplicitInstallConfig(&merged, flagConfig, map[string]bool{"install_ghproxy": true})
	if merged.UpdateProxy != flagConfig.UpdateProxy {
		t.Fatalf("UpdateProxy = %q, want %q", merged.UpdateProxy, flagConfig.UpdateProxy)
	}

	existingAuto := existing
	existingAuto.AutoUpdate = true
	merged = existingAuto
	off := flagConfig
	off.AutoUpdate = false
	mergeExplicitInstallConfig(&merged, off, map[string]bool{"auto_update": true})
	if merged.AutoUpdate {
		t.Fatal("AutoUpdate = true, want false when -auto_update=0 is explicit")
	}
}

func TestWriteSystemdUserServiceUsesUserSafeSettings(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		ServiceName: "cf-probe",
		BinaryFile:  filepath.Join(tmp, "bin", "cf-probe"),
		ConfigDir:   filepath.Join(tmp, ".cf-probe"),
		ConfigFile:  filepath.Join(tmp, ".cf-probe", "config.conf"),
		ServiceFile: filepath.Join(tmp, "cf-probe.service"),
	}

	if err := writeSystemdUserService(paths, false); err != nil {
		t.Fatalf("writeSystemdUserService returned error: %v", err)
	}
	data, err := os.ReadFile(paths.ServiceFile)
	if err != nil {
		t.Fatalf("read service file: %v", err)
	}
	content := string(data)
	wantExec := "ExecStart=" + quoteSystemdExecArg(paths.BinaryFile) + " run -config=" + quoteSystemdExecArg(paths.ConfigFile) + " -debug=0"
	if !strings.Contains(content, wantExec) {
		t.Fatalf("service content missing ExecStart %q:\n%s", wantExec, content)
	}
	for _, disallowed := range []string{
		"WorkingDirectory=",
		"CPUSchedulingPolicy=",
		"IOSchedulingClass=",
		"IOSchedulingPriority=",
	} {
		if strings.Contains(content, disallowed) {
			t.Fatalf("user service should not contain %s:\n%s", disallowed, content)
		}
	}
}

func TestWriteLaunchdUserServiceUsesUserPaths(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		ServiceName:     "cf-probe",
		BinaryFile:      filepath.Join(tmp, ".cf-probe", "bin", "cf-probe"),
		ConfigFile:      filepath.Join(tmp, ".cf-probe", "config.conf"),
		LogFile:         filepath.Join(tmp, ".cf-probe", "cf-probe.log"),
		LaunchdLabel:    "com.cfsm.cf-probe",
		LaunchdUserFile: filepath.Join(tmp, "Library", "LaunchAgents", "com.cfsm.cf-probe.plist"),
		LaunchdRootFile: filepath.Join(tmp, "Library", "LaunchDaemons", "com.cfsm.cf-probe.plist"),
		UserMode:        true,
		RunUID:          501,
	}

	if err := writeLaunchdService(paths, false); err != nil {
		t.Fatalf("writeLaunchdService returned error: %v", err)
	}
	if _, err := os.Stat(paths.LaunchdUserFile); err != nil {
		t.Fatalf("user launchd plist missing: %v", err)
	}
	if _, err := os.Stat(paths.LaunchdRootFile); !os.IsNotExist(err) {
		t.Fatalf("root launchd plist should not be written: %v", err)
	}
	data, err := os.ReadFile(paths.LaunchdUserFile)
	if err != nil {
		t.Fatalf("read launchd plist: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"<string>" + paths.BinaryFile + "</string>",
		"<string>run</string>",
		"<string>-config=" + paths.ConfigFile + "</string>",
		"<string>-debug=0</string>",
		"<string>" + paths.LogFile + "</string>",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("launchd plist missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "UserName") || strings.Contains(content, "root") {
		t.Fatalf("user launchd plist should not contain root user block:\n%s", content)
	}
}

func TestManagementLogFileByServiceSystem(t *testing.T) {
	paths := Paths{
		ServiceName: "cf-probe",
		LogFile:     "/var/log/cf-probe.log",
	}
	tests := []struct {
		system string
		want   string
	}{
		{"systemd", ""},
		{"systemd-user", ""},
		{"procd", ""},
		{"openrc", "/var/log/cf-probe.log"},
		{"launchd", "/var/log/cf-probe.log"},
		{"synology-rc", "/var/log/cf-probe.log"},
		{"background", "/var/log/cf-probe.log"},
		{"windows", "/var/log/cf-probe.log"},
		{"upstart", filepath.Join("/var/log/upstart", "cf-probe.log")},
	}

	for _, tt := range tests {
		t.Run(tt.system, func(t *testing.T) {
			if got := managementLogFile(paths, tt.system); got != tt.want {
				t.Fatalf("managementLogFile() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMigrateTrafficMovesLegacyTraffic(t *testing.T) {
	tmp := t.TempDir()
	oldDir := filepath.Join(tmp, "old")
	newDir := filepath.Join(tmp, "new")
	oldTraffic := filepath.Join(oldDir, "traffic.dat")
	newTraffic := filepath.Join(newDir, "traffic.dat")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldTraffic, []byte("RX_PREV=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := migrateTraffic(Paths{
		ConfigDir:      newDir,
		TrafficFile:    newTraffic,
		OldTrafficFile: oldTraffic,
	})
	if err != nil {
		t.Fatalf("migrateTraffic returned error: %v", err)
	}
	got, err := os.ReadFile(newTraffic)
	if err != nil {
		t.Fatalf("new traffic missing: %v", err)
	}
	if string(got) != "RX_PREV=1\n" {
		t.Fatalf("traffic = %q", got)
	}
	if _, err := os.Stat(oldTraffic); !os.IsNotExist(err) {
		t.Fatalf("old traffic still exists or stat failed: %v", err)
	}
}

func TestMigrateTrafficKeepsExistingTraffic(t *testing.T) {
	tmp := t.TempDir()
	oldDir := filepath.Join(tmp, "old")
	newDir := filepath.Join(tmp, "new")
	oldTraffic := filepath.Join(oldDir, "traffic.dat")
	newTraffic := filepath.Join(newDir, "traffic.dat")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldTraffic, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newTraffic, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := migrateTraffic(Paths{ConfigDir: newDir, TrafficFile: newTraffic, OldTrafficFile: oldTraffic}); err != nil {
		t.Fatalf("migrateTraffic returned error: %v", err)
	}
	got, err := os.ReadFile(newTraffic)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Fatalf("traffic = %q", got)
	}
	if _, err := os.Stat(oldTraffic); err != nil {
		t.Fatalf("old traffic should be preserved when current exists: %v", err)
	}
}
