package cfprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Agent struct {
	cfg     Config
	cfgMu   sync.RWMutex
	paths   Paths
	log     logger
	version string
	ctx     context.Context

	// 自动建号失败后置位：本次运行不再重试建号，也不再上报（空 id 的上报必然 404）。
	discoveryFailed atomic.Bool
	// 建号成功的那一次上报只用来取 id：置位后由 applyReportResult 丢弃该次上报的节奏记录，
	// 让下一次 tick 立刻带着新 id 再上报一次，服务端才能按正常安装的流程比对 md5 并入库。
	discoveryJustBound atomic.Bool
	// 上面那次“带 id 的补报”完成后才启动 WSS，保证补报一定走 POST（md5 比对 + 入库）。
	discoverySyncPending atomic.Bool

	mu         sync.RWMutex
	probes     ProbeSnapshot
	basic      BasicStats
	basicAt    time.Time
	fullAt     time.Time
	prevNet    NetBytes
	prevTime   time.Time
	prevDisk   DiskIOCounters
	prevDiskAt time.Time
	diskIO     DiskIOStats
	monthlyRX  uint64
	monthlyTX  uint64
	clock      calibratedClock

	samples                  []metricSample
	lastSample               time.Time
	lastReport               time.Time
	lastPost                 time.Time
	lastPostAttempt          time.Time
	lastConfigStateReportAt  time.Time
	lastConfigStateReportMD5 string
	// 上一次被拒的配置原因：同一份坏配置服务端会持续重发，只提示一次。
	lastConfigRejectKey string
	// 自动发现拿到但没能写入配置文件的 id，后续上报会重试落盘。
	pendingPersistID string
	updateMu         sync.Mutex
	reporter         *reportTransport
	reportMu         sync.Mutex
	// reportFailures 是 WSS 与 POST 共用的连续失败计数：握手 401/403、WSS error 帧、
	// POST 4xx/5xx、WS close 1008 都计入，任意一次成功（POST 2xx、非实时提示 ack、
	// 或 WSS 握手通过）清零。
	// 代价是两类通道互相干扰，例如 WSS 持续被拒而 POST 兜底偶发成功时计数会来回震荡，
	// 熔断可能不触发；当前接受该误差，若后续需要区分通道再拆成独立计数器。
	reportFailures    int
	reportPausedUntil time.Time
	reportBackoff     time.Duration
	// lastReportFailureAt 用于失败计数衰减：超过 reportFailureDecay 没有新失败就重新计数。
	lastReportFailureAt      time.Time
	wake                     chan struct{}
	remoteConfig             chan remoteConfigRequest
	wssRuntimeMu             sync.Mutex
	wssRuntimeDisabledAt     time.Time
	wssRuntimeDisabledReason string
}

const (
	agentWSSModeDisabled          = "disabled"
	agentWSSScheduleDisabled      = "wss_disabled"
	metricsProbeInterval          = 20 * time.Second
	metricsProbeMedianWindow      = 2 * time.Minute
	metricsProbeWindowSampleCount = 6
	metricsProbeSampleCount       = 1
	configStateReportInterval     = time.Minute
	// 连续服务端拒绝后开始退避；只影响上报与采样。WSS 建连不再受暂停限制，
	// 配置下发与自动更新不受影响；HTTP 模式下的配置仍依赖 POST 2xx 响应体。
	reportFailureThreshold = 3
	reportBackoffMax       = 10 * time.Minute
	// 超过该时长没有新的失败，连续失败计数归零，避免历史故障让后续单次失败直接跳到最高档。
	reportFailureDecay       = 30 * time.Minute
	agentWSSModeHeader       = "X-Agent-Wss-Mode"
	agentWSSReasonHeader     = "X-Agent-Wss-Reason"
	agentWSSModeActive       = "active"
	agentWSSModeInactive     = "inactive"
	agentWSSScheduleInactive = "wss_schedule_inactive"
	agentWSSScheduleEmpty    = "wss_schedule_empty"

	// 服务端自动建号结果：created | reused | failed[:code]
	agentAutoDiscoveryHeader      = "X-CFSM-Auto-Discovery"
	agentAutoDiscoveryServerIDKey = "X-CFSM-Server-Id"
	agentAutoDiscoveryCreated     = "created"
	agentAutoDiscoveryReused      = "reused"
	agentAutoDiscoveryFailed      = "failed"
	// agentAutoDiscoveryDisabled 是 Agent 本地判定的中止原因：未绑定 id 时收到
	// 不带自动发现标记的 404，说明服务端没有开启自动发现（或版本过旧），服务端不会返回这个值。
	agentAutoDiscoveryDisabled = "disabled"
)

// ErrAutoDiscoveryFailed 表示未绑定 server id 且自动建号无法完成（失败或重名），进程应直接退出。
var ErrAutoDiscoveryFailed = errors.New("自动建号已中止，cf-probe 退出")

// 连续失败后的暂停时长：2m → 3m → 5m → 7m → 10m，之后保持 10m。
var reportBackoffSteps = [...]time.Duration{
	2 * time.Minute,
	3 * time.Minute,
	5 * time.Minute,
	7 * time.Minute,
	reportBackoffMax,
}

type timedProbeResult struct {
	at     time.Time
	result ProbeResult
}

type rollingProbeHistory struct {
	target  string
	samples []timedProbeResult
}

type metricSample struct {
	at      time.Time
	metrics map[string]any
}

type reportSendResult struct {
	ok      bool
	viaWSS  bool
	viaPOST bool
}

type remoteConfigRequest struct {
	body            []byte
	headers         http.Header
	allowMissingMD5 bool
	done            chan error
}

func (h *rollingProbeHistory) add(now time.Time, target string, result ProbeResult) {
	target = strings.TrimSpace(target)
	if target == "" {
		h.target = ""
		h.samples = nil
		return
	}
	if h.target != target {
		h.target = target
		h.samples = nil
	}
	h.samples = append(h.samples, timedProbeResult{at: now, result: result})
	if len(h.samples) > metricsProbeWindowSampleCount {
		h.samples = h.samples[len(h.samples)-metricsProbeWindowSampleCount:]
	}
}

func (h rollingProbeHistory) snapshot(now time.Time) ProbeResult {
	if len(h.samples) == 0 {
		return ProbeResult{}
	}

	cutoff := now.Add(-metricsProbeMedianWindow)
	values := make([]int, 0, len(h.samples))
	for _, sample := range h.samples {
		if sample.at.Before(cutoff) || !sample.result.OK || sample.result.RTTMs < 0 {
			continue
		}
		values = append(values, sample.result.RTTMs)
	}

	lossSamples := h.samples
	if len(lossSamples) > metricsProbeWindowSampleCount {
		lossSamples = lossSamples[len(lossSamples)-metricsProbeWindowSampleCount:]
	}
	lost := 0
	for _, sample := range lossSamples {
		if !sample.result.OK {
			lost++
		}
	}
	loss := lost * 100 / len(lossSamples)

	if len(values) == 0 {
		return ProbeResult{RTTMs: -1, Loss: loss, OK: false}
	}
	return ProbeResult{RTTMs: medianInt(values), Loss: loss, OK: true}
}

