package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

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
		authFile := ""
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "--auth-file" && i+1 < len(os.Args) {
				authFile = os.Args[i+1]
				i++
			}
		}
		if authFile != "" {
			auth.SetManagerPath(expandHome(authFile))
		}
		if err := auth.Login(); err != nil {
			fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
			os.Exit(1)
		}

	case "serve":
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})))

		host := "0.0.0.0"
		port := "10531"
		configPath := ""

		for i := 2; i < len(os.Args); i++ {
			if i+1 >= len(os.Args) {
				break
			}
			switch os.Args[i] {
			case "--host":
				host = os.Args[i+1]
				i++
			case "--port":
				port = os.Args[i+1]
				i++
			case "--config":
				configPath = os.Args[i+1]
				i++
			}
		}

		initPool(configPath, &host, &port)

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		auth.Pool.StartBackgroundRefresh(ctx)
		defer auth.Pool.Stop()

		ks, _ := loadKeys()
		if envKey := os.Getenv("CODEX_PROXY_API_KEY"); envKey != "" {
			ks.Keys = append(ks.Keys, APIKey{Key: envKey, Name: "env"})
		}
		if len(ks.Keys) == 0 {
			slog.Warn("no API keys configured — all requests will be rejected until keys are added via 'codex-proxy key add'")
		}
		validateKey := proxy.KeyValidator(ks.ValidKey)

		if err := proxy.Serve(ctx, host, port, validateKey); err != nil {
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
		if runtime.GOOS == "linux" {
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
		cmdUsage()

	case "logout":
		auth.Logout()

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

func initPool(configPath string, host, port *string) {
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
		return
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
}

func cmdUsage() {
	raw := false
	for _, a := range os.Args[2:] {
		if a == "--raw" {
			raw = true
		}
	}

	cfg, err := loadConfig(defaultConfigPath())
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
		if raw {
			fmt.Println(info.RawJSON)
			return
		}
		printAccountUsage("default", info)
		return
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
		if raw {
			fmt.Printf("  [%s]\n%s\n\n", acc.Name, info.RawJSON)
			continue
		}
		printAccountUsage(acc.Name, info)
	}
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
  codex-proxy login [--auth-file PATH]                   Login via browser OAuth
  codex-proxy serve [--host H] [--port P] [--config F]  Start proxy (foreground)
  codex-proxy status                                     Show auth & service status
  codex-proxy usage                                      Show account rate limit usage
  codex-proxy logout                                     Remove stored credentials

API key management:
  codex-proxy key add [--name NAME] [--key KEY]          Add API key (auto-generate if no --key)
  codex-proxy key list                                   List all API keys
  codex-proxy key delete <key-or-name>                   Delete an API key

Service management (Linux):
  codex-proxy install                  Install systemd user service
  codex-proxy uninstall                Remove systemd service
  codex-proxy start                    Start background service
  codex-proxy stop                     Stop background service
  codex-proxy restart                  Restart background service
  codex-proxy logs                     Tail service logs

Multi-account:
  Create ~/.codex-proxy/proxy.json with multiple accounts for load balancing.
  See proxy.example.json for format.

After login, any OpenAI-compatible client can use:
  base_url = http://127.0.0.1:10531/v1

Example:
  export OPENAI_BASE_URL=http://127.0.0.1:10531/v1
  export OPENAI_API_KEY=unused
  python -c "from openai import OpenAI; print(OpenAI().chat.completions.create(model='o3-pro', messages=[{'role':'user','content':'hi'}]))"`)
}
