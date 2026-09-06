package cfprobe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func Install(opts InstallOptions, version string) error {
	paths := defaultPaths()
	printBanner(version)
	if err := requireInstallPermission(paths); err != nil {
		return err
	}
	if err := checkInstallConflicts(paths); err != nil {
		return err
	}

	if !paths.UserMode {
		if cleaned, residuals := cleanupLegacyInstall(paths); len(cleaned) > 0 || len(residuals) > 0 {
			if len(cleaned) > 0 {
				fmt.Printf("[INFO] Cleaned legacy shell probe artifacts: %s\n", strings.Join(cleaned, ", "))
			}
			if len(residuals) > 0 {
				for _, item := range residuals {
					fmt.Printf("[WARN] Legacy shell probe residual: %s\n", item)
				}
				return fmt.Errorf("legacy shell probe cleanup incomplete: %s", strings.Join(residuals, ", "))
			}
		}
	}

	existing, existingPath, existingErr := readInstallConfig(paths)
	usingExistingConfig := existingErr == nil
	if usingExistingConfig {
		fmt.Printf("[INFO] Config source: %s\n", existingPath)
		fmt.Printf("[INFO] 检测到已有配置，沿用 %s\n", paths.ConfigFile)
		flagConfig := opts.Config
		opts.Config = existing
		mergeExplicitInstallConfig(&opts.Config, flagConfig, opts.Explicit)
	} else if opts.ServerID == "" || opts.Secret == "" || opts.WorkerURL == "" {
		printUsage(os.Stderr)
		return errors.New("运行所需的 -id/-secret/-url 参数不完整")
	}
	if !usingExistingConfig || opts.ConfigMD5 == "" {
		opts.ConfigMD5 = "none"
	}
	normalizeConfigIntervals(&opts.Config)

	fmt.Printf("[INFO] Platform: %s/%s (%s)\n", runtime.GOOS, runtime.GOARCH, platformName())
	if paths.UserMode {
		fmt.Printf("[INFO] Install mode: user (%s)\n", firstNonEmpty(paths.RunUser, "current"))
	} else {
		fmt.Println("[INFO] Install mode: system")
	}
	fmt.Printf("[INFO] Install binary: %s\n", paths.BinaryFile)
	fmt.Printf("[INFO] Config file: %s\n", paths.ConfigFile)
	printWindowsAutoUpdateWarning(paths, opts)

	stopService(paths)
	stopCurrentUserProbeInstances()
	if err := copySelfTo(paths.BinaryFile); err != nil {
		return fmt.Errorf("安装二进制失败: %w", err)
	}
	if err := chmodExecutable(paths.BinaryFile); err != nil {
		return fmt.Errorf("设置可执行权限失败: %w", err)
	}
	if err := migrateTraffic(paths); err != nil {
		return err
	}
	if err := writeConfig(paths.ConfigFile, opts.Config); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if opts.RXCorrectionGB != "" || opts.TXCorrectionGB != "" {
		if err := applyTrafficCorrection(paths.TrafficFile, readNetBytes(opts.Interface), opts.Interface, opts.RXCorrectionGB, opts.TXCorrectionGB); err != nil {
			return err
		}
	}
	if err := writeService(paths, opts.Debug); err != nil {
		return err
	}
	if err := prepareManagementLogFile(paths); err != nil {
		return err
	}
	if !opts.NoStart {
		if err := startService(paths, opts.Debug); err != nil {
			return err
		}
	}
	printInstallSummary(paths, opts)
	return nil
}

func mergeExplicitInstallConfig(dst *Config, src Config, explicit map[string]bool) {
	for name := range explicit {
		switch name {
		case "id":
			dst.ServerID = src.ServerID
		case "secret":
			dst.Secret = src.Secret
		case "url":
			dst.WorkerURL = src.WorkerURL
		case "interval":
			dst.ReportInterval = src.ReportInterval
		case "collect_interval":
			dst.CollectInterval = src.CollectInterval
		case "ct":
			dst.CTNode = src.CTNode
		case "cu":
			dst.CUNode = src.CUNode
		case "cm":
			dst.CMNode = src.CMNode
		case "bd":
			dst.BDNode = src.BDNode
		case "node_1":
			dst.Node1 = src.Node1
		case "node_2":
			dst.Node2 = src.Node2
		case "node_3":
			dst.Node3 = src.Node3
		case "node_4":
			dst.Node4 = src.Node4
		case "interface":
			dst.Interface = src.Interface
		case "reset_day":
			dst.ResetDay = src.ResetDay
		case "connection_mode":
			dst.ConnectionMode = src.ConnectionMode
		case "ping_mode":
			dst.PingMode = src.PingMode
		case "auto_update":
			dst.AutoUpdate = src.AutoUpdate
		case "install_ghproxy":
			dst.UpdateProxy = src.UpdateProxy
		}
	}
}

