package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"codex-proxy/auth"
	"codex-proxy/proxy"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "login":
		loginOpts, err := parseLoginArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid login args: %v\n", err)
			os.Exit(1)
		}
		authFile := loginOpts.authFile
		auth.SetManagerPath(authFile)
		if err := auth.Login(); err != nil {
			fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
			os.Exit(1)
		}
		if added, err := registerLoginAccount(defaultConfigPath(), loginOpts.name, authFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: login succeeded but account registration failed: %v\n", err)
		} else if added {
			fmt.Printf("Registered account in %s\n", defaultConfigPath())
		}

	case "serve":
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})))

		serveOpts, err := parseServeArgs(os.Args[2:])
		if err != nil {
			slog.Error("invalid serve args", "error", err)
			os.Exit(1)
		}
		host := serveOpts.host
		port := serveOpts.port

		configPath := initPool(serveOpts.configPath, &host, &port)

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		auth.Pool.StartBackgroundRefresh(ctx)
		defer auth.Pool.Stop()
		startConfigReloader(ctx, configPath)

		ks, err := loadKeys()
		if err != nil {
			slog.Error("failed to load API keys", "error", err)
			os.Exit(1)
		}
		if envKey := os.Getenv("CODEX_PROXY_API_KEY"); envKey != "" {
			ks.Keys = append(ks.Keys, APIKey{Key: envKey, Name: "env"})
		}
		if len(ks.Keys) == 0 {
			slog.Warn("no API keys configured — all requests will be rejected until keys are added via 'codex-proxy key add'")
		}
		validateKey := proxy.KeyValidator(ks.ValidKey)

		telegram := startTelegramMonitor(ctx)

		if err := proxy.Serve(ctx, host, port, validateKey); err != nil {
			telegram.sendServiceError(err)
			slog.Error("proxy server stopped", "error", err)
			os.Exit(1)
		}

	case "status":
		cfg, err := loadConfig(defaultConfigPath())
		if err != nil {
			auth.ShowStatus()
		} else {
			fmt.Printf("  Strategy:  %s\n", cfg.Strategy)
			fmt.Printf("  Accounts:  %d\n\n", len(cfg.Accounts))
			for _, acc := range cfg.Accounts {
				fmt.Printf("  [%s] %s\n", acc.Name, acc.AuthFile)
				auth.ShowStatusForFile(acc.Name, expandHome(acc.AuthFile))
			}
		}
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			fmt.Println()
			printServiceStatus()
		}

	case "key":
		if len(os.Args) < 3 {
			fmt.Println("Usage: codex-proxy key <add|delete|list>")
			os.Exit(1)
		}
		switch os.Args[2] {
		case "add":
			cmdKeyAdd(os.Args[3:])
		case "delete", "del", "rm":
			cmdKeyDelete(os.Args[3:])
		case "list", "ls":
			cmdKeyList()
		default:
			fmt.Fprintf(os.Stderr, "Unknown key command: %s\n", os.Args[2])
			os.Exit(1)
		}

	case "usage":
		if err := cmdUsage(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Invalid usage args: %v\n", err)
			os.Exit(1)
		}

	case "doctor":
		cmdDoctor()

	case "logout":
		logoutOpts, err := parseLogoutArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid logout args: %v\n", err)
			os.Exit(1)
		}
		auth.SetManagerPath(logoutOpts.authFile)
		auth.Logout()
		if removed, err := unregisterLoginAccount(defaultConfigPath(), logoutOpts.authFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: logout succeeded but account registration update failed: %v\n", err)
		} else if removed {
			fmt.Printf("Removed account from %s\n", defaultConfigPath())
		}

	case "install":
		serviceInstall()
	case "uninstall":
		serviceUninstall()
	case "start":
		serviceStart()
	case "stop":
		serviceStop()
	case "restart":
		serviceRestart()
	case "logs":
		serviceLogs()

	default:
		printUsage()
		os.Exit(1)
	}
}

func initPool(configPath string, host, port *string) string {
	if configPath == "" {
		configPath = defaultConfigPath()
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		auth.Pool = auth.NewTokenPool(
			[]auth.AccountConfig{{Name: "default", AuthFile: auth.DefaultAuthPath()}},
			"round-robin",
		)
		slog.Info("single-account mode", "auth_file", auth.DefaultAuthPath())
		return configPath
	}

	if cfg.Host != "" {
		*host = cfg.Host
	}
	if cfg.Port != "" {
		*port = cfg.Port
	}
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "round-robin"
	}

	auth.Pool = auth.NewTokenPool(configToAccountConfigs(cfg), strategy)
	slog.Info("multi-account mode",
		"accounts", len(cfg.Accounts),
		"strategy", strategy)
	return configPath
}

