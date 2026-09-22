//go:build !windows

package cfprobe

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func defaultPaths() Paths {
	if !isRootUser() {
		return currentUserPaths()
	}
	return systemDefaultPaths()
}

func systemDefaultPaths() Paths {
	serviceName := serviceNameDefault
	installDir := "/usr/local/bin"
	if isOpenWrt() {
		installDir = "/usr/bin"
	}
	configDir := "/etc/config/cf-probe"
	pidFile := filepath.Join("/run", serviceName+".pid")
	logFile := filepath.Join("/var/log", serviceName+".log")
	launchdUserFile := filepath.Join(userHomeDir(), "Library", "LaunchAgents", "com.cfsm."+serviceName+".plist")
	if runtime.GOOS == "darwin" {
		configDir = "/usr/local/etc/cf-probe"
		launchdUserFile = ""
	}
	return Paths{
		ServiceName:     serviceName,
		BinaryFile:      filepath.Join(installDir, serviceName),
		ConfigDir:       configDir,
		ConfigFile:      filepath.Join(configDir, "config.conf"),
		TrafficFile:     filepath.Join(configDir, "traffic.dat"),
		OldTrafficFile:  "/var/lib/cf-probe/traffic.dat",
		PIDFile:         pidFile,
		LogFile:         logFile,
		ServiceFile:     filepath.Join("/etc/systemd/system", serviceName+".service"),
		DebugEnvFile:    filepath.Join("/run", serviceName+"-debug.env"),
		LaunchdLabel:    "com.cfsm." + serviceName,
		LaunchdUserFile: launchdUserFile,
		LaunchdRootFile: filepath.Join("/Library/LaunchDaemons", "com.cfsm."+serviceName+".plist"),
		UserMode:        false,
		RunUser:         "root",
		RunUID:          0,
		HomeDir:         userHomeDir(),
	}
}

func currentUserPaths() Paths {
	name, home, uid := currentAccount()
	return userPaths(name, uid, home)
}

func userPaths(name string, uid int, home string) Paths {
	serviceName := serviceNameDefault
	if home == "" {
		home = userHomeDir()
	}
	configDir := filepath.Join(home, ".cf-probe")
	return Paths{
		ServiceName:     serviceName,
		BinaryFile:      filepath.Join(configDir, "bin", serviceName),
		ConfigDir:       configDir,
		ConfigFile:      filepath.Join(configDir, "config.conf"),
		TrafficFile:     filepath.Join(configDir, "traffic.dat"),
		OldTrafficFile:  "",
		PIDFile:         filepath.Join(configDir, serviceName+".pid"),
		LogFile:         filepath.Join(configDir, serviceName+".log"),
		ServiceFile:     filepath.Join(home, ".config", "systemd", "user", serviceName+".service"),
		DebugEnvFile:    filepath.Join(configDir, "debug.env"),
		LaunchdLabel:    "com.cfsm." + serviceName,
		LaunchdUserFile: filepath.Join(home, "Library", "LaunchAgents", "com.cfsm."+serviceName+".plist"),
		LaunchdRootFile: filepath.Join("/Library/LaunchDaemons", "com.cfsm."+serviceName+".plist"),
		UserMode:        true,
		RunUser:         name,
		RunUID:          uid,
		HomeDir:         home,
	}
}

func darwinUserPaths(home string) Paths {
	if home == "" {
		home = userHomeDir()
	}
	uid := sudoUserUID(home)
	return userPaths(usernameForUID(uid), uid, home)
}

func sudoUserHomeDir() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	if user := os.Getenv("SUDO_USER"); user != "" && user != "root" {
		if home := darwinAccountHome(user); home != "" {
			return home
		}
		return filepath.Join("/Users", user)
	}
	home := os.Getenv("HOME")
	if home == "" || home == "/var/root" || home == "/" {
		return ""
	}
	return home
}

