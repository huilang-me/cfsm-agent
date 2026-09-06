package cfprobe

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func Execute(args []string, buildVersion string) error {
	if buildVersion == "" || buildVersion == "dev" {
		buildVersion = legacyAgentVersion
	}
	if len(args) == 0 {
		args = []string{"install"}
	}

	switch args[0] {
	case "install":
		opts, err := parseInstallOptions(args[1:])
		if err != nil {
			return err
		}
		return Install(opts, buildVersion)
	case "run", "start-foreground":
		debug, configFile, err := parseRunOptions(args[1:])
		if err != nil {
			return err
		}
		return Run(configFile, debug, buildVersion)
	case "uninstall", "remove", "delete", "purge":
		if err := parseUninstallArgs(args[1:]); err != nil {
			printUsage(os.Stderr)
			return err
		}
		return Uninstall(buildVersion)
	case "upgrade-apply":
		return ApplyScheduledUpdate(buildVersion)
	case "version", "-v", "--version":
		fmt.Printf("CF-Server-Monitor Go Probe %s\n", buildVersion)
		return nil
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return nil
	default:
		return fmt.Errorf("未知指令 %q，可选命令: install | run | uninstall | version", args[0])
	}
}

func parseInstallOptions(args []string) (InstallOptions, error) {
	opts := InstallOptions{Config: defaultConfig()}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var autoUpdate, debug string
	var ignoredInstallVersion string
	fs.StringVar(&opts.ServerID, "id", "", "")
	fs.StringVar(&opts.Secret, "secret", "", "")
	fs.StringVar(&opts.WorkerURL, "url", "", "")
	fs.IntVar(&opts.ReportInterval, "interval", defaultReportIntervalSec, "")
	fs.IntVar(&opts.CollectInterval, "collect_interval", 0, "")
	fs.IntVar(&opts.CollectInterval, "collect", 0, "")
	fs.StringVar(&opts.CTNode, "ct", "", "")
	fs.StringVar(&opts.CUNode, "cu", "", "")
	fs.StringVar(&opts.CMNode, "cm", "", "")
	fs.StringVar(&opts.BDNode, "bd", "", "")
	fs.StringVar(&opts.Node1, "node_1", "", "")
	fs.StringVar(&opts.Node2, "node_2", "", "")
	fs.StringVar(&opts.Node3, "node_3", "", "")
	fs.StringVar(&opts.Node4, "node_4", "", "")
	fs.StringVar(&opts.Interface, "interface", "", "")
	fs.StringVar(&opts.Interface, "interfaces", "", "")
	fs.StringVar(&opts.Interface, "iface", "", "")
	fs.IntVar(&opts.ResetDay, "reset_day", 1, "")
	fs.StringVar(&opts.ConnectionMode, "connection_mode", connectionModeAuto, "")
	fs.StringVar(&opts.ConnectionMode, "connection-mode", connectionModeAuto, "")
	fs.StringVar(&opts.PingMode, "ping_mode", pingModeTCP, "")
	fs.StringVar(&opts.PingMode, "ping-mode", pingModeTCP, "")
	fs.StringVar(&autoUpdate, "auto_update", "", "")
	fs.StringVar(&autoUpdate, "auto-update", "", "")
	fs.StringVar(&opts.RXCorrectionGB, "rx_correction", "", "")
	fs.StringVar(&opts.TXCorrectionGB, "tx_correction", "", "")
	fs.StringVar(&debug, "debug", "", "")
	fs.StringVar(&opts.UpdateProxy, "install-ghproxy", "", "")
	fs.StringVar(&ignoredInstallVersion, "install-version", "", "")
	fs.BoolVar(&opts.NoStart, "no_start", false, "")
	fs.BoolVar(&opts.NoStart, "no-start", false, "")

	if err := fs.Parse(args); err != nil {
		printUsage(os.Stderr)
		return opts, err
	}
	opts.Explicit = map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		opts.Explicit[canonicalInstallFlag(f.Name)] = true
	})
	var err error
	opts.AutoUpdate, err = normalizeBinaryValue(autoUpdate, false)
	if err != nil {
		return opts, fmt.Errorf("auto_update 参数非法: %w", err)
	}
	opts.Debug, err = normalizeBinaryValue(debug, false)
	if err != nil {
		return opts, fmt.Errorf("debug 参数非法: %w", err)
	}
	opts.Interface, err = normalizeInterfaceList(opts.Interface)
	if err != nil {
		return opts, err
	}
	opts.ConnectionMode, err = normalizeConnectionMode(opts.ConnectionMode)
	if err != nil {
		return opts, err
	}
	opts.PingMode, err = normalizePingMode(opts.PingMode)
	if err != nil {
		return opts, err
	}
	normalizeConfigIntervals(&opts.Config)
	return opts, nil
}

func canonicalInstallFlag(name string) string {
	name = strings.ReplaceAll(name, "-", "_")
	switch name {
	case "collect":
		return "collect_interval"
	case "interfaces", "iface":
		return "interface"
	case "connection-mode":
		return "connection_mode"
	case "ping-mode":
		return "ping_mode"
	default:
		return name
	}
}

func parseRunOptions(args []string) (bool, string, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var debugRaw string
	var configFile string
	fs.StringVar(&debugRaw, "debug", "0", "")
	fs.StringVar(&configFile, "config", "", "")
	if err := fs.Parse(args); err != nil {
		return false, "", err
	}
	debug, err := normalizeBinaryValue(debugRaw, false)
	return debug, configFile, err
}

func parseUninstallArgs(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("uninstall 不支持参数: %s", strings.Join(fs.Args(), " "))
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "用法:")
	fmt.Fprintln(w, "  cf-probe install -id=SERVER_ID -secret=SECRET -url=WORKER_URL [选项]")
	fmt.Fprintln(w, "  cf-probe run [-debug=0|1]")
	fmt.Fprintln(w, "  cf-probe uninstall")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "安装选项:")
	fmt.Fprintln(w, "  -interval=N          上报间隔(秒)，默认60")
	fmt.Fprintln(w, "  -collect_interval=N  采样间隔(秒)，默认0")
	fmt.Fprintln(w, "  -ct/-cu/-cm/-bd=HOST 自定义测试节点，可写 host 或 host:port")
	fmt.Fprintln(w, "  -interface=IFACES    指定网卡，多个用英文逗号分隔")
	fmt.Fprintln(w, "  -reset_day=N         流量重置日(1-31, 0=不重置)，默认1")
	fmt.Fprintln(w, "  -connection_mode=MODE 连接模式 auto|http，默认auto")
	fmt.Fprintln(w, "  -ping_mode=MODE      Ping 模式 tcp|icmp，默认tcp")
	fmt.Fprintln(w, "  -auto_update=0|1     开启自动检查更新，默认0")
	fmt.Fprintln(w, "  -rx_correction=N     下行流量校正(GB)")
	fmt.Fprintln(w, "  -tx_correction=N     上行流量校正(GB)")
	fmt.Fprintln(w, "  -debug=0|1           运行调试日志，默认0")
}

func atoi64Default(raw string, def int64) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return def
	}
	return n
}
