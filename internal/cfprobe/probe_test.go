package cfprobe

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSplitProbeTarget(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		wantHost string
		wantPort int
	}{
		{name: "host default", target: "example.com", wantHost: "example.com", wantPort: defaultMetricsTCPPort},
		{name: "host port", target: "example.com:8443", wantHost: "example.com", wantPort: 8443},
		{name: "ipv6 bracket", target: "[2001:db8::1]:443", wantHost: "2001:db8::1", wantPort: 443},
		{name: "ipv6 default", target: "2001:db8::1", wantHost: "2001:db8::1", wantPort: defaultMetricsTCPPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, err := splitProbeTarget(tt.target, defaultMetricsTCPPort)
			if err != nil {
				t.Fatalf("splitProbeTarget returned error: %v", err)
			}
			if host != tt.wantHost || port != tt.wantPort {
				t.Fatalf("got %s:%d, want %s:%d", host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func resetDNSCacheForTest(t *testing.T) {
	t.Helper()
	oldLookup := lookupIP
	dnsCacheMu.Lock()
	dnsCache = map[string]dnsCacheEntry{}
	dnsCacheMu.Unlock()
	t.Cleanup(func() {
		lookupIP = oldLookup
		dnsCacheMu.Lock()
		dnsCache = map[string]dnsCacheEntry{}
		dnsCacheMu.Unlock()
	})
}

func TestResolveFirstIPCachesDNSForThirtyMinutes(t *testing.T) {
	resetDNSCacheForTest(t)
	calls := 0
	lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.1")}}, nil
	}

	first, err := resolveFirstIP(context.Background(), "Example.COM")
	if err != nil {
		t.Fatalf("resolveFirstIP returned error: %v", err)
	}
	second, err := resolveFirstIP(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("resolveFirstIP returned error: %v", err)
	}
	if first != "192.0.2.1" || second != "192.0.2.1" {
		t.Fatalf("got %q and %q, want cached 192.0.2.1", first, second)
	}
	if calls != 1 {
		t.Fatalf("lookup calls = %d, want 1", calls)
	}
	dnsCacheMu.RLock()
	entry := dnsCache["example.com"]
	dnsCacheMu.RUnlock()
	if !entry.expiresAt.After(time.Now().Add(29 * time.Minute)) {
		t.Fatalf("cache expires too soon: %s", entry.expiresAt)
	}
}

func TestResolveFirstIPRefreshesExpiredCache(t *testing.T) {
	resetDNSCacheForTest(t)
	dnsCacheMu.Lock()
	dnsCache["example.com"] = dnsCacheEntry{ip: "192.0.2.1", expiresAt: time.Now().Add(-time.Minute)}
	dnsCacheMu.Unlock()
	calls := 0
	lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.2")}}, nil
	}

	got, err := resolveFirstIP(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("resolveFirstIP returned error: %v", err)
	}
	if got != "192.0.2.2" {
		t.Fatalf("got %q, want refreshed 192.0.2.2", got)
	}
	if calls != 1 {
		t.Fatalf("lookup calls = %d, want 1", calls)
	}
}

func TestResolveFirstIPSkipsDNSForIPLiteral(t *testing.T) {
	resetDNSCacheForTest(t)
	calls := 0
	lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return nil, errors.New("unexpected dns lookup")
	}

	got, err := resolveFirstIP(context.Background(), "192.0.2.10")
	if err != nil {
		t.Fatalf("resolveFirstIP returned error: %v", err)
	}
	if got != "192.0.2.10" {
		t.Fatalf("got %q, want literal IP", got)
	}
	if calls != 0 {
		t.Fatalf("lookup calls = %d, want 0", calls)
	}
}

func TestRetryMeasuredPingAcceptsLowLatency(t *testing.T) {
	calls := 0
	got, err := retryMeasuredPing("tcp", func() (int, error) {
		calls++
		return 42, nil
	})
	if err != nil {
		t.Fatalf("retryMeasuredPing returned error: %v", err)
	}
	if got != 42 || calls != 1 {
		t.Fatalf("got latency=%d calls=%d, want latency=42 calls=1", got, calls)
	}
}

func TestRetryMeasuredPingRejectsSuspiciousTCPRetransmission(t *testing.T) {
	values := []int{1200, 100}
	calls := 0
	got, err := retryMeasuredPing("tcp", func() (int, error) {
		v := values[calls]
		calls++
		return v, nil
	})
	if err == nil || !strings.Contains(err.Error(), "suspicious retransmission") {
		t.Fatalf("error = %v, want suspicious retransmission", err)
	}
	if got != -1 || calls != 2 {
		t.Fatalf("got latency=%d calls=%d, want latency=-1 calls=2", got, calls)
	}
}

func TestRetryMeasuredPingAcceptsRecoveredHTTP(t *testing.T) {
	values := []int{1200, 100}
	calls := 0
	got, err := retryMeasuredPing("http", func() (int, error) {
		v := values[calls]
		calls++
		return v, nil
	})
	if err != nil {
		t.Fatalf("retryMeasuredPing returned error: %v", err)
	}
	if got != 100 || calls != 2 {
		t.Fatalf("got latency=%d calls=%d, want latency=100 calls=2", got, calls)
	}
}

func TestRetryMeasuredPingRejectsPersistentHighLatency(t *testing.T) {
	calls := 0
	got, err := retryMeasuredPing("icmp", func() (int, error) {
		calls++
		return 1200, nil
	})
	if err == nil || !strings.Contains(err.Error(), "latency remains high") {
		t.Fatalf("error = %v, want latency remains high", err)
	}
	if got != -1 || calls != 4 {
		t.Fatalf("got latency=%d calls=%d, want latency=-1 calls=4", got, calls)
	}
}

func TestRetryMeasuredPingStopsOnRetryError(t *testing.T) {
	calls := 0
	got, err := retryMeasuredPing("tcp", func() (int, error) {
		calls++
		if calls == 2 {
			return -1, errors.New("dial failed")
		}
		return 1200, nil
	})
	if err == nil || err.Error() != "dial failed" {
		t.Fatalf("error = %v, want dial failed", err)
	}
	if got != -1 || calls != 2 {
		t.Fatalf("got latency=%d calls=%d, want latency=-1 calls=2", got, calls)
	}
}

func TestBuildProbeResultCalculatesLossFromFailedSamples(t *testing.T) {
	got := buildProbeResult(4, []int{40, 10, 20})
	if !got.OK {
		t.Fatal("expected probe result to be OK")
	}
	if got.RTTMs != 20 {
		t.Fatalf("RTTMs = %d, want 20", got.RTTMs)
	}
	if got.Loss != 25 {
		t.Fatalf("Loss = %d, want 25", got.Loss)
	}
}

func TestBuildProbeResultAllSamplesLost(t *testing.T) {
	got := buildProbeResult(4, nil)
	if got.OK {
		t.Fatal("expected probe result to be failed")
	}
	if got.RTTMs != -1 {
		t.Fatalf("RTTMs = %d, want -1", got.RTTMs)
	}
	if got.Loss != 100 {
		t.Fatalf("Loss = %d, want 100", got.Loss)
	}
}

func TestRollingProbeHistoryAggregatesTwoMinuteWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	history := rollingProbeHistory{}
	results := []ProbeResult{
		{RTTMs: 10, Loss: 0, OK: true},
		{RTTMs: 20, Loss: 0, OK: true},
		{RTTMs: -1, Loss: 100, OK: false},
		{RTTMs: 30, Loss: 0, OK: true},
		{RTTMs: 100, Loss: 0, OK: true},
		{RTTMs: -1, Loss: 100, OK: false},
	}
	for i, result := range results {
		history.add(now.Add(time.Duration(i)*metricsProbeInterval), "example.com", result)
	}

	got := history.snapshot(now.Add(5 * metricsProbeInterval))
	if !got.OK {
		t.Fatal("expected rolling probe result to be OK")
	}
	if got.RTTMs != 25 {
		t.Fatalf("RTTMs = %d, want 25", got.RTTMs)
	}
	if got.Loss != 33 {
		t.Fatalf("Loss = %d, want 33", got.Loss)
	}
}

