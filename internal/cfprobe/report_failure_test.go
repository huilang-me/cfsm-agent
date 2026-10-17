package cfprobe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestAppendSampleKeepsLatestSamples(t *testing.T) {
	agent := &Agent{log: newLogger(false)}
	base := time.Now()
	total := maxBufferedMetricSamples + 25
	for index := 0; index < total; index++ {
		agent.appendSample(base.Add(time.Duration(index)*time.Second), Metrics{CPU: strconv.Itoa(index)})
	}
	if len(agent.samples) != maxBufferedMetricSamples {
		t.Fatalf("samples = %d, want %d", len(agent.samples), maxBufferedMetricSamples)
	}
	first := agent.samples[0].metrics["cpu"]
	last := agent.samples[len(agent.samples)-1].metrics["cpu"]
	if first != strconv.Itoa(total-maxBufferedMetricSamples) {
		t.Fatalf("first sample cpu = %v, want %d", first, total-maxBufferedMetricSamples)
	}
	if last != strconv.Itoa(total-1) {
		t.Fatalf("last sample cpu = %v, want %d", last, total-1)
	}
	if !agent.lastSample.Equal(base.Add(time.Duration(total-1) * time.Second)) {
		t.Fatalf("lastSample = %s", agent.lastSample)
	}
}

func TestReportBackoffForFailures(t *testing.T) {
	tests := map[int]time.Duration{
		0:   0,
		2:   0,
		3:   2 * time.Minute,
		4:   3 * time.Minute,
		5:   5 * time.Minute,
		6:   7 * time.Minute,
		7:   10 * time.Minute,
		100: 10 * time.Minute,
	}
	for failures, want := range tests {
		if got := reportBackoffForFailures(failures); got != want {
			t.Fatalf("reportBackoffForFailures(%d) = %s, want %s", failures, got, want)
		}
	}
}

func TestNoteReportFailurePausesAfterThreshold(t *testing.T) {
	agent := &Agent{log: newLogger(false)}
	agent.noteReportFailure(404)
	agent.noteReportFailure(404)
	if remaining := agent.reportPauseRemaining(time.Now()); remaining > 0 {
		t.Fatalf("paused too early: remaining=%s", remaining)
	}
	agent.noteReportFailure(404)
	remaining := agent.reportPauseRemaining(time.Now())
	if agent.reportFailures != reportFailureThreshold {
		t.Fatalf("failures = %d, want %d", agent.reportFailures, reportFailureThreshold)
	}
	if remaining <= 0 || remaining > 2*time.Minute {
		t.Fatalf("remaining = %s, want 0 < remaining <= 2m", remaining)
	}

	agent.noteReportSuccess()
	remaining = agent.reportPauseRemaining(time.Now())
	if agent.reportFailures != 0 || remaining > 0 {
		t.Fatalf("after success failures = %d remaining = %s, want 0/0", agent.reportFailures, remaining)
	}
}

func TestTickDropsSamplesWhileReportPaused(t *testing.T) {
	agent := &Agent{log: newLogger(false)}
	agent.samples = []metricSample{{at: time.Now()}, {at: time.Now()}}
	agent.reportPausedUntil = time.Now().Add(time.Minute)

	agent.tick()

	// 熔断期间样本直接丢弃，恢复后不会带旧时间戳补发。
	if len(agent.samples) != 0 {
		t.Fatalf("samples = %d, want 0 while paused", len(agent.samples))
	}
	if !agent.lastSample.IsZero() {
		t.Fatalf("lastSample should stay zero while paused, got %s", agent.lastSample)
	}
}

func TestApplyReportResultAlwaysClearsSamples(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name         string
		result       reportSendResult
		wantReportAt bool
		wantPostAt   bool
	}{
		{"wss ok", reportSendResult{ok: true, viaWSS: true}, true, true},
		{"post ok", reportSendResult{ok: true, viaPOST: true}, false, true},
		{"failed", reportSendResult{}, false, false},
	}
	for _, test := range tests {
		agent := &Agent{log: newLogger(false)}
		agent.samples = []metricSample{{at: now}, {at: now}, {at: now}}

		agent.applyReportResult(test.result, now)

		if len(agent.samples) != 0 {
			t.Fatalf("%s: samples = %d, want 0", test.name, len(agent.samples))
		}
		if agent.lastReport.Equal(now) != test.wantReportAt {
			t.Fatalf("%s: lastReport = %s, wantReportAt %v", test.name, agent.lastReport, test.wantReportAt)
		}
		if agent.lastPost.Equal(now) != test.wantPostAt {
			t.Fatalf("%s: lastPost = %s, wantPostAt %v", test.name, agent.lastPost, test.wantPostAt)
		}
	}
}

func TestHandleConnectionErrorCountsClose1008(t *testing.T) {
	agent := &Agent{log: newLogger(false)}
	transport := &reportTransport{agent: agent}
	backoff := time.Duration(0)

	if !transport.handleConnectionError(context.Background(), &wsCloseError{Code: 1008, Reason: "rejected"}, &backoff) {
		t.Fatal("handleConnectionError should ask caller to retry")
	}
	if agent.reportFailures != 1 {
		t.Fatalf("failures = %d, want 1", agent.reportFailures)
	}
}

func TestAckRealtimeHintParsing(t *testing.T) {
	tests := map[string]bool{
		`{"type":"ack","realtimeHint":true}`: true,
		`{"type":"ack","ts":1}`:              false,
	}
	for payload, want := range tests {
		var frame wsServerFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("unmarshal %s: %v", payload, err)
		}
		if frame.RealtimeHint != want {
			t.Fatalf("%s realtimeHint = %v, want %v", payload, frame.RealtimeHint, want)
		}
	}
}

func TestReportFailureDecayResetsCounter(t *testing.T) {
	agent := &Agent{log: newLogger(false)}
	agent.reportFailures = 5
	agent.reportBackoff = 10 * time.Minute
	agent.lastReportFailureAt = time.Now().Add(-reportFailureDecay - time.Minute)

	agent.noteReportFailure(500)

	if agent.reportFailures != 1 {
		t.Fatalf("failures = %d, want 1 after decay", agent.reportFailures)
	}
	if remaining := agent.reportPauseRemaining(time.Now()); remaining > 0 {
		t.Fatalf("should not pause after decayed single failure, remaining=%s", remaining)
	}
}

func TestSendReportReportsNon2xxAsFailure(t *testing.T) {
	tests := map[int]bool{
		http.StatusOK:                 true,
		http.StatusNotFound:           false,
		http.StatusServiceUnavailable: false,
	}
	for status, wantOK := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		agent := &Agent{
			cfg: Config{
				ServerID:        "sid",
				Secret:          "secret",
				WorkerURL:       server.URL,
				ReportInterval:  60,
				CollectInterval: 2,
			},
			log: newLogger(false),
		}
		agent.samples = []metricSample{{at: time.Now(), metrics: map[string]any{"cpu": "1"}}}

		result := agent.sendReport(agent.configSnapshot(), Metrics{CPU: "1"}, false, true, time.Minute)

		server.Close()
		if result.ok != wantOK {
			t.Fatalf("status %d ok = %v, want %v", status, result.ok, wantOK)
		}
	}
}