func darwinAccountHome(user string) string {
	if runtime.GOOS != "darwin" || user == "" {
		return ""
	}
	out := commandOutput("dscl", ".", "-read", "/Users/"+user, "NFSHomeDirectory")
	const prefix = "NFSHomeDirectory:"
	if strings.HasPrefix(out, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(out, prefix))
	}
	return ""
}

func sudoUserUID(home string) int {
	if raw := os.Getenv("SUDO_UID"); raw != "" {
		if uid, err := strconv.Atoi(raw); err == nil && uid > 0 {
			return uid
		}
	}
	if home == "" {
		return -1
	}
	info, err := os.Stat(home)
	if err != nil {
		return -1
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

func deepUninstall(paths Paths) []string {
	if paths.UserMode {
		stopService(paths)
		stopCurrentUserProbeInstances()
		removeService(paths)
		removeInstalledFiles(paths)
		return userUninstallResiduals(paths)
	}
	stopUnixAutostart(paths)
	removeUnixAutostart(paths)
	removeInstalledFiles(paths)
	if runtime.GOOS == "darwin" {
		removeDarwinInstallVariants()
	}
	return unixUninstallResiduals(paths)
}

func userUninstallResiduals(paths Paths) []string {
	residuals := existingPaths(
		paths.BinaryFile,
		paths.ConfigDir,
		paths.PIDFile,
		paths.LogFile,
		paths.DebugEnvFile,
		paths.ServiceFile,
	)
	if runtime.GOOS == "darwin" {
		residuals = append(residuals, existingPaths(paths.LaunchdUserFile, darwinLegacyUserLogFile(paths))...)
		if launchdLabelLoaded(launchdDomain(paths), paths.LaunchdLabel) {
			residuals = append(residuals, "launchd:/"+launchdDomain(paths)+"/"+paths.LaunchdLabel)
		}
	}
	return uniqueStrings(residuals)
}

func stopUnixAutostart(paths Paths) {
	if runtime.GOOS == "darwin" {
		bootoutLaunchd("system", paths.LaunchdRootFile)
		bootoutLaunchdLabel("system", paths.LaunchdLabel)
		stopDetached(paths.PIDFile)
		return
	}
	if commandExists("systemctl") {
		_ = runCommandQuiet("systemctl", "stop", paths.ServiceName+".service")
		_ = runCommandQuiet("systemctl", "disable", paths.ServiceName+".service")
	}
	if commandExists("rc-service") {
		_ = runCommandQuiet("rc-service", paths.ServiceName, "stop")
	}
	if commandExists("rc-update") {
		_ = runCommandQuiet("rc-update", "del", paths.ServiceName, "default")
	}
	initScript := filepath.Join("/etc/init.d", paths.ServiceName)
	if fileExists(initScript) {
		_ = runCommandQuiet(initScript, "stop")
		_ = runCommandQuiet(initScript, "disable")
	}
	if commandExists("initctl") {
		_ = runCommandQuiet("initctl", "stop", paths.ServiceName)
	}
	if fileExists(synologyServiceFile(paths)) {
		_ = runCommandQuiet(synologyServiceFile(paths), "stop")
	}
	stopDetached(paths.PIDFile)
}

func removeUnixAutostart(paths Paths) {
	if runtime.GOOS == "darwin" {
		_ = os.Remove(paths.LaunchdRootFile)
		return
	}
	_ = os.Remove(paths.ServiceFile)
	_ = os.Remove(filepath.Join("/etc/init.d", paths.ServiceName))
	_ = os.Remove(filepath.Join("/etc/init", paths.ServiceName+".conf"))
	_ = os.Remove(synologyServiceFile(paths))
	if commandExists("systemctl") {
		_ = runCommandQuiet("systemctl", "daemon-reload")
		_ = runCommandQuiet("systemctl", "reset-failed", paths.ServiceName)
		_ = runCommandQuiet("systemctl", "reset-failed", paths.ServiceName+".service")
	}
}

func unixUninstallResiduals(paths Paths) []string {
	if runtime.GOOS == "darwin" {
		return darwinUninstallResiduals(paths)
	}
	return existingPaths(
		paths.BinaryFile,
		paths.ConfigDir,
		paths.PIDFile,
		paths.LogFile,
		paths.DebugEnvFile,
		paths.ServiceFile,
		filepath.Join("/etc/init.d", paths.ServiceName),
		filepath.Join("/etc/init", paths.ServiceName+".conf"),
		synologyServiceFile(paths),
	)
}

func removeDarwinInstallVariants() {
	if home := sudoUserHomeDir(); home != "" {
		userPaths := darwinUserPaths(home)
		removeDarwinUserInstall(userPaths, sudoUserUID(home))
	}
}

func removeDarwinUserInstall(paths Paths, uid int) {
	if uid > 0 {
		domain := "gui/" + strconv.Itoa(uid)
		bootoutLaunchd(domain, paths.LaunchdUserFile)
		bootoutLaunchdLabel(domain, paths.LaunchdLabel)
	}
	_ = os.Remove(paths.LaunchdUserFile)
	removeInstalledFiles(paths)
}

func darwinUninstallResiduals(paths Paths) []string {
	var residuals []string
	residuals = append(residuals, existingPaths(
		paths.BinaryFile,
		paths.ConfigDir,
		paths.PIDFile,
		paths.LogFile,
		paths.LaunchdRootFile,
	)...)
	if launchdLabelLoaded("system", paths.LaunchdLabel) {
		residuals = append(residuals, "launchd:"+"/system/"+paths.LaunchdLabel)
	}
	if home := sudoUserHomeDir(); home != "" {
		userPaths := darwinUserPaths(home)
		residuals = append(residuals, existingPaths(
			userPaths.BinaryFile,
			userPaths.ConfigDir,
			userPaths.PIDFile,
			userPaths.LogFile,
			darwinLegacyUserLogFile(userPaths),
			userPaths.LaunchdUserFile,
		)...)
		if uid := sudoUserUID(home); uid > 0 && launchdLabelLoaded("gui/"+strconv.Itoa(uid), userPaths.LaunchdLabel) {
			residuals = append(residuals, "launchd:/gui/"+strconv.Itoa(uid)+"/"+userPaths.LaunchdLabel)
		}
	}
	return uniqueStrings(residuals)
}

func bootoutLaunchd(domain, plist string) {
	if runtime.GOOS != "darwin" || domain == "" || plist == "" || !commandExists("launchctl") {
		return
	}
	_ = runCommandQuiet("launchctl", "bootout", domain, plist)
}

func bootoutLaunchdLabel(domain, label string) {
	if runtime.GOOS != "darwin" || domain == "" || label == "" || !commandExists("launchctl") {
		return
	}
	_ = runCommandQuiet("launchctl", "bootout", domain+"/"+label)
}

func launchdLabelLoaded(domain, label string) bool {
	if runtime.GOOS != "darwin" || domain == "" || label == "" || !commandExists("launchctl") {
		return false
	}
	return runCommandQuiet("launchctl", "print", domain+"/"+label) == nil
}

func requireInstallPermission(paths Paths) error {
	if runtime.GOOS == "darwin" && isRootUser() {
		return errors.New("macOS 当前版本仅支持普通用户安装，请不要使用 sudo/root；如已安装旧的 root/system 版本，请先执行 sudo /usr/local/bin/cf-probe uninstall 清理后，再以普通用户重新安装")
	}
	if paths.UserMode {
		if runtime.GOOS == "darwin" {
			if !commandExists("launchctl") {
				return errors.New("当前 macOS 未检测到 launchctl，无法注册用户 LaunchAgent")
			}
			return nil
		}
		if platform := nonRootSystemServicePlatform(); platform != "" {
			return fmt.Errorf("%s 当前仅支持 root/system 服务安装，请使用 root 权限重新执行安装命令", platform)
		}
		// No service manager at all (e.g. a container without systemd): install a
		// plain background process under the user's home, which needs no root.
		if runtime.GOOS == "linux" && initSystem() == "background" {
			return nil
		}
		if runtime.GOOS != "linux" || !systemdUserSupported() {
			return errors.New("当前系统不支持非 root 运行（未检测到 systemd 用户服务能力），请使用 root 权限重新执行安装命令")
		}
		if !systemdUserAvailable() {
			return fmt.Errorf("无法连接到 systemd --user 服务\n"+
				"可能原因：\n"+
				"1. 当前是 root/su/sudo 切换出来的会话，请退出后直接以非 root 用户 %s 登录\n"+
				"2. 当前会话缺少 XDG_RUNTIME_DIR 或用户 bus\n"+
				"3. 该用户的 systemd user manager 尚未可用\n"+
				"建议：先执行 loginctl enable-linger；如无权限，请用 root 执行 loginctl enable-linger %s，然后重新登录该用户再安装",
				paths.RunUser, paths.RunUser)
		}
		if !systemdUserLingerEnabled(paths.RunUser) {
			return fmt.Errorf("当前用户 %s 未开启 linger，退出登录后服务将停止\n"+
				"请执行以下命令后重新安装:\n"+
				"  loginctl enable-linger\n"+
				"如无权限，请用 root 执行:\n"+
				"  loginctl enable-linger %s",
				paths.RunUser, paths.RunUser)
		}
		return nil
	}
	if !isRootUser() {
		return errors.New("请使用 root 权限重新执行安装命令")
	}
	return nil
}

func nonRootSystemServicePlatform() string {
	if runtime.GOOS == "darwin" {
		return ""
	}
	if runtime.GOOS != "linux" {
		return platformName()
	}
	if isSynology() {
		return "Synology DSM"
	}
	if isOpenWrt() {
		return "OpenWrt"
	}
	if systemdUserSupported() {
		return ""
	}
	platform := platformName()
	switch initSystem() {
	case "openrc":
		if platform == "Alpine Linux" {
			return "Alpine Linux/OpenRC"
		}
		return platform + "/OpenRC"
	case "procd":
		return "OpenWrt"
	case "synology-rc":
		return "Synology DSM"
	case "upstart":
		return platform + "/Upstart"
	}
	// "background" (no service manager) is intentionally not listed here: a
	// non-root user can run a plain background process, so it must not be
	// reported as a root/system-only platform.
	return ""
}

func requireUninstallPermission(paths Paths) error {
	if paths.UserMode {
		if platform := nonRootSystemServicePlatform(); platform != "" {
			return fmt.Errorf("%s 当前仅支持 root/system 服务卸载，请使用 root 权限重新执行卸载命令", platform)
		}
		return nil
	}
	if !isRootUser() {
		return errors.New("请使用 root 权限重新执行卸载命令")
	}
	if conflicts := userInstallConflicts(); len(conflicts) > 0 {
		return fmt.Errorf("检测到已有非 root 用户版 cf-probe 安装: %s；root 卸载只清理系统级安装，请先切换到对应用户执行卸载",
			strings.Join(conflicts, ", "))
	}
	return nil
}

func systemdUserAvailable() bool {
	return systemdUserSupported() &&
		commandExists("systemctl") &&
		runCommandQuiet("systemctl", "--user", "show-environment") == nil
}

func systemdUserSupported() bool {
	return runtime.GOOS == "linux" &&
		commandExists("systemctl") &&
		commandExists("loginctl") &&
		(fileExists("/run/systemd/system") || commandOutput("ps", "-p", "1", "-o", "comm=") == "systemd")
}

func systemdUserLingerEnabled(name string) bool {
	if runtime.GOOS != "linux" || strings.TrimSpace(name) == "" || !commandExists("loginctl") {
		return false
	}
	return strings.EqualFold(commandOutput("loginctl", "show-user", name, "-p", "Linger", "--value"), "yes")
}

func checkInstallConflicts(paths Paths) error {
	if err := checkOtherUserProbeRunning(); err != nil {
		return err
	}
	if paths.UserMode {
		if conflicts := systemInstallConflicts(); len(conflicts) > 0 {
			if runtime.GOOS == "darwin" {
				return fmt.Errorf("检测到 macOS root/system 版 cf-probe 安装: %s；请先执行 sudo /usr/local/bin/cf-probe uninstall 清理旧版，然后不要使用 sudo，以普通用户重新安装",
					strings.Join(conflicts, ", "))
			}
			return fmt.Errorf("检测到系统级/root 版 cf-probe 安装: %s；当前版本暂未实现迁移，请先使用 root 清理旧版本后再以当前用户安装",
				strings.Join(conflicts, ", "))
		}
	} else if conflicts := userInstallConflicts(); len(conflicts) > 0 {
		return fmt.Errorf("检测到已有非 root 用户版 cf-probe 安装: %s；为避免重复上报，请先切换到对应用户卸载后再继续 root 安装",
			strings.Join(conflicts, ", "))
	}
	return nil
}

func systemInstallConflicts() []string {
	paths := systemDefaultPaths()
	checks := []string{
		paths.BinaryFile,
		filepath.Join("/usr/local/bin", paths.ServiceName),
		filepath.Join("/usr/bin", paths.ServiceName),
		paths.ConfigFile,
		paths.TrafficFile,
		paths.OldTrafficFile,
		paths.ServiceFile,
		filepath.Join("/etc/init.d", paths.ServiceName),
		filepath.Join("/etc/init", paths.ServiceName+".conf"),
		synologyServiceFile(paths),
		legacyUnixScriptFile,
		legacyUnixScriptFile + ".ctl",
	}
	if runtime.GOOS == "darwin" {
		checks = append(checks, paths.LaunchdRootFile, legacyDarwinLaunchdFile)
	}
	conflicts := existingPaths(checks...)
	if runtime.GOOS == "darwin" {
		if launchdLabelLoaded("system", paths.LaunchdLabel) {
			conflicts = append(conflicts, "launchd:/system/"+paths.LaunchdLabel)
		}
		if launchdLabelLoaded("system", "com.cf.probe") {
			conflicts = append(conflicts, "launchd:/system/com.cf.probe")
		}
	}
	return uniqueStrings(conflicts)
}

func userInstallConflicts() []string {
	if runtime.GOOS != "linux" || !isRootUser() {
		return nil
	}
	var conflicts []string
	for _, home := range linuxUserHomeDirs() {
		service := filepath.Join(home, ".config", "systemd", "user", serviceNameDefault+".service")
		binary := filepath.Join(home, ".cf-probe", "bin", serviceNameDefault)
		config := filepath.Join(home, ".cf-probe", "config.conf")
		if hits := existingPaths(service, binary, config); len(hits) > 0 {
			conflicts = append(conflicts, home+":"+strings.Join(hits, "|"))
		}
	}
	return uniqueStrings(conflicts)
}

func linuxUserHomeDirs() []string {
	return uniqueStrings(passwdUserHomeDirs())
}

func passwdUserHomeDirs() []string {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return nil
	}
	return parsePasswdUserHomeDirs(string(data))
}

func parsePasswdUserHomeDirs(raw string) []string {
	var homes []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil || uid == 0 || uid < 100 {
			continue
		}
		home := strings.TrimSpace(fields[5])
		shell := strings.TrimSpace(fields[6])
		if home == "" || home == "/" || strings.HasPrefix(shell, "/usr/sbin/nologin") || strings.HasPrefix(shell, "/sbin/nologin") || strings.HasPrefix(shell, "/bin/false") {
			continue
		}
		homes = append(homes, home)
	}
	return homes
}

