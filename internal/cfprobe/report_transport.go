package cfprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	wssProtocolRetryDelay = 120 * time.Second
	wssNetworkMinRetry    = 60 * time.Second
	wssNetworkMaxRetry    = 5 * time.Minute
	wssHandshakeTimeout   = 10 * time.Second
	wssHelloTimeout       = 10 * time.Second
	wssWriteTimeout       = 8 * time.Second
	wssReadIdleGrace      = 15 * time.Second
	wssReadIdleMin        = 15 * time.Second
	wssConfigMinInterval  = time.Minute
	wssDynamicMinInterval = time.Second
	wssDynamicMaxInterval = 5 * time.Minute
)

type reportTransport struct {
	agent *Agent
	wsURL string

	mu               sync.Mutex
	conn             *webSocketConn
	pauseUntil       time.Time
	pauseReason      string
	lastPostDelayLog time.Time
	lastConfigAt     time.Time
	nextReportAfter  time.Duration
	wake             chan struct{}
	running          bool
	stopped          bool
	restartRequested bool
}

type wsProtocolDelayError struct {
	reason string
}

type wsScheduleInactiveError struct {
	reason string
}

func (e *wsProtocolDelayError) Error() string {
	return e.reason
}

func (e *wsScheduleInactiveError) Error() string {
	if e.reason == "" {
		return agentWSSScheduleInactive
	}
	return e.reason
}

type wsHelloFrame struct {
	Type     string `json:"type"`
	TS       int64  `json:"ts"`
	Protocol string `json:"protocol"`
}

type wsServerFrame struct {
	Type                 string `json:"type"`
	TS                   int64  `json:"ts"`
	Persisted            *bool  `json:"persisted"`
	NextD1WriteAfterMs   *int64 `json:"nextD1WriteAfterMs"`
	NextWSSReportAfterMs *int64 `json:"nextWssReportAfterMs"`
	Error                string `json:"error"`
	Code                 int    `json:"code"`
	Text                 string `json:"text"`
	ConnectionMode       string `json:"connection_mode"`
	Body                 string `json:"body"`
	ConfigBody           string `json:"config_body"`
	Config               any    `json:"config"`
	ConfigMD5            string `json:"config_md5"`
	ConfigMD5Camel       string `json:"configMd5"`
	MD5                  string `json:"md5"`
	Payload              any    `json:"payload"`
	Headers              any    `json:"headers"`
}

type wsScheduleInactivePayload struct {
	Text           string `json:"text"`
	Code           int    `json:"code"`
	ConnectionMode string `json:"connection_mode"`
	Error          string `json:"error"`
}

func newReportTransport(a *Agent) *reportTransport {
	cfg := a.configSnapshot()
	wsURL, err := reportWebSocketURL(cfg.WorkerURL)
	if err != nil {
		a.log.info("WSS retry delayed reason=invalid_url error=%v", err)
	}
	return &reportTransport{
		agent: a,
		wsURL: wsURL,
		wake:  make(chan struct{}, 1),
	}
}

func reportWebSocketURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host in %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return u.String(), nil
}

func reportWebSocketURLWithConfig(raw, schema, md5 string) (string, error) {
	wsURL, err := reportWebSocketURL(raw)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", err
	}
	values := u.Query()
	if strings.TrimSpace(schema) != "" {
		values.Set("config_schema", strings.TrimSpace(schema))
	}
	if strings.TrimSpace(md5) != "" {
		values.Set("config_md5", strings.TrimSpace(md5))
	}
	u.RawQuery = values.Encode()
	return u.String(), nil
}

func (r *reportTransport) reportInterval(fallback time.Duration) time.Duration {
	if r == nil {
		return fallback
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nextReportAfter > 0 {
		return r.nextReportAfter
	}
	return fallback
}

func (r *reportTransport) setNextReportAfterMs(ms int64) {
	if r == nil || ms <= 0 {
		return
	}
	next := time.Duration(ms) * time.Millisecond
	if next < wssDynamicMinInterval {
		next = wssDynamicMinInterval
	}
	if next > wssDynamicMaxInterval {
		next = wssDynamicMaxInterval
	}
	r.mu.Lock()
	previous := r.nextReportAfter
	r.nextReportAfter = next
	r.mu.Unlock()
	if r.agent != nil && (previous == 0 || next < previous) {
		r.agent.wakeTick()
	}
}

func (r *reportTransport) resetReportInterval() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.nextReportAfter = 0
	r.mu.Unlock()
}

