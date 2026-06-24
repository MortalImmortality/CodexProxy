package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-proxy/auth"
	"codex-proxy/proxy"
)

func TestTelegramConfigFromEnv(t *testing.T) {
	t.Setenv("CODEX_PROXY_TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("CODEX_PROXY_TELEGRAM_CHAT_ID", "-100123")

	cfg, err := telegramConfigFromEnv()
	if err != nil {
		t.Fatalf("telegramConfigFromEnv: %v", err)
	}
	if !cfg.Enabled || cfg.BotToken != "token" || cfg.ChatID != -100123 {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestTelegramConfigDisabledWhenUnset(t *testing.T) {
	cfg, err := telegramConfigFromEnv()
	if err != nil {
		t.Fatalf("telegramConfigFromEnv: %v", err)
	}
	if cfg.Enabled {
		t.Fatalf("config enabled unexpectedly: %#v", cfg)
	}
}

func TestTelegramConfigRequiresBothValues(t *testing.T) {
	t.Setenv("CODEX_PROXY_TELEGRAM_BOT_TOKEN", "token")

	if _, err := telegramConfigFromEnv(); err == nil {
		t.Fatal("expected error when chat id is missing")
	}
}

func TestTelegramCommandResponse(t *testing.T) {
	bot := &telegramBot{}

	if got := bot.commandResponse(""); got != "" {
		t.Fatalf("empty command = %q, want empty response", got)
	}
	if got := bot.commandResponse("/help"); !strings.Contains(got, "<code>/status</code>") {
		t.Fatalf("/help response = %q", got)
	}
	if got := bot.commandResponse("/help"); !strings.Contains(got, "<code>/key</code>") || !strings.Contains(got, "<code>/doctor</code>") {
		t.Fatalf("/help response missing new commands = %q", got)
	}
	if got := bot.commandResponse("/unknown"); !strings.Contains(got, "未知命令") {
		t.Fatalf("/unknown response = %q", got)
	}
}

func TestTelegramMessageFormatting(t *testing.T) {
	if got := telegramStartupText(); !strings.Contains(got, "🚀 <b>codex-proxy 已启动</b>") {
		t.Fatalf("startup text = %q", got)
	}
	if got := telegramHelpText(); !strings.Contains(got, "🤖 <b>codex-proxy 监控</b>") {
		t.Fatalf("help text = %q", got)
	}
	if got := telegramMetricsText(); !strings.Contains(got, "📈 <b>运行指标</b>") || !strings.Contains(got, "⏳ <b>Uptime</b>") {
		t.Fatalf("metrics text = %q", got)
	}
	lastErrorRows := telegramLastErrorRows(&proxy.ErrorDetail{
		Time:    time.Date(2026, 6, 23, 12, 30, 0, 0, time.UTC),
		Status:  http.StatusBadGateway,
		Type:    "upstream_error",
		Message: "bad <upstream>",
	})
	if got := strings.Join(lastErrorRows, "\n"); !strings.Contains(got, "状态码：502") || !strings.Contains(got, "bad &lt;upstream&gt;") {
		t.Fatalf("last error rows = %q", got)
	}
	if got := tgEscape("a<b&c"); got != "a&lt;b&amp;c" {
		t.Fatalf("tgEscape = %q", got)
	}
	if got := formatDuration(3661); got != "1h1m" {
		t.Fatalf("formatDuration = %q, want 1h1m", got)
	}
	if got := telegramServiceErrorText(assertErr("bad <err>")); !strings.Contains(got, "bad &lt;err&gt;") {
		t.Fatalf("service error text = %q", got)
	}
	if got := telegramRateLimitAlertText(proxy.UpstreamRateLimitEvent{
		AccountName: "acct <name>",
		AccountID:   "acct_<id>",
		Status:      http.StatusTooManyRequests,
		Message:     "usage <limit> reached",
		ResetAt:     time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC),
	}); !strings.Contains(got, "⛔ <b>账号额度可能已用尽</b>") || !strings.Contains(got, "acct &lt;name&gt;") || !strings.Contains(got, "acct_&lt;id&gt;") || !strings.Contains(got, "usage &lt;limit&gt; reached") || !strings.Contains(got, "预计恢复") {
		t.Fatalf("rate limit alert text = %q", got)
	}
	if got := telegramDoctorTextFromChecks([]doctorCheck{
		{Name: "Auth <file>", OK: true, Detail: "ok"},
		{Name: "Caddy", OK: false, Detail: "missing <conf>"},
	}); !strings.Contains(got, "🩺 <b>部署诊断</b>") || !strings.Contains(got, "1/2") || !strings.Contains(got, "missing &lt;conf&gt;") {
		t.Fatalf("doctor text = %q", got)
	}
}

func TestTelegramKeyText(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_PROXY_API_KEY", "env-secret<1>")
	ks := &KeyStore{Keys: []APIKey{{
		Key:       "cpx-abcdefghijklmnopqrstuvwxyz<2>",
		Name:      "prod <key>",
		CreatedAt: time.Now(),
	}}}
	data, err := json.Marshal(ks)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "keys.json"), data, 0600); err != nil {
		t.Fatalf("write keys: %v", err)
	}

	got := telegramKeyText()
	if !strings.Contains(got, "🔑 <b>API Key 状态</b>") || !strings.Contains(got, "环境变量：<code>") {
		t.Fatalf("key text = %q", got)
	}
	if !strings.Contains(got, "env-secret&lt;1&gt;") || !strings.Contains(got, "cpx-abcdefghijklmnopqrstuvwxyz&lt;2&gt;") {
		t.Fatalf("key text missing full escaped keys = %q", got)
	}
	if !strings.Contains(got, "prod &lt;key&gt;") || !strings.Contains(got, "⚠️ <b>敏感信息</b>") {
		t.Fatalf("key text missing labels = %q", got)
	}
}