type runningProbeInstance struct {
	PID    int
	UID    int
	User   string
	Exe    string
	Cmd    string
	Legacy bool
}

func checkOtherUserProbeRunning() error {
	for _, inst := range runningProbeInstances() {
		if inst.UID == currentUID() {
			continue
		}
		if inst.Legacy {
			return fmt.Errorf("检测到旧 shell 版 cf-probe 已在运行，用户 %s(uid=%d, pid=%d)，命令: %s；请先停止旧版本后再安装",
				firstNonEmpty(inst.User, strconv.Itoa(inst.UID)), inst.UID, inst.PID, firstNonEmpty(inst.Cmd, inst.Exe))
		}
		return fmt.Errorf("检测到 cf-probe 已由用户 %s(uid=%d, pid=%d) 运行，命令: %s；请先停止该实例后再安装",
			firstNonEmpty(inst.User, strconv.Itoa(inst.UID)), inst.UID, inst.PID, firstNonEmpty(inst.Cmd, inst.Exe))
	}
	return nil
}

func stopCurrentUserProbeInstances() {
	for _, inst := range runningProbeInstances() {
		if inst.UID == currentUID() {
			signalProbeInstance(inst.PID, syscall.SIGTERM)
		}
	}
	for i := 0; i < 20; i++ {
		if !currentUserProbeRunning() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, inst := range runningProbeInstances() {
		if inst.UID == currentUID() {
			signalProbeInstance(inst.PID, syscall.SIGKILL)
		}
	}
}

func currentUserProbeRunning() bool {
	for _, inst := range runningProbeInstances() {
		if inst.UID == currentUID() {
			return true
		}
	}
	return false
}

func signalProbeInstance(pid int, sig syscall.Signal) {
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(sig)
	}
}

