//go:build !windows

package cfprobe

import "runtime"

// nativeArch reports the machine architecture. On non-Windows platforms a
// binary always runs natively, so runtime.GOARCH is exact; Windows provides
// its own implementation (arch_windows.go) because emulation makes
// runtime.GOARCH and PROCESSOR_ARCHITECTURE misleading on ARM64 hosts.
func nativeArch() string {
	return runtime.GOARCH
}
