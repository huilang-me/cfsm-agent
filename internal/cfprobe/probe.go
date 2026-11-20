package cfprobe

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ping "github.com/prometheus-community/pro-bing"
)

const (
	defaultPingTimeout      = 1500 * time.Millisecond
	defaultTaskPingTCPPort  = 80
	defaultMetricsTCPPort   = 80
	dnsCacheTTL             = 30 * time.Minute
)

type dnsCacheEntry struct {
	ip        string
	expiresAt time.Time
}

var (
	dnsCacheMu sync.RWMutex
	dnsCache   = map[string]dnsCacheEntry{}
	lookupIP   = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		resolver := net.Resolver{}
		return resolver.LookupIPAddr(ctx, host)
	}
)

func splitProbeTarget(target string, defaultPort int) (string, int, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", 0, errors.New("empty target")
	}
	host := target
	port := defaultPort
	if h, p, err := net.SplitHostPort(target); err == nil {
		host = h
		parsed, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, err
		}
		port = parsed
	} else if strings.Count(target, ":") == 1 {
		before, after, ok := strings.Cut(target, ":")
		if ok && before != "" && after != "" {
			parsed, err := strconv.Atoi(after)
			if err != nil {
				return "", 0, err
			}
			host = before
			port = parsed
		}
	}
	host = strings.Trim(host, "[]")
	if host == "" || strings.HasPrefix(host, "-") {
		return "", 0, errors.New("invalid host")
	}
	if port < 1 || port > 65535 {
		return "", 0, errors.New("invalid port")
	}
	return host, port, nil
}

func resolveFirstIP(ctx context.Context, host string) (string, error) {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	key := strings.ToLower(host)
	now := time.Now()
	dnsCacheMu.RLock()
	if entry, ok := dnsCache[key]; ok && now.Before(entry.expiresAt) {
		dnsCacheMu.RUnlock()
		return entry.ip, nil
	}
	dnsCacheMu.RUnlock()

	addrs, err := lookupIP(ctx, host)
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		if addr.IP != nil {
			ip := addr.IP.String()
			dnsCacheMu.Lock()
			dnsCache[key] = dnsCacheEntry{ip: ip, expiresAt: now.Add(dnsCacheTTL)}
			dnsCacheMu.Unlock()
			return ip, nil
		}
	}
	return "", errors.New("no address")
}

func tcpPing(target string, defaultPort int, timeout time.Duration) (int, error) {
	host, port, err := splitProbeTarget(target, defaultPort)
	if err != nil {
		return -1, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ip, err := resolveFirstIP(ctx, host)
	if err != nil {
		return -1, err
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	ms := int(time.Since(start).Milliseconds())
	if ms < 1 {
		ms = 1
	}
	return ms, nil
}

func icmpPing(target string, timeout time.Duration) (int, error) {
	host, _, err := splitProbeTarget(target, defaultTaskPingTCPPort)
	if err != nil {
		host = strings.Trim(strings.TrimSpace(target), "[]")
	}
	if host == "" {
		return -1, errors.New("empty target")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	ip, err := resolveFirstIP(ctx, host)
	defer cancel()
	if err != nil {
		return -1, err
	}

	pinger, err := ping.NewPinger(ip)
	if err != nil {
		return -1, err
	}
	pinger.Count = 1
	pinger.Timeout = timeout
	pinger.SetPrivileged(true)
	if err := pinger.Run(); err != nil {
		return -1, err
	}
	stats := pinger.Statistics()
	if stats.PacketsRecv == 0 {
		return -1, errors.New("no packets received")
	}
	ms := int(stats.AvgRtt.Milliseconds())
	if ms < 1 {
		ms = 1
	}
	return ms, nil
}

func measureProbe(kind, target string, count, defaultPort int, log logger) ProbeResult {
	if strings.TrimSpace(target) == "" {
		return ProbeResult{RTTMs: -1, Loss: 100, OK: false}
	}
	if kind != pingModeICMP {
		kind = pingModeTCP
	}
	if count < 1 {
		count = 4
	}
	if defaultPort <= 0 {
		defaultPort = defaultMetricsTCPPort
	}
	ok := 0
	values := make([]int, 0, count)
	for i := 0; i < count; i++ {
		var ms int
		var err error
		if kind == pingModeICMP {
			ms, err = icmpPing(target, defaultPingTimeout)
		} else {
			ms, err = tcpPing(target, defaultPort, defaultPingTimeout)
		}
		if err == nil {
			ok++
			values = append(values, ms)
		} else {
			log.debugf("probe %s failed target=%s err=%v", kind, target, err)
		}
	}
	return buildProbeResult(count, values[:ok])
}

func probeHistoryKey(kind, target string) string {
	if kind != pingModeICMP {
		kind = pingModeTCP
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	return kind + "\x00" + target
}

func buildProbeResult(count int, values []int) ProbeResult {
	if count < 1 {
		count = 1
	}
	ok := len(values)
	if ok == 0 {
		return ProbeResult{RTTMs: -1, Loss: 100, OK: false}
	}
	return ProbeResult{RTTMs: medianInt(values), Loss: (count - ok) * 100 / count, OK: true}
}

func medianInt(values []int) int {
	sort.Ints(values)
	median := values[len(values)/2]
	if len(values)%2 == 0 {
		median = (values[len(values)/2-1] + values[len(values)/2]) / 2
	}
	return median
}