func printWindowsAutoUpdateWarning(paths Paths, opts InstallOptions) {
	if runtime.GOOS != "windows" || !opts.AutoUpdate {
		return
	}
	fmt.Println("[WARN] Windows 自动更新会下载并执行 cf-probe-update.exe，可能被杀毒软件或安全策略拦截。")
	fmt.Printf("[WARN] 如被拦截，请将以下路径加入白名单: %s, %s, %s\n",
		paths.BinaryFile,
		filepath.Join(paths.ConfigDir, "cf-probe-update.exe"),
		windowsTaskWrapperFile(paths))
}

func Uninstall(version string) error {
	paths := defaultPaths()
	printBanner(version)
	if err := requireUninstallPermission(paths); err != nil {
		return err
	}
	fmt.Printf("[INFO] 开始卸载 %s\n", paths.ServiceName)
	residuals := deepUninstall(paths)
	if len(residuals) > 0 {
		for _, item := range residuals {
			fmt.Printf("[WARN] 卸载残留: %s\n", item)
		}
		return fmt.Errorf("卸载未完全清理，仍有 %d 项残留", len(residuals))
	}
	fmt.Println("[INFO] 卸载完成")
	return nil
}

func removeInstalledFiles(paths Paths) {
	stopDetached(paths.PIDFile)
	_ = os.Remove(paths.BinaryFile)
	removeDir(paths.ConfigDir)
	_ = os.Remove(paths.PIDFile)
	_ = os.Remove(paths.LogFile)
	if legacyLog := darwinLegacyUserLogFile(paths); legacyLog != "" {
		_ = os.Remove(legacyLog)
	}
	_ = os.Remove(paths.DebugEnvFile)
}

func darwinLegacyUserLogFile(paths Paths) string {
	if runtime.GOOS != "darwin" || !paths.UserMode {
		return ""
	}
	home := paths.HomeDir
	if home == "" && filepath.Base(paths.ConfigDir) == ".cf-probe" {
		home = filepath.Dir(paths.ConfigDir)
	}
	if home == "" || home == "." || home == "/" {
		return ""
	}
	return filepath.Join(home, "Library", "Logs", firstNonEmpty(paths.ServiceName, serviceNameDefault)+".log")
}

func removeDir(path string) {
	if path == "" || path == "/" || path == "." {
		return
	}
	_ = os.RemoveAll(path)
}

func existingPaths(paths ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Lstat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func printBanner(version string) {
	if version == "" {
		version = legacyAgentVersion
	}
	fmt.Println("===========================================")
	fmt.Println("    CF-Server-Monitor Go Probe")
	fmt.Printf("    Version: %s\n", version)
	fmt.Println("===========================================")
}

func prepareManagementLogFile(paths Paths) error {
	logFile := managementLogFile(paths, serviceSystem(paths))
	if logFile == "" {
		return nil
	}
	return ensurePlatformLogFile(logFile)
}

func managementLogFile(paths Paths, system string) string {
	switch system {
	case "openrc", "launchd", "synology-rc", "background", "windows":
		return paths.LogFile
	case "upstart":
		return filepath.Join("/var/log/upstart", paths.ServiceName+".log")
	default:
		return ""
	}
}

func ensureLogFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func printInstallSummary(paths Paths, opts InstallOptions) {
	fmt.Println("")
	fmt.Println("===========================================")
	fmt.Println("    CF-Server-Monitor Go Probe 安装成功")
	fmt.Println("===========================================")
	fmt.Printf("  Service     : %s\n", paths.ServiceName)
	fmt.Printf("  Binary      : %s\n", paths.BinaryFile)
	fmt.Printf("  Config      : %s\n", paths.ConfigFile)
	fmt.Printf("  Server ID   : %s\n", opts.ServerID)
	fmt.Printf("  Secret      : ********\n")
	fmt.Printf("  Worker URL  : %s\n", opts.WorkerURL)
	fmt.Printf("  Report      : %d秒\n", opts.ReportInterval)
	fmt.Printf("  Collect     : %d秒\n", opts.CollectInterval)
	fmt.Printf("  Connection  : %s\n", opts.ConnectionMode)
	fmt.Printf("  Auto Update : %v\n", opts.AutoUpdate)
	fmt.Printf("  Debug       : %v\n", opts.Debug)
	fmt.Printf("  Interface   : %s\n", firstNonEmpty(opts.Interface, "自动汇总"))
	if opts.ResetDay == 0 {
		fmt.Println("  Reset Day   : 不重置")
	} else {
		fmt.Printf("  Reset Day   : %d号\n", opts.ResetDay)
	}
	printProbeNodes(opts)
	printManagementCommands(paths)
	fmt.Println("===========================================")
}

func initSystem() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	if runtime.GOOS == "darwin" && commandExists("launchctl") {
		return "launchd"
	}
	if isOpenWrt() {
		return "procd"
	}
	if isSynology() {
		return "synology-rc"
	}
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		if commandExists("rc-service") || fileExists("/sbin/openrc-run") {
			return "openrc"
		}
	}
	if commandExists("systemctl") && (fileExists("/run/systemd/system") || commandOutput("ps", "-p", "1", "-o", "comm=") == "systemd") {
		return "systemd"
	}
	if commandExists("rc-service") && fileExists("/etc/init.d") {
		return "openrc"
	}
	if commandExists("initctl") && fileExists("/etc/init") {
		return "upstart"
	}
	return "background"
}

