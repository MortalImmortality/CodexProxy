package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"codex-proxy/auth"
)

type ProxyConfig struct {
	Accounts []ProxyAccount `json:"accounts"`
	Strategy string         `json:"strategy"` // "round-robin" | "random"
	Host     string         `json:"host,omitempty"`
	Port     string         `json:"port,omitempty"`
}

type ProxyAccount struct {
	Name     string `json:"name"`
	AuthFile string `json:"auth_file"`
}

func loadConfig(path string) (*ProxyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg ProxyConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	for i := range cfg.Accounts {
		cfg.Accounts[i].AuthFile = expandHome(cfg.Accounts[i].AuthFile)
	}
	return &cfg, nil
}

func loadConfigForWrite(path string) (*ProxyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg ProxyConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveConfig(path string, cfg *ProxyConfig) error {
	if cfg.Strategy == "" {
		cfg.Strategy = "round-robin"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func ensureConfig(path string) (*ProxyConfig, error) {
	cfg, err := loadConfig(path)
	if err == nil {
		return cfg, nil
	}
	if !os.IsNotExist(err) || path != defaultConfigPath() {
		return nil, err
	}
	if _, err := bootstrapConfigFromAuthFiles(path); err != nil {
		return nil, err
	}
	return loadConfig(path)
}

func bootstrapConfigFromAuthFiles(configPath string) (bool, error) {
	if _, err := os.Stat(configPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}

	authFiles, err := discoverAuthFiles(authStorageDir())
	if err != nil {
		return false, err
	}
	if len(authFiles) == 0 {
		return false, os.ErrNotExist
	}

	cfg := &ProxyConfig{Strategy: "round-robin"}
	for _, authFile := range authFiles {
		name := inferAccountName(authFile)
		name = uniqueAccountName(cfg, name, authFile)
		cfg.Accounts = append(cfg.Accounts, ProxyAccount{Name: name, AuthFile: authFile})
	}
	return true, saveConfig(configPath, cfg)
}

func discoverAuthFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var authFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "auth.json" || strings.HasPrefix(name, "auth-") && strings.HasSuffix(name, ".json") {
			authFiles = append(authFiles, filepath.Join(dir, name))
		}
	}
	sort.Slice(authFiles, func(i, j int) bool {
		if filepath.Base(authFiles[i]) == "auth.json" {
			return true
		}
		if filepath.Base(authFiles[j]) == "auth.json" {
			return false
		}
		return authFiles[i] < authFiles[j]
	})
	return authFiles, nil
}

func defaultConfigPath() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "proxy.json")
	}
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".codex-proxy", "proxy.json")
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		homeDir, _ := os.UserHomeDir()
		return filepath.Join(homeDir, path[2:])
	}
	return path
}

func configToAccountConfigs(cfg *ProxyConfig) []auth.AccountConfig {
	accounts := make([]auth.AccountConfig, len(cfg.Accounts))
	for i, a := range cfg.Accounts {
		accounts[i] = auth.AccountConfig{
			Name:     a.Name,
			AuthFile: a.AuthFile,
		}
	}
	return accounts
}
