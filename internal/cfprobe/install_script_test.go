//go:build !windows

package cfprobe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBootstrapStagingExitStatus(t *testing.T) {
	if os.Getenv("HOME") == "" {
		t.Skip("bootstrap home staging requires HOME")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(data), "log \"[WARN] cannot execute bootstrap binary")
	if start < 0 {
		t.Fatal("bootstrap staging block not found")
	}
	for _, tt := range []struct {
		name     string
		codes    string
		wantExit int
		attempts int
	}{
		{"home installer failure", "1", 1, 1},
		{"fallback installer failure", "126 7", 7, 2},
		{"fallback success", "125 126 0", 0, 3},
		{"all staging directories fail", "125", 1, 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Execute the real fallback control flow with a stub that never writes
			// or runs an installer in the host's staging directories.
			script := `set -eu
log() { :; }
die() { exit 1; }
stage_and_run_payload() {
    printf 'attempt\n'
    code="${codes%% *}"
    if [ "$codes" != "$code" ]; then codes="${codes#* }"; fi
    return "$code"
}
os_name=linux
tmp=unused-bootstrap
set -- install
codes='` + tt.codes + "'\n" + string(data[start:])
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "sh", "-c", script).CombinedOutput()
			exitCode := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if exitCode != tt.wantExit || strings.Count(string(out), "attempt\n") != tt.attempts {
				t.Fatalf("exit=%d output=%q; want exit=%d attempts=%d", exitCode, out, tt.wantExit, tt.attempts)
			}
		})
	}
}
