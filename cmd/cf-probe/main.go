package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/huilang-me/cfsm-agent/internal/cfprobe"
)

var version = "dev"

func main() {
	if err := cfprobe.Execute(os.Args[1:], version); err != nil {
		if errors.Is(err, cfprobe.ErrAutoDiscoveryFailed) {
			// 自动建号中止属于不可恢复的业务失败：具体原因已由 Agent 写入日志。
			// 以 0 退出，配合服务管理器"仅在异常退出时重启"的策略，让进程彻底停止，
			// 避免被反复拉起后重复发起建号请求。
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
		os.Exit(1)
	}
}
