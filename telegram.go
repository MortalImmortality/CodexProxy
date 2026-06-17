package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"codex-proxy/auth"
	"codex-proxy/proxy"
)

const telegramAPIBase = "https://api.telegram.org"

type telegramConfig struct {
	BotToken string
	ChatID   int64
	Enabled  bool
}

type telegramBot struct {
	cfg        telegramConfig
	client     *http.Client
	apiBase    string
	nextOffset int64
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramMessage struct {
	Text string       `json:"text"`
	Chat telegramChat `json:"chat"`
	From *struct {
		Username string `json:"username"`
	} `json:"from"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

func telegramConfigFromEnv() (telegramConfig, error) {
	token := strings.TrimSpace(os.Getenv("CODEX_PROXY_TELEGRAM_BOT_TOKEN"))
	chatIDRaw := strings.TrimSpace(os.Getenv("CODEX_PROXY_TELEGRAM_CHAT_ID"))
	if token == "" && chatIDRaw == "" {
		return telegramConfig{}, nil
	}
	if token == "" || chatIDRaw == "" {
		return telegramConfig{}, fmt.Errorf("both CODEX_PROXY_TELEGRAM_BOT_TOKEN and CODEX_PROXY_TELEGRAM_CHAT_ID are required")
	}
	chatID, err := strconv.ParseInt(chatIDRaw, 10, 64)
	if err != nil {
		return telegramConfig{}, fmt.Errorf("invalid CODEX_PROXY_TELEGRAM_CHAT_ID: %w", err)
	}
	return telegramConfig{BotToken: token, ChatID: chatID, Enabled: true}, nil
}

func startTelegramMonitor(ctx context.Context) {
	cfg, err := telegramConfigFromEnv()
	if err != nil {
		slog.Error("telegram monitor disabled", "error", err)
		return
	}
	if !cfg.Enabled {
		return
	}
	bot := &telegramBot{
		cfg:     cfg,
		client:  &http.Client{Timeout: 35 * time.Second},
		apiBase: telegramAPIBase,
	}
	go bot.run(ctx)
}

func (b *telegramBot) run(ctx context.Context) {
	slog.Info("telegram monitor started", "chat_id", b.cfg.ChatID)
	if err := b.sendMessage(ctx, b.cfg.ChatID, "codex-proxy started\n\n"+telegramHelpText()); err != nil {
		slog.Warn("telegram startup notification failed", "error", err)
	}
	for {
		updates, err := b.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("telegram getUpdates failed", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		for _, update := range updates {
			if update.UpdateID >= b.nextOffset {
				b.nextOffset = update.UpdateID + 1
			}
			b.handleUpdate(ctx, update)
		}
	}
}

func (b *telegramBot) getUpdates(ctx context.Context) ([]telegramUpdate, error) {
	values := url.Values{}
	values.Set("timeout", "30")
	if b.nextOffset > 0 {
		values.Set("offset", strconv.FormatInt(b.nextOffset, 10))
	}
	endpoint := b.apiURL("getUpdates") + "?" + values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram getUpdates returned %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}
	var result struct {
		OK     bool             `json:"ok"`
		Result []telegramUpdate `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram getUpdates returned ok=false")
	}
	return result.Result, nil
}

func (b *telegramBot) handleUpdate(ctx context.Context, update telegramUpdate) {
	if update.Message == nil {
		return
	}
	msg := update.Message
	if msg.Chat.ID != b.cfg.ChatID {
		username := ""
		if msg.From != nil {
			username = msg.From.Username
		}
		slog.Warn("ignored telegram message from unauthorized chat", "chat_id", msg.Chat.ID, "username", username)
		return
	}
	text := b.commandResponse(strings.TrimSpace(msg.Text))
	if text == "" {
		return
	}
	if err := b.sendMessage(ctx, msg.Chat.ID, text); err != nil {
		slog.Warn("telegram sendMessage failed", "error", err)
	}
}

func (b *telegramBot) commandResponse(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	cmd := strings.ToLower(fields[0])
	if i := strings.Index(cmd, "@"); i >= 0 {
		cmd = cmd[:i]
	}
	switch cmd {
	case "/start", "/help":
		return telegramHelpText()
	case "/status":
		return telegramStatusText()
	case "/metrics":
		return telegramMetricsText()
	case "/usage":
		return telegramUsageText()
	case "/models":
		return telegramModelsText()
	default:
		return "Unknown command.\n\n" + telegramHelpText()
	}
}

func telegramHelpText() string {
	return strings.Join([]string{
		"Available commands:",
		"/status - auth and proxy health",
		"/usage - account usage",
		"/metrics - proxy counters",
		"/models - available models",
		"/help - show this help",
	}, "\n")
}

func telegramStatusText() string {
	healthy, reason := auth.Pool.IsHealthy()
	state := "ok"
	if !healthy {
		state = "degraded"
	}
	return fmt.Sprintf("Status: %s\nAuth: %s", state, reason)
}

func telegramMetricsText() string {
	m := proxy.SnapshotMetrics()
	return fmt.Sprintf(strings.Join([]string{
		"Metrics:",
		"requests_total: %d",
		"requests_active: %d",
		"errors_total: %d",
		"retries: %d",
		"token_refreshes: %d",
		"uptime_seconds: %d",
	}, "\n"), m.RequestsTotal, m.RequestsActive, m.ErrorsTotal, m.Retries, m.TokenRefreshes, m.UptimeSeconds)
}

func telegramUsageText() string {
	managers := auth.Pool.Managers()
	if len(managers) == 0 {
		return "Usage: no accounts configured"
	}
	var lines []string
	lines = append(lines, "Usage:")
	for _, tm := range managers {
		token := tm.GetAccessToken()
		if token == "" {
			lines = append(lines, fmt.Sprintf("[%s] no token", tm.Name()))
			continue
		}
		info, err := auth.QueryUsage(token)
		if err != nil {
			lines = append(lines, fmt.Sprintf("[%s] error: %v", tm.Name(), err))
			continue
		}
		label := tm.Name()
		if info.Email != "" {
			label = info.Email
		}
		status := "allowed"
		if info.LimitHit {
			status = "limit reached"
		} else if !info.Allowed {
			status = "not allowed"
		}
		lines = append(lines, fmt.Sprintf("[%s] %s plan=%s", label, status, info.PlanType))
		if len(info.Windows) == 0 {
			lines = append(lines, "  no rate limit windows")
		}
		for _, w := range info.Windows {
			lines = append(lines, fmt.Sprintf("  %s: %d%% reset=%s", w.Name, w.UsedPercent, formatReset(w.ResetSecs)))
		}
	}
	return strings.Join(lines, "\n")
}

func telegramModelsText() string {
	handle, err := auth.Pool.Acquire()
	if err != nil {
		return "Models error: " + err.Error()
	}
	models, err := auth.DiscoverModels(handle.Token)
	if err != nil {
		return "Models error: " + err.Error()
	}
	if len(models) == 0 {
		return "Models: none"
	}
	models = append(models, "gpt-image-2")
	return "Models:\n" + strings.Join(models, "\n")
}

func (b *telegramBot) sendMessage(ctx context.Context, chatID int64, text string) error {
	payload, err := json.Marshal(map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL("sendMessage"), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram sendMessage returned %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram sendMessage returned ok=false")
	}
	return nil
}

func (b *telegramBot) apiURL(method string) string {
	return strings.TrimRight(b.apiBase, "/") + "/bot" + b.cfg.BotToken + "/" + method
}