func startConfigReloader(ctx context.Context, configPath string) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		lastSignature, _ := poolConfigSignature(configPath)
		for {
			select {
			case <-ctx.Done():
				slog.Info("config reloader stopped")
				return
			case <-ticker.C:
				signature, err := poolConfigSignature(configPath)
				if err != nil {
					slog.Warn("proxy config reload skipped", "config", configPath, "error", err)
					continue
				}
				if signature == lastSignature {
					continue
				}
				accounts, strategy, err := loadPoolAccounts(configPath)
				if err != nil {
					slog.Warn("proxy config reload skipped", "config", configPath, "error", err)
					continue
				}
				auth.Pool.UpdateAccounts(accounts, strategy)
				lastSignature = signature
				slog.Info("proxy config reloaded",
					"config", configPath,
					"accounts", len(accounts),
					"strategy", strategy)
			}
		}
	}()
}

func loadPoolAccounts(configPath string) ([]auth.AccountConfig, string, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []auth.AccountConfig{{Name: "default", AuthFile: auth.DefaultAuthPath()}}, "round-robin", nil
		}
		return nil, "", err
	}
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "round-robin"
	}
	return configToAccountConfigs(cfg), strategy, nil
}

func poolConfigSignature(configPath string) (string, error) {
	accounts, strategy, err := loadPoolAccounts(configPath)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(strategy)
	for _, acc := range accounts {
		b.WriteByte('\n')
		b.WriteString(acc.Name)
		b.WriteByte('\t')
		b.WriteString(acc.AuthFile)
	}
	return b.String(), nil
}

type serveOptions struct {
	host       string
	port       string
	configPath string
}

type loginOptions struct {
	authFile string
	name     string
}

type logoutOptions struct {
	authFile string
	name     string
}

func parseLoginArgs(args []string) (loginOptions, error) {
	opts := loginOptions{}
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.name, "name", "", "account name in proxy config")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	opts.authFile = accountAuthPath(opts.name)
	return opts, nil
}

func parseLogoutArgs(args []string) (logoutOptions, error) {
	opts := logoutOptions{}
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.name, "name", "", "account name in proxy config")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	opts.authFile = accountAuthPath(opts.name)
	return opts, nil
}

func accountAuthPath(name string) string {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(name) == "default" {
		return auth.DefaultAuthPath()
	}
	return filepath.Join(authStorageDir(), "auth-"+slugifyAccountName(name)+".json")
}

func authStorageDir() string {
	return filepath.Dir(auth.DefaultAuthPath())
}

func slugifyAccountName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" || slug == "default" {
		return "account"
	}
	return slug
}

func registerLoginAccount(configPath, name, authFile string) (bool, error) {
	cfg, err := loadConfigForWrite(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, err
		}
		cfg = &ProxyConfig{Strategy: "round-robin"}
		defaultAuthPath := auth.DefaultAuthPath()
		if cleanPath(authFile) != cleanPath(defaultAuthPath) && fileExists(defaultAuthPath) {
			cfg.Accounts = append(cfg.Accounts, ProxyAccount{
				Name:     uniqueAccountName(cfg, "default", ""),
				AuthFile: defaultAuthPath,
			})
		}
	}

	for i := range cfg.Accounts {
		if cleanPath(cfg.Accounts[i].AuthFile) == cleanPath(authFile) {
			if name != "" && cfg.Accounts[i].Name != name {
				cfg.Accounts[i].Name = name
				return true, saveConfig(configPath, cfg)
			}
			return false, nil
		}
	}

	if name == "" {
		name = inferAccountName(authFile)
	}
	name = uniqueAccountName(cfg, name, authFile)
	cfg.Accounts = append(cfg.Accounts, ProxyAccount{Name: name, AuthFile: authFile})
	return true, saveConfig(configPath, cfg)
}

