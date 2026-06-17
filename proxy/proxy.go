package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"codex-proxy/auth"
)

const (
	upstreamBase       = "https://chatgpt.com/backend-api/codex"
	maxRequestBodySize = 10 << 20 // 10 MB
	maxRetries         = 2
	maxSSEEventSize    = 32 << 20 // 32 MB
)

var (
	normalClient = &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        20,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	streamClient = &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			MaxIdleConns:          20,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}

	startTime = time.Now()
)

// ──────────────────────────────────────────────
// Metrics
// ──────────────────────────────────────────────

var stats struct {
	requestsTotal  atomic.Int64
	requestsActive atomic.Int64
	errorsTotal    atomic.Int64
	retries        atomic.Int64
	tokenRefreshes atomic.Int64
}

type MetricsSnapshot struct {
	RequestsTotal  int64 `json:"requests_total"`
	RequestsActive int64 `json:"requests_active"`
	ErrorsTotal    int64 `json:"errors_total"`
	Retries        int64 `json:"retries"`
	TokenRefreshes int64 `json:"token_refreshes"`
	UptimeSeconds  int   `json:"uptime_seconds"`
}

func SnapshotMetrics() MetricsSnapshot {
	return MetricsSnapshot{
		RequestsTotal:  stats.requestsTotal.Load(),
		RequestsActive: stats.requestsActive.Load(),
		ErrorsTotal:    stats.errorsTotal.Load(),
		Retries:        stats.retries.Load(),
		TokenRefreshes: stats.tokenRefreshes.Load(),
		UptimeSeconds:  int(time.Since(startTime).Seconds()),
	}
}

// ──────────────────────────────────────────────
// Serve starts the OpenAI-compatible API proxy
// ──────────────────────────────────────────────

type KeyValidator func(string) bool

func Serve(ctx context.Context, host, port string, validateKey KeyValidator) error {
	handle, err := auth.Pool.Acquire()
	if err != nil {
		return fmt.Errorf("cannot start proxy: %w", err)
	}

	models, err := auth.DiscoverModels(handle.Token)
	if err != nil {
		slog.Warn("model discovery failed; /v1/models will be empty and image generation unavailable until it succeeds", "error", err)
		models = nil
	}

	// baseModel for image requests is models[0] (a real chat model). If
	// discovery failed it stays empty and the image handlers reject requests
	// rather than guessing a model name.
	baseModel := ""
	if len(models) > 0 {
		baseModel = models[0]
	}
	listedModels := append([]string{}, models...)
	if baseModel != "" {
		listedModels = append(listedModels, "gpt-image-2")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/v1/messages", handleAnthropicMessages)
	mux.HandleFunc("/v1/images/generations", makeImageHandler(baseModel))
	mux.HandleFunc("/v1/images/edits", makeImageEditHandler(baseModel))
	mux.HandleFunc("/usage", handleUsage)
	mux.HandleFunc("/v1/responses", handleResponses)
	mux.HandleFunc("/v1/models", makeModelsHandler(listedModels))

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		healthy, reason := auth.Pool.IsHealthy()
		w.Header().Set("Content-Type", "application/json")
		status := "ok"
		httpCode := 200
		if !healthy {
			status = "degraded"
			httpCode = 503
		}
		w.WriteHeader(httpCode)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": status,
			"auth":   reason,
		})
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SnapshotMetrics())
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"service":           "codex-proxy",
			"openai_compatible": true,
			"endpoints": []string{
				"/v1/chat/completions",
				"/v1/messages",
				"/v1/images/generations",
				"/v1/images/edits",
				"/v1/responses",
				"/v1/models",
				"/health",
				"/metrics",
			},
		})
	})

	addr := fmt.Sprintf("%s:%s", host, port)
	fmt.Println()
	fmt.Printf("  ╭──────────────────────────────────────────────────╮\n")
	fmt.Printf("  │  Codex OAuth Proxy ready                        │\n")
	fmt.Printf("  │  Endpoint: http://%-30s │\n", addr+"/v1")
	fmt.Printf("  │  Models:   %-30s       │\n", strings.Join(models[:min(3, len(models))], ", "))
	fmt.Printf("  ╰──────────────────────────────────────────────────╯\n")
	fmt.Println()
	fmt.Printf("  Auth:     API key required\n")
	fmt.Println()
	fmt.Println("  Use with any OpenAI SDK:")
	fmt.Printf("    export OPENAI_BASE_URL=http://%s/v1\n", addr)
	fmt.Println("    export OPENAI_API_KEY=<your-api-key>")
	fmt.Println()

	server := &http.Server{
		Addr:         addr,
		Handler:      withLogging(withCORS(withAuth(validateKey, mux))),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down gracefully...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
			return err
		}
		slog.Info("server stopped")
		return nil
	case err := <-errCh:
		return err
	}
}

// ──────────────────────────────────────────────
// Upstream call with retry (401 refresh + 429/5xx backoff)
// ──────────────────────────────────────────────

func callUpstream(ctx context.Context, upstreamURL string, body []byte, isStreaming bool) (*http.Response, error) {
	handle, err := auth.Pool.Acquire()
	if err != nil {
		return nil, err
	}
	token := handle.Token

	client := normalClient
	if isStreaming {
		client = streamClient
	}

	accountID := handle.AccountID
	sessionID := newUUID()
	refreshed := false
	var resp *http.Response

	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "codex-proxy/1.0")
		// Headers the Codex CLI sends; backend expects the Responses beta flag,
		// originator, a session id, and SSE Accept. account-id when known.
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("session_id", sessionID)
		req.Header.Set("Accept", "text/event-stream")
		if accountID != "" {
			req.Header.Set("chatgpt-account-id", accountID)
		}

		resp, err = client.Do(req)
		if err != nil {
			return nil, err
		}

		switch {
		case resp.StatusCode == 401 && !refreshed:
			resp.Body.Close()
			slog.Warn("upstream 401, refreshing token",
				"account", handle.Manager.Name())
			stats.tokenRefreshes.Add(1)
			token, err = handle.Refresh()
			if err != nil {
				handle2, err2 := auth.Pool.Acquire()
				if err2 != nil {
					return nil, fmt.Errorf("refresh failed: %w; fallback: %w", err, err2)
				}
				handle = handle2
				token = handle.Token
				accountID = handle.AccountID
			}
			accountID = handle.AccountID
			refreshed = true
			stats.retries.Add(1)
			continue

		case resp.StatusCode == 429 || resp.StatusCode >= 500:
			if attempt < maxRetries {
				delay := retryDelay(resp, attempt)
				slog.Warn("upstream error, retrying",
					"status", resp.StatusCode,
					"attempt", attempt+1,
					"delay", delay)
				resp.Body.Close()
				stats.retries.Add(1)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
				continue
			}
			return resp, nil

		default:
			return resp, nil
		}
	}

	return resp, nil
}

