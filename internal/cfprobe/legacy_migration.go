package cfprobe

import (
	"os"
	"path/filepath"
)

func readInstallConfig(paths Paths) (Config, string, error) {
	cfg, err := readConfig(paths.ConfigFile)
	if err == nil {
		return cfg, paths.ConfigFile, nil
	}
	firstErr := err
	if paths.Instance != "" {
		// 旧版配置属于默认实例，命名实例不能继承。
		return cfg, paths.ConfigFile, firstErr
	}
	for _, path := range legacyConfigFiles(paths) {
		if samePath(path, paths.ConfigFile) {
			continue
		}
		cfg, err := readConfig(path)
		if err == nil {
			return cfg, path, nil
		}
	}
	return cfg, paths.ConfigFile, firstErr
}

func migrateTraffic(paths Paths) error {
	if _, err := os.Stat(paths.TrafficFile); err == nil {
		return nil
	}
	if paths.Instance == "" {
		for _, oldTrafficFile := range legacyTrafficFiles(paths) {
			if oldTrafficFile == "" || samePath(oldTrafficFile, paths.TrafficFile) {
				continue
			}
			if _, err := os.Stat(oldTrafficFile); err != nil {
				continue
			}
			if err := os.MkdirAll(paths.ConfigDir, 0o755); err != nil {
				return err
			}
			if err := os.Rename(oldTrafficFile, paths.TrafficFile); err != nil {
				return err
			}
			removeDirIfEmpty(filepath.Dir(oldTrafficFile))
			return nil
		}
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o755); err != nil {
		return err
	}
	f, createErr := os.OpenFile(paths.TrafficFile, os.O_CREATE, 0o600)
	if createErr == nil {
		_ = f.Close()
	}
	return createErr
}
