package cfprobe

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

type logger struct {
	debug bool
	// journald 会给每一行加上时间，进程内再打一份就是重复；
	// openrc / launchd / schtasks / background 是把 stdout 重定向到日志文件，
	// 那些部署必须自己带时间。
	timestamp bool
	l         *log.Logger
	// 周期性重复的告警按 key 限流：这类问题每个上报周期都会重现，
	// 全量输出会刷屏，完全静默又会漏掉。key -> 上次输出时间。
	seen *sync.Map
}

func newLogger(debug bool) logger {
	return logger{
		debug:     debug,
		timestamp: !loggingUnderJournald(),
		l:         log.New(os.Stdout, "", 0),
		seen:      &sync.Map{},
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

// warnRepeatInterval 是 warnfOnce 同一 key 的最小输出间隔：
// 既不刷屏，也不会因为一次输出后再也不提示而漏掉持续存在的问题。
const warnRepeatInterval = 10 * time.Minute

// warnfOnce 用于每个上报周期都可能重现的告警（载荷序列化失败、请求构造失败、
// WSS 配置被拒等）：同一个 key 最多每 10 分钟输出一次，首次一定输出。
func (l logger) warnfOnce(key, format string, args ...any) {
	if l.seen != nil {
		now := time.Now()
		if last, ok := l.seen.Load(key); ok {
			if at, ok := last.(time.Time); ok && now.Sub(at) < warnRepeatInterval {
				return
			}
		}
		l.seen.Store(key, now)
	}
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
