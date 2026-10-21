package cfprobe

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newDiscoveryTestAgent(t *testing.T, cfg Config) *Agent {
	t.Helper()
	dir := t.TempDir()
	return &Agent{
		cfg:   cfg,
		log:   newLogger(false),
		wake:  make(chan struct{}, 1),
		paths: Paths{ConfigDir: dir, ConfigFile: filepath.Join(dir, "config.conf")},
	}
}

func discoveryHeaders(status, serverID string) http.Header {
	headers := http.Header{}
	headers.Set(agentAutoDiscoveryHeader, status)
	if serverID != "" {
		headers.Set(agentAutoDiscoveryServerIDKey, serverID)
	}
	return headers
}

// 未绑定 id 时不能走 WSS：否则首报之前就会卡在 WSS 上，永远没有 POST 去触发自动建号。
func TestUnboundAgentReportsOverPOST(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
		ReportInterval: 60,
	})
	agent.reporter = &reportTransport{agent: agent}

	if agent.usesWSS() {
		t.Fatal("usesWSS() = true before server id is bound")
	}
	if got := agent.currentWSSReportInterval(); got != time.Minute {
		t.Fatalf("currentWSSReportInterval() = %s, want the POST interval 1m", got)
	}
}

func TestApplyAutoDiscoveryCreatedBindsAndPersistsServerID(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		Name:           "hk-1",
		ConnectionMode: connectionModeAuto,
		ReportInterval: 60,
	})

	agent.applyAutoDiscoveryHeaders(discoveryHeaders(agentAutoDiscoveryCreated, "srv-created"))

	if got := agent.configSnapshot().ServerID; got != "srv-created" {
		t.Fatalf("ServerID = %q, want srv-created", got)
	}
	if !agent.usesWSS() {
		t.Fatal("usesWSS() = false after server id is bound")
	}
	if agent.discoveryAbandoned() {
		t.Fatal("discoveryAbandoned() = true after successful creation")
	}

	saved, err := readConfig(agent.paths.ConfigFile)
	if err != nil {
		t.Fatalf("readConfig() error: %v", err)
	}
	if saved.ServerID != "srv-created" {
		t.Fatalf("persisted ServerID = %q, want srv-created", saved.ServerID)
	}
	if saved.Name != "hk-1" || saved.Secret != "secret" || saved.WorkerURL != "https://example.com" {
		t.Fatalf("persisted config lost fields: %+v", saved)
	}
}

// 同名服务器不复用：把本机数据写进别人的服务器会串号，必须提示后退出。
func TestApplyAutoDiscoveryReusedAborts(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
	})

	agent.applyAutoDiscoveryHeaders(discoveryHeaders(agentAutoDiscoveryReused, "srv-existing"))

	if got := agent.configSnapshot().ServerID; got != "" {
		t.Fatalf("ServerID = %q, want empty: reused id must not be bound", got)
	}
	if !agent.discoveryAbandoned() {
		t.Fatal("discoveryAbandoned() = false after reused")
	}
	if agent.usesWSS() {
		t.Fatal("usesWSS() = true after reused")
	}
}

func TestApplyAutoDiscoveryFailedAborts(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
	})

	agent.applyAutoDiscoveryHeaders(discoveryHeaders(agentAutoDiscoveryFailed+":db_error", ""))

	if got := agent.configSnapshot().ServerID; got != "" {
		t.Fatalf("ServerID = %q, want empty after failed discovery", got)
	}
	if !agent.discoveryAbandoned() {
		t.Fatal("discoveryAbandoned() = false after failed discovery")
	}
	if agent.usesWSS() {
		t.Fatal("usesWSS() = true after failed discovery")
	}
}

// 失败只在 404 响应上出现，必须早于状态码判断被处理，否则永远读不到。
func TestAutoDiscoveryFailureIsHandledOn404Response(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
		ReportInterval: 60,
	})

	agent.handleTimedReportResponse(
		http.StatusNotFound,
		[]byte("Not Found"),
		discoveryHeaders(agentAutoDiscoveryFailed+":no_name", ""),
		time.Now(),
		time.Now(),
	)

	if !agent.discoveryAbandoned() {
		t.Fatal("discoveryAbandoned() = false after 404 with failed header")
	}
}

// 服务端建号后仍可能因载荷校验失败返回 4xx，此时也要绑定 id，否则下次上报会再建一台。
func TestAutoDiscoveryCreatedIsHandledOnNon2xxResponse(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
		ReportInterval: 60,
	})

	agent.handleTimedReportResponse(
		http.StatusBadRequest,
		[]byte("Missing metrics"),
		discoveryHeaders(agentAutoDiscoveryCreated, "srv-created"),
		time.Now(),
		time.Now(),
	)

	if got := agent.configSnapshot().ServerID; got != "srv-created" {
		t.Fatalf("ServerID = %q, want srv-created on 400 with created header", got)
	}
	if agent.discoveryAbandoned() {
		t.Fatal("discoveryAbandoned() = true after successful binding on 4xx")
	}
}

func TestApplyAutoDiscoveryIgnoresMissingHeader(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
	})

	agent.applyAutoDiscoveryHeaders(http.Header{})

	if got := agent.configSnapshot().ServerID; got != "" {
		t.Fatalf("ServerID = %q, want empty without discovery header", got)
	}
	if agent.discoveryAbandoned() {
		t.Fatal("discoveryAbandoned() = true without discovery header")
	}
}

// 落盘失败也必须保留内存中的 id，否则下一次上报仍是空 id，会重复建号。
func TestApplyAutoDiscoveryKeepsServerIDWhenPersistFails(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
	})
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	agent.paths.ConfigFile = filepath.Join(blocker, "config.conf")

	agent.applyAutoDiscoveryHeaders(discoveryHeaders(agentAutoDiscoveryCreated, "srv-memory"))

	if got := agent.configSnapshot().ServerID; got != "srv-memory" {
		t.Fatalf("ServerID = %q, want srv-memory even when persist fails", got)
	}
}

// 中止后不再上报：空 id 的上报必然 404，继续发只是白白消耗 Worker 与 D1。
func TestAbortedDiscoveryStopsReporting(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
		ReportInterval: 60,
	})
	agent.applyAutoDiscoveryHeaders(discoveryHeaders(agentAutoDiscoveryFailed+":db_error", ""))

	agent.tick()

	if !agent.lastPost.IsZero() || !agent.lastReport.IsZero() {
		t.Fatalf("lastPost = %s lastReport = %s, want no report after aborted discovery", agent.lastPost, agent.lastReport)
	}
	if len(agent.samples) != 0 {
		t.Fatalf("samples = %d, want 0 after aborted discovery", len(agent.samples))
	}
}

// 中止流程不能留下任何文件：配置目录必须保持干净（config.conf 本身除外）。
func TestAbortedDiscoveryLeavesNoFiles(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
	})
	agent.applyAutoDiscoveryHeaders(discoveryHeaders(agentAutoDiscoveryFailed+":db_error", ""))

	entries, err := os.ReadDir(agent.paths.ConfigDir)
	if err != nil {
		t.Fatalf("ReadDir() error: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("config dir is not clean after aborted discovery: %v", names)
	}
}
