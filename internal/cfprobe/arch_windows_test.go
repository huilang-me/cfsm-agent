//go:build windows

package cfprobe

import "testing"

func TestMapWindowsEnvArch(t *testing.T) {
	tests := map[string]string{
		"ARM64":    "arm64",
		"arm64":    "arm64",
		" ARM64EC": "arm64",
		"AMD64":    "amd64",
		"x86":      "386",
		"":         "",
		"IA64":     "",
	}
	for value, want := range tests {
		if got := mapWindowsEnvArch(value); got != want {
			t.Fatalf("mapWindowsEnvArch(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestNativeArchWithEmulatedEnv(t *testing.T) {
	// PROCESSOR_ARCHITEW6432 is only consulted when the API-based probe
	// fails, so the result must still be a known Go arch name.
	t.Setenv("PROCESSOR_ARCHITEW6432", "ARM64")
	switch got := nativeArch(); got {
	case "arm64", "amd64", "386":
	default:
		t.Fatalf("nativeArch() = %q, want a known Go arch name", got)
	}
}