// newUUID returns a random RFC-4122 v4 UUID string for the session_id header.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func retryDelay(resp *http.Response, attempt int) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			d := time.Duration(secs) * time.Second
			if d > 60*time.Second {
				d = 60 * time.Second
			}
			return d
		}
	}
	return time.Duration(1<<attempt) * time.Second
}

// ──────────────────────────────────────────────
// /v1/chat/completions → Codex /responses
// ──────────────────────────────────────────────

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "POST only")
		return
	}

	stats.requestsTotal.Add(1)
	stats.requestsActive.Add(1)
	defer stats.requestsActive.Add(-1)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 400, "bad_request", "cannot read request body (max 10MB)")
		return
	}

	var chatReq map[string]interface{}
	if err := json.Unmarshal(body, &chatReq); err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 400, "bad_request", "invalid JSON")
		return
	}

	clientWantsStream, _ := chatReq["stream"].(bool)
	model, _ := chatReq["model"].(string)
	includeUsage := false
	if so, ok := chatReq["stream_options"].(map[string]interface{}); ok {
		includeUsage, _ = so["include_usage"].(bool)
	}

	// The Codex backend only emits SSE for /responses, so always stream
	// upstream. For a non-streaming client we aggregate the SSE into one JSON
	// chat.completion below.
	chatReq["stream"] = true
	codexBody, err := auth.BuildCodexRequestBody(chatReq)
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 500, "internal", "failed to build upstream request")
		return
	}

	resp, err := callUpstream(r.Context(), upstreamBase+"/responses", codexBody, true)
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		stats.errorsTotal.Add(1)
		respBody, _ := io.ReadAll(resp.Body)
		slog.Error("upstream error",
			"status", resp.StatusCode,
			"body", string(respBody[:min(500, len(respBody))]))
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	if clientWantsStream {
		streamChatCompletion(w, resp, model, includeUsage)
	} else {
		respObj, err := aggregateCodexResponse(resp.Body)
		if err != nil {
			stats.errorsTotal.Add(1)
			writeError(w, 502, "upstream_error", err.Error())
			return
		}
		if respObj == nil {
			stats.errorsTotal.Add(1)
			writeError(w, 502, "upstream_error", "no response from upstream")
			return
		}
		converted := convertToOpenAIFormat(respObj, chatReq)
		w.Header().Set("Content-Type", "application/json")
		w.Write(converted)
	}
}

// ──────────────────────────────────────────────
// /v1/messages → Anthropic-compatible Messages API
// ──────────────────────────────────────────────

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "POST only")
		return
	}

	stats.requestsTotal.Add(1)
	stats.requestsActive.Add(1)
	defer stats.requestsActive.Add(-1)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 400, "bad_request", "cannot read request body (max 10MB)")
		return
	}

	var anthropicReq map[string]interface{}
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 400, "bad_request", "invalid JSON")
		return
	}

	clientWantsStream, _ := anthropicReq["stream"].(bool)
	clientModel, _ := anthropicReq["model"].(string)
	chatReq := anthropicToChatRequest(anthropicReq)

	// Codex /responses emits SSE; aggregate for non-streaming Anthropic
	// clients and translate to Anthropic SSE for streaming clients.
	chatReq["stream"] = true
	codexBody, err := auth.BuildCodexRequestBody(chatReq)
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 500, "internal", "failed to build upstream request")
		return
	}

	resp, err := callUpstream(r.Context(), upstreamBase+"/responses", codexBody, true)
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		stats.errorsTotal.Add(1)
		respBody, _ := io.ReadAll(resp.Body)
		slog.Error("upstream error",
			"status", resp.StatusCode,
			"body", string(respBody[:min(500, len(respBody))]))
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	if clientWantsStream {
		streamAnthropicMessages(w, resp, clientModel)
		return
	}

	respObj, err := aggregateCodexResponse(resp.Body)
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	if respObj == nil {
		stats.errorsTotal.Add(1)
		writeError(w, 502, "upstream_error", "no response from upstream")
		return
	}
	converted := convertToAnthropicFormat(respObj, clientModel)
	w.Header().Set("Content-Type", "application/json")
	w.Write(converted)
}

func anthropicToChatRequest(req map[string]interface{}) map[string]interface{} {
	model, _ := req["model"].(string)
	chatReq := map[string]interface{}{
		"model":    anthropicUpstreamModel(model),
		"messages": anthropicMessagesToChat(req),
	}
	for _, key := range []string{"temperature", "top_p", "stop"} {
		if v, ok := req[key]; ok {
			chatReq[key] = v
		}
	}
	if v, ok := req["max_tokens"]; ok {
		chatReq["max_tokens"] = v
	}
	if tools, ok := req["tools"].([]interface{}); ok {
		chatReq["tools"] = anthropicToolsToChat(tools)
	}
	if toolChoice, ok := req["tool_choice"]; ok {
		if converted := anthropicToolChoiceToChat(toolChoice); converted != nil {
			chatReq["tool_choice"] = converted
		}
	}
	return chatReq
}

func anthropicUpstreamModel(model string) string {
	if model == "" || strings.HasPrefix(model, "claude-") {
		return "gpt-5.4"
	}
	return model
}

