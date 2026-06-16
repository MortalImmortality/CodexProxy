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

		apiKey := os.Getenv("CODEX_PROXY_API_KEY")
		if err := proxy.Serve(ctx, host, port, apiKey); err != nil {
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

func printUsage() {
	fmt.Println(`codex-proxy - Codex OAuth API Proxy

Usage:
  codex-proxy login [--auth-file PATH]                   Login via browser OAuth
  codex-proxy serve [--host H] [--port P] [--config F]  Start proxy (foreground)
  codex-proxy status                                     Show auth & service status
  codex-proxy logout                                     Remove stored credentials

Service management (Linux):
  codex-proxy install                  Install systemd user service
  codex-proxy uninstall                Remove systemd service
  codex-proxy start                    Start background service
  codex-proxy stop                     Stop background service
  codex-proxy restart                  Restart background service
  codex-proxy logs                     Tail service logs

Multi-account:
  Create ~/.codex/proxy.json with multiple accounts for load balancing.
  See proxy.example.json for format.

After login, any OpenAI-compatible client can use:
  base_url = http://127.0.0.1:10531/v1

Example:
  export OPENAI_BASE_URL=http://127.0.0.1:10531/v1
  export OPENAI_API_KEY=unused
  python -c "from openai import OpenAI; print(OpenAI().chat.completions.create(model='o3-pro', messages=[{'role':'user','content':'hi'}]))"`)
}
