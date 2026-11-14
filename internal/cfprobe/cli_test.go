package cfprobe

import "testing"

func TestParseUninstallArgsAllowsNoArgs(t *testing.T) {
	opts, err := parseUninstallArgs(nil)
	if err != nil {
		t.Fatalf("parseUninstallArgs(nil) error = %v", err)
	}
	if opts.Instance != "" || opts.All {
		t.Fatalf("parseUninstallArgs(nil) = %+v, want default instance", opts)
	}
}

func TestParseInstallOptionsRejectsCustomPathAndService(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "install dir underscore", args: []string{"-install_dir=/tmp/cf-probe"}},
		{name: "install dir hyphen", args: []string{"-install-dir=/tmp/cf-probe"}},
		{name: "service name underscore", args: []string{"-service_name=probe-a"}},
		{name: "service name hyphen", args: []string{"-service-name=probe-a"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseInstallOptions(tt.args); err == nil {
				t.Fatalf("parseInstallOptions(%v) expected error", tt.args)
			}
		})
	}
}

func TestParseUninstallArgsRejectsExtraArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "service name underscore", args: []string{"-service_name=probe-a"}},
		{name: "service name hyphen", args: []string{"-service-name=probe-a"}},
		{name: "positional arg", args: []string{"probe-a"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseUninstallArgs(tt.args); err == nil {
				t.Fatalf("parseUninstallArgs(%v) expected error", tt.args)
			}
		})
	}
}

func TestParseUninstallArgsInstanceAndAll(t *testing.T) {
	opts, err := parseUninstallArgs([]string{"-instance=eth1"})
	if err != nil {
		t.Fatalf("parseUninstallArgs(-instance=eth1) error = %v", err)
	}
	if opts.Instance != "eth1" || opts.All {
		t.Fatalf("opts = %+v, want instance eth1", opts)
	}

	opts, err = parseUninstallArgs([]string{"--all"})
	if err != nil {
		t.Fatalf("parseUninstallArgs(--all) error = %v", err)
	}
	if !opts.All || opts.Instance != "" {
		t.Fatalf("opts = %+v, want all", opts)
	}

	if _, err := parseUninstallArgs([]string{"-instance=eth1", "--all"}); err == nil {
		t.Fatal("parseUninstallArgs(-instance + --all) expected error")
	}
	if _, err := parseUninstallArgs([]string{"-instance=bad name"}); err == nil {
		t.Fatal("parseUninstallArgs(invalid instance) expected error")
	}
}

func TestParseUninstallArgsIgnoresBootstrapFlags(t *testing.T) {
	opts, err := parseUninstallArgs([]string{
		"-instance=eth1",
		"--install-version=Snapshot-2611140423",
		"--install-ghproxy=https://gh-proxy.example.com",
	})
	if err != nil {
		t.Fatalf("parseUninstallArgs() error = %v", err)
	}
	if opts.Instance != "eth1" || opts.All {
		t.Fatalf("opts = %+v, want instance eth1", opts)
	}
}

func TestParseInstallOptionsInstance(t *testing.T) {
	got, err := parseInstallOptions([]string{"-instance=eth1"})
	if err != nil {
		t.Fatalf("parseInstallOptions(-instance=eth1) error = %v", err)
	}
	if got.Instance != "eth1" {
		t.Fatalf("Instance = %q, want eth1", got.Instance)
	}
	if !got.Explicit["instance"] {
		t.Fatalf("Explicit[instance] = false, want true (all: %v)", got.Explicit)
	}

	if _, err := parseInstallOptions([]string{"-instance=bad/name"}); err == nil {
		t.Fatal("parseInstallOptions(invalid instance) expected error")
	}
}

func TestParseRunOptionsInstance(t *testing.T) {
	instance, debug, configFile, err := parseRunOptions([]string{"-instance=eth1", "-config=/tmp/config.conf", "-debug=1"})
	if err != nil {
		t.Fatalf("parseRunOptions() error = %v", err)
	}
	if instance != "eth1" || !debug || configFile != "/tmp/config.conf" {
		t.Fatalf("got instance=%q debug=%v config=%q", instance, debug, configFile)
	}
}