func anthropicMessagesToChat(req map[string]interface{}) []interface{} {
	var messages []interface{}
	if system, ok := req["system"]; ok {
		if text := anthropicSystemText(system); text != "" {
			messages = append(messages, map[string]interface{}{
				"role":    "system",
				"content": text,
			})
		}
	}
	if raw, ok := req["messages"].([]interface{}); ok {
		for _, item := range raw {
			msg, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role == "" {
				role = "user"
			}
			messages = append(messages, anthropicMessageToChat(role, msg["content"])...)
		}
	}
	return messages
}

func anthropicMessageToChat(role string, content interface{}) []interface{} {
	if role == "assistant" {
		textContent, toolCalls := anthropicAssistantContentToChat(content)
		msg := map[string]interface{}{"role": "assistant"}
		if textContent != nil {
			msg["content"] = textContent
		} else {
			msg["content"] = ""
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		return []interface{}{msg}
	}

	if role == "user" {
		textContent, toolResults := anthropicUserContentToChat(content)
		var out []interface{}
		if textContent != nil {
			out = append(out, map[string]interface{}{
				"role":    "user",
				"content": textContent,
			})
		}
		out = append(out, toolResults...)
		if len(out) > 0 {
			return out
		}
	}

	return []interface{}{map[string]interface{}{
		"role":    role,
		"content": anthropicContentToChat(content),
	}}
}

func anthropicSystemText(system interface{}) string {
	switch v := system.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, p := range v {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if part["type"] == "text" {
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func anthropicContentToChat(content interface{}) interface{} {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]interface{}, 0, len(v))
		for _, raw := range v {
			part, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			switch part["type"] {
			case "text":
				parts = append(parts, map[string]interface{}{
					"type": "text",
					"text": part["text"],
				})
			case "image":
				if imageURL := anthropicImageURL(part); imageURL != "" {
					parts = append(parts, map[string]interface{}{
						"type": "image_url",
						"image_url": map[string]interface{}{
							"url": imageURL,
						},
					})
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return ""
	}
}

func anthropicAssistantContentToChat(content interface{}) (interface{}, []interface{}) {
	switch v := content.(type) {
	case string:
		return v, nil
	case []interface{}:
		var textParts []interface{}
		var toolCalls []interface{}
		for _, raw := range v {
			part, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			switch part["type"] {
			case "text":
				textParts = append(textParts, map[string]interface{}{
					"type": "text",
					"text": part["text"],
				})
			case "tool_use":
				id, _ := part["id"].(string)
				name, _ := part["name"].(string)
				args, _ := json.Marshal(part["input"])
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": string(args),
					},
				})
			}
		}
		if len(textParts) == 0 {
			return nil, toolCalls
		}
		return textParts, toolCalls
	default:
		return nil, nil
	}
}

func anthropicUserContentToChat(content interface{}) (interface{}, []interface{}) {
	switch v := content.(type) {
	case string:
		return v, nil
	case []interface{}:
		var parts []interface{}
		var toolResults []interface{}
		for _, raw := range v {
			part, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			switch part["type"] {
			case "tool_result":
				id, _ := part["tool_use_id"].(string)
				toolResults = append(toolResults, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": id,
					"content":      anthropicToolResultText(part["content"]),
				})
			case "text", "image":
				converted := anthropicContentToChat([]interface{}{part})
				if convertedParts, ok := converted.([]interface{}); ok {
					parts = append(parts, convertedParts...)
				}
			}
		}
		if len(parts) == 0 {
			return nil, toolResults
		}
		return parts, toolResults
	default:
		return nil, nil
	}
}

func anthropicToolResultText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, raw := range v {
			part, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if part["type"] == "text" {
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if v == nil {
			return ""
		}
		body, _ := json.Marshal(v)
		return string(body)
	}
}

func anthropicToolsToChat(tools []interface{}) []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		fn := map[string]interface{}{}
		for _, key := range []string{"name", "description"} {
			if v, ok := tool[key]; ok {
				fn[key] = v
			}
		}
		if schema, ok := tool["input_schema"]; ok {
			fn["parameters"] = schema
		}
		out = append(out, map[string]interface{}{
			"type":     "function",
			"function": fn,
		})
	}
	return out
}

func anthropicToolChoiceToChat(choice interface{}) interface{} {
	choiceMap, ok := choice.(map[string]interface{})
	if !ok {
		return nil
	}
	switch choiceMap["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		name, _ := choiceMap["name"].(string)
		if name == "" {
			return nil
		}
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": name},
		}
	default:
		return nil
	}
}

func anthropicImageURL(part map[string]interface{}) string {
	source, ok := part["source"].(map[string]interface{})
	if !ok {
		return ""
	}
	if source["type"] != "base64" {
		return ""
	}
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	if mediaType == "" || data == "" {
		return ""
	}
	return "data:" + mediaType + ";base64," + data
}

func convertToAnthropicFormat(respBody []byte, model string) []byte {
	var codexResp map[string]interface{}
	if err := json.Unmarshal(respBody, &codexResp); err != nil {
		return respBody
	}
	message, finishReason := extractMessage(codexResp)
	content := anthropicResponseContent(message)
	usage := anthropicUsage(codexResp["usage"])
	resp := map[string]interface{}{
		"id":            codexResp["id"],
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStopReason(finishReason),
		"stop_sequence": nil,
		"usage":         usage,
	}
	result, _ := json.Marshal(resp)
	return result
}

func anthropicResponseContent(message map[string]interface{}) []interface{} {
	var content []interface{}
	if text, _ := message["content"].(string); text != "" {
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": text,
		})
	}
	if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
		for _, raw := range toolCalls {
			call, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			id, _ := call["id"].(string)
			fn, _ := call["function"].(map[string]interface{})
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			argsRaw, _ := fn["arguments"].(string)
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    id,
				"name":  name,
				"input": parseToolArguments(argsRaw),
			})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]interface{}{"type": "text", "text": ""})
	}
	return content
}

func parseToolArguments(args string) interface{} {
	if strings.TrimSpace(args) == "" {
		return map[string]interface{}{}
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return map[string]interface{}{}
	}
	return parsed
}