func (r *reportTransport) start(ctx context.Context) bool {
	if r == nil || r.wsURL == "" {
		return false
	}
	r.mu.Lock()
	if r.wake == nil {
		r.wake = make(chan struct{}, 1)
	}
	wake := r.wake
	if r.running {
		wasStopped := r.stopped
		r.stopped = false
		if wasStopped {
			r.restartRequested = true
		}
		r.mu.Unlock()
		if wasStopped {
			signalReportTransportWake(wake)
		}
		return false
	}
	r.running = true
	r.stopped = false
	r.mu.Unlock()
	go r.run(ctx)
	return true
}

func (r *reportTransport) stop(reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopped = true
	r.pauseUntil = time.Time{}
	r.pauseReason = ""
	conn := r.conn
	r.conn = nil
	wake := r.wake
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	signalReportTransportWake(wake)
	if reason != "" {
		r.agent.log.info("WSS stopped reason=%s", reason)
	}
}

func (r *reportTransport) isStopped() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *reportTransport) run(ctx context.Context) {
	if r == nil || r.wsURL == "" {
		return
	}
	defer func() {
		if r.finishRun(ctx) {
			go r.run(ctx)
		}
	}()
	backoff := wssNetworkMinRetry
	for {
		if !r.shouldRun(ctx) {
			return
		}
		r.clearRestartRequest()
		if !r.waitProtocolPause(ctx) {
			return
		}
		if !r.shouldRun(ctx) {
			return
		}
		r.clearRestartRequest()
		conn, headers, started, received, err := r.dial(ctx)
		if err != nil {
			if handshakeErr := (*wsHandshakeError)(nil); errors.As(err, &handshakeErr) {
				if reason, ok := wssScheduleInactiveFromHandshake(handshakeErr); ok {
					r.agent.disableWSSRuntime(reason)
					backoff = wssNetworkMinRetry
					continue
				}
				if isAuthConfigHTTPStatus(handshakeErr.StatusCode) {
					r.delayProtocol(fmt.Sprintf("WSS handshake http=%d", handshakeErr.StatusCode))
					backoff = wssNetworkMinRetry
					continue
				}
			}
			if !r.waitNetworkRetry(ctx, err, backoff) {
				return
			}
			backoff = nextWSSBackoff(backoff)
			continue
		}
		if !r.shouldRun(ctx) {
			_ = conn.Close()
			return
		}
		backoff = wssNetworkMinRetry
		r.calibrateHandshakeDate(headers, started, received)

		hello, err := r.waitHello(conn)
		if err != nil {
			_ = conn.Close()
			if r.handleConnectionError(ctx, err, &backoff) {
				continue
			}
			return
		}
		if !r.setConnIfActive(ctx, conn) {
			_ = conn.Close()
			return
		}
		r.agent.log.info("WSS connected url=%s protocol=%s ts=%d", r.wsURL, hello.Protocol, hello.TS)

		err = r.readLoop(ctx, conn)
		r.clearConn(conn)
		_ = conn.Close()
		if r.handleConnectionError(ctx, err, &backoff) {
			continue
		}
		return
	}
}