func runningProbeInstances() []runningProbeInstance {
	if runtime.GOOS != "linux" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var instances []runningProbeInstance
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		inst, ok := probeInstanceFromProc(pid)
		if ok {
			instances = append(instances, inst)
		}
	}
	return instances
}

func probeInstanceFromProc(pid int) (runningProbeInstance, bool) {
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	cmdline := procCmdline(filepath.Join(procDir, "cmdline"))
	exe, _ := os.Readlink(filepath.Join(procDir, "exe"))
	if !isProbeRunCommand(exe, cmdline) {
		return runningProbeInstance{}, false
	}
	uid := procUID(procDir)
	return runningProbeInstance{
		PID:    pid,
		UID:    uid,
		User:   usernameForUID(uid),
		Exe:    exe,
		Cmd:    strings.Join(cmdline, " "),
		Legacy: isLegacyShellCommand(cmdline),
	}, true
}

func procCmdline(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	raw := strings.TrimRight(string(data), "\x00")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\x00")
}

func procUID(procDir string) int {
	var st syscall.Stat_t
	if err := syscall.Stat(procDir, &st); err != nil {
		return -1
	}
	return int(st.Uid)
}

func isProbeRunCommand(exe string, cmdline []string) bool {
	if isLegacyShellCommand(cmdline) {
		return true
	}
	if len(cmdline) == 0 {
		return false
	}
	base := filepath.Base(exe)
	if base == "" || base == "." {
		base = filepath.Base(cmdline[0])
	}
	if !strings.HasPrefix(base, serviceNameDefault) && !strings.Contains(cmdline[0], serviceNameDefault) {
		return false
	}
	for _, arg := range cmdline[1:] {
		if arg == "run" || arg == "start-foreground" {
			return true
		}
	}
	return false
}

