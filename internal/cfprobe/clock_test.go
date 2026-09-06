package cfprobe

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestDateCalibrationUsesRTTMidpointAndMonotonicAge(t *testing.T) {
	anchor := time.Now()
	dateTime := anchor.UnixMilli() + 500
	calibration := newDateCalibration(dateTime, 80*time.Millisecond, anchor)
	snapshot := calibration.snapshot(anchor.Add(20 * time.Millisecond))

	if snapshot.AccurateTS == nil || *snapshot.AccurateTS != dateTime+60 {
		t.Fatalf("AccurateTS = %v, want %d", snapshot.AccurateTS, dateTime+60)
	}
	if snapshot.OffsetMS == nil || *snapshot.OffsetMS != 540 {
		t.Fatalf("OffsetMS = %v, want 540", snapshot.OffsetMS)
	}
	if snapshot.Source == nil || *snapshot.Source != "date" {
		t.Fatalf("Source = %v, want date", snapshot.Source)
	}
	if snapshot.RoundTripMS == nil || *snapshot.RoundTripMS != 80 {
		t.Fatalf("RoundTripMS = %v, want 80", snapshot.RoundTripMS)
	}
	if snapshot.SampleAgeMS == nil || *snapshot.SampleAgeMS != 20 {
		t.Fatalf("SampleAgeMS = %v, want 20", snapshot.SampleAgeMS)
	}
}

func TestResponseDateTimeParsesHTTPDateHeader(t *testing.T) {
	headers := http.Header{responseDateHeader: []string{"Thu, 13 Aug 2026 00:23:22 GMT"}}
	got, ok := responseDateTime(headers)
	want := time.Date(2026, time.August, 13, 0, 23, 22, 0, time.UTC).UnixMilli()
	if !ok || got != want {
		t.Fatalf("responseDateTime() = (%d, %v), want (%d, true)", got, ok, want)
	}
}

func TestResponseDateTimeRejectsMissingOrInvalidDateHeader(t *testing.T) {
	if timestamp, ok := responseDateTime(nil); ok {
		t.Fatalf("missing Date header parsed as %d", timestamp)
	}
	headers := http.Header{responseDateHeader: []string{"2026-08-13 00:23:22"}}
	if timestamp, ok := responseDateTime(headers); ok {
		t.Fatalf("invalid Date header parsed as %d", timestamp)
	}
}

func TestCalibratedClockUsesFreshDateAndExpiresOldSample(t *testing.T) {
	now := time.Now()
	clock := calibratedClock{
		calibration: &clockCalibration{
			source:           "date",
			anchor:           now,
			accurateAtAnchor: now.UnixMilli() + 200,
		},
	}
	snapshot := clock.snapshot(now.Add(time.Second))
	if snapshot.Source == nil || *snapshot.Source != "date" {
		t.Fatalf("fresh Source = %v, want date", snapshot.Source)
	}

	clock.calibration.anchor = now.Add(-maxCalibrationAge - time.Second)
	snapshot = clock.snapshot(now.Add(time.Second))
	if snapshot.AccurateTS != nil || snapshot.Source != nil {
		t.Fatalf("expired snapshot = %+v, want uncalibrated", snapshot)
	}
}

func TestCalibratedClockSkipsDateUpdateWithinThreshold(t *testing.T) {
	anchor := time.Unix(1_000, 0)
	clock := calibratedClock{
		calibration: &clockCalibration{
			source:           "date",
			anchor:           anchor,
			accurateAtAnchor: anchor.UnixMilli(),
		},
	}

	if _, updated := clock.updateDate(anchor.Add(15*time.Second).UnixMilli(), 0, anchor); updated {
		t.Fatal("Date update inside threshold was not skipped")
	}
	snapshot := clock.snapshot(anchor)
	if snapshot.AccurateTS == nil || *snapshot.AccurateTS != anchor.UnixMilli() {
		t.Fatalf("AccurateTS = %v, want %d", snapshot.AccurateTS, anchor.UnixMilli())
	}
}

func TestCalibratedClockUpdatesDateBeyondThreshold(t *testing.T) {
	anchor := time.Unix(1_000, 0)
	clock := calibratedClock{
		calibration: &clockCalibration{
			source:           "date",
			anchor:           anchor,
			accurateAtAnchor: anchor.UnixMilli(),
		},
	}

	want := anchor.Add(21 * time.Second).UnixMilli()
	if _, updated := clock.updateDate(want, 0, anchor); !updated {
		t.Fatal("Date update beyond threshold was skipped")
	}
	snapshot := clock.snapshot(anchor)
	if snapshot.AccurateTS == nil || *snapshot.AccurateTS != want {
		t.Fatalf("AccurateTS = %v, want %d", snapshot.AccurateTS, want)
	}
}

func TestCalibratedClockCorrectsSamplesCollectedBeforeCalibration(t *testing.T) {
	anchor := time.Now()
	clock := calibratedClock{
		calibration: &clockCalibration{
			source:           "date",
			anchor:           anchor,
			accurateAtAnchor: 1_000_000,
		},
	}

	timestamp, ok := clock.timestamp(anchor.Add(-3 * time.Second))
	if !ok {
		t.Fatal("retroactive timestamp was not calibrated")
	}
	if timestamp != 997_000 {
		t.Fatalf("timestamp = %d, want 997000", timestamp)
	}

	if timestamp, ok := clock.timestamp(anchor.Add(-maxCalibrationAge - time.Millisecond)); ok {
		t.Fatalf("expired retroactive timestamp was calibrated as %d", timestamp)
	}
}

