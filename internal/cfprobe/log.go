package cfprobe

import (
	"fmt"
	"log"
	"os"
	"time"
)

type logger struct {
	debug bool
	// journald 会给每一行加上时间，进程内再打一份就是重复；
	// openrc / launchd / schtasks / background 是把 stdout 重定向到日志文件，
	// 那些部署必须自己带时间。
	timestamp bool
	l         *log.Logger
}

func newLogger(debug bool) logger {
	return logger{
		debug:     debug,
		timestamp: !loggingUnderJournald(),
		l:         log.New(os.Stdout, "", 0),
	}
}

// systemd 的服务进程会带上这两个环境变量，说明 stdout 已接入 journald。
func loggingUnderJournald() bool {
	return os.Getenv("JOURNAL_STREAM") != "" || os.Getenv("INVOCATION_ID") != ""
}

func (l logger) info(format string, args ...any) {
	l.emit("INFO", format, args...)
}

func (l logger) debugf(format string, args ...any) {
	if l.debug {
		l.emit("DEBUG", format, args...)
	}
}

// warnf 不受 debug 开关影响：自动发现中止、配置被拒等属于必须让用户看到的告警。
func (l logger) warnf(format string, args ...any) {
	l.emit("WARN", format, args...)
}

func (l logger) emit(level, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if l.timestamp {
		l.l.Printf("[%s] %s %s", level, time.Now().UTC().Format(time.RFC3339), message)
		return
	}
	l.l.Printf("[%s] %s", level, message)
}