func Run(configFile string, debug bool, version string) error {
	paths := defaultPaths()
	if configFile != "" {
		paths.ConfigFile = configFile
		paths.ConfigDir = filepath.Dir(configFile)
		paths.TrafficFile = filepath.Join(paths.ConfigDir, "traffic.dat")
	}
	releaseLock, err := acquireInstanceLock(paths)
	if err != nil {
		return err
	}
	defer releaseLock()

	cfg, err := readConfig(paths.ConfigFile)
	if err != nil {
		return fmt.Errorf("读取配置失败 %s: %w", paths.ConfigFile, err)
	}
	if cfg.Secret == "" || cfg.WorkerURL == "" {
		return errors.New("配置缺失: SECRET/WORKER_URL 不能为空")
	}
	normalizeConfigIntervals(&cfg)

	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()

	a := &Agent{
		cfg:          cfg,
		paths:        paths,
		log:          newLogger(debug),
		version:      version,
		ctx:          ctx,
		prevNet:      readNetBytes(cfg.Interface),
		prevTime:     time.Now(),
		wake:         make(chan struct{}, 1),
		remoteConfig: make(chan remoteConfigRequest, 4),
	}
	a.reporter = newReportTransport(a)
	a.basic = collectBasicStats()
	a.basicAt = time.Now()

	a.log.info("CF-Server-Monitor Go Probe started version=%s platform=%s config=%s", version, platformName(), paths.ConfigFile)
	a.log.debugf("config id=%s url=%s report_interval=%ds collect_interval=%ds reset_day=%d connection_mode=%s ping_mode=%s interface=%s auto_update=%v",
		cfg.ServerID, cfg.WorkerURL, cfg.ReportInterval, cfg.CollectInterval, cfg.ResetDay, cfg.ConnectionMode, cfg.PingMode, firstNonEmpty(cfg.Interface, "auto"), cfg.AutoUpdate)

	go a.networkWorker(ctx)
	if a.usesWSS() {
		a.reporter.start(ctx)
	} else if cfg.ServerID != "" {
		a.log.info("WSS disabled connection_mode=http")
	}
	if cfg.AutoUpdate {
		go a.autoUpdateWorker(ctx)
	} else {
		a.log.debugf("auto update disabled: local AUTO_UPDATE=0")
	}
	return a.loop(ctx)
}

func (a *Agent) loop(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			a.log.info("probe stopped")
			return nil
		case <-timer.C:
			a.tick()
			if a.discoveryAbandoned() {
				return ErrAutoDiscoveryFailed
			}
			resetTimer(timer, a.tickInterval())
		case <-a.wake:
			a.tick()
			if a.discoveryAbandoned() {
				return ErrAutoDiscoveryFailed
			}
			resetTimer(timer, a.tickInterval())
		case req := <-a.remoteConfig:
			err := a.applyRemoteConfigWithOptions(req.body, req.headers, req.allowMissingMD5)
			req.done <- err
			resetTimer(timer, a.tickInterval())
		}
	}
}

