package cfprobe

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractIPFromBody(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		wantV4 bool
		want   string
	}{
		{name: "trace ipv4", body: "fl=123f45\nh=cloudflare.com\nip=203.0.113.10\nts=1700000000.1\n", wantV4: true, want: "203.0.113.10"},
		{name: "trace ipv6", body: "fl=123f45\nh=cloudflare.com\nip=2001:db8::1\nts=1700000000.1\n", wantV4: false, want: "2001:db8::1"},
		{name: "trace ipv4 wants v6", body: "ip=203.0.113.10\n", wantV4: false, want: ""},
		{name: "trace ipv6 wants v4", body: "ip=2001:db8::1\n", wantV4: true, want: ""},
		{name: "json ipv4", body: `{"ip":"198.51.100.7"}`, wantV4: true, want: "198.51.100.7"},
		{name: "plain ipv4", body: "192.0.2.1\n", wantV4: true, want: "192.0.2.1"},
		{name: "json ipv6", body: `{"ip":"2001:db8:85a3::8a2e:370:7334"}`, wantV4: false, want: "2001:db8:85a3::8a2e:370:7334"},
		{name: "plain ipv6", body: "2001:db8::ff00:42:8329", wantV4: false, want: "2001:db8::ff00:42:8329"},
		{name: "invalid ipv4 octets", body: "999.999.999.999", wantV4: true, want: ""},
		{name: "empty body", body: "", wantV4: true, want: ""},
		{name: "geo ipv4 with extra text", body: `{"country":"CN","ip":"203.0.113.99","asn":4134}`, wantV4: true, want: "203.0.113.99"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractIPFromBody(tt.body, tt.wantV4); got != tt.want {
				t.Fatalf("extractIPFromBody(%q, %v) = %q, want %q", tt.body, tt.wantV4, got, tt.want)
			}
		})
	}
}

func TestWorkerTraceURL(t *testing.T) {
	tests := []struct {
		name      string
		workerURL string
		want      string
	}{
		{name: "https with path", workerURL: "https://example.com/update", want: "https://example.com/cdn-cgi/trace"},
		{name: "http with port and query", workerURL: "http://example.com:8080/update?token=x", want: "http://example.com:8080/cdn-cgi/trace"},
		{name: "bare origin", workerURL: "https://example.com", want: "https://example.com/cdn-cgi/trace"},
		{name: "surrounding spaces", workerURL: "  https://example.com/update  ", want: "https://example.com/cdn-cgi/trace"},
		{name: "user info dropped", workerURL: "https://user:pass@example.com/update", want: "https://example.com/cdn-cgi/trace"},
		{name: "empty", workerURL: "", want: ""},
		{name: "no host", workerURL: "https:///update", want: ""},
		{name: "unsupported scheme", workerURL: "wss://example.com/update", want: ""},
		{name: "garbage", workerURL: "://bad", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workerTraceURL(tt.workerURL); got != tt.want {
				t.Fatalf("workerTraceURL(%q) = %q, want %q", tt.workerURL, got, tt.want)
			}
		})
	}
}

func TestLookupViaWorker(t *testing.T) {
	var gotUA, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotPath = r.Header.Get("User-Agent"), r.URL.Path
		if r.URL.Path != workerTracePath {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "fl=123f45\nh=example.com\nip=203.0.113.10\nts=1700000000.1\n")
	}))
	defer server.Close()

	ip := lookupViaWorker("tcp4", server.URL+workerTracePath, logger{})
	if ip != "203.0.113.10" {
		t.Fatalf("lookupViaWorker() = %q, want 203.0.113.10", ip)
	}
	if gotUA != ipLookupUserAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUA, ipLookupUserAgent)
	}
	if gotPath != workerTracePath {
		t.Fatalf("path = %q, want %q", gotPath, workerTracePath)
	}
}

func TestLookupViaWorkerFallsBackOnError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	if ip := lookupViaWorker("tcp4", server.URL+workerTracePath, logger{}); ip != "" {
		t.Fatalf("lookupViaWorker() on 404 = %q, want empty", ip)
	}
}

func TestLookupPublicIPEndpointsConfigured(t *testing.T) {
	if len(ipv4LookupEndpoints) == 0 {
		t.Fatal("ipv4LookupEndpoints is empty")
	}
	if len(ipv6LookupEndpoints) == 0 {
		t.Fatal("ipv6LookupEndpoints is empty")
	}
}
