package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
		authFile, err := parseLoginArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid login args: %v\n", err)
			os.Exit(1)
		}
		if authFile != "" {
			auth.SetManagerPath(authFile)
		}
		if err := auth.Login(); err != nil {
			fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
			os.Exit(1)
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

		initPool(serveOpts.configPath, &host, &port)

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		auth.Pool.StartBackgroundRefresh(ctx)
		defer auth.Pool.Stop()

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
		cmdUsage()

	case "doctor":
		cmdDoctor()

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

type serveOptions struct {
	host       string
	port       string
	configPath string
}

func parseLoginArgs(args []string) (string, error) {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	authFile := fs.String("auth-file", "", "auth file path")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *authFile == "" {
		return "", nil
	}
	return expandHome(*authFile), nil
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
  codex-proxy doctor                                     Diagnose deployment configuration
  codex-proxy logout                                     Remove stored credentials

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
  Create ~/.codex-proxy/proxy.json with multiple accounts for load balancing.
  See proxy.example.json for format.

After login, any OpenAI-compatible client can use:
  base_url = http://127.0.0.1:10531/v1

Example:
  export OPENAI_BASE_URL=http://127.0.0.1:10531/v1
  export OPENAI_API_KEY=unused
  python -c "from openai import OpenAI; print(OpenAI().chat.completions.create(model='o3-pro', messages=[{'role':'user','content':'hi'}]))"`)
}
