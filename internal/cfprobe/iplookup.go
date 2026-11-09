package cfprobe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// 公网 IP 查询接口列表，参考 komari-agent 的多源回退策略：
// 任意一个源失败时依次尝试下一个，优先国内可达性较好的源。
var ipv4LookupEndpoints = []string{
	"https://www.visa.cn/cdn-cgi/trace",
	"https://www.qualcomm.cn/cdn-cgi/trace",
	"https://edge-ip.html.zone/geo",
	"https://vercel-ip.html.zone/geo",
	"http://ipv4.ip.sb",
	"https://api.ipify.org?format=json",
	"https://cloudflare.com/cdn-cgi/trace",
}

var ipv6LookupEndpoints = []string{
	"https://v6.ip.zxinc.org/info.php?type=json",
	"https://api6.ipify.org?format=json",
	"https://ipv6.icanhazip.com",
	"http://api-ipv6.ip.sb/geoip",
	"https://cloudflare.com/cdn-cgi/trace",
}

const (
	ipLookupUserAgent = "curl/8.0.1"
	ipLookupTimeout   = 8 * time.Second
	ipLookupMaxBody   = 4096
	// 单个候选地址的预算，避免一个坏地址吃掉整轮 8 秒。
	ipAttemptTimeout = 3 * time.Second
	// Worker 域名优先查询的预算。
	ipWorkerLookupTimeout = 3 * time.Second
	workerTracePath       = "/cdn-cgi/trace"
)

var (
	ipv4BodyRE = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
	ipv6BodyRE = regexp.MustCompile(`([0-9A-Fa-f]{1,4}:){7}[0-9A-Fa-f]{1,4}|([0-9A-Fa-f]{1,4}:){1,6}:([0-9A-Fa-f]{1,4}:){0,4}[0-9A-Fa-f]{0,4}`)
)

// lookupPublicIP 通过多个公共接口查询本机公网 IP。
// network 为 "tcp4" 或 "tcp6"，锁定协议族避免取到错误版本的地址。
// workerURL 非空时先用其 origin 下的 /cdn-cgi/trace 查一次，失败再回退到内置源。
// 全程使用系统 DNS、直连；全部源失败时返回空字符串。
func lookupPublicIP(network string, log logger, workerURL string) string {
	budget := ipLookupTimeout
	if endpoint := workerTraceURL(workerURL); endpoint != "" {
		started := time.Now()
		if ip := lookupViaWorker(network, endpoint, log); ip != "" {
			return ip
		}
		if budget -= time.Since(started); budget <= 0 {
			return ""
		}
	}
	endpoints := ipv4LookupEndpoints
	wantV4 := true
	if network == "tcp6" {
		endpoints = ipv6LookupEndpoints
		wantV4 = false
	}
	client := newIPLookupClient(network, budget)
	defer client.CloseIdleConnections()
	for _, endpoint := range endpoints {
		ip, err := fetchIPFromEndpoint(client, endpoint, wantV4)
		if err != nil {
			log.debugf("lookup %s via %s failed: %v", network, endpoint, err)
			continue
		}
		if ip == "" {
			log.debugf("lookup %s via %s: no valid address in response", network, endpoint)
			continue
		}
		log.debugf("lookup %s via %s success: %s", network, endpoint, ip)
		return ip
	}
	return ""
}

// lookupViaWorker 用上报域名取一次 IP，失败时返回空字符串。
func lookupViaWorker(network, endpoint string, log logger) string {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				dialer := &net.Dialer{Timeout: ipWorkerLookupTimeout, KeepAlive: 30 * time.Second}
				return dialer.DialContext(ctx, network, addr)
			},
			DisableKeepAlives:     true,
			TLSHandshakeTimeout:   ipWorkerLookupTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: ipWorkerLookupTimeout,
	}
	defer client.CloseIdleConnections()
	ip, err := fetchIPFromEndpoint(client, endpoint, network == "tcp4")
	if err != nil {
		log.debugf("lookup %s via %s failed: %v", network, endpoint, err)
		return ""
	}
	if ip == "" {
		log.debugf("lookup %s via %s: no valid address in response", network, endpoint)
		return ""
	}
	log.debugf("lookup %s via %s success: %s", network, endpoint, ip)
	return ip
}