func anthropicUsage(raw interface{}) map[string]int {
	body, err := json.Marshal(raw)
	if err != nil {
		return map[string]int{"input_tokens": 0, "output_tokens": 0}
	}
	openaiUsage := convertUsage(body)
	if openaiUsage == nil {
		return map[string]int{"input_tokens": 0, "output_tokens": 0}
	}
	return map[string]int{
		"input_tokens":  intFromInterface(openaiUsage["prompt_tokens"]),
		"output_tokens": intFromInterface(openaiUsage["completion_tokens"]),
	}
}

func intFromInterface(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func anthropicStopReason(finishReason string) string {
	switch finishReason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func streamAnthropicMessages(w http.ResponseWriter, resp *http.Response, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "internal", "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	id := "msg_" + newUUID()
	started := false
	textOpen := false
	nextBlockIndex := 0
	textBlockIndex := -1
	toolBlocks := map[int]int{}
	openToolBlocks := map[int]bool{}
	hasToolUse := false
	var usage map[string]int
	send := func(event string, payload map[string]interface{}) error {
		body, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	startMessage := func() error {
		if started {
			return nil
		}
		started = true
		return send("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            id,
				"type":          "message",
				"role":          "assistant",
				"model":         model,
				"content":       []interface{}{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
			},
		})
	}
	startText := func() error {
		if err := startMessage(); err != nil {
			return err
		}
		if textOpen {
			return nil
		}
		textBlockIndex = nextBlockIndex
		nextBlockIndex++
		textOpen = true
		return send("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         textBlockIndex,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
	}
	stopText := func() error {
		if !textOpen {
			return nil
		}
		textOpen = false
		return send("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": textBlockIndex,
		})
	}
	startTool := func(outputIndex int, callID, name string) error {
		if err := startMessage(); err != nil {
			return err
		}
		if err := stopText(); err != nil {
			return err
		}
		if _, ok := toolBlocks[outputIndex]; ok {
			return nil
		}
		blockIndex := nextBlockIndex
		nextBlockIndex++
		toolBlocks[outputIndex] = blockIndex
		openToolBlocks[outputIndex] = true
		hasToolUse = true
		return send("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    callID,
				"name":  name,
				"input": map[string]interface{}{},
			},
		})
	}
	stopTool := func(outputIndex int) error {
		blockIndex, ok := toolBlocks[outputIndex]
		if !ok || !openToolBlocks[outputIndex] {
			return nil
		}
		openToolBlocks[outputIndex] = false
		return send("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": blockIndex,
		})
	}

	err := scanSSE(resp.Body, maxSSEEventSize, func(sse sseEvent) error {
		data := sse.data
		if data == "[DONE]" {
			return errSSEDone
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil
		}
		eventType, _ := payload["type"].(string)
		switch eventType {
		case "response.created":
			return startMessage()

		case "response.output_text.delta":
			delta, _ := payload["delta"].(string)
			if delta == "" {
				return nil
			}
			if err := startText(); err != nil {
				return err
			}
			return send("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": textBlockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": delta},
			})

		case "response.output_item.added":
			var itemEvent struct {
				OutputIndex int `json:"output_index"`
				Item        struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(data), &itemEvent) == nil && itemEvent.Item.Type == "function_call" {
				return startTool(itemEvent.OutputIndex, itemEvent.Item.CallID, itemEvent.Item.Name)
			}

		case "response.function_call_arguments.delta":
			var argsEvent struct {
				OutputIndex int    `json:"output_index"`
				Delta       string `json:"delta"`
				CallID      string `json:"call_id"`
				Name        string `json:"name"`
			}
			if json.Unmarshal([]byte(data), &argsEvent) != nil || argsEvent.Delta == "" {
				return nil
			}
			if _, ok := toolBlocks[argsEvent.OutputIndex]; !ok {
				if err := startTool(argsEvent.OutputIndex, argsEvent.CallID, argsEvent.Name); err != nil {
					return err
				}
			}
			return send("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": toolBlocks[argsEvent.OutputIndex],
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": argsEvent.Delta,
				},
			})

		case "response.output_item.done":
			var itemEvent struct {
				OutputIndex int `json:"output_index"`
				Item        struct {
					Type string `json:"type"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(data), &itemEvent) == nil && itemEvent.Item.Type == "function_call" {
				return stopTool(itemEvent.OutputIndex)
			}

		case "response.completed":
			if respObj, ok := payload["response"].(map[string]interface{}); ok {
				usage = anthropicUsage(respObj["usage"])
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errSSEDone) {
		slog.Error("anthropic stream read error", "error", err)
	}
	if !started {
		_ = startMessage()
	}
	_ = stopText()
	for outputIndex := range openToolBlocks {
		_ = stopTool(outputIndex)
	}
	if usage == nil {
		usage = map[string]int{"output_tokens": 0}
	}
	stopReason := "end_turn"
	if hasToolUse {
		stopReason = "tool_use"
	}
	_ = send("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	})
	_ = send("message_stop", map[string]interface{}{"type": "message_stop"})
}

type sseEvent struct {
	event string
	data  string
}

var errSSEDone = errors.New("sse done")

func scanSSE(body io.Reader, maxEventSize int, handle func(sseEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), maxEventSize)

	var eventType string
	var dataLines []string

	flush := func() error {
		if eventType == "" && len(dataLines) == 0 {
			return nil
		}
		err := handle(sseEvent{
			event: eventType,
			data:  strings.Join(dataLines, "\n"),
		})
		eventType = ""
		dataLines = nil
		if errors.Is(err, errSSEDone) {
			return errSSEDone
		}
		return err
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				if errors.Is(err, errSSEDone) {
					return nil
				}
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			eventType = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil && !errors.Is(err, errSSEDone) {
		return err
	}
	return nil
}

// aggregateCodexResponse scans a Codex SSE stream and returns the final
// `response` object JSON from the response.completed/done event, so a
// non-streaming client gets a single chat.completion. Falls back to a
// synthesized response built from output_text deltas if no completed event
// carries a response object.
func aggregateCodexResponse(body io.Reader) ([]byte, error) {
	var textBuf strings.Builder
	var completedResponse json.RawMessage
	var outputItems []interface{}

	err := scanSSE(body, maxSSEEventSize, func(sse sseEvent) error {
		data := sse.data
		if data == "[DONE]" {
			return errSSEDone
		}

		var ev struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Item     json.RawMessage `json:"item"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return nil
		}
		evType := ev.Type
		if evType == "" {
			evType = sse.event
		}

		switch evType {
		case "response.output_text.delta":
			textBuf.WriteString(ev.Delta)
		case "response.output_item.done":
			if item := normalizedOutputItem(ev.Item); item != nil {
				outputItems = append(outputItems, item)
			}
		case "response.completed", "response.done":
			if len(ev.Response) > 0 {
				completedResponse = ev.Response
				return errSSEDone
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("upstream stream read failed: %w", err)
	}

	if completedResponse != nil {
		if len(outputItems) > 0 || textBuf.Len() > 0 {
			if patched := injectOutputIfMissing(completedResponse, outputItems, textBuf.String()); patched != nil {
				return patched, nil
			}
		}
		return completedResponse, nil
	}

	if textBuf.Len() == 0 {
		return nil, nil
	}
	// Fallback: no response object seen, wrap accumulated text.
	return synthesizeTextResponse(textBuf.String()), nil
}

func normalizedOutputItem(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return nil
	}
	var item map[string]interface{}
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	if item["type"] == "function_call" {
		delete(item, "id")
		delete(item, "status")
	}
	return item
}