func TestParseUpgradeApplyOptionsInstance(t *testing.T) {
	instance, err := parseUpgradeApplyOptions([]string{"-instance=eth1"})
	if err != nil {
		t.Fatalf("parseUpgradeApplyOptions() error = %v", err)
	}
	if instance != "eth1" {
		t.Fatalf("instance = %q, want eth1", instance)
	}
}

func TestParseInstallOptionsAutoUpdateDefaultOff(t *testing.T) {
	got, err := parseInstallOptions(nil)
	if err != nil {
		t.Fatalf("parseInstallOptions(nil) error = %v", err)
	}
	if got.AutoUpdate {
		t.Fatal("AutoUpdate = true, want false")
	}
}

func TestParseInstallOptionsAutoUpdateEnabled(t *testing.T) {
	got, err := parseInstallOptions([]string{"-auto_update=1"})
	if err != nil {
		t.Fatalf("parseInstallOptions(-auto_update=1) error = %v", err)
	}
	if !got.AutoUpdate {
		t.Fatal("AutoUpdate = false, want true")
	}
}

func TestParseInstallOptionsInstallProxy(t *testing.T) {
	got, err := parseInstallOptions([]string{"--install-ghproxy=https://gh-proxy.example.com"})
	if err != nil {
		t.Fatalf("parseInstallOptions(--install-ghproxy) error = %v", err)
	}
	if got.UpdateProxy != "https://gh-proxy.example.com" {
		t.Fatalf("UpdateProxy = %q", got.UpdateProxy)
	}
}

func TestParseInstallOptionsConnectionMode(t *testing.T) {
	got, err := parseInstallOptions([]string{"-connection_mode=http"})
	if err != nil {
		t.Fatalf("parseInstallOptions(-connection_mode=http) error = %v", err)
	}
	if got.ConnectionMode != connectionModeHTTP {
		t.Fatalf("ConnectionMode = %q, want %q", got.ConnectionMode, connectionModeHTTP)
	}

	got, err = parseInstallOptions([]string{"--connection-mode=websocket"})
	if err != nil {
		t.Fatalf("parseInstallOptions(--connection-mode=websocket) error = %v", err)
	}
	if got.ConnectionMode != connectionModeAuto {
		t.Fatalf("ConnectionMode = %q, want %q", got.ConnectionMode, connectionModeAuto)
	}

	if _, err := parseInstallOptions([]string{"-connection_mode=bad"}); err == nil {
		t.Fatal("parseInstallOptions(-connection_mode=bad) expected error")
	}
}

func TestParseInstallOptionsPingMode(t *testing.T) {
	got, err := parseInstallOptions([]string{"-ping_mode=icmp"})
	if err != nil {
		t.Fatalf("parseInstallOptions(-ping_mode=icmp) error = %v", err)
	}
	if got.PingMode != pingModeICMP {
		t.Fatalf("PingMode = %q, want %q", got.PingMode, pingModeICMP)
	}

	got, err = parseInstallOptions([]string{"--ping-mode=tcp"})
	if err != nil {
		t.Fatalf("parseInstallOptions(--ping-mode=tcp) error = %v", err)
	}
	if got.PingMode != pingModeTCP {
		t.Fatalf("PingMode = %q, want %q", got.PingMode, pingModeTCP)
	}

	if _, err := parseInstallOptions([]string{"-ping_mode=bad"}); err == nil {
		t.Fatal("parseInstallOptions(-ping_mode=bad) expected error")
	}
}

func TestParseInstallOptionsTracksExplicitFlags(t *testing.T) {
	got, err := parseInstallOptions([]string{"-auto_update=1", "--auto-update=1", "-collect=5", "-connection_mode=http", "-ping_mode=icmp", "--install-ghproxy=https://gh-proxy.example.com"})
	if err != nil {
		t.Fatalf("parseInstallOptions() error = %v", err)
	}
	for _, name := range []string{"auto_update", "collect_interval", "connection_mode", "ping_mode", "install_ghproxy"} {
		if !got.Explicit[name] {
			t.Fatalf("Explicit[%q] = false, want true (all: %v)", name, got.Explicit)
		}
	}
	if got.Explicit["reset_day"] || got.Explicit["interface"] {
		t.Fatalf("unexpected explicit flags: %v", got.Explicit)
	}
}