func resetTimer(timer *time.Timer, d time.Duration) {
	if d <= 0 {
		d = time.Second
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (a *Agent) wakeTick() {
	if a == nil || a.wake == nil {
		return
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// discoveryAbandoned 表示未绑定 server id 且自动建号已中止（失败或重名），进程应退出。
func (a *Agent) discoveryAbandoned() bool {
	return a != nil && a.discoveryFailed.Load() && a.configSnapshot().ServerID == ""
}

func (a *Agent) tickInterval() time.Duration {
	cfg := a.configSnapshot()
	active := a.currentWSSReportIntervalForConfig(cfg)
	if collect := effectiveCollectInterval(cfg); collect > 0 {
		active = durationGCD(active, collect)
	}
	if active <= 0 {
		return time.Second
	}
	return active
}

func (a *Agent) effectiveCollectInterval() time.Duration {
	return effectiveCollectInterval(a.configSnapshot())
}

func effectiveCollectInterval(cfg Config) time.Duration {
	collect := time.Duration(cfg.CollectInterval) * time.Second
	if collect < 0 {
		collect = 0
	}
	return collect
}

func (a *Agent) currentWSSReportInterval() time.Duration {
	return a.currentWSSReportIntervalForConfig(a.configSnapshot())
}

func (a *Agent) currentWSSReportIntervalForConfig(cfg Config) time.Duration {
	if a.reporter != nil && a.usesWSSConfig(cfg) {
		fallback := time.Duration(defaultWSSReportIntervalSec) * time.Second
		return a.reporter.reportInterval(fallback)
	}
	reportInterval := time.Duration(cfg.ReportInterval) * time.Second
	if reportInterval < time.Second {
		return time.Duration(defaultReportIntervalSec) * time.Second
	}
	return reportInterval
}

func (a *Agent) persistentUsesWSS() bool {
	return persistentUsesWSSConfig(a.configSnapshot())
}

func persistentUsesWSSConfig(cfg Config) bool {
	mode, err := normalizeConnectionMode(cfg.ConnectionMode)
	if err != nil {
		mode = connectionModeAuto
	}
	return mode != connectionModeHTTP
}

func (a *Agent) wssRuntimeDisabled() (bool, string) {
	if a == nil {
		return false, ""
	}
	a.wssRuntimeMu.Lock()
	defer a.wssRuntimeMu.Unlock()
	return !a.wssRuntimeDisabledAt.IsZero(), a.wssRuntimeDisabledReason
}

func (a *Agent) usesWSS() bool {
	return a.usesWSSConfig(a.configSnapshot())
}

func (a *Agent) usesWSSConfig(cfg Config) bool {
	// 未绑定 server id 时只走 POST：WSS 通道要求有效 id，空 id 会被直接拒绝。
	if cfg.ServerID == "" {
		return false
	}
	if !persistentUsesWSSConfig(cfg) {
		return false
	}
	disabled, _ := a.wssRuntimeDisabled()
	return !disabled
}

func (a *Agent) configSnapshot() Config {
	if a == nil {
		return Config{}
	}
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

func (a *Agent) setConfig(cfg Config) {
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
}

// reportPauseRemaining 返回距离恢复上报的剩余时间，<=0 表示未暂停。
func (a *Agent) reportPauseRemaining(now time.Time) time.Duration {
	if a == nil {
		return 0
	}
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	return a.reportPausedUntil.Sub(now)
}

// noteReportSuccess 清零连续失败并解除熔断。恢复本身不打日志：进入暂停时已有 info，
// 且每次 ack 都打会刷屏；排障时看 "report paused" 之后再无该日志即表示已恢复。
func (a *Agent) noteReportSuccess() {
	if a == nil {
		return
	}
	a.reportMu.Lock()
	a.reportFailures = 0
	a.reportPausedUntil = time.Time{}
	a.reportBackoff = 0
	a.lastReportFailureAt = time.Time{}
	a.reportMu.Unlock()
}

func reportBackoffForFailures(failures int) time.Duration {
	if failures < reportFailureThreshold {
		return 0
	}
	index := failures - reportFailureThreshold
	if index >= len(reportBackoffSteps) {
		index = len(reportBackoffSteps) - 1
	}
	return reportBackoffSteps[index]
}

// noteReportFailure 记录一次服务端拒绝。code 是调用方的原始码：HTTP 状态码，
// 或 WSS close code（1008），因此日志里的 status 字段不总是 HTTP 语义。
// 时间基准用 time.Now() 而非 tick 的 now，误差在秒级以内，不影响退避档位。
func (a *Agent) noteReportFailure(statusCode int) {
	if a == nil {
		return
	}
	a.reportMu.Lock()
	now := time.Now()
	if !a.lastReportFailureAt.IsZero() && now.Sub(a.lastReportFailureAt) >= reportFailureDecay {
		a.reportFailures = 0
		a.reportBackoff = 0
	}
	a.lastReportFailureAt = now
	a.reportFailures++
	failures := a.reportFailures
	previousBackoff := a.reportBackoff
	var delay time.Duration
	if failures >= reportFailureThreshold {
		a.reportBackoff = reportBackoffForFailures(failures)
		a.reportPausedUntil = now.Add(a.reportBackoff)
		delay = a.reportBackoff
	}
	a.reportMu.Unlock()
	// 只在首次触发或退避档位变化时记录，避免达到上限后反复刷日志
	if delay > 0 && delay != previousBackoff {
		a.log.info("report paused failures=%d status=%d delay=%s", failures, statusCode, delay)
	}
}

func (a *Agent) disableWSSRuntime(reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = agentWSSScheduleInactive
	}
	now := time.Now()
	a.wssRuntimeMu.Lock()
	alreadyDisabled := !a.wssRuntimeDisabledAt.IsZero() && a.wssRuntimeDisabledReason == reason
	if alreadyDisabled {
		a.wssRuntimeMu.Unlock()
		return
	}
	a.wssRuntimeDisabledAt = now
	a.wssRuntimeDisabledReason = reason
	a.wssRuntimeMu.Unlock()
	a.log.info("WSS temporarily disabled reason=%s; using POST report", reason)
	if a.reporter != nil {
		a.reporter.stop(reason)
	}
	a.wakeTick()
}

func (a *Agent) clearWSSRuntimeDisabled(reason string) {
	a.wssRuntimeMu.Lock()
	wasDisabled := !a.wssRuntimeDisabledAt.IsZero()
	a.wssRuntimeDisabledAt = time.Time{}
	a.wssRuntimeDisabledReason = ""
	a.wssRuntimeMu.Unlock()
	if !wasDisabled {
		return
	}
	if reason == "" {
		reason = "server_active"
	}
	a.log.info("WSS temporary disable cleared reason=%s", reason)
	a.syncReportTransport()
	a.wakeTick()
}

func (a *Agent) handleWSSRuntimeHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	mode := strings.ToLower(strings.TrimSpace(headers.Get(agentWSSModeHeader)))
	reason := strings.ToLower(strings.TrimSpace(headers.Get(agentWSSReasonHeader)))
	switch mode {
	case agentWSSModeActive:
		a.clearWSSRuntimeDisabled(reason)
	case agentWSSModeInactive:
		if isWSSScheduleInactiveReason(reason) {
			a.disableWSSRuntime(reason)
		}
	case agentWSSModeDisabled:
		if isWSSScheduleInactiveReason(reason) {
			a.disableWSSRuntime(reason)
		}
	}
}

// applyAutoDiscoveryHeaders 处理服务端自动建号结果：
//   - created：绑定 id 并立即落盘生效，落盘失败也保留内存值，避免下次上报仍为空 id 而重复建号；
//   - reused：同名服务器已存在，不复用（避免把本机数据写进别人的服务器），提示后退出；
//   - failed：建号失败，提示后退出，不重试。
//
// 退出由 loop 检测 discoveryAbandoned 后完成。
func (a *Agent) applyAutoDiscoveryHeaders(headers http.Header) {
	if a == nil || headers == nil {
		return
	}
	raw := strings.ToLower(strings.TrimSpace(headers.Get(agentAutoDiscoveryHeader)))
	if raw == "" {
		return
	}

	status, code, _ := strings.Cut(raw, ":")
	switch status {
	case agentAutoDiscoveryCreated:
		serverID := strings.TrimSpace(headers.Get(agentAutoDiscoveryServerIDKey))
		if serverID == "" {
			return
		}
		cfg := a.configSnapshot()
		if cfg.ServerID == serverID {
			return
		}
		nextCfg := cfg
		nextCfg.ServerID = serverID
		if err := writeConfig(a.paths.ConfigFile, nextCfg); err != nil {
			// 只写内存也能继续上报，但重启会丢掉 id，所以后续上报里持续重试写入。
			a.pendingPersistID = serverID
			a.log.warnf("自动发现 id 写入配置失败（本次运行仍生效，之后会自动重试）: %v", err)
		}
		a.setConfig(nextCfg)
		a.log.info("自动发现已创建服务器，请两分钟后查看")
		// 建号成功后等同于一次普通安装：下一次 tick 立即用新 id 再 POST 一次
		// （服务端据此比对 md5 并入库），那次完成后再按 connection_mode 启动传输层。
		a.discoveryJustBound.Store(true)
		// 建号那次 POST 已经占用了一次尝试配额，这里必须清掉，
		// 否则紧接着的补报会被 postAttemptAllowed 挡到下一个上报周期。
		a.lastPostAttempt = time.Time{}
		a.reporter.resetReportInterval()
		a.wakeTick()
	case agentAutoDiscoveryReused:
		// 服务端出于防串号不会回传同名服务器的 id，这里不能引用它。
		a.abortAutoDiscovery(agentAutoDiscoveryReused, "")
	case agentAutoDiscoveryFailed:
		a.abortAutoDiscovery(agentAutoDiscoveryFailed, firstNonEmpty(code, "unknown"))
	}
}

// abortAutoDiscovery 只输出提示并置位，随后由 loop 返回错误让进程退出；不写任何文件。
func (a *Agent) abortAutoDiscovery(kind, detail string) {
	if a.discoveryFailed.Swap(true) {
		return
	}
	switch kind {
	case agentAutoDiscoveryReused:
		a.log.warnf("自动发现中止：服务端已存在同名服务器，请更换 -name 后重试，或获取该服务器 ID 后改用 install -id 安装")
	case agentAutoDiscoveryDisabled:
		a.log.warnf("服务端未开启自动发现：请在面板「设置 → 站点设置」中开启自动发现")
	default:
		a.log.warnf("自动发现失败：code=%s，不再重试。%s", detail, autoDiscoveryFailureHint(detail))
	}
}

// autoDiscoveryFailureHint 把服务端返回的失败码翻译成可执行建议。
func autoDiscoveryFailureHint(code string) string {
	switch code {
	case "no_name":
		return "服务端拿不到服务器名称，请改用 install -name=NAME 重试，或使用面板生成的 -id 安装"
	case "db_error":
		return "服务端建号时数据库出错，请稍后重试；持续失败请查看 Worker 日志，或改用 install -id 安装"
	default:
		return "请查看 Worker 日志排查，或改用 install -id 安装"
	}
}

func (a *Agent) tick() {
	now := time.Now()
	if a.discoveryAbandoned() {
		// 未绑定 id 且自动建号已中止：上报必然是 404，停止采样与上报。
		a.samples = nil
		return
	}
	if a.reportPauseRemaining(now) > 0 {
		// 熔断期间停止采样与序列化，这是 CPU 的主要节省点。
		// 缓冲里剩下的样本属于熔断前的窗口，直接丢弃：恢复后补发会带旧时间戳，
		// 服务端按样本时间分桶时会出现错位数据。samples 只由本 goroutine 写入，
		// 在这里清理不会与 transport 的失败计数产生竞争。
		a.samples = nil
		return
	}
	cfg := a.configSnapshot()
	reportInterval := time.Duration(cfg.ReportInterval) * time.Second
	if reportInterval < time.Second {
		reportInterval = time.Duration(defaultReportIntervalSec) * time.Second
	}
	wssEnabled := a.usesWSSConfig(cfg)
	wssConnected := wssEnabled && a.reporter != nil && a.reporter.connected()
	shouldWSSReport := wssConnected && (a.lastReport.IsZero() || now.Sub(a.lastReport) >= a.currentWSSReportIntervalForConfig(cfg))
	postDue := !wssConnected && (a.lastPost.IsZero() || now.Sub(a.lastPost) >= reportInterval)
	postAllowed := !wssEnabled || a.reporter == nil || a.reporter.postFallbackAllowed()
	if postDue && !postAllowed && wssEnabled && a.reporter != nil {
		a.reporter.logPostFallbackDelayed()
	}
	shouldPostReport := postDue && postAllowed && a.postAttemptAllowed(now, reportInterval)
	shouldSample := false
	if collectInterval := effectiveCollectInterval(cfg); collectInterval > 0 {
		shouldSample = a.lastSample.IsZero() || now.Sub(a.lastSample) >= collectInterval
	}
	if !shouldWSSReport && !shouldPostReport && !shouldSample {
		return
	}
	reportDue := shouldWSSReport || shouldPostReport
	fullDue := reportDue && (a.fullAt.IsZero() || now.Sub(a.fullAt) >= reportInterval)
	if fullDue {
		a.basic = collectBasicStats()
		a.basicAt = now
	} else {
		a.refreshRealtimeBasicStats()
	}
	netNow := readNetBytes(cfg.Interface)
	dt := now.Sub(a.prevTime).Seconds()
	if dt <= 0 {
		dt = float64(cfg.ReportInterval)
	}
	rxDelta := uint64(0)
	txDelta := uint64(0)
	if netNow.RX >= a.prevNet.RX {
		rxDelta = netNow.RX - a.prevNet.RX
	}
	if netNow.TX >= a.prevNet.TX {
		txDelta = netNow.TX - a.prevNet.TX
	}
	rxSpeed := uint64(float64(rxDelta) / dt)
	txSpeed := uint64(float64(txDelta) / dt)
	a.prevNet = netNow
	a.prevTime = now

	cpu := "0.00"
	if usage, ok := readCPUPercent(); ok {
		cpu = cpuPercentString(usage)
	}

	if fullDue {
		a.monthlyRX, a.monthlyTX = calcMonthlyTraffic(a.paths.TrafficFile, netNow, cfg.ResetDay, cfg.Interface)
		a.diskIO = a.sampleDiskIO(now)
		a.fullAt = now
	}
	m := a.buildMetrics(cfg, cpu, netNow, rxSpeed, txSpeed, a.monthlyRX, a.monthlyTX, a.diskIO)
	if shouldSample {
		a.appendSample(now, m)
	}
	if shouldWSSReport || shouldPostReport {
		a.applyReportResult(a.sendReport(cfg, m, shouldWSSReport, shouldPostReport, reportInterval), now)
	}
}

// applyReportResult 更新上报时间，并在每次上报尝试后清空样本窗口：
// 样本只代表当前上报间隔内的数据，失败也不留给下一次。
// 这意味着服务端拒绝期间该窗口的采样点直接丢弃，不会在下次成功上报时补发；
// 这是刻意取舍：补发会拖长 payload 且时间戳已经过期，服务端图表宁可有缺口。
func (a *Agent) applyReportResult(result reportSendResult, now time.Time) {
	defer a.persistPendingServerID()
	// 建号那一次上报不算“已完成的上报”：不记录 lastPost/lastReport，
	// 下一次 tick 立即带新 id 重报一次，让服务端走正常的 md5 比对与入库流程。
	if a != nil && a.discoveryJustBound.Swap(false) {
		a.samples = nil
		a.discoverySyncPending.Store(true)
		return
	}
	if result.ok {
		if result.viaWSS {
			a.lastReport = now
			a.lastPost = now
		} else if result.viaPOST {
			a.lastPost = now
		}
	}
	a.samples = nil
	// 带 id 的补报已经完成，这时才按 connection_mode 启动传输层（普通安装启动即如此），
	// 之后的节奏由 WSS / POST 自行决定。
	if a != nil && a.discoverySyncPending.Swap(false) && a.usesWSS() {
		a.syncReportTransport()
	}
}

// persistPendingServerID 重试写入自动发现拿到的 id：首次写入失败时 id 只在内存里，
// 重启后配置仍是空 id，会重新走建号流程（同名会中止、名字变了会建出重复服务器）。
func (a *Agent) persistPendingServerID() {
	if a == nil || a.pendingPersistID == "" {
		return
	}
	if err := writeConfig(a.paths.ConfigFile, a.configSnapshot()); err != nil {
		return
	}
	a.pendingPersistID = ""
	a.log.info("自动发现 id 已写入配置 %s，重启后无需重新建号", a.paths.ConfigFile)
}

func (a *Agent) appendSample(now time.Time, m Metrics) {
	a.samples = append(a.samples, metricSample{
		at:      now,
		metrics: sampleMetricsToMap(m),
	})
	if len(a.samples) > maxBufferedMetricSamples {
		// 单个上报窗口的硬上限：REPORT_INTERVAL 远大于 COLLECT_INTERVAL 时也不会撑大 body。
		copy(a.samples, a.samples[len(a.samples)-maxBufferedMetricSamples:])
		a.samples = a.samples[:maxBufferedMetricSamples]
	}
	a.lastSample = now
}

func (a *Agent) refreshRealtimeBasicStats() {
	mem, ok := readMemoryStats()
	if !ok {
		return
	}
	a.basic.MemTotalMB = mem.MemTotalMB
	a.basic.MemUsedMB = mem.MemUsedMB
	a.basic.SwapTotalMB = mem.SwapTotalMB
	a.basic.SwapUsedMB = mem.SwapUsedMB
}

func (a *Agent) sampleDiskIO(now time.Time) DiskIOStats {
	current := readDiskIOCounters(a.basic.DiskDevices)
	if a.prevDiskAt.IsZero() {
		a.prevDisk = current
		a.prevDiskAt = now
		return DiskIOStats{}
	}
	elapsed := now.Sub(a.prevDiskAt).Seconds()
	stats := diskIOStatsFromCounters(a.prevDisk, current, elapsed)
	a.prevDisk = current
	a.prevDiskAt = now
	return stats
}

func (a *Agent) buildMetrics(cfg Config, cpu string, netNow NetBytes, rxSpeed, txSpeed, rxMonthly, txMonthly uint64, diskIO DiskIOStats) Metrics {
	a.mu.RLock()
	probes := a.probes
	a.mu.RUnlock()
	b := a.basic
	return Metrics{
		CPU:          cpu,
		RAMTotal:     uintString(b.MemTotalMB),
		RAMUsed:      uintString(b.MemUsedMB),
		SwapTotal:    uintString(b.SwapTotalMB),
		SwapUsed:     uintString(b.SwapUsedMB),
		DiskTotal:    uintString(b.DiskTotalMB),
		DiskUsed:     uintString(b.DiskUsedMB),
		Disk:         diskIO,
		LoadAvg:      firstNonEmpty(b.LoadAvg, "0 0 0"),
		BootTime:     strconv.FormatInt(b.BootTimeMS, 10),
		NetRX:        uintString(netNow.RX),
		NetTX:        uintString(netNow.TX),
		NetRXMonthly: uintString(rxMonthly),
		NetTXMonthly: uintString(txMonthly),
		NetInSpeed:   uintString(rxSpeed),
		NetOutSpeed:  uintString(txSpeed),
		OS:           firstNonEmpty(b.OSName, runtime.GOOS),
		Arch:         firstNonEmpty(b.Arch, fallbackArch()),
		Kernel:       b.Kernel,
		CPUInfo:      firstNonEmpty(b.CPUInfo, fallbackArch()),
		CPUCores:     intString(b.CPUCores),
		GPUInfo:      b.GPUInfo,
		Processes:    intString(b.Processes),
		TCPConn:      intString(b.TCPConn),
		UDPConn:      intString(b.UDPConn),
		IPv4:         firstNonEmpty(probes.IPv4, "0"),
		IPv6:         firstNonEmpty(probes.IPv6, "0"),
		PingCT:       probeRTTValue(cfg.CTNode, probes.CT),
		PingCU:       probeRTTValue(cfg.CUNode, probes.CU),
		PingCM:       probeRTTValue(cfg.CMNode, probes.CM),
		PingBD:       probeRTTValue(cfg.BDNode, probes.BD),
		PingNode1:    probeRTTValue(cfg.Node1, probes.Node1),
		PingNode2:    probeRTTValue(cfg.Node2, probes.Node2),
		PingNode3:    probeRTTValue(cfg.Node3, probes.Node3),
		PingNode4:    probeRTTValue(cfg.Node4, probes.Node4),
		LossCT:       probeLossValue(cfg.CTNode, probes.CT),
		LossCU:       probeLossValue(cfg.CUNode, probes.CU),
		LossCM:       probeLossValue(cfg.CMNode, probes.CM),
		LossBD:       probeLossValue(cfg.BDNode, probes.BD),
		LossNode1:    probeLossValue(cfg.Node1, probes.Node1),
		LossNode2:    probeLossValue(cfg.Node2, probes.Node2),
		LossNode3:    probeLossValue(cfg.Node3, probes.Node3),
		LossNode4:    probeLossValue(cfg.Node4, probes.Node4),
	}
}

func probeRTTValue(node string, r ProbeResult) any {
	if strings.TrimSpace(node) == "" {
		return false
	}
	if !r.OK || r.RTTMs < 0 {
		return "null"
	}
	return strconv.Itoa(r.RTTMs)
}

func probeLossValue(node string, r ProbeResult) any {
	if strings.TrimSpace(node) == "" {
		return false
	}
	if r.Loss < 0 {
		return "100"
	}
	return strconv.Itoa(r.Loss)
}

func (a *Agent) report(m Metrics) {
	cfg := a.configSnapshot()
	reportAt := time.Now()
	body, sampleCount, err := a.buildReportBodyForConfig(cfg, m, reportAt)
	if err != nil {
		a.log.warnfOnce("marshal", "marshal payload failed: %v", err)
		return
	}
	a.logMetricsSummary(m)
	_, _ = a.postReportBody(cfg, body, sampleCount, false)
}

func (a *Agent) sendReport(cfg Config, m Metrics, preferWSS, allowPOST bool, reportInterval time.Duration) reportSendResult {
	reportAt := time.Now()
	body, sampleCount, err := a.buildReportBodyForConfig(cfg, m, reportAt)
	if err != nil {
		a.log.warnfOnce("marshal", "marshal payload failed: %v", err)
		return reportSendResult{}
	}
	a.logMetricsSummary(m)
	wssFailed := false
	if preferWSS && a.reporter != nil {
		a.log.debugf("WSS report attempt url=%s payload_bytes=%d samples=%d", a.reporter.url(), len(body), sampleCount)
		if a.reporter.send(body) {
			// 写入成功不代表服务端接受，成功计数由 ack 帧触发。
			return reportSendResult{ok: true, viaWSS: true}
		}
		wssFailed = true
	}
	if !allowPOST && !wssFailed {
		return reportSendResult{}
	}
	if preferWSS && a.reporter != nil && !a.reporter.postFallbackAllowed() {
		a.reporter.logPostFallbackDelayed()
		return reportSendResult{}
	}
	if !a.postAttemptAllowed(reportAt, reportInterval) {
		return reportSendResult{}
	}
	a.lastPostAttempt = reportAt
	statusCode, err := a.postReportBody(cfg, body, sampleCount, preferWSS)
	if isAuthConfigHTTPStatus(statusCode) && a.reporter != nil {
		a.reporter.delayProtocol(fmt.Sprintf("POST fallback http=%d", statusCode))
	}
	if err != nil || statusCode < 200 || statusCode >= 300 {
		// 只把 4xx/5xx 视为服务端拒绝。网络错误（statusCode=0）和 3xx 不计入连续失败，
		// 前者由 transport 自己的重连退避处理，后者理论上被 http client 跟随。
		if statusCode >= 400 {
			a.noteReportFailure(statusCode)
		}
		return reportSendResult{}
	}
	a.noteReportSuccess()
	return reportSendResult{ok: true, viaPOST: true}
}

func (a *Agent) postAttemptAllowed(now time.Time, reportInterval time.Duration) bool {
	if a == nil || a.lastPostAttempt.IsZero() {
		return true
	}
	if reportInterval < time.Second {
		reportInterval = time.Duration(defaultReportIntervalSec) * time.Second
	}
	return now.Sub(a.lastPostAttempt) >= reportInterval
}

func (a *Agent) buildReportBody(m Metrics, reportAt time.Time) ([]byte, int, error) {
	return a.buildReportBodyForConfig(a.configSnapshot(), m, reportAt)
}

func (a *Agent) buildReportBodyForConfig(cfg Config, m Metrics, reportAt time.Time) ([]byte, int, error) {
	payload := map[string]any{
		"id":               cfg.ServerID,
		"secret":           cfg.Secret,
		"time":             a.clock.snapshot(reportAt),
		"metrics":          a.metricsForReport(m, reportAt),
		"collect_interval": cfg.CollectInterval,
		"report_interval":  cfg.ReportInterval,
	}
	// 仅在自动发现阶段有意义：服务端建号时用它作为服务器名称。
	// 绑定 id 之后不再发送，避免每次上报都带一个无用的字段。
	if cfg.ServerID == "" {
		if name := strings.TrimSpace(cfg.Name); name != "" {
			payload["name"] = name
		}
	}
	if a.shouldReportConfigState(firstNonEmpty(cfg.ConfigMD5, "none"), reportAt) {
		payload["config_schema"] = configSchemaVersion
		payload["config_md5"] = firstNonEmpty(cfg.ConfigMD5, "none")
	}
	if len(a.samples) > 0 {
		payload["samples"] = a.samplesForReport()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	return body, len(a.samples), nil
}

func (a *Agent) shouldReportConfigState(configMD5 string, reportAt time.Time) bool {
	if a.lastConfigStateReportAt.IsZero() ||
		configMD5 != a.lastConfigStateReportMD5 ||
		reportAt.Sub(a.lastConfigStateReportAt) >= configStateReportInterval {
		a.lastConfigStateReportAt = reportAt
		a.lastConfigStateReportMD5 = configMD5
		return true
	}
	return false
}

func (a *Agent) logMetricsSummary(m Metrics) {
	gpuSummary, _ := json.Marshal(m.GPUInfo)
	a.log.debugf("metrics summary cpu=%s gpu_info=%s", m.CPU, string(gpuSummary))
}

func (a *Agent) postReportBody(cfg Config, body []byte, sampleCount int, fallback bool) (int, error) {
	label := "report"
	if fallback {
		label = "POST fallback"
	}
	a.log.debugf("%s attempt url=%s payload_bytes=%d samples=%d", label, cfg.WorkerURL, len(body), sampleCount)
	req, err := http.NewRequest(http.MethodPost, cfg.WorkerURL, bytes.NewReader(body))
	if err != nil {
		a.log.warnfOnce("create-request:"+label, "create %s request failed: %v", label, err)
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	a.setAgentHeaders(req)
	req.Header.Set("X-Agent-Config-Schema", configSchemaVersion)
	req.Header.Set("X-Agent-Version", a.version)
	req.Header.Set("X-Agent-Config-Md5", firstNonEmpty(cfg.ConfigMD5, "none"))

	started := time.Now()
	resp, err := sharedReportHTTPClient(8*time.Second, usePublicDNSResolver(cfg)).Do(req)
	if err != nil {
		a.log.debugf("%s failed: %v", label, err)
		return 0, err
	}
	headerReceived := time.Now()
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	respHeaders := resp.Header
	reportHTTPCode := resp.StatusCode
	a.handleTimedReportResponse(reportHTTPCode, respBody, respHeaders, started, headerReceived)
	return reportHTTPCode, nil
}

func (a *Agent) samplesForReport() []map[string]any {
	samples := make([]map[string]any, 0, len(a.samples))
	for _, sample := range a.samples {
		timestamp, _ := a.clock.timestamp(sample.at)
		samples = append(samples, map[string]any{
			"ts":      timestamp,
			"metrics": sample.metrics,
		})
	}
	return samples
}

func (a *Agent) metricsForReport(m Metrics, reportAt time.Time) map[string]any {
	metrics := metricsToMap(m)
	bootTime, err := strconv.ParseInt(m.BootTime, 10, 64)
	if err == nil && bootTime > 0 {
		metrics["boot_time"] = strconv.FormatInt(a.clock.correctLocalTimestamp(bootTime, reportAt), 10)
	}
	return metrics
}

func (a *Agent) handleReportResponse(statusCode int, respBody []byte, headers http.Header) {
	now := time.Now()
	a.handleTimedReportResponse(statusCode, respBody, headers, now, now)
}

func (a *Agent) handleTimedReportResponse(statusCode int, respBody []byte, headers http.Header, started, received time.Time) {
	a.log.debugf("report response http=%d body=%s", statusCode, strings.TrimSpace(string(respBody)))
	// 自动建号结果可能出现在任意状态码上（created/reused 通常为 2xx，failed 为 404；
	// 服务端建号后仍可能因载荷校验失败返回 4xx，此时同样要绑定 id，否则下次上报会再建一台），
	// 因此必须在状态判断之前处理；也必须在 applyRemoteConfig 之前绑定 id。
	a.applyAutoDiscoveryHeaders(headers)
	// 未绑定 id 且收到不带自动发现标记的 404：服务端没开自动发现（或版本过旧）。
	// 这种情况下每次上报都必然是 404，直接提示后退出，避免静默空转。
	if statusCode == http.StatusNotFound &&
		strings.TrimSpace(headers.Get(agentAutoDiscoveryHeader)) == "" &&
		a.configSnapshot().ServerID == "" {
		a.abortAutoDiscovery(agentAutoDiscoveryDisabled, "")
		return
	}
	if statusCode < 200 || statusCode >= 300 {
		return
	}
	a.handleWSSRuntimeHeaders(headers)
	if dateTime, ok := responseDateTime(headers); ok {
		snapshot, updated := a.clock.updateDate(dateTime, received.Sub(started), received)
		if updated {
			a.log.debugf("Date header time calibrated offset_ms=%d round_trip_ms=%d",
				valueOrZero(snapshot.OffsetMS), valueOrZero(snapshot.RoundTripMS))
		} else {
			a.log.debugf("Date header time calibration skipped offset_ms=%d threshold_ms=%d",
				valueOrZero(snapshot.OffsetMS), int64(dateCalibrationThreshold/time.Millisecond))
		}
	}
	rawBody := strings.TrimSpace(string(respBody))
	if rawBody == "" || rawBody == "{}" || strings.EqualFold(rawBody, "OK") {
		return
	}
	if strings.HasPrefix(rawBody, "{") {
		var envelope map[string]json.RawMessage
		if json.Unmarshal(respBody, &envelope) == nil && len(envelope) == 0 {
			return
		}
	}
	if statusCode == http.StatusOK {
		if err := a.applyRemoteConfig(respBody, headers); err != nil {
			// 服务端会一直重发同一份配置，warnf 现在始终输出：
			// 同一原因只提示一次，避免每个上报周期刷一行。
			if key := err.Error(); key != a.lastConfigRejectKey {
				a.lastConfigRejectKey = key
				a.log.warnf("dynamic configuration rejected: %v", err)
			}
		} else {
			a.lastConfigRejectKey = ""
		}
	}
}

func valueOrZero[T ~int64 | ~uint64](value *T) T {
	if value == nil {
		return 0
	}
	return *value
}

func (a *Agent) networkWorker(ctx context.Context) {
	var lastIP, lastProbe time.Time
	var ctHistory, cuHistory, cmHistory, bdHistory, node1History, node2History, node3History, node4History rollingProbeHistory
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cfg := a.configSnapshot()
			snap := ProbeSnapshot{}
			needUpdate := false
			if lastIP.IsZero() || now.Sub(lastIP) >= 10*time.Minute {
				usePublicDNS := usePublicDNSResolver(cfg)
				snap.IPv4 = lookupPublicIP("tcp4", a.log, usePublicDNS)
				snap.IPv6 = lookupPublicIP("tcp6", a.log, usePublicDNS)
				lastIP = now
				needUpdate = true
			}
			if lastProbe.IsZero() || now.Sub(lastProbe) >= metricsProbeInterval {
				ctHistory.add(now, probeHistoryKey(cfg.PingMode, cfg.CTNode), measureProbe(cfg.PingMode, cfg.CTNode, metricsProbeSampleCount, defaultMetricsTCPPort, a.log))
				cuHistory.add(now, probeHistoryKey(cfg.PingMode, cfg.CUNode), measureProbe(cfg.PingMode, cfg.CUNode, metricsProbeSampleCount, defaultMetricsTCPPort, a.log))
				cmHistory.add(now, probeHistoryKey(cfg.PingMode, cfg.CMNode), measureProbe(cfg.PingMode, cfg.CMNode, metricsProbeSampleCount, defaultMetricsTCPPort, a.log))
				bdHistory.add(now, probeHistoryKey(cfg.PingMode, cfg.BDNode), measureProbe(cfg.PingMode, cfg.BDNode, metricsProbeSampleCount, defaultMetricsTCPPort, a.log))
				node1History.add(now, probeHistoryKey(cfg.PingMode, cfg.Node1), measureProbe(cfg.PingMode, cfg.Node1, metricsProbeSampleCount, defaultMetricsTCPPort, a.log))
				node2History.add(now, probeHistoryKey(cfg.PingMode, cfg.Node2), measureProbe(cfg.PingMode, cfg.Node2, metricsProbeSampleCount, defaultMetricsTCPPort, a.log))
				node3History.add(now, probeHistoryKey(cfg.PingMode, cfg.Node3), measureProbe(cfg.PingMode, cfg.Node3, metricsProbeSampleCount, defaultMetricsTCPPort, a.log))
				node4History.add(now, probeHistoryKey(cfg.PingMode, cfg.Node4), measureProbe(cfg.PingMode, cfg.Node4, metricsProbeSampleCount, defaultMetricsTCPPort, a.log))
				snap.CT = ctHistory.snapshot(now)
				snap.CU = cuHistory.snapshot(now)
				snap.CM = cmHistory.snapshot(now)
				snap.BD = bdHistory.snapshot(now)
				snap.Node1 = node1History.snapshot(now)
				snap.Node2 = node2History.snapshot(now)
				snap.Node3 = node3History.snapshot(now)
				snap.Node4 = node4History.snapshot(now)
				lastProbe = now
				needUpdate = true
			}
			if needUpdate {
				a.mu.Lock()
				if snap.IPv4 == "" {
					snap.IPv4 = a.probes.IPv4
				}
				if snap.IPv6 == "" {
					snap.IPv6 = a.probes.IPv6
				}
				if snap.CT == (ProbeResult{}) {
					snap.CT = a.probes.CT
				}
				if snap.CU == (ProbeResult{}) {
					snap.CU = a.probes.CU
				}
				if snap.CM == (ProbeResult{}) {
					snap.CM = a.probes.CM
				}
				if snap.BD == (ProbeResult{}) {
					snap.BD = a.probes.BD
				}
				if snap.Node1 == (ProbeResult{}) {
					snap.Node1 = a.probes.Node1
				}
				if snap.Node2 == (ProbeResult{}) {
					snap.Node2 = a.probes.Node2
				}
				if snap.Node3 == (ProbeResult{}) {
					snap.Node3 = a.probes.Node3
				}
				if snap.Node4 == (ProbeResult{}) {
					snap.Node4 = a.probes.Node4
				}
				a.probes = snap
				a.mu.Unlock()
			}
		}
	}
}

var remoteBodyRE = regexp.MustCompile(`^[A-Za-z0-9_=&.,:%+\-\*\?\[\]]*$`)

func (a *Agent) applyRemoteConfig(body []byte, headers http.Header) error {
	return a.applyRemoteConfigWithOptions(body, headers, false)
}

func (a *Agent) applyWSSRemoteConfig(body []byte, headers http.Header) error {
	if a == nil || a.remoteConfig == nil || a.ctx == nil {
		return a.applyRemoteConfigWithOptions(body, headers, true)
	}
	req := remoteConfigRequest{
		body:            append([]byte(nil), body...),
		headers:         headers.Clone(),
		allowMissingMD5: true,
		done:            make(chan error, 1),
	}
	select {
	case a.remoteConfig <- req:
	case <-a.ctx.Done():
		return a.ctx.Err()
	}
	select {
	case err := <-req.done:
		return err
	case <-a.ctx.Done():
		return a.ctx.Err()
	}
}

func (a *Agent) applyRemoteConfigWithOptions(body []byte, headers http.Header, allowMissingMD5 bool) error {
	if len(body) == 0 {
		return errors.New("empty body")
	}
	if len(body) > 1024 {
		return errors.New("response too large")
	}
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return errors.New("empty body")
	}
	if !remoteBodyRE.MatchString(raw) {
		return errors.New("invalid body characters")
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		"collect_interval":    true,
		"report_interval":     true,
		"wss_report_interval": true,
		"reset_day":           true,
		"schema_version":      true,
		"custom_ct":           true,
		"custom_cu":           true,
		"custom_cm":           true,
		"custom_bd":           true,
		"node_1":              true,
		"node_2":              true,
		"node_3":              true,
		"node_4":              true,
		"interface":           true,
		"connection_mode":     true,
		"ping_mode":           true,
		"rx_correction":       true,
		"tx_correction":       true,
		"update":              true,
	}
	for key := range values {
		if !allowed[key] {
			return fmt.Errorf("unknown field %s", key)
		}
	}
	update := values.Get("update")
	if update != "" && update != "0" && update != "1" {
		return fmt.Errorf("invalid update %s", update)
	}
	hasConfig := values.Has("collect_interval") || values.Has("report_interval") || values.Has("wss_report_interval") || values.Has("reset_day") || values.Has("schema_version") || values.Has("interface") || values.Has("connection_mode") || values.Has("ping_mode") || values.Has("node_1") || values.Has("node_2") || values.Has("node_3") || values.Has("node_4")
	hasCorrection := values.Has("rx_correction") || values.Has("tx_correction")
	cfg := a.configSnapshot()
	if !hasConfig {
		if hasCorrection {
			rx := values.Get("rx_correction")
			tx := values.Get("tx_correction")
			if err := applyTrafficCorrection(a.paths.TrafficFile, readNetBytes(cfg.Interface), cfg.Interface, rx, tx); err != nil {
				return err
			}
			_ = a.sendCorrectionConfirm(cfg, rx, tx)
			return nil
		}
		if values.Has("update") {
			return nil
		}
		return errors.New("no config fields")
	}
	newMD5 := strings.ToLower(strings.TrimSpace(headers.Get("X-Agent-Config-Md5")))
	hasRemoteMD5 := validConfigMD5(newMD5)
	if !hasRemoteMD5 && !allowMissingMD5 {
		return errors.New("invalid remote md5")
	}
	collect := parseIntDefault(values.Get("collect_interval"), -1)
	report := parseIntDefault(values.Get("report_interval"), -1)
	wssReport := parseIntDefault(values.Get("wss_report_interval"), defaultWSSReportIntervalSec)
	reset := parseIntDefault(values.Get("reset_day"), -1)
	// Keep accepting the legacy sampling interval values. In auto mode a
	// value above the WSS cadence is normalized to wss_report_interval below,
	// while HTTP mode continues to honor the configured sampling interval.
	if !inIntSet(collect, 0, 1, 2, 5, 10) {
		return fmt.Errorf("invalid collect_interval %d", collect)
	}
	if !inIntSet(report, 30, 60, 120, 180) {
		return fmt.Errorf("invalid report_interval %d", report)
	}
	if wssReport < minWSSReportIntervalSec || wssReport > maxWSSReportIntervalSec {
		return fmt.Errorf("invalid wss_report_interval %d", wssReport)
	}
	if reset < 0 || reset > 31 {
		return fmt.Errorf("invalid reset_day %d", reset)
	}
	if values.Get("schema_version") != configSchemaVersion {
		return fmt.Errorf("invalid schema_version %s", values.Get("schema_version"))
	}
	if report < collect {
		return errors.New("report_interval less than collect_interval")
	}
	iface, err := normalizeInterfaceList(values.Get("interface"))
	if err != nil {
		return err
	}
	connectionMode, err := normalizeConnectionMode(values.Get("connection_mode"))
	if err != nil {
		return err
	}
	pingMode, err := normalizePingMode(values.Get("ping_mode"))
	if err != nil {
		return err
	}
	effectiveCollect := collect
	if connectionMode == connectionModeAuto && (collect == 0 || collect > wssReport) {
		effectiveCollect = wssReport
	}
	shouldApply := false
	if hasRemoteMD5 {
		shouldApply = newMD5 != cfg.ConfigMD5
	} else {
		shouldApply = remoteConfigDiffers(cfg, values, effectiveCollect, report, reset, iface, connectionMode, pingMode)
	}
	if shouldApply {
		nextCfg := cfg
		nextCfg.CollectInterval = effectiveCollect
		nextCfg.ReportInterval = report
		nextCfg.ResetDay = reset
		nextCfg.CTNode = values.Get("custom_ct")
		nextCfg.CUNode = values.Get("custom_cu")
		nextCfg.CMNode = values.Get("custom_cm")
		nextCfg.BDNode = values.Get("custom_bd")
		nextCfg.Node1 = values.Get("node_1")
		nextCfg.Node2 = values.Get("node_2")
		nextCfg.Node3 = values.Get("node_3")
		nextCfg.Node4 = values.Get("node_4")
		nextCfg.Interface = iface
		nextCfg.ConnectionMode = connectionMode
		nextCfg.PingMode = pingMode
		if hasRemoteMD5 {
			nextCfg.ConfigMD5 = newMD5
		}
		if err := writeConfig(a.paths.ConfigFile, nextCfg); err != nil {
			return err
		}
		a.setConfig(nextCfg)
		now := time.Now()
		a.prevNet = readNetBytes(nextCfg.Interface)
		a.prevTime = now
		a.prevDisk = DiskIOCounters{}
		a.prevDiskAt = time.Time{}
		a.diskIO = DiskIOStats{}
		a.monthlyRX = 0
		a.monthlyTX = 0
		a.fullAt = time.Time{}
		a.samples = nil
		a.lastSample = time.Time{}
		a.lastReport = time.Time{}
		a.lastPost = time.Time{}
		a.lastPostAttempt = time.Time{}
		a.lastConfigStateReportAt = time.Time{}
		a.lastConfigStateReportMD5 = ""
		if a.reporter != nil {
			a.reporter.resetReportInterval()
		}
		a.syncReportTransport()
		a.wakeTick()
		a.log.info("dynamic configuration applied md5=%s connection_mode=%s ping_mode=%s interface=%s", firstNonEmpty(nextCfg.ConfigMD5, "none"), nextCfg.ConnectionMode, nextCfg.PingMode, firstNonEmpty(iface, "auto"))
		cfg = nextCfg
	}
	if hasCorrection {
		rx := values.Get("rx_correction")
		tx := values.Get("tx_correction")
		if err := applyTrafficCorrection(a.paths.TrafficFile, readNetBytes(cfg.Interface), cfg.Interface, rx, tx); err != nil {
			return err
		}
		_ = a.sendCorrectionConfirm(cfg, rx, tx)
	}
	return nil
}
func validConfigMD5(value string) bool {
	return len(value) == 32 && !strings.ContainsFunc(value, func(r rune) bool {
		return !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f')
	})
}

func (a *Agent) syncReportTransport() {
	if a.usesWSS() {
		if a.ctx == nil {
			return
		}
		if a.reporter == nil {
			a.reporter = newReportTransport(a)
		}
		a.reporter.start(a.ctx)
		return
	}
	if a.reporter != nil {
		a.reporter.stop("connection_mode=http")
	}
}

func remoteConfigDiffers(cfg Config, values url.Values, collect, report, reset int, iface, connectionMode, pingMode string) bool {
	return cfg.CollectInterval != collect ||
		cfg.ReportInterval != report ||
		cfg.ResetDay != reset ||
		cfg.CTNode != values.Get("custom_ct") ||
		cfg.CUNode != values.Get("custom_cu") ||
		cfg.CMNode != values.Get("custom_cm") ||
		cfg.BDNode != values.Get("custom_bd") ||
		cfg.Node1 != values.Get("node_1") ||
		cfg.Node2 != values.Get("node_2") ||
		cfg.Node3 != values.Get("node_3") ||
		cfg.Node4 != values.Get("node_4") ||
		cfg.Interface != iface ||
		cfg.ConnectionMode != connectionMode ||
		cfg.PingMode != pingMode
}
func inIntSet(v int, allowed ...int) bool {
	for _, item := range allowed {
		if v == item {
			return true
		}
	}
	return false
}

func (a *Agent) setAgentHeaders(req *http.Request) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "cfsm")
}

func (a *Agent) sendCorrectionConfirm(cfg Config, rx, tx string) error {
	if _, err := parseTrafficCorrectionGB(rx); err != nil {
		return err
	}
	if _, err := parseTrafficCorrectionGB(tx); err != nil {
		return err
	}
	payload := map[string]any{
		"id":            cfg.ServerID,
		"secret":        cfg.Secret,
		"rx_correction": parseFloatDefault(rx, 0),
		"tx_correction": parseFloatDefault(tx, 0),
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, cfg.WorkerURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	a.setAgentHeaders(req)
	client := http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	statusCode := resp.StatusCode
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("http %d", statusCode)
	}
	a.log.info("traffic correction confirm sent rx=%sGB tx=%sGB", firstNonEmpty(rx, "0"), firstNonEmpty(tx, "0"))
	return nil
}
func parseFloatDefault(raw string, def float64) float64 {
	if strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}