func (r *reportTransport) finishRun(ctx context.Context) bool {
	usesWSS := r != nil && r.agent != nil && r.agent.usesWSS()
	ctxDone := isContextDone(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	restart := !ctxDone && r.wsURL != "" && !r.stopped && (usesWSS || r.restartRequested)
	r.restartRequested = false
	if restart {
		r.running = true
		return true
	}
	r.running = false
	return false
}

func (r *reportTransport) shouldRun(ctx context.Context) bool {
	if r == nil || r.agent == nil || isContextDone(ctx) || !r.agent.usesWSS() {
		return false
	}
	return !r.isStopped()
}

func (r *reportTransport) clearRestartRequest() {
	r.mu.Lock()
	r.restartRequested = false
	r.mu.Unlock()
}

func (r *reportTransport) dial(ctx context.Context) (*webSocketConn, http.Header, time.Time, time.Time, error) {
	cfg := r.agent.configSnapshot()
	configMD5 := firstNonEmpty(cfg.ConfigMD5, "none")
	wsURL, err := reportWebSocketURLWithConfig(r.wsURL, configSchemaVersion, configMD5)
	if err != nil {
		wsURL = r.wsURL
	}
	headers := http.Header{}
	headers.Set("Accept", "*/*")
	headers.Set("User-Agent", "cfsm")
	headers.Set("X-Agent-Config-Schema", configSchemaVersion)
	headers.Set("X-Agent-Version", r.agent.version)
	headers.Set("X-Agent-Config-Md5", configMD5)
	return dialReportWebSocket(ctx, wsURL, headers, usePublicDNSResolver(cfg), wssHandshakeTimeout)
}

func (r *reportTransport) waitHello(conn *webSocketConn) (wsHelloFrame, error) {
	_ = conn.SetReadDeadline(time.Now().Add(wssHelloTimeout))
	defer conn.SetReadDeadline(time.Time{})

	payload, opcode, err := conn.ReadDataMessage()
	if err != nil {
		return wsHelloFrame{}, err
	}
	if opcode != wsOpcodeText {
		return wsHelloFrame{}, fmt.Errorf("WSS hello invalid opcode=%d", opcode)
	}
	var hello wsHelloFrame
	if err := json.Unmarshal(payload, &hello); err != nil {
		return wsHelloFrame{}, fmt.Errorf("WSS hello invalid json: %w", err)
	}
	if hello.Type != "hello" || hello.Protocol != "update" {
		return wsHelloFrame{}, fmt.Errorf("WSS hello invalid type=%q protocol=%q", hello.Type, hello.Protocol)
	}
	return hello, nil
}

func (r *reportTransport) readLoop(ctx context.Context, conn *webSocketConn) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(r.readIdleTimeout()))
		payload, opcode, err := conn.ReadDataMessage()
		if err != nil {
			return err
		}
		if opcode != wsOpcodeText {
			r.agent.log.debugf("WSS message ignored opcode=%d bytes=%d", opcode, len(payload))
			continue
		}
		var frame wsServerFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			r.agent.log.debugf("WSS message ignored invalid_json=%v", err)
			continue
		}
		switch frame.Type {
		case "ack":
			persisted := false
			if frame.Persisted != nil {
				persisted = *frame.Persisted
			}
			nextD1 := int64(0)
			if frame.NextD1WriteAfterMs != nil {
				nextD1 = *frame.NextD1WriteAfterMs
			}
			nextWSS := int64(0)
			if frame.NextWSSReportAfterMs != nil {
				nextWSS = *frame.NextWSSReportAfterMs
				r.setNextReportAfterMs(nextWSS)
			}
			r.agent.log.debugf("WSS ack ts=%d persisted=%v nextD1WriteAfterMs=%d nextWssReportAfterMs=%d", frame.TS, persisted, nextD1, nextWSS)
			if body, headers, ok := wssConfigBodyAndHeaders(frame); ok {
				r.applyConfigBody(body, headers)
			}
		case "error":
			reason := firstNonEmpty(frame.Error, "server_error")
			if scheduleReason, ok := wssScheduleInactiveFromFrame(frame); ok {
				r.agent.log.info("WSS unavailable ts=%d reason=%s", frame.TS, scheduleReason)
				r.agent.disableWSSRuntime(scheduleReason)
				return &wsScheduleInactiveError{reason: scheduleReason}
			}
			r.agent.log.info("WSS error ts=%d code=%d error=%s", frame.TS, frame.Code, reason)
			return &wsProtocolDelayError{reason: fmt.Sprintf("server error code=%d error=%s", frame.Code, reason)}
		case "config", "remote_config":
			r.handleConfigFrame(frame)
		case "hello":
			r.agent.log.debugf("WSS hello repeated ts=%d", frame.TS)
		default:
			r.agent.log.debugf("WSS message ignored type=%q", frame.Type)
		}
	}
}

