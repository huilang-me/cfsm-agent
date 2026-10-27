package cfprobe

import (
	"os"
	"testing"
)

func TestNormalizeInterfaceList(t *testing.T) {
	got, err := normalizeInterfaceList(" eth0,ens3,eth0, pppoe-wan ")
	if err != nil {
		t.Fatalf("normalizeInterfaceList returned error: %v", err)
	}
	if got != "eth0,ens3,pppoe-wan" {
		t.Fatalf("unexpected normalized interfaces: %q", got)
	}
}

func TestNormalizeInterfaceListRejectsBadName(t *testing.T) {
	if _, err := normalizeInterfaceList("eth0, bad/name"); err == nil {
		t.Fatal("expected invalid interface name to be rejected")
	}
}

func TestNormalizeInterfaceListAllowsGlob(t *testing.T) {
	got, err := normalizeInterfaceList(" eth*,en[ops]* ")
	if err != nil {
		t.Fatalf("normalizeInterfaceList returned error: %v", err)
	}
	if got != "eth*,en[ops]*" {
		t.Fatalf("unexpected normalized interfaces: %q", got)
	}
}

func TestConfigPersistsUpdateProxy(t *testing.T) {
	path := t.TempDir() + "/config.conf"
	cfg := defaultConfig()
	cfg.ServerID = "sid"
	cfg.Secret = "secret"
	cfg.WorkerURL = "https://worker.example.com/report"
	cfg.AutoUpdate = true
	cfg.UpdateProxy = "https://gh-proxy.example.com"
	cfg.ConnectionMode = connectionModeHTTP
	cfg.PingMode = pingModeICMP

	if err := writeConfig(path, cfg); err != nil {
		t.Fatalf("writeConfig returned error: %v", err)
	}
	got, err := readConfig(path)
	if err != nil {
		t.Fatalf("readConfig returned error: %v", err)
	}
	if got.UpdateProxy != cfg.UpdateProxy {
		t.Fatalf("UpdateProxy = %q, want %q", got.UpdateProxy, cfg.UpdateProxy)
	}
	if !got.AutoUpdate {
		t.Fatal("AutoUpdate = false, want true")
	}
	if got.ConnectionMode != connectionModeHTTP {
		t.Fatalf("ConnectionMode = %q, want %q", got.ConnectionMode, connectionModeHTTP)
	}
	if got.PingMode != pingModeICMP {
		t.Fatalf("PingMode = %q, want %q", got.PingMode, pingModeICMP)
	}
}

func TestReadConfigDefaultsConnectionModeAuto(t *testing.T) {
	path := t.TempDir() + "/config.conf"
	data := []byte("SERVER_ID=\"sid\"\nSECRET=\"secret\"\nWORKER_URL=\"https://worker.example.com/update\"\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	got, err := readConfig(path)
	if err != nil {
		t.Fatalf("readConfig returned error: %v", err)
	}
	if got.ConnectionMode != connectionModeAuto {
		t.Fatalf("ConnectionMode = %q, want %q", got.ConnectionMode, connectionModeAuto)
	}
	if got.PingMode != pingModeTCP {
		t.Fatalf("PingMode = %q, want %q", got.PingMode, pingModeTCP)
	}
	if got.UA != defaultUserAgent {
		t.Fatalf("UA = %q, want default %q", got.UA, defaultUserAgent)
	}
}

func TestNormalizeUserAgentValue(t *testing.T) {
	accepted := []string{
		"cfsm",
		"MyAgent/1.0",
		"cfsm (edge)",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
	}
	for _, value := range accepted {
		if got := normalizeUserAgentValue(value); got != value {
			t.Fatalf("normalizeUserAgentValue(%q) = %q", value, got)
		}
	}

	rejected := []string{
		"",
		"ua\"quote",
		"ua\\backslash",
		"ua'single",
		"ua$var",
		"ua&other",
		"ua|pipe",
		"ua%20",
		"中文UA",
		"ua\r\nInjected: 1",
	}
	for _, value := range rejected {
		if got := normalizeUserAgentValue(value); got != defaultUserAgent {
			t.Fatalf("normalizeUserAgentValue(%q) = %q, want %q", value, got, defaultUserAgent)
		}
	}

	longest := ""
	for len(longest) < maxUserAgentLength {
		longest += "a"
	}
	if got := normalizeUserAgentValue(longest); got != longest {
		t.Fatalf("max length UA should be accepted")
	}
	if got := normalizeUserAgentValue(longest + "a"); got != defaultUserAgent {
		t.Fatalf("over-long UA should fall back to default")
	}
}

func TestConfigPersistsUserAgent(t *testing.T) {
	path := t.TempDir() + "/config.conf"
	cfg := defaultConfig()
	cfg.ServerID = "sid"
	cfg.Secret = "secret"
	cfg.WorkerURL = "https://worker.example.com/update"
	cfg.UA = "MyAgent/1.0"

	if err := writeConfig(path, cfg); err != nil {
		t.Fatalf("writeConfig returned error: %v", err)
	}
	got, err := readConfig(path)
	if err != nil {
		t.Fatalf("readConfig returned error: %v", err)
	}
	if got.UA != cfg.UA {
		t.Fatalf("UA = %q, want %q", got.UA, cfg.UA)
	}
}

func TestApplyRemoteUserAgentUpdatesConfigOnce(t *testing.T) {
	tmp := t.TempDir()
	configFile := tmp + "/config.conf"
	agent := Agent{
		cfg: Config{
			ServerID:       "sid",
			Secret:         "secret",
			WorkerURL:      "https://worker.example.com/update",
			ReportInterval: 60,
			ResetDay:       1,
			ConnectionMode: connectionModeAuto,
			PingMode:       pingModeTCP,
			UA:             defaultUserAgent,
			ConfigMD5:      "none",
		},
		paths: Paths{ConfigFile: configFile, TrafficFile: tmp + "/traffic.dat"},
		log:   newLogger(false),
	}
	if err := writeConfig(configFile, agent.cfg); err != nil {
		t.Fatalf("writeConfig returned error: %v", err)
	}

	agent.applyRemoteUserAgent("MyAgent/1.0")
	if got := agent.configSnapshot().UA; got != "MyAgent/1.0" {
		t.Fatalf("in-memory UA = %q", got)
	}
	persisted, err := readConfig(configFile)
	if err != nil {
		t.Fatalf("readConfig returned error: %v", err)
	}
	if persisted.UA != "MyAgent/1.0" {
		t.Fatalf("persisted UA = %q", persisted.UA)
	}

	// 相同值不应重复写盘（避免每个上报周期写 flash）。
	before, err := os.Stat(configFile)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	agent.applyRemoteUserAgent("MyAgent/1.0")
	after, err := os.Stat(configFile)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("config file rewritten for identical UA")
	}

	// 非法值直接忽略，不能把已有 UA 打回默认值。
	agent.applyRemoteUserAgent("bad\"ua")
	if got := agent.configSnapshot().UA; got != "MyAgent/1.0" {
		t.Fatalf("UA after invalid input = %q", got)
	}
}
