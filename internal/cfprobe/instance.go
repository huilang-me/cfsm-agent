package cfprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 实例名会直接拼进服务名、目录名、systemd unit、launchd label 和 Windows 计划任务名，
// 因此只允许文件名与这些标识符都安全的字符，且必须以字母或数字开头。
// 统一转小写：macOS/Windows 文件系统大小写不敏感，避免 eth1 与 ETH1 映射到同一套路径。
var instanceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func normalizeInstanceName(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		return "", nil
	}
	if !instanceNameRE.MatchString(name) {
		return "", fmt.Errorf("instance 参数非法: %q（仅允许字母、数字、下划线、连字符，1-32 位，且必须以字母或数字开头）", raw)
	}
	return name, nil
}

func instanceServiceName(instance string) string {
	if instance == "" {
		return serviceNameDefault
	}
	return serviceNameDefault + "-" + instance
}

// instanceMarkerFile 写在命名实例的配置目录里，记录该目录属于哪个实例。
// --all 只清理带匹配标记的目录，避免误删用户自建的备份目录。
const instanceMarkerFile = ".instance"

func writeInstanceMarker(paths Paths) error {
	if paths.Instance == "" {
		return nil
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(paths.ConfigDir, instanceMarkerFile), []byte(paths.Instance+"\n"), 0o600)
}

func instanceMarkerMatches(dir, name string) bool {
	data, err := os.ReadFile(filepath.Join(dir, instanceMarkerFile))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == name
}

func validInstanceSuffix(suffix string) string {
	suffix = strings.ToLower(strings.TrimSpace(suffix))
	if instanceNameRE.MatchString(suffix) {
		return suffix
	}
	return ""
}

// probeInstanceNameFromCmdline 从进程参数中解析 -instance；无该参数即默认实例。
// 只做精确 token 匹配，避免 eth1 误匹配 eth10。
func probeInstanceNameFromCmdline(cmdline []string) string {
	for i, arg := range cmdline {
		switch {
		case arg == "-instance" || arg == "--instance":
			if i+1 < len(cmdline) {
				if name := validInstanceSuffix(cmdline[i+1]); name != "" {
					return name
				}
			}
		case strings.HasPrefix(arg, "-instance="):
			return validInstanceSuffix(strings.TrimPrefix(arg, "-instance="))
		case strings.HasPrefix(arg, "--instance="):
			return validInstanceSuffix(strings.TrimPrefix(arg, "--instance="))
		}
	}
	return ""
}

// instanceFromExecutablePath 从当前可执行文件路径推断实例名，仅用于卸载时的防误删保护。
// 只识别本程序安装后的固定位置，避免把下载到当前目录的发行包
// （cf-probe-linux-amd64 等）误判成实例。
func instanceFromExecutablePath(exe string) string {
	if exe == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	base := strings.TrimSuffix(filepath.Base(exe), ".exe")
	dir := filepath.Dir(exe)
	if base == serviceNameDefault {
		// 用户版：~/.cf-probe-NAME/bin/cf-probe
		parent := filepath.Base(filepath.Dir(dir))
		if strings.HasPrefix(parent, "."+serviceNameDefault+"-") {
			return validInstanceSuffix(strings.TrimPrefix(parent, "."+serviceNameDefault+"-"))
		}
		return ""
	}
	if strings.HasPrefix(base, serviceNameDefault+"-") {
		// 系统版：/usr/local/bin/cf-probe-NAME、/usr/bin/cf-probe-NAME
		// Windows：%ProgramFiles%\cf-probe\cf-probe-NAME.exe
		if dir == filepath.Clean("/usr/local/bin") || dir == filepath.Clean("/usr/bin") || strings.EqualFold(filepath.Base(dir), serviceNameDefault) {
			return validInstanceSuffix(strings.TrimPrefix(base, serviceNameDefault+"-"))
		}
	}
	return ""
}

func currentExecutableInstance() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	name := instanceFromExecutablePath(exe)
	if name == "" {
		return ""
	}
	// 只有确实装过该实例（配置存在）才触发防误删保护。这样手动把发行包
	// cf-probe-linux-amd64 放到 /usr/local/bin 也不会被误判为实例。
	if !fileExists(pathsForInstance(name).ConfigFile) {
		return ""
	}
	return name
}

// installedInstanceNames 枚举当前权限范围内的命名实例（不含默认实例）。
// 实例目录固定为默认配置目录加 "-NAME" 后缀，因此直接扫描同级目录即可，
// 不需要额外维护注册表文件。
func installedInstanceNames(base Paths) []string {
	if base.ConfigDir == "" {
		return nil
	}
	parent := filepath.Dir(base.ConfigDir)
	prefix := filepath.Base(base.ConfigDir) + "-"
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		if suffix == entry.Name() {
			continue
		}
		name := validInstanceSuffix(suffix)
		if name == "" {
			continue
		}
		// 只把真正装过的目录当作实例，避免把用户自建的备份目录
		// （如 .cf-probe-backup）纳入 --all 的清理范围。
		if !instanceMarkerMatches(filepath.Join(parent, entry.Name()), name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
