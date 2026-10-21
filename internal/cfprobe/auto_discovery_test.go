package cfprobe

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
		paths: Paths{ConfigDir: dir, ConfigFile: filepath.Join(dir, "config.conf"), TrafficFile: filepath.Join(dir, "traffic.dat")},
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

// 建号成功等同于一次普通安装：建号那次上报只用来取 id，不占用上报节奏，
// 下一次 tick 必须立刻带着新 id 再报一次，服务端才能按正常流程比对 md5 并入库。
func TestAutoDiscoveryBindResetsReportCadence(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
		ReportInterval: 60,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent.ctx = ctx
	agent.reporter = &reportTransport{agent: agent, wsURL: "ws://127.0.0.1:9/update"}

	agent.applyAutoDiscoveryHeaders(discoveryHeaders(agentAutoDiscoveryCreated, "srv-bound"))
	if agent.reporter.running {
		t.Fatal("WSS started before the follow-up report: it must not steal the first POST with the new id")
	}
	agent.applyReportResult(reportSendResult{ok: true, viaPOST: true}, time.Now())

	if !agent.lastPost.IsZero() || !agent.lastReport.IsZero() {
		t.Fatalf("lastPost = %s lastReport = %s, want zero so the next tick reports immediately", agent.lastPost, agent.lastReport)
	}
	if !agent.postAttemptAllowed(time.Now(), 60*time.Second) {
		t.Fatal("postAttemptAllowed() = false: the follow-up report would be throttled to the next interval")
	}
	if agent.reporter.running {
		t.Fatal("WSS started before the follow-up report is done")
	}

	// 带新 id 的补报完成：这时才按 connection_mode 启动传输层。
	agent.applyReportResult(reportSendResult{ok: true, viaPOST: true}, time.Now())
	if agent.lastPost.IsZero() {
		t.Fatal("lastPost stays zero after a normal report")
	}
	if !agent.reporter.running {
		t.Fatal("WSS not started after the follow-up report")
	}
}

// 首次写入失败时 id 只在内存里，后续上报必须重试落盘，否则重启会重新走建号流程。
func TestAutoDiscoveryRetriesPersistingServerID(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeHTTP,
		ReportInterval: 60,
	})
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	agent.paths.ConfigFile = filepath.Join(blocker, "config.conf")

	agent.applyAutoDiscoveryHeaders(discoveryHeaders(agentAutoDiscoveryCreated, "srv-retry"))
	if agent.pendingPersistID != "srv-retry" {
		t.Fatalf("pendingPersistID = %q, want srv-retry", agent.pendingPersistID)
	}

	// 配置目录恢复可写后，下一次上报结果处理就应该补写成功。
	agent.paths.ConfigFile = filepath.Join(t.TempDir(), "config.conf")
	agent.applyReportResult(reportSendResult{ok: true, viaPOST: true}, time.Now())

	saved, err := readConfig(agent.paths.ConfigFile)
	if err != nil {
		t.Fatalf("readConfig() error: %v", err)
	}
	if saved.ServerID != "srv-retry" {
		t.Fatalf("persisted ServerID = %q, want srv-retry", saved.ServerID)
	}
	if agent.pendingPersistID != "" {
		t.Fatalf("pendingPersistID = %q, want empty after a successful retry", agent.pendingPersistID)
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

// 服务端未开启自动发现时，空 id 的上报只会拿到一个不带任何标记的 404。
// 这种情况必须提示「请开启自动发现」后退出，不能静默地每 60 秒空转一次。
func TestUnboundAgentAbortsOnPlain404(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
		ReportInterval: 60,
	})

	agent.handleTimedReportResponse(
		http.StatusNotFound,
		[]byte("Not Found"),
		http.Header{},
		time.Now(),
		time.Now(),
	)

	if !agent.discoveryAbandoned() {
		t.Fatal("discoveryAbandoned() = false: a plain 404 must stop an unbound agent")
	}
}

// 已绑定 id 的服务器被删除后仍要保持原来的 404 语义，不能被当成自动发现失败而退出。
func TestBoundAgentKeepsRunningOnPlain404(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		ServerID:       "srv-test",
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		ConnectionMode: connectionModeAuto,
		ReportInterval: 60,
	})

	agent.handleTimedReportResponse(
		http.StatusNotFound,
		[]byte("Not Found"),
		http.Header{},
		time.Now(),
		time.Now(),
	)

	if agent.discoveryAbandoned() {
		t.Fatal("discoveryAbandoned() = true for a bound server")
	}
}

// 失败码到建议的映射：能收到 failed 响应头说明自动发现已经开启，
// 所以任何一条建议都不能再让用户去「开启自动发现」。
func TestAutoDiscoveryFailureHint(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{"no_name", "-name"},
		{"db_error", "数据库"},
		{"something_new", "Worker 日志"},
	}

	for _, tc := range cases {
		hint := autoDiscoveryFailureHint(tc.code)
		if strings.Contains(hint, "开启自动发现") {
			t.Fatalf("autoDiscoveryFailureHint(%q) = %q, must not suggest enabling auto discovery", tc.code, hint)
		}
		if !strings.Contains(hint, tc.want) {
			t.Fatalf("autoDiscoveryFailureHint(%q) = %q, want it to contain %q", tc.code, hint, tc.want)
		}
	}
}

// name 只在未绑定 id 时随上报发送：绑定后服务端不再需要它。
func TestReportBodyCarriesNameOnlyBeforeBinding(t *testing.T) {
	agent := newDiscoveryTestAgent(t, Config{
		Secret:         "secret",
		WorkerURL:      "https://example.com",
		Name:           "hk-1",
		ConnectionMode: connectionModeAuto,
		ReportInterval: 60,
	})

	payload := func(cfg Config) map[string]any {
		t.Helper()
		body, _, err := agent.buildReportBodyForConfig(cfg, Metrics{}, time.Now())
		if err != nil {
			t.Fatalf("buildReportBodyForConfig() error: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("Unmarshal() error: %v", err)
		}
		return decoded
	}

	if got := payload(agent.configSnapshot())["name"]; got != "hk-1" {
		t.Fatalf("name = %v before binding, want hk-1", got)
	}

	bound := agent.configSnapshot()
	bound.ServerID = "srv-bound"
	if _, ok := payload(bound)["name"]; ok {
		t.Fatal("name must not be sent after the server id is bound")
	}
}