func isLegacyShellCommand(cmdline []string) bool {
	return strings.Contains(strings.Join(cmdline, " "), legacyShellScriptName)
}

func acquireInstanceLock(paths Paths) (func(), error) {
	lockPath := filepath.Join(os.TempDir(), paths.ServiceName+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil, fmt.Errorf("检查运行实例失败 %s: %w", lockPath, err)
	}
	_ = os.Chmod(lockPath, 0o666)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_, _ = f.Seek(0, io.SeekStart)
		data, _ := io.ReadAll(f)
		_ = f.Close()
		if owner := formatInstanceLockOwner(data); owner != "" {
			return nil, fmt.Errorf("cf-probe 已在运行: %s", owner)
		}
		return nil, errors.New("cf-probe 已在运行")
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.Seek(0, io.SeekStart)
		exe, _ := os.Executable()
		_, _ = fmt.Fprintf(f, "pid=%d\nuid=%d\nuser=%s\nexe=%s\nconfig=%s\n",
			os.Getpid(), currentUID(), firstNonEmpty(paths.RunUser, usernameForUID(currentUID())), exe, paths.ConfigFile)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func formatInstanceLockOwner(data []byte) string {
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[k] = v
		}
	}
	if len(values) == 0 {
		return ""
	}
	return fmt.Sprintf("user=%s uid=%s pid=%s exe=%s config=%s",
		values["user"], values["uid"], values["pid"], values["exe"], values["config"])
}