func (r *reportTransport) readIdleTimeout() time.Duration {
	interval := time.Duration(defaultWSSReportIntervalSec) * time.Second
	if r != nil {
		interval = r.reportInterval(interval)
	}
	timeout := interval + wssReadIdleGrace
	if timeout < wssReadIdleMin {
		return wssReadIdleMin
	}
	return timeout
}

func (r *reportTransport) handleConfigFrame(frame wsServerFrame) {
	body, headers, ok := wssConfigBodyAndHeaders(frame)
	if !ok {
		r.agent.log.debugf("WSS config ignored: empty body")
		return
	}
	r.applyConfigBody(body, headers)
}

func (r *reportTransport) applyConfigBody(body []byte, headers http.Header) {
	if !r.reserveConfigApplySlot() {
		return
	}
	if err := r.agent.applyWSSRemoteConfig(body, headers); err != nil {
		r.agent.log.warnf("WSS config rejected: %v", err)
		return
	}
	r.agent.log.info("WSS config processed bytes=%d", len(body))
}

func (r *reportTransport) reserveConfigApplySlot() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.lastConfigAt.IsZero() && now.Sub(r.lastConfigAt) < wssConfigMinInterval {
		remaining := wssConfigMinInterval - now.Sub(r.lastConfigAt)
		r.agent.log.debugf("WSS config delayed remaining=%s", remaining.Round(time.Second))
		return false
	}
	r.lastConfigAt = now
	return true
}

func wssConfigBodyAndHeaders(frame wsServerFrame) ([]byte, http.Header, bool) {
	body := firstNonEmpty(frame.Body, frame.ConfigBody, wssStringFromAny(frame.Config))
	md5 := firstNonEmpty(frame.ConfigMD5, frame.ConfigMD5Camel, frame.MD5)
	headers := http.Header{}
	if h := stringMapFromAny(frame.Headers); len(h) > 0 {
		for key, value := range h {
			headers.Set(key, value)
		}
		md5 = firstNonEmpty(md5, h["X-Agent-Config-Md5"], h["x-agent-config-md5"])
	}
	if body == "" {
		if payloadBody, payloadMD5, payloadHeaders := wssConfigPayload(frame.Payload); payloadBody != "" {
			body = payloadBody
			md5 = firstNonEmpty(md5, payloadMD5)
			for key, value := range payloadHeaders {
				headers.Set(key, value)
			}
		}
	}
	if body == "" {
		if configBody, configMD5, configHeaders := wssConfigPayload(frame.Config); configBody != "" {
			body = configBody
			md5 = firstNonEmpty(md5, configMD5)
			for key, value := range configHeaders {
				headers.Set(key, value)
			}
		}
	}
	if md5 != "" {
		headers.Set("X-Agent-Config-Md5", md5)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, headers, false
	}
	return []byte(body), headers, true
}

func wssConfigPayload(payload any) (string, string, map[string]string) {
	switch value := payload.(type) {
	case string:
		return strings.TrimSpace(value), "", nil
	case map[string]any:
		headers := stringMapFromAny(value["headers"])
		body := firstNonEmpty(wssStringFromAny(value["body"]), wssStringFromAny(value["config"]))
		md5 := firstNonEmpty(wssStringFromAny(value["config_md5"]), wssStringFromAny(value["configMd5"]), wssStringFromAny(value["md5"]))
		if body != "" {
			return body, md5, headers
		}
		values := url.Values{}
		for _, key := range []string{
			"collect_interval", "report_interval", "wss_report_interval", "reset_day", "schema_version", "interface", "connection_mode", "ping_mode",
			"custom_ct", "custom_cu", "custom_cm", "custom_bd", "node_1", "node_2", "node_3", "node_4",
			"rx_correction", "tx_correction", "update",
		} {
			if raw, ok := value[key]; ok {
				values.Set(key, wssStringFromAny(raw))
			}
		}
		return values.Encode(), md5, headers
	default:
		return "", "", nil
	}
}

