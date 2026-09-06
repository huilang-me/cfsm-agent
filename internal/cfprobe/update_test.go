package cfprobe

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseUpdateVersionAndCompare(t *testing.T) {
	tests := []struct {
		current string
		latest  string
		want    int
	}{
		{current: "1.0.0", latest: "v1.0.1", want: 1},
		{current: "v1.2.3", latest: "1.2.3", want: 0},
		{current: "1.2.4", latest: "1.2.3", want: -1},
		{current: "1.2.3-beta.1", latest: "1.2.3", want: 1},
		{current: "1.2.3", latest: "1.2.3-beta.1", want: -1},
		{current: "1.2.3-beta.1", latest: "1.2.3-beta.2", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.current+" to "+tt.latest, func(t *testing.T) {
			current, err := parseUpdateVersion(tt.current)
			if err != nil {
				t.Fatalf("parseUpdateVersion(%q) error = %v", tt.current, err)
			}
			latest, err := parseUpdateVersion(tt.latest)
			if err != nil {
				t.Fatalf("parseUpdateVersion(%q) error = %v", tt.latest, err)
			}
			got := compareUpdateVersion(latest, current)
			if got != tt.want {
				t.Fatalf("compareUpdateVersion(%q, %q) = %d, want %d", tt.latest, tt.current, got, tt.want)
			}
		})
	}
}