func applyWindowsScheduledUpdate(_ Paths) error {
	return errors.New("scheduled Windows update apply is unavailable on this platform")
}

func copySelfTo(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	src, _ = filepath.EvalSymlinks(src)
	dst, _ = filepath.Abs(dst)
	if src == dst {
		return os.Chmod(dst, 0o755)
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
	if err = os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func startDetached(binary string, args []string, logFile, pidFile string) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(binary, args...)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err == nil {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)
	}
	return cmd.Process.Release()
}

func stopDetached(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(pidFile)
		return
	}
	proc, err := os.FindProcess(pid)
	if err == nil {
		_ = proc.Signal(syscall.SIGTERM)
	}
	_ = os.Remove(pidFile)
}

func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp"
	}
	return home
}

func currentAccount() (string, string, int) {
	uid := os.Getuid()
	if account, err := osuser.Current(); err == nil {
		name := account.Username
		if idx := strings.LastIndexAny(name, `\`); idx >= 0 {
			name = name[idx+1:]
		}
		home := account.HomeDir
		if home == "" {
			home = userHomeDir()
		}
		return firstNonEmpty(name, os.Getenv("USER"), strconv.Itoa(uid)), home, uid
	}
	return firstNonEmpty(os.Getenv("USER"), strconv.Itoa(uid)), userHomeDir(), uid
}

func usernameForUID(uid int) string {
	if uid < 0 {
		return ""
	}
	if account, err := osuser.LookupId(strconv.Itoa(uid)); err == nil && strings.TrimSpace(account.Username) != "" {
		name := account.Username
		if idx := strings.LastIndexAny(name, `\`); idx >= 0 {
			name = name[idx+1:]
		}
		return name
	}
	return strconv.Itoa(uid)
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

func commandOutput(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func quoteShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func windowsServiceArgs(_ Paths, _ bool) []string {
	return nil
}

func writeWindowsTaskWrapper(_ Paths, _ bool) error {
	return nil
}

func windowsTaskWrapperFile(_ Paths) string {
	return ""
}

var errWindowsService = errors.New("Windows 服务管理仅在 Windows 平台可用")

func writeWindowsScheduledTaskXML(_ Paths, _ bool) (string, func(), error) {
	return "", func() {}, errWindowsService
}

func stopWindowsScheduledTask(_ Paths) {
}

func ensurePlatformLogFile(path string) error {
	return ensureLogFile(path)
}

func chmodExecutable(path string) error {
	return os.Chmod(path, 0o755)
}

func platformName() string {
	if runtime.GOOS == "darwin" {
		return "macOS"
	}
	if isSynology() {
		return "Synology DSM"
	}
	if isOpenWrt() {
		return "OpenWrt"
	}
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		return "Alpine Linux"
	}
	return runtime.GOOS
}

func isOpenWrt() bool {
	if _, err := os.Stat("/etc/openwrt_release"); err == nil {
		return true
	}
	if _, err := os.Stat("/etc/rc.common"); err == nil && commandExists("uci") {
		return true
	}
	return false
}

func isSynology() bool {
	for _, p := range []string{"/etc.defaults/VERSION", "/etc/VERSION", "/etc.defaults/synoinfo.conf"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func executableForRelease(goos, goarch string) string {
	name := fmt.Sprintf("cf-probe-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func isRootUser() bool {
	return os.Geteuid() == 0
}

func currentUID() int {
	return os.Getuid()
}