func TestTelegramHealthAlert(t *testing.T) {
	oldPool := auth.Pool
	t.Cleanup(func() { auth.Pool = oldPool })
	auth.Pool = auth.NewTokenPool(nil, "round-robin")

	bot := &telegramBot{
		alertCooldown: time.Minute,
		lastAlerts:    map[string]time.Time{},
		alertState: telegramAlertState{
			initialized: true,
			healthy:     true,
			reason:      "all 1 accounts healthy",
			metrics:     proxy.SnapshotMetrics(),
		},
	}

	alerts := bot.checkAlerts(time.Now())
	if len(alerts) != 1 {
		t.Fatalf("alerts = %#v, want one health alert", alerts)
	}
	if !strings.Contains(alerts[0], "🔴 <b>服务异常</b>") {
		t.Fatalf("health alert = %q", alerts[0])
	}
}

func TestTelegramMetricAlertCooldown(t *testing.T) {
	oldPool := auth.Pool
	t.Cleanup(func() { auth.Pool = oldPool })
	auth.Pool = auth.NewTokenPool(nil, "round-robin")
	healthy, reason := auth.Pool.IsHealthy()
	current := proxy.SnapshotMetrics()
	baseline := current
	baseline.ErrorsTotal = current.ErrorsTotal - 1

	bot := &telegramBot{
		alertCooldown: time.Minute,
		lastAlerts:    map[string]time.Time{},
		alertState: telegramAlertState{
			initialized: true,
			healthy:     healthy,
			reason:      reason,
			metrics:     baseline,
		},
	}

	now := time.Now()
	alerts := bot.checkAlerts(now)
	if len(alerts) != 1 || !strings.Contains(alerts[0], "服务错误增加") {
		t.Fatalf("alerts = %#v, want one error alert", alerts)
	}

	bot.alertState.metrics = baseline
	alerts = bot.checkAlerts(now.Add(30 * time.Second))
	if len(alerts) != 0 {
		t.Fatalf("alerts within cooldown = %#v, want none", alerts)
	}
}

func TestTelegramTokenRefreshAlert(t *testing.T) {
	oldPool := auth.Pool
	t.Cleanup(func() { auth.Pool = oldPool })
	auth.Pool = auth.NewTokenPool(nil, "round-robin")
	healthy, reason := auth.Pool.IsHealthy()
	current := proxy.SnapshotMetrics()
	baseline := current
	baseline.TokenRefreshes = current.TokenRefreshes - 1

	bot := &telegramBot{
		alertCooldown: time.Minute,
		lastAlerts:    map[string]time.Time{},
		alertState: telegramAlertState{
			initialized: true,
			healthy:     healthy,
			reason:      reason,
			metrics:     baseline,
		},
	}

	alerts := bot.checkAlerts(time.Now())
	if len(alerts) != 1 || !strings.Contains(alerts[0], "Token refresh 发生") {
		t.Fatalf("alerts = %#v, want token refresh alert", alerts)
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

func TestTelegramIgnoresUnauthorizedChat(t *testing.T) {
	sent := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = true
		t.Fatalf("unexpected telegram API call: %s", r.URL.Path)
	}))
	defer server.Close()

	bot := &telegramBot{
		cfg:     telegramConfig{BotToken: "token", ChatID: 1, Enabled: true},
		client:  server.Client(),
		apiBase: server.URL,
	}
	bot.handleUpdate(context.Background(), telegramUpdate{
		UpdateID: 10,
		Message: &telegramMessage{
			Text: "/help",
			Chat: telegramChat{ID: 2},
		},
	})

	if sent {
		t.Fatal("unauthorized chat should not receive a response")
	}
}

func TestTelegramSendMessage(t *testing.T) {
	var got struct {
		ChatID                int64  `json:"chat_id"`
		Text                  string `json:"text"`
		ParseMode             string `json:"parse_mode"`
		DisableWebPagePreview bool   `json:"disable_web_page_preview"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottoken/sendMessage" {
			t.Fatalf("path = %s, want /bottoken/sendMessage", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()

	bot := &telegramBot{
		cfg:     telegramConfig{BotToken: "token", ChatID: 123, Enabled: true},
		client:  server.Client(),
		apiBase: server.URL,
	}
	if err := bot.sendMessage(context.Background(), 123, "hello"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if got.ChatID != 123 || got.Text != "hello" {
		t.Fatalf("payload = %#v", got)
	}
	if got.ParseMode != "HTML" || !got.DisableWebPagePreview {
		t.Fatalf("payload formatting fields = %#v", got)
	}
}

func TestTelegramGetUpdates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottoken/getUpdates" {
			t.Fatalf("path = %s, want /bottoken/getUpdates", r.URL.Path)
		}
		if r.URL.Query().Get("offset") != "42" {
			t.Fatalf("offset = %s, want 42", r.URL.Query().Get("offset"))
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":42,"message":{"text":"/help","chat":{"id":123}}}]}`))
	}))
	defer server.Close()

	bot := &telegramBot{
		cfg:        telegramConfig{BotToken: "token", ChatID: 123, Enabled: true},
		client:     server.Client(),
		apiBase:    server.URL,
		nextOffset: 42,
	}
	updates, err := bot.getUpdates(context.Background())
	if err != nil {
		t.Fatalf("getUpdates: %v", err)
	}
	if len(updates) != 1 || updates[0].UpdateID != 42 || updates[0].Message.Chat.ID != 123 {
		t.Fatalf("updates = %#v", updates)
	}
}
