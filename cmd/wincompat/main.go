// Command wincompat verifies that a Windows binary still loads on the oldest
// supported target, Windows Server 2012 R2 / Windows 8 (NT 6.2).
//
// Go 1.21 dropped Windows 8 and Server 2012 from its supported platform list,
// but binaries still load there as long as every statically imported kernel32
// function exists on NT 6.2. A missing import is the only failure mode that
// shows up before main() runs ("the procedure entry point ... could not be
// located in the dynamic link library"), so this check fails the build when a
// dependency starts importing a newer API.
package main

import (
	"bytes"
	"debug/pe"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// kernel32APIs is the set of kernel32 functions the probe is allowed to import.
// Every entry exists on Windows 8 / Server 2012. Add a function only after
// confirming it on the oldest target; otherwise the binary stops loading there.
var kernel32APIs = map[string]bool{
	"AddVectoredContinueHandler":  true,
	"AddVectoredExceptionHandler": true,
	"CloseHandle":                 true,
	"CreateEventA":                true,
	"CreateIoCompletionPort":      true,
	"CreateThread":                true,
	"CreateWaitableTimerExW":      true,
	"DuplicateHandle":             true,
	"ExitProcess":                 true,
	"FreeEnvironmentStringsW":     true,
	"GetConsoleMode":              true,
	"GetCurrentThreadId":          true,
	"GetEnvironmentStringsW":      true,
	"GetErrorMode":                true,
	"GetProcAddress":              true,
	"GetProcessAffinityMask":      true,
	"GetQueuedCompletionStatusEx": true,
	"GetStdHandle":                true,
	"GetSystemDirectoryA":         true,
	"GetSystemInfo":               true,
	"GetThreadContext":            true,
	"IsProcessorFeaturePresent":   true,
	"LoadLibraryExW":              true,
	"PostQueuedCompletionStatus":  true,
	"QueryPerformanceCounter":     true,
	"RaiseFailFastException":      true,
	"ResumeThread":                true,
	"RtlLookupFunctionEntry":      true,
	"RtlVirtualUnwind":            true,
	"SetConsoleCtrlHandler":       true,
	"SetErrorMode":                true,
	"SetEvent":                    true,
	"SetProcessPriorityBoost":     true,
	"SetThreadContext":            true,
	"SetUnhandledExceptionFilter": true,
	"SetWaitableTimer":            true,
	"SuspendThread":               true,
	"SwitchToThread":              true,
	"TlsAlloc":                    true,
	"VirtualAlloc":                true,
	"VirtualFree":                 true,
	"VirtualQuery":                true,
	"WaitForMultipleObjects":      true,
	"WaitForSingleObject":         true,
	"WerGetFlags":                 true,
	"WerSetFlags":                 true,
	"WriteConsoleW":               true,
	"WriteFile":                   true,
}

// win8DLLs lists DLLs shipped with Windows 8 / Server 2012. A static import
// from any other DLL makes the loader fail on the oldest target.
var win8DLLs = map[string]bool{
	"advapi32.dll":         true,
	"bcrypt.dll":           true,
	"bcryptprimitives.dll": true,
	"cfgmgr32.dll":         true,
	"combase.dll":          true,
	"crypt32.dll":          true,
	"dnsapi.dll":           true,
	"gdi32.dll":            true,
	"iphlpapi.dll":         true,
	"kernel32.dll":         true,
	"mswsock.dll":          true,
	"netapi32.dll":         true,
	"ntdll.dll":            true,
	"ole32.dll":            true,
	"oleaut32.dll":         true,
	"pdh.dll":              true,
	"powrprof.dll":         true,
	"psapi.dll":            true,
	"rpcrt4.dll":           true,
	"sechost.dll":          true,
	"secur32.dll":          true,
	"setupapi.dll":         true,
	"shell32.dll":          true,
	"user32.dll":           true,
	"userenv.dll":          true,
	"version.dll":          true,
	"winmm.dll":            true,
	"wintrust.dll":         true,
	"ws2_32.dll":           true,
	"wtsapi32.dll":         true,
}

// win10OnlyAPIs are resolved dynamically, so they never appear in the import
// table and cannot fail at load time. They are reported only: Go's runtime and
// the standard library fall back when they are missing, but a dependency that
// calls one directly would break a specific metric on Windows 8.
var win10OnlyAPIs = []string{
	"DiscardVirtualMemory",
	"GetDpiForWindow",
	"GetSystemCpuSetInformation",
	"GetThreadDescription",
	"MapViewOfFile3",
	"SetProcessDpiAwarenessContext",
	"SetThreadDescription",
	"VirtualAlloc2",
}

func main() {
	binary := flag.String("binary", "", "path to the Windows executable to check")
	flag.Parse()
	if *binary == "" {
		fmt.Fprintln(os.Stderr, "usage: wincompat -binary <path-to-exe>")
		os.Exit(2)
	}
	if err := check(*binary); err != nil {
		fmt.Fprintf(os.Stderr, "wincompat: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wincompat: %s: import table is Windows 8 / Server 2012 compatible\n", *binary)
}

// subsystemVersion returns the minimum Windows version the loader requires.
func subsystemVersion(f *pe.File) (major, minor uint16) {
	switch h := f.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		return h.MajorSubsystemVersion, h.MinorSubsystemVersion
	case *pe.OptionalHeader64:
		return h.MajorSubsystemVersion, h.MinorSubsystemVersion
	}
	return 0, 0
}

func check(path string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()

	syms, err := f.ImportedSymbols()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	var problems []string

	// Windows compares the subsystem version with its own and refuses to start
	// the process ("This app can't run on your PC") before the loader ever looks
	// at imports. Go 1.21+ writes 10.0, so this must be lowered by cmd/winsubsys.
	if major, minor := subsystemVersion(f); major > 6 || (major == 6 && minor > 2) {
		problems = append(problems, fmt.Sprintf("PE header requires Windows %d.%d: run cmd/winsubsys to lower it to 6.02", major, minor))
	}

	for _, sym := range syms {
		i := strings.LastIndex(sym, ":")
		if i < 0 {
			continue
		}
		fn, dll := sym[:i], strings.ToLower(sym[i+1:])
		if !win8DLLs[dll] {
			problems = append(problems, fmt.Sprintf("%s!%s: DLL is not part of Windows 8 / Server 2012", dll, fn))
			continue
		}
		if dll == "kernel32.dll" && !kernel32APIs[fn] {
			problems = append(problems, fmt.Sprintf("kernel32!%s: not in the Windows 8 / Server 2012 allowlist", fn))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s no longer loads on Windows 8 / Server 2012:\n  %s\n\nVerify the API on the oldest target before adding it to the allowlist", path, strings.Join(problems, "\n  "))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var found []string
	for _, api := range win10OnlyAPIs {
		if bytes.Contains(data, []byte(api)) {
			found = append(found, api)
		}
	}
	if len(found) > 0 {
		fmt.Printf("wincompat: %s: note: references Windows 10+ APIs resolved at runtime: %s\n", path, strings.Join(found, ", "))
	}
	return nil
}
