package cfprobe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeInstanceName(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "", want: ""},
		{raw: "a", want: "a"},
		{raw: "eth1", want: "eth1"},
		{raw: "Eth_1", want: "eth_1"},
		{raw: "HK-1", want: "hk-1"},
		{raw: "my-instance", want: "my-instance"},
		{raw: strings.Repeat("a", 32), want: strings.Repeat("a", 32)},
	}
	for _, tt := range tests {
		got, err := normalizeInstanceName(tt.raw)
		if err != nil {
			t.Fatalf("normalizeInstanceName(%q) error = %v", tt.raw, err)
		}
		if got != tt.want {
			t.Fatalf("normalizeInstanceName(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}

	invalid := []string{"-x", "_x", "a.b", "a/b", "a b", "中文", strings.Repeat("a", 33)}
	for _, raw := range invalid {
		if _, err := normalizeInstanceName(raw); err == nil {
			t.Fatalf("normalizeInstanceName(%q) expected error", raw)
		}
	}
}

func TestProbeInstanceNameFromCmdline(t *testing.T) {
	tests := []struct {
		name    string
		cmdline []string
		want    string
	}{
		{name: "default", cmdline: []string{"/usr/local/bin/cf-probe", "run", "-debug=0"}, want: ""},
		{name: "equals form", cmdline: []string{"cf-probe", "run", "-instance=eth1", "-debug=0"}, want: "eth1"},
		{name: "space form", cmdline: []string{"cf-probe", "run", "-instance", "eth1"}, want: "eth1"},
		{name: "double dash", cmdline: []string{"cf-probe", "run", "--instance=eth1"}, want: "eth1"},
		{name: "empty value", cmdline: []string{"cf-probe", "run", "-instance="}, want: ""},
		{name: "invalid value", cmdline: []string{"cf-probe", "run", "-instance=bad/name"}, want: ""},
		{name: "no prefix match", cmdline: []string{"cf-probe", "run", "-instance=eth10"}, want: "eth10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := probeInstanceNameFromCmdline(tt.cmdline); got != tt.want {
				t.Fatalf("probeInstanceNameFromCmdline(%v) = %q, want %q", tt.cmdline, got, tt.want)
			}
		})
	}
}

func TestInstanceFromExecutablePath(t *testing.T) {
	tests := []struct {
		exe  string
		want string
	}{
		{exe: filepath.Join("/home/alice", ".cf-probe-eth1", "bin", "cf-probe"), want: "eth1"},
		{exe: filepath.Join("/usr/local/bin", "cf-probe-eth1"), want: "eth1"},
		{exe: filepath.Join("/usr/bin", "cf-probe-eth1"), want: "eth1"},
		{exe: filepath.Join("/usr/local/bin", "cf-probe"), want: ""},
		{exe: filepath.Join("/home/alice", ".cf-probe", "bin", "cf-probe"), want: ""},
		{exe: filepath.Join("/tmp", "cf-probe-linux-amd64"), want: ""},
	}
	for _, tt := range tests {
		if got := instanceFromExecutablePath(tt.exe); got != tt.want {
			t.Fatalf("instanceFromExecutablePath(%q) = %q, want %q", tt.exe, got, tt.want)
		}
	}
}

func TestInstalledInstanceNames(t *testing.T) {
	root := t.TempDir()
	base := Paths{ConfigDir: filepath.Join(root, ".cf-probe")}
	for _, name := range []string{".cf-probe", ".cf-probe-eth1", ".cf-probe-eth2", ".cf-probe-bad.name", ".cf-probe-backup", ".cf-probe-renamed", "other"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 只有带匹配实例标记的目录才算真正安装过的实例。
	for dir, name := range map[string]string{
		".cf-probe-eth1":    "eth1",
		".cf-probe-eth2":    "eth2",
		".cf-probe-renamed": "eth1",
	} {
		if err := os.WriteFile(filepath.Join(root, dir, instanceMarkerFile), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := installedInstanceNames(base)
	want := []string{"eth1", "eth2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("installedInstanceNames() = %v, want %v", got, want)
	}
}

func TestServiceWritersIncludeInstance(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		Instance:    "eth1",
		ServiceName: "cf-probe-eth1",
		BinaryFile:  filepath.Join(tmp, "cf-probe-eth1"),
		ConfigFile:  filepath.Join(tmp, "config.conf"),
		LogFile:     filepath.Join(tmp, "cf-probe-eth1.log"),
		ServiceFile: filepath.Join(tmp, "cf-probe-eth1.service"),
	}

	if err := writeSystemdService(paths, false); err != nil {
		t.Fatalf("writeSystemdService: %v", err)
	}
	data, err := os.ReadFile(paths.ServiceFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "run -instance=eth1 -debug=0") {
		t.Fatalf("systemd service missing instance arg:\n%s", data)
	}

	paths.UserMode = true
	paths.LaunchdLabel = "com.cfsm.cf-probe-eth1"
	paths.LaunchdUserFile = filepath.Join(tmp, "com.cfsm.cf-probe-eth1.plist")
	if err := writeSystemdUserService(paths, false); err != nil {
		t.Fatalf("writeSystemdUserService: %v", err)
	}
	data, err = os.ReadFile(paths.ServiceFile)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "-instance=eth1") || !strings.Contains(content, "-config=") {
		t.Fatalf("systemd user service missing instance/config arg:\n%s", content)
	}

	if err := writeLaunchdService(paths, false); err != nil {
		t.Fatalf("writeLaunchdService: %v", err)
	}
	data, err = os.ReadFile(paths.LaunchdUserFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<string>-instance=eth1</string>") {
		t.Fatalf("launchd plist missing instance arg:\n%s", data)
	}
}

func TestServiceWritersDefaultInstanceUnchanged(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		ServiceName: "cf-probe",
		BinaryFile:  filepath.Join(tmp, "cf-probe"),
		ConfigFile:  filepath.Join(tmp, "config.conf"),
		LogFile:     filepath.Join(tmp, "cf-probe.log"),
		ServiceFile: filepath.Join(tmp, "cf-probe.service"),
	}
	if err := writeSystemdService(paths, false); err != nil {
		t.Fatalf("writeSystemdService: %v", err)
	}
	data, err := os.ReadFile(paths.ServiceFile)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "run -debug=0") {
		t.Fatalf("systemd service missing legacy exec line:\n%s", content)
	}
	if strings.Contains(content, "-instance") {
		t.Fatalf("default service should not contain -instance:\n%s", content)
	}
}