func injectOutputIfMissing(respBody []byte, outputItems []interface{}, text string) []byte {
	var resp map[string]interface{}
	if json.Unmarshal(respBody, &resp) != nil {
		return nil
	}
	if responseHasOutput(resp) {
		return nil
	}
	if len(outputItems) > 0 {
		resp["output"] = outputItems
	} else if text != "" {
		resp["output"] = []interface{}{
			map[string]interface{}{
				"type": "message",
				"content": []interface{}{
					map[string]interface{}{"type": "output_text", "text": text},
				},
			},
		}
	}
	b, _ := json.Marshal(resp)
	return b
}

func responseHasOutput(resp map[string]interface{}) bool {
	message, _ := extractMessage(resp)
	if content, ok := message["content"].(string); ok && content != "" {
		return true
	}
	if _, ok := message["refusal"].(string); ok {
		return true
	}
	if toolCalls, ok := message["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
		return true
	}
	output, ok := resp["output"].([]interface{})
	if !ok {
		return false
	}
	for _, item := range output {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		switch itemMap["type"] {
		case "function_call", "image_generation_call":
			return true
		}
	}
	return false
}

func synthesizeTextResponse(text string) []byte {
	synth := map[string]interface{}{
		"output": []interface{}{
			map[string]interface{}{
				"type": "message",
				"content": []interface{}{
					map[string]interface{}{"type": "output_text", "text": text},
				},
			},
		},
	}
	b, _ := json.Marshal(synth)
	return b
}

// ──────────────────────────────────────────────
// /v1/images/generations → Codex image_generation tool
// ──────────────────────────────────────────────

func makeImageHandler(baseModel string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleImage(w, r, baseModel, false)
	}
}

func makeImageEditHandler(baseModel string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleImage(w, r, baseModel, true)
	}
}

// extractImageRefs collects image URLs/data-URLs from the OpenAI images.edits
// JSON body, which may carry them in `image` (string or array) or `images[]`.
func extractImageRefs(raw map[string]interface{}) []string {
	var refs []string

	addVal := func(v interface{}) {
		switch t := v.(type) {
		case string:
			if t != "" {
				refs = append(refs, t)
			}
		case map[string]interface{}:
			if u, ok := t["image_url"].(string); ok && u != "" {
				refs = append(refs, u)
			} else if u, ok := t["url"].(string); ok && u != "" {
				refs = append(refs, u)
			}
		}
	}

	for _, key := range []string{"image", "images"} {
		switch t := raw[key].(type) {
		case string:
			addVal(t)
		case []interface{}:
			for _, item := range t {
				addVal(item)
			}
		case map[string]interface{}:
			addVal(t)
		}
	}
	return refs
}

