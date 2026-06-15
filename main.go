package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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
		deviceAuth := false
		for _, arg := range os.Args[2:] {
			if arg == "--device-auth" {
				deviceAuth = true
			}
		}
		if err := auth.Login(deviceAuth); err != nil {
			fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
			os.Exit(1)
		}

	case "serve":
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})))

		host := "127.0.0.1"
		port := "10531"
		for i := 2; i < len(os.Args)-1; i++ {
			switch os.Args[i] {
			case "--host":
				host = os.Args[i+1]
			case "--port":
				port = os.Args[i+1]
			}
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		auth.Manager.StartBackgroundRefresh(ctx)
		defer auth.Manager.Stop()

		if err := proxy.Serve(ctx, host, port); err != nil {
			slog.Error("proxy server stopped", "error", err)
			os.Exit(1)
		}

	case "status":
		auth.ShowStatus()

	case "logout":
		auth.Logout()

	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`codex-proxy - Codex OAuth API Proxy

Usage:
  codex-proxy login [--device-auth]   Login via Codex OAuth (browser or device code)
  codex-proxy serve [--host HOST] [--port PORT]   Start API proxy server
  codex-proxy status                  Show current auth status
  codex-proxy logout                  Remove stored credentials

After login, any OpenAI-compatible client can use:
  base_url = http://127.0.0.1:10531/v1

Example:
  export OPENAI_BASE_URL=http://127.0.0.1:10531/v1
  export OPENAI_API_KEY=unused  # any non-empty string
  python -c "from openai import OpenAI; print(OpenAI().chat.completions.create(model='o3-pro', messages=[{'role':'user','content':'hi'}]))"`)
}
