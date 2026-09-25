//go:build windows

package cfprobe

import (
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
)

// nativeArch reports the machine architecture the way Go names it.
// runtime.GOARCH and PROCESSOR_ARCHITECTURE are unreliable on Windows:
// an amd64 binary running under Prism emulation on Windows ARM64 sees
// PROCESSOR_ARCHITECTURE="AMD64", which made ARM64 hosts report amd64.
func nativeArch() string {
	if arch := archFromNativeSystemInfo(); arch != "" {
		return arch
	}
	// Set when the process runs under WOW64/Prism emulation; carries the
	// real machine architecture regardless of the emulated process type.
	if env := mapWindowsEnvArch(os.Getenv("PROCESSOR_ARCHITEW6432")); env != "" {
		return env
	}
	return runtime.GOARCH
}

// Machine constants from winnt.h (IMAGE_FILE_MACHINE_*), not exported by
// golang.org/x/sys/windows.
const (
	imageFileMachineUnknown = 0x0
	imageFileMachineI386    = 0x14c
	imageFileMachineAMD64   = 0x8664
	imageFileMachineARM64   = 0xaa64
	imageFileMachineARM64EC = 0xa641
)

// archFromNativeSystemInfo asks the OS for the native (unemulated) system
// architecture via IsWow64Process2; native processes report IMAGE_FILE_MACHINE_UNKNOWN.
func archFromNativeSystemInfo() string {
	handle := windows.CurrentProcess()
	var processMachine, nativeMachine uint16
	err := windows.IsWow64Process2(handle, &processMachine, &nativeMachine)
	if err == nil {
		switch nativeMachine {
		case imageFileMachineUnknown: // Native process: the machine matches the binary.
			return runtime.GOARCH
		case imageFileMachineARM64, imageFileMachineARM64EC:
			return "arm64"
		case imageFileMachineAMD64:
			return "amd64"
		case imageFileMachineI386:
			return "386"
		}
		return ""
	}
	// API missing on old Windows releases (pre Win10): 32-bit processes still
	// expose the real machine via PROCESSOR_ARCHITEW6432, handled by the caller.
	return ""
}

// mapWindowsEnvArch converts PROCESSOR_ARCHITECTURE style values to Go arch
// names, returning "" for values it does not recognize.
func mapWindowsEnvArch(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "ARM64", "ARM64EC":
		return "arm64"
	case "AMD64", "X64":
		return "amd64"
	case "X86":
		return "386"
	}
	return ""
}