func TestSamplesAndBootTimeUseCalibratedTimeline(t *testing.T) {
	anchor := time.Now()
	offset := int64(500)
	agent := Agent{
		clock: calibratedClock{
			calibration: &clockCalibration{
				source:           "date",
				anchor:           anchor,
				accurateAtAnchor: anchor.UnixMilli() + offset,
			},
		},
		samples: []metricSample{{
			at:      anchor.Add(-3 * time.Second),
			metrics: map[string]any{"cpu": "1.00"},
		}},
	}

	samples := agent.samplesForReport()
	wantSampleTime := anchor.UnixMilli() + offset - 3_000
	if got := samples[0]["ts"]; got != wantSampleTime {
		t.Fatalf("samples[0].ts = %v, want %d", got, wantSampleTime)
	}

	localBootTime := anchor.UnixMilli() - int64(time.Hour/time.Millisecond)
	metrics := agent.metricsForReport(Metrics{BootTime: strconv.FormatInt(localBootTime, 10)}, anchor)
	wantBootTime := strconv.FormatInt(localBootTime+offset, 10)
	if got := metrics["boot_time"]; got != wantBootTime {
		t.Fatalf("metrics.boot_time = %v, want %s", got, wantBootTime)
	}
}

func TestUncalibratedClockSerializesNullCalibrationFields(t *testing.T) {
	snapshot := (&calibratedClock{}).snapshot(time.UnixMilli(1234))
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	want := `{"local_ts":1234,"accurate_ts":null,"offset_ms":null,"source":null,"round_trip_ms":null,"sample_age_ms":null}`
	if string(encoded) != want {
		t.Fatalf("encoded snapshot = %s, want %s", encoded, want)
	}
}

func TestHandleReportResponseCalibratesDateHeaderWithoutConfig(t *testing.T) {
	agent := Agent{log: newLogger(false)}
	started := time.Now()
	completed := started.Add(80 * time.Millisecond)
	dateTime := time.Date(2026, time.August, 13, 0, 23, 22, 0, time.UTC)
	agent.handleTimedReportResponse(
		http.StatusOK,
		[]byte("OK"),
		http.Header{responseDateHeader: []string{"Thu, 13 Aug 2026 00:23:22 GMT"}},
		started,
		completed,
	)
	snapshot := agent.clock.snapshot(completed)
	if snapshot.Source == nil || *snapshot.Source != "date" {
		t.Fatalf("Source = %v, want date", snapshot.Source)
	}
	want := dateTime.UnixMilli() + 40
	if snapshot.AccurateTS == nil || *snapshot.AccurateTS != want {
		t.Fatalf("AccurateTS = %v, want %d", snapshot.AccurateTS, want)
	}
}

func TestReportDateCalibrationAnchorsAtHeaderReceiveTime(t *testing.T) {
	const slowBodyDelay = 200 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(responseDateHeader, "Thu, 13 Aug 2026 00:23:22 GMT")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(slowBodyDelay)
		_, _ = w.Write([]byte("OK"))
	}))
	defer server.Close()

	agent := Agent{
		cfg: Config{
			ServerID:  "sid",
			Secret:    "secret",
			WorkerURL: server.URL,
		},
		log: newLogger(false),
	}
	agent.report(Metrics{})

	snapshot := agent.clock.snapshot(time.Now())
	if snapshot.SampleAgeMS == nil {
		t.Fatal("SampleAgeMS = nil, want calibrated Date sample")
	}
	minAgeMS := uint64((slowBodyDelay - 50*time.Millisecond) / time.Millisecond)
	if *snapshot.SampleAgeMS < minAgeMS {
		t.Fatalf("SampleAgeMS = %d, want at least %d", *snapshot.SampleAgeMS, minAgeMS)
	}
}

func TestDateHeaderCoexistsWithURLConfigResponse(t *testing.T) {
	tmp := t.TempDir()
	configFile := tmp + "/config.conf"
	agent := Agent{
		cfg: Config{
			ServerID:       "sid",
			Secret:         "secret",
			WorkerURL:      "https://worker.example.com/report",
			ReportInterval: 60,
			ResetDay:       1,
			ConfigMD5:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		paths: Paths{ConfigFile: configFile, TrafficFile: tmp + "/traffic.dat"},
		log:   newLogger(false),
	}
	headers := http.Header{
		responseDateHeader:   []string{"Thu, 13 Aug 2026 00:23:22 GMT"},
		"X-Agent-Config-Md5": []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}
	body := []byte("collect_interval=0&report_interval=60&reset_day=1&schema_version=7&interface=&node_1=&node_2=&node_3=&node_4=&connection_mode=auto&ping_mode=tcp")
	started := time.Now()
	agent.handleTimedReportResponse(http.StatusOK, body, headers, started, started.Add(20*time.Millisecond))

	if agent.cfg.ConfigMD5 != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("ConfigMD5 = %q", agent.cfg.ConfigMD5)
	}
	snapshot := agent.clock.snapshot(started.Add(20 * time.Millisecond))
	if snapshot.Source == nil || *snapshot.Source != "date" {
		t.Fatalf("Source = %v, want date", snapshot.Source)
	}
}