func handleImage(w http.ResponseWriter, r *http.Request, baseModel string, isEdit bool) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "POST only")
		return
	}

	if baseModel == "" {
		writeError(w, 503, "model_unavailable", "model discovery failed at startup; image generation unavailable")
		return
	}

	stats.requestsTotal.Add(1)
	stats.requestsActive.Add(1)
	defer stats.requestsActive.Add(-1)

	var req struct {
		Prompt         string `json:"prompt"`
		Model          string `json:"model"`
		N              int    `json:"n"`
		Size           string `json:"size"`
		Quality        string `json:"quality"`
		ResponseFormat string `json:"response_format"`
		Background     string `json:"background"`
		OutputFormat   string `json:"output_format"`
	}
	var inputImages []string

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		// OpenAI SDK sends images.edits as multipart with image file(s).
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		if err := r.ParseMultipartForm(maxRequestBodySize); err != nil {
			stats.errorsTotal.Add(1)
			writeError(w, 400, "bad_request", "cannot parse multipart form")
			return
		}
		req.Prompt = r.FormValue("prompt")
		req.Model = r.FormValue("model")
		req.Size = r.FormValue("size")
		req.Quality = r.FormValue("quality")
		req.ResponseFormat = r.FormValue("response_format")
		req.Background = r.FormValue("background")
		req.OutputFormat = r.FormValue("output_format")
		if n := r.FormValue("n"); n != "" {
			req.N, _ = strconv.Atoi(n)
		}

		if r.MultipartForm != nil {
			for _, field := range []string{"image", "image[]", "images", "images[]"} {
				for _, fh := range r.MultipartForm.File[field] {
					f, err := fh.Open()
					if err != nil {
						continue
					}
					raw, err := io.ReadAll(f)
					f.Close()
					if err != nil || len(raw) == 0 {
						continue
					}
					mime := fh.Header.Get("Content-Type")
					if mime == "" {
						mime = http.DetectContentType(raw)
					}
					dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
					inputImages = append(inputImages, dataURL)
				}
			}
		}
	} else {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
		if err != nil {
			stats.errorsTotal.Add(1)
			writeError(w, 400, "bad_request", "cannot read request body")
			return
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(body, &raw); err != nil {
			stats.errorsTotal.Add(1)
			writeError(w, 400, "bad_request", "invalid JSON")
			return
		}
		json.Unmarshal(body, &req)
		if isEdit {
			inputImages = extractImageRefs(raw)
		}
	}

	if req.Prompt == "" {
		stats.errorsTotal.Add(1)
		writeError(w, 400, "bad_request", "prompt is required")
		return
	}

	if isEdit && len(inputImages) == 0 {
		stats.errorsTotal.Add(1)
		writeError(w, 400, "bad_request", "edits requires at least one image")
		return
	}

	imageModel := req.Model
	if imageModel == "" {
		imageModel = "gpt-image-2"
	}
	if req.N == 0 {
		req.N = 1
	}
	if req.N < 0 || req.N > 10 {
		stats.errorsTotal.Add(1)
		writeError(w, 400, "bad_request", "n must be between 1 and 10")
		return
	}

	imageTool := map[string]interface{}{
		"type":  "image_generation",
		"model": imageModel,
	}
	if req.Size != "" {
		imageTool["size"] = req.Size
	}
	if req.Quality != "" {
		imageTool["quality"] = req.Quality
	}
	if req.Background != "" {
		imageTool["background"] = req.Background
	}
	outputFormat := req.OutputFormat
	if outputFormat == "" {
		outputFormat = "png"
	}
	imageTool["output_format"] = outputFormat

	content := []interface{}{
		map[string]interface{}{
			"type": "input_text",
			"text": req.Prompt,
		},
	}
	for _, imgURL := range inputImages {
		content = append(content, map[string]interface{}{
			"type":      "input_image",
			"image_url": imgURL,
		})
	}

	codexReq := map[string]interface{}{
		"model":        baseModel,
		"stream":       true,
		"store":        false,
		"instructions": "Generate the image as requested.",
		"tool_choice":  "auto",
		"tools":        []interface{}{imageTool},
		"input": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": content,
			},
		},
	}

	var images []imageResult
	for len(images) < req.N {
		batch, status, respBody, err := generateImagesOnce(r.Context(), codexReq)
		if err != nil {
			stats.errorsTotal.Add(1)
			writeError(w, 502, "upstream_error", err.Error())
			return
		}
		if status != 200 {
			stats.errorsTotal.Add(1)
			slog.Error("upstream image error",
				"status", status,
				"body", string(respBody[:min(500, len(respBody))]))
			w.WriteHeader(status)
			w.Write(respBody)
			return
		}
		if len(batch) == 0 {
			break
		}
		images = append(images, batch...)
	}
	if len(images) == 0 {
		stats.errorsTotal.Add(1)
		writeError(w, 502, "upstream_error", "no image generated")
		return
	}
	if len(images) > req.N {
		images = images[:req.N]
	}

	data := make([]map[string]interface{}, 0, len(images))
	for _, img := range images {
		item := map[string]interface{}{
			"revised_prompt": img.revisedPrompt,
		}
		if req.ResponseFormat == "url" {
			item["url"] = "data:image/" + outputFormat + ";base64," + img.b64JSON
		} else {
			item["b64_json"] = img.b64JSON
		}
		data = append(data, item)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    data,
	})
}

func generateImagesOnce(ctx context.Context, codexReq map[string]interface{}) ([]imageResult, int, []byte, error) {
	codexBody, _ := json.Marshal(codexReq)
	resp, err := callUpstream(ctx, upstreamBase+"/responses", codexBody, true)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode, respBody, nil
	}

	images, err := parseImageSSE(resp.Body)
	if err != nil {
		return nil, 0, nil, err
	}
	return images, resp.StatusCode, nil, nil
}

type imageResult struct {
	b64JSON       string
	revisedPrompt string
}