func TestRollingProbeHistoryKeepsSixSamples(t *testing.T) {
	now := time.Unix(1000, 0)
	history := rollingProbeHistory{}
	history.add(now, "example.com", ProbeResult{RTTMs: -1, Loss: 100, OK: false})
	for i := 1; i <= 6; i++ {
		history.add(now.Add(time.Duration(i)*metricsProbeInterval), "example.com", ProbeResult{RTTMs: i * 10, OK: true})
	}

	got := history.snapshot(now.Add(6 * metricsProbeInterval))
	if got.Loss != 0 {
		t.Fatalf("Loss = %d, want 0", got.Loss)
	}
	if len(history.samples) != metricsProbeWindowSampleCount {
		t.Fatalf("samples = %d, want %d", len(history.samples), metricsProbeWindowSampleCount)
	}
}

func TestRollingProbeHistoryUsesAvailableSamplesBeforeWindowIsFull(t *testing.T) {
	now := time.Unix(1000, 0)
	history := rollingProbeHistory{}
	history.add(now, "example.com", ProbeResult{RTTMs: 40, OK: true})
	history.add(now.Add(metricsProbeInterval), "example.com", ProbeResult{RTTMs: -1, Loss: 100, OK: false})

	got := history.snapshot(now.Add(metricsProbeInterval))
	if !got.OK {
		t.Fatal("expected available successful sample to produce an OK result")
	}
	if got.RTTMs != 40 {
		t.Fatalf("RTTMs = %d, want 40", got.RTTMs)
	}
	if got.Loss != 50 {
		t.Fatalf("Loss = %d, want 50", got.Loss)
	}
}
