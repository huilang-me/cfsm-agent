package cfprobe

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	labelRealtimeLog = "\u67e5\u770b\u5b9e\u65f6\u65e5\u5fd7"
	labelStatus      = "\u67e5\u770b\u8fd0\u884c\u72b6\u6001"
	labelStop        = "\u505c\u6b62\u63a2\u9488\u670d\u52a1"
	labelCommands    = "\u7ba1\u7406\u6307\u4ee4"
	labelCTNode      = "CT\u8282\u70b9"
	labelCUNode      = "CU\u8282\u70b9"
	labelCMNode      = "CM\u8282\u70b9"
	labelBDNode      = "BD\u8282\u70b9"
	labelNode1       = "Node 1"
	labelNode2       = "Node 2"
	labelNode3       = "Node 3"
	labelNode4       = "Node 4"
	bullet           = "\u25cf"
)

type managementCommand struct {
	label   string
	command string
}

func printProbeNodes(opts InstallOptions) {
	printProbeNode(labelCTNode, opts.CTNode)
	printProbeNode(labelCUNode, opts.CUNode)
	printProbeNode(labelCMNode, opts.CMNode)
	printProbeNode(labelBDNode, opts.BDNode)
	printProbeNode(labelNode1, opts.Node1)
	printProbeNode(labelNode2, opts.Node2)
	printProbeNode(labelNode3, opts.Node3)
	printProbeNode(labelNode4, opts.Node4)
}

func printProbeNode(label, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	fmt.Printf("    %s %-10s : %s\n", bullet, label, value)
}

func printManagementCommands(paths Paths) {
	cmds := managementCommands(paths, serviceSystem(paths))
	if len(cmds) == 0 {
		return
	}
	fmt.Printf("  %s :\n", labelCommands)
	for _, cmd := range cmds {
		fmt.Print(formatManagementCommand(cmd))
	}
}

func formatManagementCommand(cmd managementCommand) string {
	return fmt.Sprintf("    %s %s :\n      %s\n", bullet, cmd.label, cmd.command)
}

func managementCommands(paths Paths, system string) []managementCommand {
	switch system {
	case "systemd":
		return []managementCommand{
			{labelRealtimeLog, "journalctl -u " + paths.ServiceName + " -f"},
			{labelStatus, "systemctl status " + paths.ServiceName},
			{labelStop, "systemctl stop " + paths.ServiceName},
		}
	case "systemd-user":
		return []managementCommand{
			{labelRealtimeLog, "journalctl --user -u " + paths.ServiceName + " -f"},
			{labelStatus, "systemctl --user status " + paths.ServiceName},
			{labelStop, "systemctl --user stop " + paths.ServiceName},
		}
	case "openrc":
		return []managementCommand{
			{labelRealtimeLog, "tail -f " + quoteShell(paths.LogFile)},
			{labelStatus, "rc-service " + paths.ServiceName + " status"},
			{labelStop, "rc-service " + paths.ServiceName + " stop"},
		}
	case "procd":
		initScript := "/etc/init.d/" + paths.ServiceName
		return []managementCommand{
			{labelRealtimeLog, "logread -f -e " + paths.ServiceName},
			{labelStatus, initScript + " status"},
			{labelStop, initScript + " stop"},
		}
	case "launchd":
		domain := launchdDomain(paths)
		launchdTarget := domain + "/" + paths.LaunchdLabel
		stopCommand := "launchctl bootout " + domain + " " + quoteShell(launchdPlist(paths))
		if !paths.UserMode {
			stopCommand = "sudo " + stopCommand
		}
		return []managementCommand{
			{labelRealtimeLog, "tail -f " + quoteShell(paths.LogFile)},
			{labelStatus, "launchctl print " + launchdTarget},
			{labelStop, stopCommand},
		}
	case "upstart":
		upstartLog := filepath.Join("/var/log/upstart", paths.ServiceName+".log")
		return []managementCommand{
			{labelRealtimeLog, "tail -f " + quoteShell(upstartLog)},
			{labelStatus, "initctl status " + paths.ServiceName},
			{labelStop, "initctl stop " + paths.ServiceName},
		}
	case "synology-rc":
		rcFile := synologyServiceFile(paths)
		return []managementCommand{
			{labelRealtimeLog, "tail -f " + quoteShell(paths.LogFile)},
			{labelStatus, "ps | grep '[c]f-probe'"},
			{labelStop, rcFile + " stop"},
		}
	case "windows":
		return []managementCommand{
			{labelRealtimeLog, "powershell -NoProfile -Command \"Get-Content -Path " + quotePowerShellDisplayLiteral(paths.LogFile) + " -Wait\""},
			{labelStatus, "schtasks /Query /TN " + paths.ServiceName},
			{labelStop, "schtasks /End /TN " + paths.ServiceName},
		}
	default:
		return []managementCommand{
			{labelRealtimeLog, "tail -f " + quoteShell(paths.LogFile)},
			{labelStatus, "kill -0 $(cat " + quoteShell(paths.PIDFile) + ") && echo running || echo stopped"},
			{labelStop, "kill $(cat " + quoteShell(paths.PIDFile) + ")"},
		}
	}
}

func quotePowerShellDisplayLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