func serviceSystem(paths Paths) string {
	if paths.UserMode {
		if runtime.GOOS == "darwin" {
			return "launchd"
		}
		return "systemd-user"
	}
	return initSystem()
}

func writeService(paths Paths, debug bool) error {
	switch serviceSystem(paths) {
	case "systemd":
		return writeSystemdService(paths, debug)
	case "systemd-user":
		return writeSystemdUserService(paths, debug)
	case "openrc":
		return writeOpenRCService(paths, debug)
	case "procd":
		return writeProcdService(paths, debug)
	case "launchd":
		return writeLaunchdService(paths, debug)
	case "upstart":
		return writeUpstartService(paths, debug)
	case "synology-rc":
		return writeSynologyRCService(paths, debug)
	case "windows":
		return writeWindowsService(paths, debug)
	case "background":
		return nil
	default:
		return nil
	}
}

func startService(paths Paths, debug bool) error {
	switch serviceSystem(paths) {
	case "systemd":
		if err := runCommand("systemctl", "daemon-reload"); err != nil {
			return err
		}
		_ = runCommand("systemctl", "enable", paths.ServiceName+".service")
		return runCommand("systemctl", "restart", paths.ServiceName+".service")
	case "systemd-user":
		if err := runCommand("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		_ = runCommand("systemctl", "--user", "enable", paths.ServiceName+".service")
		return runCommand("systemctl", "--user", "restart", paths.ServiceName+".service")
	case "openrc":
		_ = runCommand("rc-update", "add", paths.ServiceName, "default")
		return runCommand("rc-service", paths.ServiceName, "restart")
	case "procd":
		_ = runCommand("/etc/init.d/"+paths.ServiceName, "enable")
		return runCommand("/etc/init.d/"+paths.ServiceName, "restart")
	case "launchd":
		plist := launchdPlist(paths)
		domain := launchdDomain(paths)
		_ = runCommandQuiet("launchctl", "bootout", domain, plist)
		_ = runCommandQuiet("launchctl", "bootout", domain+"/"+paths.LaunchdLabel)
		return runCommand("launchctl", "bootstrap", domain, plist)
	case "upstart":
		_ = runCommand("initctl", "reload-configuration")
		_ = runCommand("initctl", "stop", paths.ServiceName)
		return runCommand("initctl", "start", paths.ServiceName)
	case "synology-rc":
		if commandExists("systemctl") && fileExists(paths.ServiceFile) {
			_ = runCommandQuiet("systemctl", "stop", paths.ServiceName+".service")
			_ = runCommandQuiet("systemctl", "disable", paths.ServiceName+".service")
		}
		return runCommand(synologyServiceFile(paths), "restart")
	case "windows":
		return runCommand("schtasks", "/Run", "/TN", paths.ServiceName)
	default:
		debugArg := "-debug=0"
		if debug {
			debugArg = "-debug=1"
		}
		return startDetached(paths.BinaryFile, []string{"run", debugArg}, paths.LogFile, paths.PIDFile)
	}
}

func stopService(paths Paths) {
	switch serviceSystem(paths) {
	case "systemd":
		_ = runCommandQuiet("systemctl", "stop", paths.ServiceName+".service")
		_ = runCommandQuiet("systemctl", "disable", paths.ServiceName+".service")
	case "systemd-user":
		_ = runCommandQuiet("systemctl", "--user", "stop", paths.ServiceName+".service")
		_ = runCommandQuiet("systemctl", "--user", "disable", paths.ServiceName+".service")
	case "openrc":
		_ = runCommand("rc-service", paths.ServiceName, "stop")
		_ = runCommand("rc-update", "del", paths.ServiceName, "default")
	case "procd":
		_ = runCommand("/etc/init.d/"+paths.ServiceName, "stop")
		_ = runCommand("/etc/init.d/"+paths.ServiceName, "disable")
	case "launchd":
		stopLaunchdService(paths)
	case "upstart":
		_ = runCommand("initctl", "stop", paths.ServiceName)
	case "synology-rc":
		_ = runCommand(synologyServiceFile(paths), "stop")
		if commandExists("systemctl") && fileExists(paths.ServiceFile) {
			_ = runCommandQuiet("systemctl", "stop", paths.ServiceName+".service")
			_ = runCommandQuiet("systemctl", "disable", paths.ServiceName+".service")
		}
	case "windows":
		stopWindowsScheduledTask(paths)
	default:
		stopDetached(paths.PIDFile)
	}
}

func removeService(paths Paths) {
	switch serviceSystem(paths) {
	case "systemd":
		_ = os.Remove(paths.ServiceFile)
		_ = runCommand("systemctl", "daemon-reload")
		_ = runCommandQuiet("systemctl", "reset-failed", paths.ServiceName)
	case "systemd-user":
		_ = os.Remove(paths.ServiceFile)
		_ = runCommandQuiet("systemctl", "--user", "daemon-reload")
		_ = runCommandQuiet("systemctl", "--user", "reset-failed", paths.ServiceName)
	case "openrc", "procd":
		_ = os.Remove("/etc/init.d/" + paths.ServiceName)
	case "launchd":
		_ = os.Remove(launchdPlist(paths))
	case "upstart":
		_ = os.Remove("/etc/init/" + paths.ServiceName + ".conf")
	case "synology-rc":
		_ = os.Remove(synologyServiceFile(paths))
		_ = os.Remove(paths.ServiceFile)
		if commandExists("systemctl") {
			_ = runCommandQuiet("systemctl", "daemon-reload")
			_ = runCommandQuiet("systemctl", "reset-failed", paths.ServiceName)
			_ = runCommandQuiet("systemctl", "reset-failed", paths.ServiceName+".service")
		}
	case "windows":
		_ = runCommand("schtasks", "/Delete", "/TN", paths.ServiceName, "/F")
	}
}

func stopLaunchdService(paths Paths) {
	plist := launchdPlist(paths)
	domain := launchdDomain(paths)
	_ = runCommandQuiet("launchctl", "bootout", domain, plist)
	_ = runCommandQuiet("launchctl", "bootout", domain+"/"+paths.LaunchdLabel)
	if paths.UserMode {
		_ = runCommandQuiet("launchctl", "remove", paths.LaunchdLabel)
	}
	stopDetached(paths.PIDFile)
}

func writeSystemdService(paths Paths, debug bool) error {
	debugArg := "0"
	if debug {
		debugArg = "1"
	}
	content := fmt.Sprintf(`[Unit]
Description=CF Server Monitor Probe Agent
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run -debug=%s
Restart=always
RestartSec=5
User=root
Group=root
Nice=19
CPUSchedulingPolicy=other
IOSchedulingClass=idle
IOSchedulingPriority=7
StandardOutput=journal
StandardError=journal
SyslogIdentifier=%s

[Install]
WantedBy=multi-user.target
`, paths.BinaryFile, debugArg, paths.ServiceName)
	return writeFileExecutable(paths.ServiceFile, content, 0o644)
}

func writeSystemdUserService(paths Paths, debug bool) error {
	debugArg := "0"
	if debug {
		debugArg = "1"
	}
	content := fmt.Sprintf(`[Unit]
Description=CF Server Monitor Probe Agent
After=default.target

[Service]
Type=simple
ExecStart=%s run -config=%s -debug=%s
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=%s

[Install]
WantedBy=default.target
`, quoteSystemdExecArg(paths.BinaryFile), quoteSystemdExecArg(paths.ConfigFile), debugArg, paths.ServiceName)
	return writeFileExecutable(paths.ServiceFile, content, 0o644)
}

func writeOpenRCService(paths Paths, debug bool) error {
	return writeFileExecutable("/etc/init.d/"+paths.ServiceName, fmt.Sprintf(`#!/sbin/openrc-run

name="CF Server Monitor Probe Agent"
description="CF Server Monitor Probe Agent"
command="%s"
command_args="run -debug=%s"
pidfile="/run/%s.pid"
retry="SIGTERM/30"
supervisor=supervise-daemon
output_log="%s"
error_log="%s"

depend() {
    need net
    after network
}
`, paths.BinaryFile, boolInt(debug), paths.ServiceName, paths.LogFile, paths.LogFile), 0o755)
}

func writeProcdService(paths Paths, debug bool) error {
	return writeFileExecutable("/etc/init.d/"+paths.ServiceName, fmt.Sprintf(`#!/bin/sh /etc/rc.common

START=99
STOP=10
USE_PROCD=1

start_service() {
    procd_open_instance
    procd_set_param command %s run -debug=%s
    procd_set_param respawn
    procd_set_param stdout 1
    procd_set_param stderr 1
    procd_close_instance
}

stop_service() {
    killall %s 2>/dev/null
}
`, paths.BinaryFile, boolInt(debug), filepath.Base(paths.BinaryFile)), 0o755)
}

func writeLaunchdService(paths Paths, debug bool) error {
	plist := launchdPlist(paths)
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	userBlock := ""
	if runtime.GOOS == "darwin" && !paths.UserMode && isRootUser() {
		userBlock = "    <key>UserName</key>\n    <string>root</string>\n"
	}
	configArg := ""
	if paths.UserMode {
		configArg = fmt.Sprintf("        <string>-config=%s</string>\n", paths.ConfigFile)
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>run</string>
%s
        <string>-debug=%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
%s    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, paths.LaunchdLabel, paths.BinaryFile, configArg, boolInt(debug), userBlock, paths.LogFile, paths.LogFile)
	return writeFileExecutable(plist, content, 0o644)
}

func writeUpstartService(paths Paths, debug bool) error {
	return writeFileExecutable("/etc/init/"+paths.ServiceName+".conf", fmt.Sprintf(`description "CF Server Monitor Probe Agent"

start on filesystem or runlevel [2345]
stop on runlevel [!2345]
respawn
respawn limit 10 5

script
    exec %s run -debug=%s
end script
`, paths.BinaryFile, boolInt(debug)), 0o644)
}

func writeSynologyRCService(paths Paths, debug bool) error {
	file := synologyServiceFile(paths)
	return writeFileExecutable(file, fmt.Sprintf(`#!/bin/sh

BIN=%s
PID=%s
LOG=%s
ARGS="run -debug=%s"

start() {
    if [ -f "$PID" ] && kill -0 "$(cat "$PID")" 2>/dev/null; then
        echo "%s already running"
        exit 0
    fi
    nohup "$BIN" $ARGS >> "$LOG" 2>&1 &
    echo $! > "$PID"
}

stop() {
    if [ -f "$PID" ]; then
        kill "$(cat "$PID")" 2>/dev/null || true
        rm -f "$PID"
    fi
}

case "$1" in
    start) start ;;
    stop) stop ;;
    restart) stop; sleep 1; start ;;
    *) echo "usage: $0 {start|stop|restart}"; exit 1 ;;
esac
`, paths.BinaryFile, paths.PIDFile, paths.LogFile, boolInt(debug), paths.ServiceName), 0o755)
}

func writeWindowsService(paths Paths, debug bool) error {
	if err := writeWindowsTaskWrapper(paths, debug); err != nil {
		return err
	}
	_ = runCommand("schtasks", "/Delete", "/TN", paths.ServiceName, "/F")
	taskXML, cleanup, err := writeWindowsScheduledTaskXML(paths, debug)
	if err != nil {
		return fmt.Errorf("%w: %v", errWindowsService, err)
	}
	defer cleanup()
	if err := runCommand("schtasks", "/Create", "/TN", paths.ServiceName, "/XML", taskXML, "/RU", "SYSTEM", "/F"); err != nil {
		return fmt.Errorf("%w: %v", errWindowsService, err)
	}
	return nil
}

func writeFileExecutable(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func launchdPlist(paths Paths) string {
	if paths.UserMode {
		return paths.LaunchdUserFile
	}
	return paths.LaunchdRootFile
}

func launchdDomain(paths Paths) string {
	if paths.UserMode {
		uid := paths.RunUID
		if uid <= 0 {
			uid = currentUID()
		}
		return "gui/" + fmt.Sprint(uid)
	}
	return "system"
}

func synologyServiceFile(paths Paths) string {
	return "/usr/local/etc/rc.d/" + paths.ServiceName + ".sh"
}

func boolInt(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