func unregisterLoginAccount(configPath, authFile string) (bool, error) {
	cfg, err := loadConfigForWrite(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	accounts := cfg.Accounts[:0]
	removed := false
	for _, acc := range cfg.Accounts {
		if cleanPath(acc.AuthFile) == cleanPath(authFile) {
			removed = true
			continue
		}
		accounts = append(accounts, acc)
	}
	if !removed {
		return false, nil
	}
	cfg.Accounts = accounts
	return true, saveConfig(configPath, cfg)
}

func inferAccountName(authFile string) string {
	if cleanPath(authFile) == cleanPath(auth.DefaultAuthPath()) {
		return "default"
	}
	base := strings.TrimSuffix(filepath.Base(authFile), filepath.Ext(authFile))
	base = strings.TrimPrefix(base, "auth-")
	if base == "" || base == "auth" {
		return "account"
	}
	return base
}

func uniqueAccountName(cfg *ProxyConfig, preferred, sameAuthFile string) string {
	if preferred == "" {
		preferred = "account"
	}
	used := map[string]bool{}
	for _, acc := range cfg.Accounts {
		if sameAuthFile != "" && cleanPath(acc.AuthFile) == cleanPath(sameAuthFile) {
			continue
		}
		used[acc.Name] = true
	}
	if !used[preferred] {
		return preferred
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", preferred, i)
		if !used[candidate] {
			return candidate
		}
	}
}

func cleanPath(path string) string {
	return filepath.Clean(expandHome(path))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func parseServeArgs(args []string) (serveOptions, error) {
	opts := serveOptions{
		host: "127.0.0.1",
		port: "10531",
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.host, "host", opts.host, "listen host")
	fs.StringVar(&opts.port, "port", opts.port, "listen port")
	fs.StringVar(&opts.configPath, "config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if opts.configPath != "" {
		opts.configPath = expandHome(opts.configPath)
	}
	return opts, nil
}

type usageOptions struct {
	raw        bool
	configPath string
}

func parseUsageArgs(args []string) (usageOptions, error) {
	opts := usageOptions{}
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.raw, "raw", false, "print raw usage JSON")
	fs.StringVar(&opts.configPath, "config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if opts.configPath != "" {
		opts.configPath = expandHome(opts.configPath)
	}
	return opts, nil
}

func cmdUsage(args []string) error {
	opts, err := parseUsageArgs(args)
	if err != nil {
		return err
	}
	configPath := opts.configPath
	if configPath == "" {
		configPath = defaultConfigPath()
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		token, err := auth.Manager.EnsureFreshToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot get token: %v\n", err)
			os.Exit(1)
		}
		info, err := auth.QueryUsage(token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Usage query failed: %v\n", err)
			os.Exit(1)
		}
		if opts.raw {
			fmt.Println(info.RawJSON)
			return nil
		}
		printAccountUsage("default", info)
		return nil
	}

	for _, acc := range cfg.Accounts {
		tm := auth.NewTokenManager(acc.Name, expandHome(acc.AuthFile))
		token, err := tm.EnsureFreshToken()
		if err != nil {
			fmt.Printf("  [%s] error: %v\n\n", acc.Name, err)
			continue
		}
		info, err := auth.QueryUsage(token)
		if err != nil {
			fmt.Printf("  [%s] usage query failed: %v\n\n", acc.Name, err)
			continue
		}
		if opts.raw {
			fmt.Printf("  [%s]\n%s\n\n", acc.Name, info.RawJSON)
			continue
		}
		printAccountUsage(acc.Name, info)
	}
	return nil
}

func printAccountUsage(name string, info *auth.UsageInfo) {
	status := "✓"
	if info.LimitHit {
		status = "✗ LIMIT HIT"
	}
	label := name
	if info.Email != "" {
		label = info.Email
	}
	fmt.Printf("  [%s]\n", label)
	fmt.Printf("    Plan:     %s\n", info.PlanType)
	fmt.Printf("    Status:   %s\n", status)
	if len(info.Windows) == 0 {
		fmt.Printf("    (no rate limit windows)\n")
	}
	for _, w := range info.Windows {
		bar := ""
		filled := w.UsedPercent / 5
		for i := 0; i < 20; i++ {
			if i < filled {
				bar += "█"
			} else {
				bar += "░"
			}
		}
		resetStr := formatReset(w.ResetSecs)
		fmt.Printf("    %-8s  [%s] %d%%  (reset: %s)\n", w.Name+":", bar, w.UsedPercent, resetStr)
	}
	fmt.Println()
}

func formatReset(secs int) string {
	if secs <= 0 {
		return "-"
	}
	h := secs / 3600
	m := (secs % 3600) / 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func printUsage() {
	fmt.Println(`codex-proxy - Codex OAuth API Proxy

Usage:
  codex-proxy login [--name NAME]                        Login via browser OAuth
  codex-proxy serve [--host H] [--port P] [--config F]  Start proxy (foreground)
  codex-proxy status                                     Show auth & service status
  codex-proxy usage [--raw] [--config F]                 Show account rate limit usage
  codex-proxy doctor                                     Diagnose deployment configuration
  codex-proxy logout [--name NAME]                       Remove stored credentials

API key management:
  codex-proxy key add [--name NAME] [--key KEY]          Add API key (auto-generate if no --key)
  codex-proxy key list                                   List all API keys
  codex-proxy key delete <key-or-name>                   Delete an API key

Service management:
  codex-proxy install                  Install user service (systemd/launchd)
  codex-proxy uninstall                Remove user service
  codex-proxy start                    Start background service
  codex-proxy stop                     Stop background service
  codex-proxy restart                  Restart background service
  codex-proxy logs                     Tail service logs

Multi-account:
  Login with different --name values; proxy.json is updated automatically.
  See proxy.example.json for format.

After login, any OpenAI-compatible client can use:
  base_url = http://127.0.0.1:10531/v1

Example:
  export OPENAI_BASE_URL=http://127.0.0.1:10531/v1
  export OPENAI_API_KEY=unused
  python -c "from openai import OpenAI; print(OpenAI().chat.completions.create(model='o3-pro', messages=[{'role':'user','content':'hi'}]))"`)
}
