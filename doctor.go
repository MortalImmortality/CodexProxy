package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"codex-proxy/auth"
)

type doctorCheck struct {
	Name   string
	OK     bool
	Detail string
}

func cmdDoctor() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	fmt.Println("codex-proxy doctor")
	fmt.Println()

	checks := runDoctorChecks(ctx)
	okCount := 0
	for _, check := range checks {
		fmt.Println(formatDoctorCheck(check))
		if check.OK {
			okCount++
		}
	}
	fmt.Println()
	fmt.Printf("Summary: %d/%d checks passed\n", okCount, len(checks))
}

func runDoctorChecks(ctx context.Context) []doctorCheck {
	return []doctorCheck{
		doctorAuthCheck(),
		doctorAPIKeyCheck(),
		doctorTelegramCheck(ctx),
		doctorServiceCheck(),
		doctorCaddyCheck(),
	}
}

func doctorAuthCheck() doctorCheck {
	path := auth.DefaultAuthPath()
	if _, err := os.Stat(path); err != nil {
		return doctorCheck{Name: "Auth file", OK: false, Detail: path + " missing; run codex-proxy login"}
	}
	return doctorCheck{Name: "Auth file", OK: true, Detail: path}
}

func doctorAPIKeyCheck() doctorCheck {
	if os.Getenv("CODEX_PROXY_API_KEY") != "" {
		return doctorCheck{Name: "API key", OK: true, Detail: "CODEX_PROXY_API_KEY is set"}
	}
	ks, err := loadKeys()
	if err != nil {
		return doctorCheck{Name: "API key", OK: false, Detail: err.Error()}
	}
	if len(ks.Keys) == 0 {
		return doctorCheck{Name: "API key", OK: false, Detail: "no keys configured; run codex-proxy key add"}
	}
	return doctorCheck{Name: "API key", OK: true, Detail: fmt.Sprintf("%d key(s) configured", len(ks.Keys))}
}

func doctorTelegramCheck(ctx context.Context) doctorCheck {
	cfg, err := telegramConfigFromEnv()
	if err != nil {
		return doctorCheck{Name: "Telegram", OK: false, Detail: err.Error()}
	}
	if !cfg.Enabled {
		return doctorCheck{Name: "Telegram", OK: false, Detail: "not configured; set CODEX_PROXY_TELEGRAM_BOT_TOKEN and CODEX_PROXY_TELEGRAM_CHAT_ID"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, telegramAPIBase+"/bot"+cfg.BotToken+"/getMe", nil)
	if err != nil {
		return doctorCheck{Name: "Telegram", OK: false, Detail: err.Error()}
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return doctorCheck{Name: "Telegram", OK: false, Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return doctorCheck{Name: "Telegram", OK: false, Detail: fmt.Sprintf("getMe returned %d", resp.StatusCode)}
	}
	return doctorCheck{Name: "Telegram", OK: true, Detail: "bot token reachable; chat_id configured"}
}

func doctorServiceCheck() doctorCheck {
	switch runtime.GOOS {
	case "linux":
		if !unitFileExists() {
			return doctorCheck{Name: "Service", OK: false, Detail: "systemd unit missing; run codex-proxy install"}
		}
		if err := exec.Command("systemctl", "--user", "is-active", "--quiet", serviceName).Run(); err != nil {
			return doctorCheck{Name: "Service", OK: false, Detail: "systemd unit installed but not active"}
		}
		return doctorCheck{Name: "Service", OK: true, Detail: "systemd user service active"}
	case "darwin":
		if _, err := os.Stat(launchAgentPath()); err != nil {
			return doctorCheck{Name: "Service", OK: false, Detail: "launchd agent missing; run codex-proxy install"}
		}
		if !launchdServiceLoaded() {
			return doctorCheck{Name: "Service", OK: false, Detail: "launchd agent installed but not loaded"}
		}
		return doctorCheck{Name: "Service", OK: true, Detail: "launchd agent loaded"}
	default:
		return doctorCheck{Name: "Service", OK: false, Detail: "unsupported OS: " + runtime.GOOS}
	}
}

func doctorCaddyCheck() doctorCheck {
	if _, err := exec.LookPath("caddy"); err != nil {
		return doctorCheck{Name: "Caddy", OK: false, Detail: "caddy not found in PATH"}
	}
	if _, err := os.Stat("/etc/caddy/Caddyfile"); err != nil {
		return doctorCheck{Name: "Caddy", OK: false, Detail: "/etc/caddy/Caddyfile missing or unreadable"}
	}
	return doctorCheck{Name: "Caddy", OK: true, Detail: "caddy binary and /etc/caddy/Caddyfile found"}
}

func formatDoctorCheck(check doctorCheck) string {
	icon := "✗"
	if check.OK {
		icon = "✓"
	}
	return fmt.Sprintf("  %s %-12s %s", icon, check.Name+":", check.Detail)
}
