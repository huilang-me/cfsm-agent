//go:build windows

package cfprobe

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	xwindows "golang.org/x/sys/windows"
)

const (
	windowsTaskNamespace = "http://schemas.microsoft.com/windows/2004/02/mit/task"
	windowsTaskAuthorID  = "Author"
)

const windowsResumeEventSubscription = `<QueryList>
  <Query Id="0" Path="System">
    <Select Path="System">*[System[Provider[@Name='Microsoft-Windows-Power-Troubleshooter'] and EventID=1]]</Select>
    <Select Path="System">*[System[Provider[@Name='Microsoft-Windows-Kernel-Power'] and EventID=107]]</Select>
  </Query>
</QueryList>`

func defaultPaths() Paths {
	serviceName := serviceNameDefault
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	installDir := filepath.Join(os.Getenv("ProgramFiles"), "cf-probe")
	if strings.TrimSpace(installDir) == "cf-probe" {
		installDir = `C:\Program Files\cf-probe`
	}
	binary := filepath.Join(installDir, serviceName+".exe")
	configDir := filepath.Join(programData, "cf-probe")
	return Paths{
		ServiceName:    serviceName,
		BinaryFile:     binary,
		ConfigDir:      configDir,
		ConfigFile:     filepath.Join(configDir, "config.conf"),
		TrafficFile:    filepath.Join(configDir, "traffic.dat"),
		OldTrafficFile: filepath.Join(programData, "cf-probe", "traffic.dat"),
		PIDFile:        filepath.Join(programData, "cf-probe", serviceName+".pid"),
		LogFile:        filepath.Join(programData, "cf-probe", serviceName+".log"),
		ServiceFile:    serviceName,
		DebugEnvFile:   filepath.Join(configDir, "debug.env"),
		LaunchdLabel:   "",
		RunUser:        "SYSTEM",
	}
}

func requireInstallPermission(_ Paths) error {
	if !isElevated() {
		return errors.New("Windows 安装需要管理员权限，请以管理员身份重新打开 PowerShell 后重试（右键 PowerShell 选择“以管理员身份运行”）")
	}
	return nil
}

func requireUninstallPermission(_ Paths) error {
	if !isElevated() {
		return errors.New("Windows 卸载需要管理员权限，请以管理员身份重新打开 PowerShell 后重试（右键 PowerShell 选择“以管理员身份运行”）")
	}
	return nil
}

// isElevated reports whether the current process token is elevated (UAC).
// Group membership alone is not enough: under UAC a standard administrator
// runs with a filtered token, so the token elevation flag must be checked.
func isElevated() bool {
	return xwindows.GetCurrentProcessToken().IsElevated()
}

func checkInstallConflicts(_ Paths) error {
	return nil
}

func stopCurrentUserProbeInstances() {
}

func acquireInstanceLock(_ Paths) (func(), error) {
	return func() {}, nil
}

func copySelfTo(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	src, _ = filepath.EvalSymlinks(src)
	dst, _ = filepath.Abs(dst)
	if strings.EqualFold(src, dst) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	backup := dst + ".previous"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	if fileExists(dst) {
		if err := renameWindowsFileWithRetry(dst, backup, 5*time.Second); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := renameWindowsFileWithRetry(tmp, dst, 5*time.Second); err != nil {
		if fileExists(backup) {
			_ = renameWindowsFileWithRetry(backup, dst, 5*time.Second)
		}
		return err
	}
	_ = os.Remove(backup)
	return nil
}

func startDetached(binary string, args []string, logFile, pidFile string) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	cmd := exec.Command(binary, args...)
	log, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644)
	return cmd.Process.Release()
}

func stopDetached(pidFile string) {
	_ = os.Remove(pidFile)
}

func stopWindowsScheduledTask(paths Paths) {
	// Quiet: the task may not exist yet (first install), and schtasks would
	// otherwise print a localized "系统找不到指定的文件" error to stderr.
	_ = runCommandQuiet("schtasks", "/End", "/TN", paths.ServiceName)
	stopWindowsProbeProcesses(paths)
	waitWindowsProbeProcessesStopped(paths, 5*time.Second)
}

func stopWindowsProbeProcesses(paths Paths) {
	_ = runCommandQuiet("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		windowsProbeProcessScript(paths, "ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }"))
}

