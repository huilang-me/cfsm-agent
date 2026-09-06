package cfprobe

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var ifacePartRE = regexp.MustCompile(`^[A-Za-z0-9_.:\-\*\?\[\]]+$`)

func defaultConfig() Config {
	return Config{
		CollectInterval: 0,
		ReportInterval:  defaultReportIntervalSec,
		ResetDay:        1,
		ConnectionMode:  connectionModeAuto,
		PingMode:        pingModeTCP,
		ConfigMD5:       "none",
	}
}

func normalizeBinaryValue(raw string, def bool) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	switch raw {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("仅支持 0 或 1")
	}
}

func normalizeConnectionMode(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "", connectionModeAuto, "wss", "websocket":
		return connectionModeAuto, nil
	case connectionModeHTTP, "post":
		return connectionModeHTTP, nil
	default:
		return "", fmt.Errorf("connection_mode 仅支持 auto 或 http")
	}
}

func normalizePingMode(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "", pingModeTCP:
		return pingModeTCP, nil
	case pingModeICMP:
		return pingModeICMP, nil
	default:
		return "", fmt.Errorf("ping_mode 仅支持 tcp 或 icmp")
	}
}

func normalizeInterfaceList(raw string) (string, error) {
	parts := strings.Split(raw, ",")
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if len(name) > 64 || !ifacePartRE.MatchString(name) {
			return "", fmt.Errorf("interface 参数非法: %q", name)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	joined := strings.Join(out, ",")
	if len(joined) > 255 {
		return "", errors.New("interface 参数过长")
	}
	return joined, nil
}

func parseKVFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"`)
		values[k] = v
	}
	return values, scanner.Err()
}

func readConfig(path string) (Config, error) {
	cfg := defaultConfig()
	values, err := parseKVFile(path)
	if err != nil {
		return cfg, err
	}

	cfg.ServerID = values["SERVER_ID"]
	cfg.Secret = values["SECRET"]
	cfg.WorkerURL = values["WORKER_URL"]
	cfg.CollectInterval = parseIntDefault(values["COLLECT_INTERVAL"], cfg.CollectInterval)
	cfg.ReportInterval = parseIntDefault(values["REPORT_INTERVAL"], cfg.ReportInterval)
	cfg.CTNode = values["CT_NODE"]
	cfg.CUNode = values["CU_NODE"]
	cfg.CMNode = values["CM_NODE"]
	cfg.BDNode = values["BD_NODE"]
	cfg.Node1 = values["NODE_1"]
	cfg.Node2 = values["NODE_2"]
	cfg.Node3 = values["NODE_3"]
	cfg.Node4 = values["NODE_4"]
	cfg.Interface, _ = normalizeInterfaceList(values["INTERFACE"])
	cfg.ResetDay = parseIntDefault(values["RESET_DAY"], cfg.ResetDay)
	cfg.ConnectionMode, _ = normalizeConnectionMode(values["CONNECTION_MODE"])
	cfg.PingMode, _ = normalizePingMode(values["PING_MODE"])
	cfg.AutoUpdate = values["AUTO_UPDATE"] == "1"
	cfg.UpdateProxy = strings.TrimSpace(values["UPDATE_PROXY"])
	cfg.ConfigMD5 = values["CONFIG_MD5"]
	if cfg.ConfigMD5 == "" {
		cfg.ConfigMD5 = "none"
	}
	normalizeConfigIntervals(&cfg)
	return cfg, nil
}

func writeConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	normalizeConfigIntervals(&cfg)
	var buf bytes.Buffer
	writeKV := func(k, v string) {
		fmt.Fprintf(&buf, "%s=\"%s\"\n", k, strings.ReplaceAll(v, `"`, `\"`))
	}
	writeKV("SERVER_ID", cfg.ServerID)
	writeKV("SECRET", cfg.Secret)
	writeKV("WORKER_URL", cfg.WorkerURL)
	writeKV("COLLECT_INTERVAL", strconv.Itoa(cfg.CollectInterval))
	writeKV("REPORT_INTERVAL", strconv.Itoa(cfg.ReportInterval))
	writeKV("CT_NODE", cfg.CTNode)
	writeKV("CU_NODE", cfg.CUNode)
	writeKV("CM_NODE", cfg.CMNode)
	writeKV("BD_NODE", cfg.BDNode)
	writeKV("NODE_1", cfg.Node1)
	writeKV("NODE_2", cfg.Node2)
	writeKV("NODE_3", cfg.Node3)
	writeKV("NODE_4", cfg.Node4)
	writeKV("INTERFACE", cfg.Interface)
	writeKV("RESET_DAY", strconv.Itoa(cfg.ResetDay))
	writeKV("CONNECTION_MODE", cfg.ConnectionMode)
	writeKV("PING_MODE", cfg.PingMode)
	if cfg.AutoUpdate {
		writeKV("AUTO_UPDATE", "1")
	} else {
		writeKV("AUTO_UPDATE", "0")
	}
	writeKV("UPDATE_PROXY", cfg.UpdateProxy)
	if cfg.ConfigMD5 == "" {
		cfg.ConfigMD5 = "none"
	}
	writeKV("CONFIG_MD5", cfg.ConfigMD5)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func normalizeConfigIntervals(cfg *Config) {
	if cfg.CollectInterval < 0 {
		cfg.CollectInterval = 0
	}
	if cfg.ReportInterval < 1 {
		cfg.ReportInterval = defaultReportIntervalSec
	}
	if cfg.CollectInterval > 0 && cfg.ReportInterval < cfg.CollectInterval {
		cfg.ReportInterval = cfg.CollectInterval
	}
	if cfg.ResetDay < 0 || cfg.ResetDay > 31 {
		cfg.ResetDay = 1
	}
	if cfg.ConfigMD5 == "" {
		cfg.ConfigMD5 = "none"
	}
	if mode, err := normalizeConnectionMode(cfg.ConnectionMode); err == nil {
		cfg.ConnectionMode = mode
	} else {
		cfg.ConnectionMode = connectionModeAuto
	}
	if mode, err := normalizePingMode(cfg.PingMode); err == nil {
		cfg.PingMode = mode
	} else {
		cfg.PingMode = pingModeTCP
	}
	cfg.UpdateProxy = strings.TrimSpace(cfg.UpdateProxy)
}

func parseIntDefault(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return n
}

func parseTrafficCorrectionGB(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 || f > maxTrafficCorrectionGB {
		return 0, fmt.Errorf("流量校正值非法: %s", raw)
	}
	return uint64(f * 1024 * 1024 * 1024), nil
}

func splitInterfaceSet(raw string) map[string]bool {
	out := map[string]bool{}
	if raw == "" {
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out[part] = true
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