func TestExpectedUpdateAssetName(t *testing.T) {
	tests := []struct {
		goos   string
		goarch string
		want   string
	}{
		{goos: "linux", goarch: "amd64", want: "cf-probe-linux-amd64"},
		{goos: "darwin", goarch: "arm64", want: "cf-probe-darwin-arm64"},
		{goos: "windows", goarch: "amd64", want: "cf-probe-windows-amd64.exe"},
	}

	for _, tt := range tests {
		if got := expectedUpdateAssetName(tt.goos, tt.goarch); got != tt.want {
			t.Fatalf("expectedUpdateAssetName(%q, %q) = %q, want %q", tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestSelectLatestStableRelease(t *testing.T) {
	assetName := "cf-probe-linux-amd64"
	current, err := parseUpdateVersion("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	releases := []githubRelease{
		testGitHubRelease("v1.0.1", false, false, base, "cf-probe-linux-arm64"),
		testGitHubRelease("v1.0.2", false, true, base.Add(time.Hour), assetName),
		testGitHubRelease("v1.0.3", true, false, base.Add(2*time.Hour), assetName),
		testGitHubRelease("v1.0.1", false, false, base.Add(3*time.Hour), assetName),
		testGitHubRelease("v0.9.9", false, false, base.Add(4*time.Hour), assetName),
	}

	got, ok := selectLatestStableRelease(releases, assetName, current)
	if !ok {
		t.Fatal("selectLatestStableRelease() found no candidate")
	}
	if got.TagName != "v1.0.1" {
		t.Fatalf("selectLatestStableRelease() tag = %q, want v1.0.1", got.TagName)
	}
}

func TestSelectLatestSnapshotRelease(t *testing.T) {
	assetName := "cf-probe-linux-amd64"
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	releases := []githubRelease{
		testGitHubRelease("v9.9.9", false, false, base.Add(5*time.Hour), assetName),
		testGitHubRelease("Snapshot-2608051400", true, true, base.Add(4*time.Hour), assetName),
		testGitHubRelease("beta-2608051500", true, false, base.Add(6*time.Hour), assetName),
		testGitHubRelease("Snapshot-2608051600", true, false, base.Add(7*time.Hour), "cf-probe-linux-arm64"),
		testGitHubRelease("Snapshot-2608051200", true, false, base, assetName),
		testGitHubRelease("Snapshot-2608051300", true, false, base.Add(time.Hour), assetName),
	}

	got, ok := selectLatestSnapshotRelease(releases, assetName)
	if !ok {
		t.Fatal("selectLatestSnapshotRelease() found no candidate")
	}
	if got.TagName != "Snapshot-2608051300" {
		t.Fatalf("selectLatestSnapshotRelease() tag = %q, want Snapshot-2608051300", got.TagName)
	}
}

func TestUpdateAssetDownloadURL(t *testing.T) {
	got, err := updateAssetDownloadURL("v1.2.3", "cf-probe-linux-amd64", "")
	if err != nil {
		t.Fatalf("updateAssetDownloadURL() error = %v", err)
	}
	want := "https://github.com/huilang-me/cfsm-agent/releases/download/v1.2.3/cf-probe-linux-amd64"
	if got != want {
		t.Fatalf("asset url = %q, want %q", got, want)
	}
}

func TestUpdateAssetDownloadURLAppliesProxy(t *testing.T) {
	got, err := updateAssetDownloadURL("v1.2.3", "cf-probe-linux-amd64", "https://gh-proxy.example.com/")
	if err != nil {
		t.Fatalf("updateAssetDownloadURL() error = %v", err)
	}
	want := "https://gh-proxy.example.com/https://github.com/huilang-me/cfsm-agent/releases/download/v1.2.3/cf-probe-linux-amd64"
	if got != want {
		t.Fatalf("proxied asset url = %q, want %q", got, want)
	}
}

func TestRemoteUpdateFlagIsNoop(t *testing.T) {
	a := Agent{cfg: Config{AutoUpdate: true}, version: "1.0.0"}
	if err := a.applyRemoteConfig([]byte("update=1"), http.Header{}); err != nil {
		t.Fatalf("applyRemoteConfig(update=1) error = %v", err)
	}
}

func TestRemoteConfigMD5UsesHeaderAndPreservesUpdateProxy(t *testing.T) {
	tmp := t.TempDir()
	configFile := tmp + "/config.conf"
	newMD5 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	a := Agent{
		cfg: Config{
			ServerID:       "sid",
			Secret:         "secret",
			WorkerURL:      "https://worker.example.com/report",
			ReportInterval: defaultReportIntervalSec,
			ResetDay:       1,
			AutoUpdate:     true,
			UpdateProxy:    "https://gh-proxy.example.com",
			ConfigMD5:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		paths: Paths{ConfigFile: configFile, TrafficFile: tmp + "/traffic.dat"},
		log:   newLogger(false),
	}
	headers := http.Header{"X-Agent-Config-Md5": []string{newMD5}}
	body := []byte("collect_interval=0&report_interval=60&reset_day=1&schema_version=7&interface=&node_1=&node_2=&node_3=&node_4=&connection_mode=auto&ping_mode=icmp")

	if err := a.applyRemoteConfig(body, headers); err != nil {
		t.Fatalf("applyRemoteConfig() error = %v", err)
	}
	got, err := readConfig(configFile)
	if err != nil {
		t.Fatalf("readConfig() error = %v", err)
	}
	if got.ConfigMD5 != newMD5 {
		t.Fatalf("ConfigMD5 = %q, want %q", got.ConfigMD5, newMD5)
	}
	if got.PingMode != pingModeICMP {
		t.Fatalf("PingMode = %q, want %q", got.PingMode, pingModeICMP)
	}
	if got.UpdateProxy != a.cfg.UpdateProxy {
		t.Fatalf("UpdateProxy = %q, want %q", got.UpdateProxy, a.cfg.UpdateProxy)
	}
	if !got.AutoUpdate {
		t.Fatal("AutoUpdate = false, want true")
	}
}

func TestChecksumForAsset(t *testing.T) {
	assetSum := strings.Repeat("a", sha256.Size*2)
	otherSum := strings.Repeat("b", sha256.Size*2)
	checksums := "not-a-checksum  cf-probe-linux-arm64\n" +
		assetSum + "  dist/cf-probe-linux-amd64\n" +
		otherSum + "  cf-probe-darwin-arm64\n"

	got, ok := checksumForAsset(checksums, "cf-probe-linux-amd64")
	if !ok {
		t.Fatal("checksumForAsset() did not find asset")
	}
	if got != assetSum {
		t.Fatalf("checksumForAsset() = %q, want %q", got, assetSum)
	}
	if _, ok := checksumForAsset(checksums, "missing"); ok {
		t.Fatal("checksumForAsset() found unexpected asset")
	}
}

func TestVerifyFileSHA256(t *testing.T) {
	body := []byte("cfsm-agent-update")
	path := filepath.Join(t.TempDir(), "asset")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	expected := hex.EncodeToString(sum[:])
	if err := verifyFileSHA256(path, expected); err != nil {
		t.Fatalf("verifyFileSHA256() error = %v", err)
	}
	if err := verifyFileSHA256(path, strings.Repeat("0", sha256.Size*2)); err == nil {
		t.Fatal("verifyFileSHA256() succeeded with wrong checksum")
	}
}
func testGitHubRelease(tag string, prerelease, draft bool, publishedAt time.Time, assetNames ...string) githubRelease {
	assets := make([]githubReleaseAsset, 0, len(assetNames))
	for _, name := range assetNames {
		assets = append(assets, githubReleaseAsset{Name: name, Size: 1024})
	}
	return githubRelease{
		TagName:     tag,
		Draft:       draft,
		Prerelease:  prerelease,
		PublishedAt: publishedAt,
		Assets:      assets,
	}
}