func waitWindowsProbeProcessesStopped(paths Paths, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		if !windowsProbeProcessesRunning(paths) || time.Now().After(deadline) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func waitWindowsProbeProcessesStarted(paths Paths, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if windowsProbeProcessesRunning(paths) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func windowsProbeProcessesRunning(paths Paths) bool {
	return commandOutput("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		windowsProbeProcessScript(paths, "Select-Object -First 1 -ExpandProperty ProcessId")) != ""
}

func windowsProbeProcessScript(paths Paths, pipeline string) string {
	return strings.Join([]string{
		"$ErrorActionPreference='SilentlyContinue'",
		"$me=$PID",
		"$bin=" + quotePowerShellLiteral(paths.BinaryFile),
		"$wrapper=" + quotePowerShellLiteral(windowsTaskWrapperFile(paths)),
		"$procs=Get-CimInstance Win32_Process | Where-Object { $cmd=[string]$_.CommandLine; $_.ProcessId -ne $me -and $cmd -and ((($cmd.IndexOf($bin, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) -and $cmd.ToLowerInvariant().Contains(' run ')) -or ($cmd.IndexOf($wrapper, [System.StringComparison]::OrdinalIgnoreCase) -ge 0)) }",
		"$procs | " + pipeline,
	}, "; ")
}

func deepUninstall(paths Paths) []string {
	_ = runCommandQuiet("schtasks", "/End", "/TN", paths.ServiceName)
	_ = runCommandQuiet("schtasks", "/Delete", "/TN", paths.ServiceName, "/F")
	removeInstalledFiles(paths)
	delayed := removeWindowsBinary(paths)
	return windowsUninstallResiduals(paths, delayed)
}

func removeWindowsBinary(paths Paths) []string {
	var delayed []string
	if removeWindowsFile(paths.BinaryFile) {
		delayed = append(delayed, paths.BinaryFile)
	}
	if removeWindowsFile(paths.BinaryFile + ".tmp") {
		delayed = append(delayed, paths.BinaryFile+".tmp")
	}
	_ = os.Remove(filepath.Dir(paths.BinaryFile))
	return delayed
}

func removeWindowsFile(path string) bool {
	if path == "" {
		return false
	}
	if err := os.Remove(path); err == nil || os.IsNotExist(err) {
		return false
	}
	return scheduleWindowsFileRemoval(path)
}

func scheduleWindowsFileRemoval(path string) bool {
	dir := filepath.Dir(path)
	script := strings.Join([]string{
		"Start-Sleep -Seconds 2",
		"Remove-Item -LiteralPath " + quotePowerShellLiteral(path) + " -Force -ErrorAction SilentlyContinue",
		"Remove-Item -LiteralPath " + quotePowerShellLiteral(dir) + " -Force -ErrorAction SilentlyContinue",
	}, "; ")
	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	if err := cmd.Start(); err == nil {
		_ = cmd.Process.Release()
		return true
	}
	return false
}

func windowsUninstallResiduals(paths Paths, delayed []string) []string {
	delayedSet := map[string]bool{}
	for _, path := range delayed {
		delayedSet[path] = true
	}
	checks := []string{
		paths.BinaryFile,
		paths.ConfigDir,
		paths.PIDFile,
		paths.LogFile,
		paths.DebugEnvFile,
	}
	var residuals []string
	for _, path := range existingPaths(checks...) {
		if !delayedSet[path] && !delayedPathInDir(delayed, path) {
			residuals = append(residuals, path)
		}
	}
	if runCommandQuiet("schtasks", "/Query", "/TN", paths.ServiceName) == nil {
		residuals = append(residuals, "scheduled-task:"+paths.ServiceName)
	}
	return uniqueStrings(residuals)
}

func delayedPathInDir(delayed []string, dir string) bool {
	if dir == "" {
		return false
	}
	cleanDir := filepath.Clean(dir)
	prefix := cleanDir + string(os.PathSeparator)
	for _, path := range delayed {
		cleanPath := filepath.Clean(path)
		if strings.EqualFold(cleanPath, cleanDir) || strings.HasPrefix(strings.ToLower(cleanPath), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}

func quotePowerShellLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func applyWindowsScheduledUpdate(paths Paths) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	src, _ = filepath.EvalSymlinks(src)
	dst, _ := filepath.Abs(paths.BinaryFile)
	if strings.EqualFold(src, dst) {
		fmt.Println("[INFO] update binary already installed; starting scheduled task")
		return startService(paths, false)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o755); err != nil {
		return err
	}

	backup := filepath.Join(paths.ConfigDir, paths.ServiceName+"-rollback.exe")
	previous := dst + ".previous"
	staged := dst + ".new"
	if err := removeWindowsUpdateScratch(staged, previous); err != nil {
		return err
	}
	if fileExists(dst) {
		if err := copyWindowsFile(dst, backup, 0o755); err != nil {
			return fmt.Errorf("backup installed binary: %w", err)
		}
	}
	if err := copyWindowsFile(src, staged, 0o755); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("stage updated binary: %w", err)
	}
	if runCommandQuiet("schtasks", "/Query", "/TN", paths.ServiceName) != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("Windows scheduled task %s is missing", paths.ServiceName)
	}
	if !fileExists(windowsTaskWrapperFile(paths)) {
		_ = os.Remove(staged)
		return fmt.Errorf("Windows task wrapper is missing: %s", windowsTaskWrapperFile(paths))
	}

	fmt.Println("[INFO] stopping Windows scheduled task for binary swap")
	stopWindowsScheduledTask(paths)
	if fileExists(dst) {
		if err := renameWindowsFileWithRetry(dst, previous, 5*time.Second); err != nil {
			_ = os.Remove(staged)
			_ = startService(paths, false)
			return fmt.Errorf("move current binary aside: %w", err)
		}
	}
	if err := renameWindowsFileWithRetry(staged, dst, 5*time.Second); err != nil {
		rollbackErr := rollbackWindowsScheduledUpdate(paths, dst, previous, backup)
		return joinErrors(fmt.Errorf("install updated binary: %w", err), rollbackErr)
	}

	fmt.Println("[INFO] starting Windows scheduled task after update")
	if err := startService(paths, false); err != nil {
		rollbackErr := rollbackWindowsScheduledUpdate(paths, dst, previous, backup)
		return joinErrors(fmt.Errorf("start updated scheduled task: %w", err), rollbackErr)
	}
	if !waitWindowsProbeProcessesStarted(paths, 10*time.Second) {
		rollbackErr := rollbackWindowsScheduledUpdate(paths, dst, previous, backup)
		return joinErrors(errors.New("updated probe did not stay running"), rollbackErr)
	}
	_ = os.Remove(previous)
	fmt.Println("[INFO] Windows scheduled update applied")
	return nil
}

func removeWindowsUpdateScratch(paths ...string) error {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func rollbackWindowsScheduledUpdate(paths Paths, dst, previous, backup string) error {
	fmt.Println("[WARN] rolling back Windows scheduled update")
	stopWindowsScheduledTask(paths)
	_ = os.Remove(dst)
	var restoreErr error
	if fileExists(previous) {
		restoreErr = renameWindowsFileWithRetry(previous, dst, 5*time.Second)
	}
	if restoreErr != nil || !fileExists(dst) {
		if fileExists(backup) {
			restoreErr = copyWindowsFile(backup, dst, 0o755)
		}
	}
	if restoreErr != nil {
		return fmt.Errorf("rollback restore failed: %w", restoreErr)
	}
	if !fileExists(dst) {
		return errors.New("rollback restore failed: no backup binary available")
	}
	if err := startService(paths, false); err != nil {
		return fmt.Errorf("rollback start failed: %w", err)
	}
	if !waitWindowsProbeProcessesStarted(paths, 10*time.Second) {
		return errors.New("rollback start failed: probe did not stay running")
	}
	fmt.Println("[INFO] Windows scheduled update rolled back")
	return nil
}

func renameWindowsFileWithRetry(src, dst string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := os.Rename(src, dst); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func copyWindowsFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err = out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func joinErrors(primary, secondary error) error {
	if secondary == nil {
		return primary
	}
	if primary == nil {
		return secondary
	}
	return fmt.Errorf("%v; %w", primary, secondary)
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runCommandQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func quoteShell(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func windowsServiceArgs(paths Paths, debug bool) []string {
	return []string{"cmd.exe", "/D", "/C", quoteWindowsCmdArg(windowsTaskWrapperFile(paths))}
}

type windowsTaskDefinition struct {
	XMLName          xml.Name                    `xml:"Task"`
	XMLNS            string                      `xml:"xmlns,attr"`
	Version          string                      `xml:"version,attr"`
	RegistrationInfo windowsTaskRegistrationInfo `xml:"RegistrationInfo"`
	Triggers         windowsTaskTriggers         `xml:"Triggers"`
	Principals       windowsTaskPrincipals       `xml:"Principals"`
	Settings         windowsTaskSettings         `xml:"Settings"`
	Actions          windowsTaskActions          `xml:"Actions"`
}

type windowsTaskRegistrationInfo struct {
	Description string `xml:"Description"`
}

type windowsTaskTriggers struct {
	BootTrigger  windowsTaskEnabledTrigger `xml:"BootTrigger"`
	LogonTrigger windowsTaskEnabledTrigger `xml:"LogonTrigger"`
	EventTrigger windowsTaskEventTrigger   `xml:"EventTrigger"`
}

type windowsTaskEnabledTrigger struct {
	Enabled bool `xml:"Enabled"`
}

type windowsTaskEventTrigger struct {
	Enabled      bool   `xml:"Enabled"`
	Subscription string `xml:"Subscription"`
	Delay        string `xml:"Delay"`
}

type windowsTaskPrincipals struct {
	Principal windowsTaskPrincipal `xml:"Principal"`
}

type windowsTaskPrincipal struct {
	ID       string `xml:"id,attr"`
	UserID   string `xml:"UserId"`
	RunLevel string `xml:"RunLevel"`
}

type windowsTaskSettings struct {
	MultipleInstancesPolicy    string                  `xml:"MultipleInstancesPolicy"`
	DisallowStartIfOnBatteries bool                    `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     bool                    `xml:"StopIfGoingOnBatteries"`
	AllowHardTerminate         bool                    `xml:"AllowHardTerminate"`
	StartWhenAvailable         bool                    `xml:"StartWhenAvailable"`
	RunOnlyIfNetworkAvailable  bool                    `xml:"RunOnlyIfNetworkAvailable"`
	IdleSettings               windowsTaskIdleSettings `xml:"IdleSettings"`
	AllowStartOnDemand         bool                    `xml:"AllowStartOnDemand"`
	Enabled                    bool                    `xml:"Enabled"`
	Hidden                     bool                    `xml:"Hidden"`
	RunOnlyIfIdle              bool                    `xml:"RunOnlyIfIdle"`
	WakeToRun                  bool                    `xml:"WakeToRun"`
	ExecutionTimeLimit         string                  `xml:"ExecutionTimeLimit"`
	Priority                   int                     `xml:"Priority"`
	RestartOnFailure           windowsTaskRestart      `xml:"RestartOnFailure"`
}

type windowsTaskIdleSettings struct {
	StopOnIdleEnd bool `xml:"StopOnIdleEnd"`
	RestartOnIdle bool `xml:"RestartOnIdle"`
}

type windowsTaskRestart struct {
	Interval string `xml:"Interval"`
	Count    int    `xml:"Count"`
}

type windowsTaskActions struct {
	Context string          `xml:"Context,attr"`
	Exec    windowsTaskExec `xml:"Exec"`
}

type windowsTaskExec struct {
	Command   string `xml:"Command"`
	Arguments string `xml:"Arguments"`
}

func windowsScheduledTaskXML(paths Paths, debug bool) (string, error) {
	command, arguments := windowsScheduledTaskAction(paths, debug)
	task := windowsTaskDefinition{
		XMLNS:   windowsTaskNamespace,
		Version: "1.4",
		RegistrationInfo: windowsTaskRegistrationInfo{
			Description: "CF Server Monitor Probe Agent",
		},
		Triggers: windowsTaskTriggers{
			BootTrigger:  windowsTaskEnabledTrigger{Enabled: true},
			LogonTrigger: windowsTaskEnabledTrigger{Enabled: true},
			EventTrigger: windowsTaskEventTrigger{
				Enabled:      true,
				Subscription: windowsResumeEventSubscription,
				Delay:        "PT10S",
			},
		},
		Principals: windowsTaskPrincipals{
			Principal: windowsTaskPrincipal{
				ID:       windowsTaskAuthorID,
				UserID:   "S-1-5-18",
				RunLevel: "HighestAvailable",
			},
		},
		Settings: windowsTaskSettings{
			MultipleInstancesPolicy:    "IgnoreNew",
			DisallowStartIfOnBatteries: false,
			StopIfGoingOnBatteries:     false,
			AllowHardTerminate:         true,
			StartWhenAvailable:         true,
			RunOnlyIfNetworkAvailable:  false,
			IdleSettings: windowsTaskIdleSettings{
				StopOnIdleEnd: false,
				RestartOnIdle: false,
			},
			AllowStartOnDemand: true,
			Enabled:            true,
			Hidden:             false,
			RunOnlyIfIdle:      false,
			WakeToRun:          false,
			ExecutionTimeLimit: "PT0S",
			Priority:           7,
			RestartOnFailure: windowsTaskRestart{
				Interval: "PT1M",
				Count:    3,
			},
		},
		Actions: windowsTaskActions{
			Context: windowsTaskAuthorID,
			Exec: windowsTaskExec{
				Command:   command,
				Arguments: arguments,
			},
		},
	}
	data, err := xml.MarshalIndent(task, "", "  ")
	if err != nil {
		return "", err
	}
	return "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\r\n" + string(data) + "\r\n", nil
}

func windowsScheduledTaskAction(paths Paths, debug bool) (string, string) {
	args := windowsServiceArgs(paths, debug)
	if len(args) == 0 {
		return "", ""
	}
	return args[0], strings.Join(args[1:], " ")
}

func writeWindowsScheduledTaskXML(paths Paths, debug bool) (string, func(), error) {
	content, err := windowsScheduledTaskXML(paths, debug)
	if err != nil {
		return "", func() {}, err
	}
	tmp, err := os.CreateTemp("", paths.ServiceName+"-task-*.xml")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		_ = os.Remove(tmp.Name())
	}
	if _, err := tmp.Write(utf16LEWithBOM(content)); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return tmp.Name(), cleanup, nil
}

func utf16LEWithBOM(s string) []byte {
	encoded := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+len(encoded)*2)
	out[0] = 0xff
	out[1] = 0xfe
	for _, r := range encoded {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

func writeWindowsTaskWrapper(paths Paths, debug bool) error {
	debugArg := "-debug=0"
	if debug {
		debugArg = "-debug=1"
	}
	content := strings.Join([]string{
		"@echo off",
		"setlocal",
		"if not exist " + quoteWindowsCmdArg(paths.ConfigDir) + " mkdir " + quoteWindowsCmdArg(paths.ConfigDir),
		"echo [%DATE% %TIME%] cf-probe task starting >> " + quoteWindowsCmdArg(paths.LogFile),
		quoteWindowsCmdArg(paths.BinaryFile) + " run " + debugArg + " >> " + quoteWindowsCmdArg(paths.LogFile) + " 2>&1",
		"exit /b %ERRORLEVEL%",
		"",
	}, "\r\n")
	return writeFileExecutable(windowsTaskWrapperFile(paths), content, 0o644)
}

func windowsTaskWrapperFile(paths Paths) string {
	return filepath.Join(paths.ConfigDir, paths.ServiceName+".cmd")
}

func quoteWindowsCmdArg(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func ensurePlatformLogFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		if fileExists(path) && isWindowsFileInUse(err) {
			return nil
		}
		return err
	}
	return f.Close()
}

func isWindowsFileInUse(err error) bool {
	return errors.Is(err, xwindows.ERROR_SHARING_VIOLATION) || errors.Is(err, xwindows.ERROR_LOCK_VIOLATION)
}

func chmodExecutable(_ string) error {
	return nil
}

func platformName() string {
	return "Windows"
}

func isOpenWrt() bool {
	return false
}

func isSynology() bool {
	return false
}

func executableForRelease(goos, goarch string) string {
	name := fmt.Sprintf("cf-probe-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

var errWindowsService = errors.New("Windows 计划任务创建失败，请确认以管理员身份运行 PowerShell")

func runtimeGOOS() string {
	return runtime.GOOS
}

func isRootUser() bool {
	return false
}

func currentUID() int {
	return 0
}
