package cfprobe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestDownloadToFileRejectsIncompleteAndOversizedBodies(t *testing.T) {
	for _, tt := range []struct {
		name      string
		body      string
		chunked   bool
		truncated bool
		wantError bool
	}{
		{name: "exact limit", body: "data"},
		{name: "known oversized body", body: "too large", wantError: true},
		{name: "streamed oversized body", body: "too large", chunked: true, wantError: true},
		{name: "empty body", wantError: true},
		{name: "truncated body", body: "da", truncated: true, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.truncated {
					w.Header().Set("Content-Length", "3")
				} else if !tt.chunked {
					w.Header().Set("Content-Length", strconv.Itoa(len(tt.body)))
				}
				if tt.chunked {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			dest := filepath.Join(t.TempDir(), "update.bin")
			if err := os.WriteFile(dest, []byte("previous binary"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := downloadToFileWithLimit(ctx, server.Client(), server.URL, dest, 4)
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v, want error=%v", err, tt.wantError)
			}
			want := tt.body
			if tt.wantError {
				want = "previous binary"
			}
			data, err := os.ReadFile(dest)
			if err != nil || string(data) != want {
				t.Fatalf("binary=%q error=%v, want %q", data, err, want)
			}
			if _, err := os.Stat(dest + ".download"); !os.IsNotExist(err) {
				t.Fatalf("temporary download left behind: %v", err)
			}
		})
	}
}

func TestNormalizeUpdateDNSServer(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"223.5.5.5", "223.5.5.5:53"},
		{"223.5.5.5:5353", "223.5.5.5:5353"},
		{"2606:4700:4700::1111", "[2606:4700:4700::1111]:53"},
		{"[2606:4700:4700::1111]:53", "[2606:4700:4700::1111]:53"},
		{"dns.example.com", "dns.example.com:53"},
		{" 1.1.1.1 ", "1.1.1.1:53"},
	}
	for _, tt := range tests {
		if got := normalizeUpdateDNSServer(tt.in); got != tt.want {
			t.Fatalf("normalizeUpdateDNSServer(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUpdateResolverServersEnvOverride(t *testing.T) {
	t.Setenv(updateDNSServerEnv, "192.0.2.1")
	servers := updateResolverServers()
	if servers[0] != "192.0.2.1:53" {
		t.Fatalf("servers[0] = %q, want %q", servers[0], "192.0.2.1:53")
	}
	if len(servers) != len(updateDNSServers)+1 {
		t.Fatalf("servers len = %d, want %d", len(servers), len(updateDNSServers)+1)
	}
	t.Setenv(updateDNSServerEnv, "")
	if got := updateResolverServers(); len(got) != len(updateDNSServers) {
		t.Fatalf("servers len = %d, want %d", len(got), len(updateDNSServers))
	}
}

func TestUsePublicDNSResolver(t *testing.T) {
	t.Setenv(updateDNSServerEnv, "")
	if usePublicDNSResolver(Config{}) {
		t.Fatal("usePublicDNSResolver() = true, want false without ghproxy")
	}
	if !usePublicDNSResolver(Config{UpdateProxy: "https://gh-proxy.example.com"}) {
		t.Fatal("usePublicDNSResolver() = false, want true with ghproxy")
	}
	t.Setenv(updateDNSServerEnv, "223.5.5.5")
	if !usePublicDNSResolver(Config{}) {
		t.Fatal("usePublicDNSResolver() = false, want true with env override")
	}
}

func TestDownloadToFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("echo ok"))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dest := filepath.Join(t.TempDir(), "cf-probe-update.bin")
	if err := downloadToFile(ctx, server.Client(), server.URL+"/asset", dest); err != nil {
		t.Fatalf("downloadToFile() error = %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "echo ok" {
		t.Fatalf("content = %q, want %q", data, "echo ok")
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 755", info.Mode().Perm())
	}
}

func TestDownloadToFileHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dest := filepath.Join(t.TempDir(), "cf-probe-update.bin")
	if err := downloadToFile(ctx, server.Client(), server.URL, dest); err == nil {
		t.Fatal("downloadToFile() expected error for http 404")
	}
}
