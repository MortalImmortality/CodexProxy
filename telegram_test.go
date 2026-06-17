package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if got := bot.commandResponse("/help"); !strings.Contains(got, "/status") {
		t.Fatalf("/help response = %q", got)
	}
	if got := bot.commandResponse("/unknown"); !strings.Contains(got, "Unknown command") {
		t.Fatalf("/unknown response = %q", got)
	}
}

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
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
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