func stringMapFromAny(raw any) map[string]string {
	values, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for key, value := range values {
		if s := wssStringFromAny(value); s != "" {
			out[key] = s
		}
	}
	return out
}

func wssStringFromAny(v any) string {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	case float64:
		asInt := int64(value)
		if value == float64(asInt) {
			return strconv.FormatInt(asInt, 10)
		}
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		if value {
			return "1"
		}
		return "0"
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	default:
		return ""
	}
}

func (r *reportTransport) connected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn != nil && time.Now().After(r.pauseUntil)
}

func (r *reportTransport) url() string {
	if r == nil {
		return ""
	}
	return r.wsURL
}

func (r *reportTransport) send(body []byte) bool {
	conn := r.currentConn()
	if conn == nil {
		return false
	}
	if err := conn.WriteText(body, wssWriteTimeout); err != nil {
		r.clearConn(conn)
		_ = conn.Close()
		return false
	}
	return true
}

func (r *reportTransport) postFallbackAllowed() bool {
	delay, _ := r.pauseDelay()
	return delay <= 0
}

func (r *reportTransport) logPostFallbackDelayed() {
	r.mu.Lock()
	delay := time.Until(r.pauseUntil)
	reason := r.pauseReason
	now := time.Now()
	if delay <= 0 || now.Sub(r.lastPostDelayLog) < 30*time.Second {
		r.mu.Unlock()
		return
	}
	r.lastPostDelayLog = now
	r.mu.Unlock()
	r.agent.log.info("POST fallback delayed reason=%s remaining=%s", reason, delay.Round(time.Second))
}

func (r *reportTransport) delayProtocol(reason string) {
	if reason == "" {
		reason = "protocol_error"
	}
	r.mu.Lock()
	r.pauseUntil = time.Now().Add(wssProtocolRetryDelay)
	r.pauseReason = reason
	conn := r.conn
	r.conn = nil
	r.lastPostDelayLog = time.Now()
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	r.agent.log.info("WSS retry delayed reason=%s delay=%s", reason, wssProtocolRetryDelay)
	r.agent.log.info("POST fallback delayed reason=%s delay=%s", reason, wssProtocolRetryDelay)
}

func (r *reportTransport) currentConn() *webSocketConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Now().Before(r.pauseUntil) {
		return nil
	}
	return r.conn
}

func (r *reportTransport) setConnIfActive(ctx context.Context, conn *webSocketConn) bool {
	if r == nil || conn == nil || r.agent == nil || isContextDone(ctx) || !r.agent.usesWSS() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	r.conn = conn
	return true
}

func (r *reportTransport) clearConn(conn *webSocketConn) {
	cleared := false
	r.mu.Lock()
	if r.conn == conn {
		r.conn = nil
		cleared = true
	}
	r.mu.Unlock()
	if cleared && r.agent != nil {
		r.agent.wakeTick()
	}
}

func (r *reportTransport) pauseDelay() (time.Duration, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delay := time.Until(r.pauseUntil)
	if delay <= 0 {
		return 0, ""
	}
	return delay, r.pauseReason
}

func (r *reportTransport) waitProtocolPause(ctx context.Context) bool {
	for {
		delay, _ := r.pauseDelay()
		if delay <= 0 {
			return true
		}
		if !r.sleep(ctx, delay) {
			return false
		}
	}
}

func (r *reportTransport) waitNetworkRetry(ctx context.Context, err error, delay time.Duration) bool {
	if delay < wssNetworkMinRetry {
		delay = wssNetworkMinRetry
	}
	r.agent.log.info("WSS retry delayed reason=%v delay=%s", err, delay)
	return r.sleep(ctx, delay)
}