func parseImageSSE(body io.Reader) ([]imageResult, error) {
	var results []imageResult

	err := scanSSE(body, maxSSEEventSize, func(sse sseEvent) error {
		data := sse.data
		if data == "[DONE]" {
			return errSSEDone
		}

		var ev struct {
			Type string `json:"type"`
			Item *struct {
				Type          string `json:"type"`
				Result        string `json:"result"`
				RevisedPrompt string `json:"revised_prompt"`
			} `json:"item"`
			Response *struct {
				Output []struct {
					Type          string `json:"type"`
					Result        string `json:"result"`
					RevisedPrompt string `json:"revised_prompt"`
				} `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return nil
		}

		evType := ev.Type
		if evType == "" {
			evType = sse.event
		}

		switch evType {
		case "response.output_item.done":
			if ev.Item != nil && ev.Item.Type == "image_generation_call" && ev.Item.Result != "" {
				results = append(results, imageResult{
					b64JSON:       ev.Item.Result,
					revisedPrompt: ev.Item.RevisedPrompt,
				})
			}
		case "response.completed", "response.done":
			if ev.Response != nil && len(results) == 0 {
				for _, out := range ev.Response.Output {
					if out.Type == "image_generation_call" && out.Result != "" {
						results = append(results, imageResult{
							b64JSON:       out.Result,
							revisedPrompt: out.RevisedPrompt,
						})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("upstream image stream read failed: %w", err)
	}
	return results, nil
}

// ──────────────────────────────────────────────
// /v1/responses → pass-through to Codex
// ──────────────────────────────────────────────

func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "POST only")
		return
	}

	stats.requestsTotal.Add(1)
	stats.requestsActive.Add(1)
	defer stats.requestsActive.Add(-1)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 400, "bad_request", "cannot read body (max 10MB)")
		return
	}

	var reqMap map[string]interface{}
	isStreaming := false
	if json.Unmarshal(body, &reqMap) == nil {
		isStreaming, _ = reqMap["stream"].(bool)
	}

	upstreamBody := body
	if !isStreaming && reqMap != nil {
		reqMap["stream"] = true
		upstreamBody, err = json.Marshal(reqMap)
		if err != nil {
			stats.errorsTotal.Add(1)
			writeError(w, 500, "internal", "failed to build upstream request")
			return
		}
	}

	resp, err := callUpstream(r.Context(), upstreamBase+"/responses", upstreamBody, true)
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		stats.errorsTotal.Add(1)
		ct := resp.Header.Get("Content-Type")
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	if !isStreaming {
		respObj, err := aggregateCodexResponse(resp.Body)
		if err != nil {
			stats.errorsTotal.Add(1)
			writeError(w, 502, "upstream_error", err.Error())
			return
		}
		if respObj == nil {
			stats.errorsTotal.Add(1)
			writeError(w, 502, "upstream_error", "no response from upstream")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(respObj)
	} else {
		streamPassthrough(w, resp)
	}
}

// ──────────────────────────────────────────────
// /v1/models
// ──────────────────────────────────────────────

func makeModelsHandler(models []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := make([]map[string]interface{}, len(models))
		for i, m := range models {
			data[i] = map[string]interface{}{
				"id":       m,
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "openai",
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   data,
		})
	}
}

// ──────────────────────────────────────────────
// SSE streaming: Codex format → OpenAI chat completion chunks
// ──────────────────────────────────────────────

func streamChatCompletion(w http.ResponseWriter, resp *http.Response, model string, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "internal", "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	created := time.Now().Unix()
	respID := fmt.Sprintf("chatcmpl-%d", created)
	firstContent := true
	hasToolCalls := false
	toolCallIndex := -1
	doneSent := false

	err := scanSSE(resp.Body, maxSSEEventSize, func(sse sseEvent) error {
		data := sse.data
		if data == "[DONE]" {
			return errSSEDone
		}
		evType := sse.event
		if evType == "" {
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(data), &probe) == nil && probe.Type != "" {
				evType = probe.Type
			}
		}

		switch evType {
		case "response.created":
			var rc struct {
				Response struct {
					ID string `json:"id"`
				} `json:"response"`
				ID string `json:"id"`
			}
			if json.Unmarshal([]byte(data), &rc) == nil {
				if rc.Response.ID != "" {
					respID = "chatcmpl-" + rc.Response.ID
				} else if rc.ID != "" {
					respID = "chatcmpl-" + rc.ID
				}
			}

		case "response.output_text.delta":
			var delta struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &delta); err != nil {
				return nil
			}
			chunk := buildStreamChunk(respID, model, created, firstContent, delta.Delta, "")
			firstContent = false
			if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
				return err
			}
			flusher.Flush()

		case "response.output_item.done":
			var ev struct {
				Item struct {
					Type          string `json:"type"`
					Result        string `json:"result"`
					RevisedPrompt string `json:"revised_prompt"`
					OutputFormat  string `json:"output_format"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Item.Type == "image_generation_call" && ev.Item.Result != "" {
				outFmt := ev.Item.OutputFormat
				if outFmt == "" {
					outFmt = "png"
				}
				dataURL := "data:image/" + outFmt + ";base64," + ev.Item.Result
				content := "![image](" + dataURL + ")"
				chunk := buildStreamChunk(respID, model, created, firstContent, content, "")
				firstContent = false
				if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
					return err
				}
				flusher.Flush()
			}

		case "response.output_item.added":
			var ev struct {
				OutputIndex int `json:"output_index"`
				Item        struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Item.Type == "function_call" {
				hasToolCalls = true
				toolCallIndex = ev.OutputIndex
				chunk := buildToolCallChunk(respID, model, created, firstContent, toolCallIndex, ev.Item.CallID, ev.Item.Name, "")
				firstContent = false
				if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
					return err
				}
				flusher.Flush()
			}

		case "response.function_call_arguments.delta":
			var ev struct {
				Delta       string `json:"delta"`
				CallID      string `json:"call_id"`
				Name        string `json:"name"`
				OutputIndex *int   `json:"output_index"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return nil
			}
			if ev.Name != "" {
				if ev.OutputIndex != nil {
					toolCallIndex = *ev.OutputIndex
				} else {
					toolCallIndex++
				}
				hasToolCalls = true
				chunk := buildToolCallChunk(respID, model, created, firstContent, toolCallIndex, ev.CallID, ev.Name, ev.Delta)
				firstContent = false
				if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
					return err
				}
			} else {
				if ev.OutputIndex != nil {
					toolCallIndex = *ev.OutputIndex
				}
				chunk := buildToolCallDeltaChunk(respID, model, created, toolCallIndex, ev.Delta)
				if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
					return err
				}
			}
			flusher.Flush()

		case "response.completed", "response.done":
			fr := "stop"
			if hasToolCalls {
				fr = "tool_calls"
			}
			chunk := buildStreamChunk(respID, model, created, false, "", fr)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
				return err
			}
			if includeUsage {
				var ev struct {
					Response struct {
						Usage json.RawMessage `json:"usage"`
					} `json:"response"`
				}
				if json.Unmarshal([]byte(data), &ev) == nil && len(ev.Response.Usage) > 0 {
					if u := convertUsage(ev.Response.Usage); u != nil {
						usageChunk, _ := json.Marshal(map[string]interface{}{
							"id":      respID,
							"object":  "chat.completion.chunk",
							"created": created,
							"model":   model,
							"choices": []interface{}{},
							"usage":   u,
						})
						if _, err := fmt.Fprintf(w, "data: %s\n\n", usageChunk); err != nil {
							return err
						}
					}
				}
			}
			if _, err := fmt.Fprintf(w, "data: [DONE]\n\n"); err != nil {
				return err
			}
			flusher.Flush()
			doneSent = true
			return errSSEDone
		}
		return nil
	})

	if err != nil {
		slog.Error("stream read error", "error", err)
	}
	if !doneSent {
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

func buildStreamChunk(id, model string, created int64, includeRole bool, content, finishReason string) []byte {
	delta := map[string]interface{}{}
	if includeRole {
		delta["role"] = "assistant"
	}
	if content != "" {
		delta["content"] = content
	}

	choice := map[string]interface{}{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}

	chunk := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []interface{}{choice},
	}

	b, _ := json.Marshal(chunk)
	return b
}

func buildToolCallChunk(id, model string, created int64, includeRole bool, index int, callID, name, args string) []byte {
	delta := map[string]interface{}{
		"tool_calls": []interface{}{
			map[string]interface{}{
				"index": index,
				"id":    callID,
				"type":  "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": args,
				},
			},
		},
	}
	if includeRole {
		delta["role"] = "assistant"
	}
	choice := map[string]interface{}{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	chunk := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []interface{}{choice},
	}
	b, _ := json.Marshal(chunk)
	return b
}

func buildToolCallDeltaChunk(id, model string, created int64, index int, args string) []byte {
	delta := map[string]interface{}{
		"tool_calls": []interface{}{
			map[string]interface{}{
				"index": index,
				"function": map[string]interface{}{
					"arguments": args,
				},
			},
		},
	}
	choice := map[string]interface{}{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	chunk := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []interface{}{choice},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// streamPassthrough passes SSE bytes through without conversion (for /v1/responses).
func streamPassthrough(w http.ResponseWriter, resp *http.Response) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "internal", "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			flusher.Flush()
		}
		if err != nil {
			if err != io.EOF {
				slog.Error("stream read error", "error", err)
			}
			break
		}
	}
}

// ──────────────────────────────────────────────
// Response format conversion (non-streaming)
// ──────────────────────────────────────────────

func convertToOpenAIFormat(respBody []byte, chatReq map[string]interface{}) []byte {
	var codexResp map[string]interface{}
	if err := json.Unmarshal(respBody, &codexResp); err != nil {
		return respBody
	}

	model, _ := chatReq["model"].(string)
	message, finishReason := extractMessage(codexResp)

	choice := map[string]interface{}{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}

	openaiResp := map[string]interface{}{
		"id":      codexResp["id"],
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{choice},
	}
	if raw, err := json.Marshal(codexResp["usage"]); err == nil {
		if u := convertUsage(raw); u != nil {
			openaiResp["usage"] = u
		}
	}

	result, _ := json.Marshal(openaiResp)
	return result
}

// convertUsage maps Responses-API usage (input_tokens/output_tokens) to the
// Chat-Completions shape (prompt_tokens/completion_tokens). Returns nil if the
// raw usage is absent or unparseable.
func convertUsage(raw []byte) map[string]interface{} {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var u struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		TotalTokens        int `json:"total_tokens"`
		OutputTokensDetail struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	out := map[string]interface{}{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      total,
	}
	if u.OutputTokensDetail.ReasoningTokens > 0 {
		out["completion_tokens_details"] = map[string]interface{}{
			"reasoning_tokens": u.OutputTokensDetail.ReasoningTokens,
		}
	}
	return out
}

func extractMessage(resp map[string]interface{}) (map[string]interface{}, string) {
	message := map[string]interface{}{
		"role":    "assistant",
		"content": nil,
	}
	finishReason := "stop"

	output, ok := resp["output"].([]interface{})
	if !ok {
		if text, ok := resp["text"].(string); ok {
			message["content"] = text
		}
		return message, finishReason
	}

	var textParts []string
	var toolCalls []interface{}

	for _, item := range output {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		switch itemMap["type"] {
		case "message":
			if content, ok := itemMap["content"].([]interface{}); ok {
				for _, c := range content {
					cMap, ok := c.(map[string]interface{})
					if !ok {
						continue
					}
					switch cMap["type"] {
					case "output_text", "text":
						if text, ok := cMap["text"].(string); ok {
							textParts = append(textParts, text)
						}
					case "refusal":
						if r, ok := cMap["refusal"].(string); ok {
							message["refusal"] = r
						}
					}
				}
			}

		case "function_call":
			callID, _ := itemMap["call_id"].(string)
			name, _ := itemMap["name"].(string)
			args, _ := itemMap["arguments"].(string)
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   callID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": args,
				},
			})

		case "image_generation_call":
			b64, _ := itemMap["result"].(string)
			if b64 != "" {
				revisedPrompt, _ := itemMap["revised_prompt"].(string)
				outputFmt, _ := itemMap["output_format"].(string)
				if outputFmt == "" {
					outputFmt = "png"
				}
				dataURL := "data:image/" + outputFmt + ";base64," + b64
				textParts = append(textParts, "![image]("+dataURL+")")
				_ = revisedPrompt
			}
		}
	}

	if len(textParts) > 0 {
		message["content"] = strings.Join(textParts, "")
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
	}

	return message, finishReason
}

// ──────────────────────────────────────────────
// Usage endpoint
// ──────────────────────────────────────────────

func handleUsage(w http.ResponseWriter, r *http.Request) {
	managers := auth.Pool.Managers()
	results := make([]map[string]interface{}, 0, len(managers))

	for _, tm := range managers {
		token := tm.GetAccessToken()
		if token == "" {
			results = append(results, map[string]interface{}{
				"account": tm.Name(),
				"error":   "no token",
			})
			continue
		}
		info, err := auth.QueryUsage(token)
		if err != nil {
			results = append(results, map[string]interface{}{
				"account": tm.Name(),
				"error":   err.Error(),
			})
			continue
		}
		entry := map[string]interface{}{
			"account":       tm.Name(),
			"email":         info.Email,
			"plan_type":     info.PlanType,
			"allowed":       info.Allowed,
			"limit_reached": info.LimitHit,
			"windows":       info.Windows,
		}
		results = append(results, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts": results,
	})
}

// ──────────────────────────────────────────────
// Middleware
// ──────────────────────────────────────────────

func withAuth(validateKey KeyValidator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public, unauthenticated paths. Everything else (including GET
		// /usage, /metrics, /v1/models) requires a valid API key.
		if r.Method == "OPTIONS" || r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}

		key := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			key = strings.TrimPrefix(h, "Bearer ")
		}
		if key == "" {
			key = r.Header.Get("X-API-Key")
		}

		if !validateKey(key) {
			writeError(w, 401, "unauthorized", "invalid or missing API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key")

		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &logWriter{ResponseWriter: w, statusCode: 200}
		next.ServeHTTP(lw, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
			"status", lw.statusCode,
			"duration", time.Since(start).Round(time.Millisecond).String())
	})
}

type logWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lw *logWriter) WriteHeader(code int) {
	lw.statusCode = code
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *logWriter) Flush() {
	if f, ok := lw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ──────────────────────────────────────────────
// Error helpers
// ──────────────────────────────────────────────

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"type":    code,
			"message": message,
		},
	})
}