// workerTraceURL 取上报地址的 origin 拼出 /cdn-cgi/trace，只接受 http/https。
func workerTraceURL(workerURL string) string {
	u, err := url.Parse(strings.TrimSpace(workerURL))
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.Scheme + "://" + u.Host + workerTracePath
}

func fetchIPFromEndpoint(client *http.Client, endpoint string, wantV4 bool) (string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", ipLookupUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// 只解析 2xx，避免错误页里的地址被正则兜底误采。
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, ipLookupMaxBody))
	if err != nil {
		return "", err
	}
	return extractIPFromBody(string(body), wantV4), nil
}

// extractIPFromBody 从响应体中提取 IP：
// 先解析 cdn-cgi/trace 的 ip= 行，再用正则兜底并用 net.ParseIP 校验，
// 同时校验协议族与期望一致，避免 IPv6 源返回 IPv4 映射地址等情况。
func extractIPFromBody(body string, wantV4 bool) string {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "ip=") {
			continue
		}
		if ip := net.ParseIP(strings.TrimSpace(strings.TrimPrefix(line, "ip="))); ip != nil && (ip.To4() != nil) == wantV4 {
			return ip.String()
		}
	}
	candidateRE := ipv4BodyRE
	if !wantV4 {
		candidateRE = ipv6BodyRE
	}
	for _, candidate := range candidateRE.FindAllString(body, -1) {
		ip := net.ParseIP(candidate)
		if ip == nil {
			continue
		}
		if (ip.To4() != nil) == wantV4 {
			return ip.String()
		}
	}
	return ""
}

// newIPLookupClient 返回锁定协议族（tcp4/tcp6）的 HTTP 客户端。
// TLS 握手在拨号循环内完成，握手失败即换下一个候选地址。
func newIPLookupClient(network string, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = ipLookupTimeout
	}
	dial := func(ctx context.Context, addr string, tlsServerName string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		candidates := lookupIPCandidates(ctx, network, host)
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no %s addresses resolved for %s", network, host)
		}
		conn, err := dialCandidates(ctx, network, candidates, port, tlsServerName)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %s: %w", host, err)
		}
		return conn, nil
	}
	// 刻意不设置 Proxy：查公网 IP 必须直连，走代理只会取到代理的出口地址。
	transport := &http.Transport{
		// 明文源走这里。
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dial(ctx, addr, "")
		},
		// HTTPS 源走这里，TLS 握手由 DialTLSContext 完成，ALPN 固定 http/1.1。
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dial(ctx, addr, host)
		},
		DisableKeepAlives:     true,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// lookupIPCandidates 只走系统 DNS 解析指定协议族的地址。
func lookupIPCandidates(ctx context.Context, network, host string) []net.IP {
	lookupNetwork := "ip"
	switch network {
	case "tcp4":
		lookupNetwork = "ip4"
	case "tcp6":
		lookupNetwork = "ip6"
	}
	ips, _ := net.DefaultResolver.LookupIP(ctx, lookupNetwork, host)
	return ips
}

// dialCandidates 逐个尝试候选地址，tlsServerName 非空时同时完成 TLS 握手；
// 单个地址最多占用 ipAttemptTimeout。
func dialCandidates(ctx context.Context, network string, candidates []net.IP, port, tlsServerName string) (net.Conn, error) {
	var lastErr error
	for _, ip := range candidates {
		attemptCtx, cancel := context.WithTimeout(ctx, ipAttemptTimeout)
		target := net.JoinHostPort(ip.String(), port)
		var conn net.Conn
		var err error
		if tlsServerName != "" {
			dialer := &tls.Dialer{
				NetDialer: &net.Dialer{Timeout: ipAttemptTimeout, KeepAlive: 30 * time.Second},
				Config: &tls.Config{
					ServerName: tlsServerName,
					NextProtos: []string{"http/1.1"},
				},
			}
			conn, err = dialer.DialContext(attemptCtx, network, target)
		} else {
			dialer := &net.Dialer{Timeout: ipAttemptTimeout, KeepAlive: 30 * time.Second}
			conn, err = dialer.DialContext(attemptCtx, network, target)
		}
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no %s addresses available", network)
	}
	return nil, lastErr
}