func (r *reportTransport) handleConnectionError(ctx context.Context, err error, backoff *time.Duration) bool {
	if err == nil {
		return !isContextDone(ctx)
	}
	if isContextDone(ctx) {
		return false
	}
	var closeErr *wsCloseError
	if errors.As(err, &closeErr) && closeErr.Code == 1013 && isWSSScheduleInactiveReason(closeErr.Reason) {
		r.agent.log.info("WSS unavailable close reason=%s", closeErr.Reason)
		r.agent.disableWSSRuntime(closeErr.Reason)
		return false
	}
	if r.isStopped() || !r.agent.usesWSS() {
		return false
	}
	var delayErr *wsProtocolDelayError
	if errors.As(err, &delayErr) {
		r.delayProtocol(delayErr.reason)
		*backoff = wssNetworkMinRetry
		return true
	}
	if errors.As(err, &closeErr) && closeErr.Code == 1008 {
		reason := fmt.Sprintf("WSS close code=1008 reason=%s", closeErr.Reason)
		r.agent.log.info("WSS error %s", reason)
		r.delayProtocol(reason)
		*backoff = wssNetworkMinRetry
		return true
	}
	delay := *backoff
	if !r.waitNetworkRetry(ctx, err, delay) {
		return false
	}
	*backoff = nextWSSBackoff(delay)
	return true
}

func (r *reportTransport) calibrateHandshakeDate(headers http.Header, started, received time.Time) {
	if dateTime, ok := responseDateTime(headers); ok {
		snapshot, updated := r.agent.clock.updateDate(dateTime, received.Sub(started), received)
		if updated {
			r.agent.log.debugf("Date header time calibrated offset_ms=%d round_trip_ms=%d",
				valueOrZero(snapshot.OffsetMS), valueOrZero(snapshot.RoundTripMS))
		} else {
			r.agent.log.debugf("Date header time calibration skipped offset_ms=%d threshold_ms=%d",
				valueOrZero(snapshot.OffsetMS), int64(dateCalibrationThreshold/time.Millisecond))
		}
	}
}

func isAuthConfigHTTPStatus(statusCode int) bool {
	return statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden || statusCode == http.StatusNotFound
}

func isWSSScheduleInactiveReason(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	return reason == agentWSSScheduleInactive || reason == agentWSSScheduleEmpty || reason == agentWSSScheduleDisabled
}

func wssScheduleInactiveFromHandshake(err *wsHandshakeError) (string, bool) {
	if err == nil || err.StatusCode != http.StatusConflict {
		return "", false
	}
	if reason, ok := wssScheduleInactiveFromHeaders(err.Headers); ok {
		return reason, true
	}
	body := strings.TrimSpace(err.Body)
	if isWSSScheduleInactiveReason(body) {
		return strings.ToLower(body), true
	}
	var payload wsScheduleInactivePayload
	if json.Unmarshal([]byte(body), &payload) != nil {
		return "", false
	}
	reason := firstNonEmpty(payload.Text, payload.Error)
	if !isWSSScheduleInactiveReason(reason) {
		return "", false
	}
	return reason, true
}

func wssScheduleInactiveFromHeaders(headers http.Header) (string, bool) {
	if headers == nil {
		return "", false
	}
	mode := strings.ToLower(strings.TrimSpace(headers.Get(agentWSSModeHeader)))
	if mode != "" && mode != agentWSSModeInactive && mode != agentWSSModeDisabled {
		return "", false
	}
	reason := strings.ToLower(strings.TrimSpace(headers.Get(agentWSSReasonHeader)))
	if !isWSSScheduleInactiveReason(reason) {
		return "", false
	}
	return reason, true
}

func wssScheduleInactiveFromFrame(frame wsServerFrame) (string, bool) {
	if frame.Code != http.StatusConflict {
		return "", false
	}
	reason := firstNonEmpty(frame.Text, frame.Error)
	if !isWSSScheduleInactiveReason(reason) {
		return "", false
	}
	return reason, true
}

func nextWSSBackoff(current time.Duration) time.Duration {
	if current < wssNetworkMinRetry {
		return wssNetworkMinRetry
	}
	next := current * 2
	if next > wssNetworkMaxRetry {
		return wssNetworkMaxRetry
	}
	return next
}

func durationGCD(a, b time.Duration) time.Duration {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *reportTransport) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	if r == nil {
		return sleepContext(ctx, d)
	}
	r.mu.Lock()
	if r.wake == nil {
		r.wake = make(chan struct{}, 1)
	}
	wake := r.wake
	r.mu.Unlock()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}

func signalReportTransportWake(wake chan struct{}) {
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func isContextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
