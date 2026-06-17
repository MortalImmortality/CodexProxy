package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
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
const telegramAlertInterval = 60 * time.Second
const telegramAlertCooldown = 5 * time.Minute

type telegramConfig struct {
	BotToken string
	ChatID   int64
	Enabled  bool
}

type telegramBot struct {
	cfg           telegramConfig
	client        *http.Client
	apiBase       string
	nextOffset    int64
	alertInterval time.Duration
	alertCooldown time.Duration
	alertState    telegramAlertState
	lastAlerts    map[string]time.Time
}

type telegramAlertState struct {
	initialized bool
	healthy     bool
	reason      string
	metrics     proxy.MetricsSnapshot
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

func startTelegramMonitor(ctx context.Context) *telegramBot {
	cfg, err := telegramConfigFromEnv()
	if err != nil {
		slog.Error("telegram monitor disabled", "error", err)
		return nil
	}
	if !cfg.Enabled {
		return nil
	}
	bot := &telegramBot{
		cfg:           cfg,
		client:        &http.Client{Timeout: 35 * time.Second},
		apiBase:       telegramAPIBase,
		alertInterval: telegramAlertInterval,
		alertCooldown: telegramAlertCooldown,
		lastAlerts:    map[string]time.Time{},
	}
	go bot.run(ctx)
	return bot
}

func (b *telegramBot) run(ctx context.Context) {
	slog.Info("telegram monitor started", "chat_id", b.cfg.ChatID)
	if err := b.sendMessage(ctx, b.cfg.ChatID, telegramStartupText()); err != nil {
		slog.Warn("telegram startup notification failed", "error", err)
	}
	b.initAlertState()
	go b.runAlerts(ctx)
	b.runUpdates(ctx)
}

func (b *telegramBot) runUpdates(ctx context.Context) {
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

func (b *telegramBot) runAlerts(ctx context.Context) {
	ticker := time.NewTicker(b.alertInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, alert := range b.checkAlerts(time.Now()) {
				if err := b.sendMessage(ctx, b.cfg.ChatID, alert); err != nil {
					slog.Warn("telegram alert send failed", "error", err)
				}
			}
		}
	}
}

func (b *telegramBot) initAlertState() {
	healthy, reason := auth.Pool.IsHealthy()
	b.alertState = telegramAlertState{
		initialized: true,
		healthy:     healthy,
		reason:      reason,
		metrics:     proxy.SnapshotMetrics(),
	}
}

func (b *telegramBot) checkAlerts(now time.Time) []string {
	if !b.alertState.initialized {
		b.initAlertState()
		return nil
	}

	var alerts []string
	healthy, reason := auth.Pool.IsHealthy()
	if healthy != b.alertState.healthy || reason != b.alertState.reason {
		switch {
		case !healthy:
			alerts = append(alerts, telegramHealthAlertText(false, reason))
		case !b.alertState.healthy && healthy:
			alerts = append(alerts, telegramHealthAlertText(true, reason))
		}
		b.alertState.healthy = healthy
		b.alertState.reason = reason
	}

	current := proxy.SnapshotMetrics()
	if delta := current.ErrorsTotal - b.alertState.metrics.ErrorsTotal; delta > 0 && b.shouldSendAlert("errors", now) {
		alerts = append(alerts, telegramMetricAlertText("🔴", "服务错误增加", []string{
			fmt.Sprintf("• 新增错误：%d", delta),
			fmt.Sprintf("• 错误总数：%d", current.ErrorsTotal),
			fmt.Sprintf("• 请求总数：%d", current.RequestsTotal),
		}))
	}
	if delta := current.Retries - b.alertState.metrics.Retries; delta > 0 && b.shouldSendAlert("retries", now) {
		alerts = append(alerts, telegramMetricAlertText("🧯", "上游重试增加", []string{
			fmt.Sprintf("• 新增重试：%d", delta),
			fmt.Sprintf("• 重试总数：%d", current.Retries),
			fmt.Sprintf("• 错误总数：%d", current.ErrorsTotal),
		}))
	}
	if delta := current.TokenRefreshes - b.alertState.metrics.TokenRefreshes; delta > 0 && b.shouldSendAlert("token_refreshes", now) {
		alerts = append(alerts, telegramMetricAlertText("🔐", "Token refresh 发生", []string{
			fmt.Sprintf("• 新增 refresh：%d", delta),
			fmt.Sprintf("• refresh 总数：%d", current.TokenRefreshes),
			fmt.Sprintf("• Auth：%s", tgEscape(reason)),
		}))
	}
	b.alertState.metrics = current
	return alerts
}

func (b *telegramBot) shouldSendAlert(key string, now time.Time) bool {
	if b.lastAlerts == nil {
		b.lastAlerts = map[string]time.Time{}
	}
	if last, ok := b.lastAlerts[key]; ok && now.Sub(last) < b.alertCooldown {
		return false
	}
	b.lastAlerts[key] = now
	return true
}

func (b *telegramBot) sendServiceError(err error) {
	if b == nil || err == nil {
		return
	}
	alertCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if sendErr := b.sendMessage(alertCtx, b.cfg.ChatID, telegramServiceErrorText(err)); sendErr != nil {
		slog.Warn("telegram service error alert failed", "error", sendErr)
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
		return "❔ <b>未知命令</b>\n\n" + telegramHelpText()
	}
}

func telegramStartupText() string {
	return strings.Join([]string{
		"🚀 <b>codex-proxy 已启动</b>",
		"",
		"📍 <b>服务</b>",
		"• 状态：运行中",
		"• 监控：Telegram 已连接",
		"",
		telegramHelpText(),
	}, "\n")
}

func telegramHelpText() string {
	return strings.Join([]string{
		"🤖 <b>codex-proxy 监控</b>",
		"",
		"<code>/status</code>   健康状态",
		"<code>/usage</code>    账号用量",
		"<code>/metrics</code>  运行指标",
		"<code>/models</code>   可用模型",
		"<code>/help</code>     帮助",
	}, "\n")
}

func telegramStatusText() string {
	healthy, reason := auth.Pool.IsHealthy()
	icon := "🟢"
	title := "状态正常"
	proxyState := "ok"
	if !healthy {
		icon = "🟡"
		title = "状态异常"
		proxyState = "degraded"
	}
	return fmt.Sprintf(strings.Join([]string{
		"%s <b>%s</b>",
		"",
		"🔐 <b>Auth</b>",
		"• 账号：%s",
		"• 代理：%s",
	}, "\n"), icon, title, tgEscape(reason), proxyState)
}

func telegramMetricsText() string {
	m := proxy.SnapshotMetrics()
	return fmt.Sprintf(strings.Join([]string{
		"📈 <b>运行指标</b>",
		"",
		"🔁 <b>请求</b>",
		"• 总数：%d",
		"• 活跃：%d",
		"• 错误：%d",
		"",
		"🧯 <b>重试</b>",
		"• 上游重试：%d",
		"• Token refresh：%d",
		"",
		"⏳ <b>Uptime</b>",
		"• %s",
	}, "\n"), m.RequestsTotal, m.RequestsActive, m.ErrorsTotal, m.Retries, m.TokenRefreshes, formatDuration(m.UptimeSeconds))
}

func telegramUsageText() string {
	managers := auth.Pool.Managers()
	if len(managers) == 0 {
		return "📊 <b>账号用量</b>\n\n• 未配置账号"
	}
	var lines []string
	lines = append(lines, "📊 <b>账号用量</b>")
	for _, tm := range managers {
		lines = append(lines, "")
		token := tm.GetAccessToken()
		if token == "" {
			lines = append(lines, fmt.Sprintf("👤 <b>%s</b>", tgEscape(tm.Name())))
			lines = append(lines, "• 状态：⚠️ no token")
			continue
		}
		info, err := auth.QueryUsage(token)
		if err != nil {
			lines = append(lines, fmt.Sprintf("👤 <b>%s</b>", tgEscape(tm.Name())))
			lines = append(lines, fmt.Sprintf("• 状态：⚠️ %s", tgEscape(err.Error())))
			continue
		}
		label := tm.Name()
		if info.Email != "" {
			label = info.Email
		}
		status := "✅ allowed"
		if info.LimitHit {
			status = "⛔ limit reached"
		} else if !info.Allowed {
			status = "⚠️ not allowed"
		}
		lines = append(lines, fmt.Sprintf("👤 <b>%s</b>", tgEscape(label)))
		lines = append(lines, fmt.Sprintf("• 状态：%s", status))
		lines = append(lines, fmt.Sprintf("• 计划：%s", tgEscape(info.PlanType)))
		if len(info.Windows) == 0 {
			lines = append(lines, "• 无 rate limit 窗口")
		}
		for _, w := range info.Windows {
			lines = append(lines, "")
			lines = append(lines, fmt.Sprintf("⏱ <b>%s</b>", tgEscape(w.Name)))
			lines = append(lines, fmt.Sprintf("• 使用：%d%%", w.UsedPercent))
			lines = append(lines, fmt.Sprintf("• 重置：%s", formatReset(w.ResetSecs)))
		}
	}
	return strings.Join(lines, "\n")
}

func telegramModelsText() string {
	handle, err := auth.Pool.Acquire()
	if err != nil {
		return "🧠 <b>可用模型</b>\n\n• 错误：" + tgEscape(err.Error())
	}
	models, err := auth.DiscoverModels(handle.Token)
	if err != nil {
		return "🧠 <b>可用模型</b>\n\n• 错误：" + tgEscape(err.Error())
	}
	if len(models) == 0 {
		return "🧠 <b>可用模型</b>\n\n• 暂无"
	}
	models = append(models, "gpt-image-2")
	lines := []string{"🧠 <b>可用模型</b>", ""}
	for _, model := range models {
		lines = append(lines, "• <code>"+tgEscape(model)+"</code>")
	}
	return strings.Join(lines, "\n")
}

func telegramHealthAlertText(recovered bool, reason string) string {
	if recovered {
		return strings.Join([]string{
			"🟢 <b>服务恢复</b>",
			"",
			"🔐 <b>Auth</b>",
			"• 状态：healthy",
			"• 账号：" + tgEscape(reason),
		}, "\n")
	}
	return strings.Join([]string{
		"🔴 <b>服务异常</b>",
		"",
		"🔐 <b>Auth</b>",
		"• 状态：degraded",
		"• 原因：" + tgEscape(reason),
	}, "\n")
}

func telegramMetricAlertText(icon, title string, rows []string) string {
	lines := []string{
		icon + " <b>" + tgEscape(title) + "</b>",
		"",
		"📈 <b>运行指标</b>",
	}
	lines = append(lines, rows...)
	return strings.Join(lines, "\n")
}

func telegramServiceErrorText(err error) string {
	return strings.Join([]string{
		"🛑 <b>服务退出</b>",
		"",
		"📍 <b>进程</b>",
		"• 状态：proxy server stopped",
		"• 错误：" + tgEscape(err.Error()),
	}, "\n")
}

func (b *telegramBot) sendMessage(ctx context.Context, chatID int64, text string) error {
	payload, err := json.Marshal(map[string]interface{}{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
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

func tgEscape(s string) string {
	return html.EscapeString(s)
}

func formatDuration(seconds int) string {
	if seconds <= 0 {
		return "0s"
	}
	d := time.Duration(seconds) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
