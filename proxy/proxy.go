package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
			TLSHandshakeTimeout:  10 * time.Second,
			MaxIdleConns:         20,
			IdleConnTimeout:      90 * time.Second,
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
		slog.Warn("model discovery failed, using fallback", "error", err)
		models = []string{"o3-pro", "gpt-5.4", "gpt-5.3-codex", "o4-mini"}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/v1/responses", handleResponses)
	mux.HandleFunc("/v1/models", makeModelsHandler(models))

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
		json.NewEncoder(w).Encode(map[string]interface{}{
			"requests_total":  stats.requestsTotal.Load(),
			"requests_active": stats.requestsActive.Load(),
			"errors_total":    stats.errorsTotal.Load(),
			"retries":         stats.retries.Load(),
			"token_refreshes": stats.tokenRefreshes.Load(),
			"uptime_seconds":  int(time.Since(startTime).Seconds()),
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"service":          "codex-proxy",
			"openai_compatible": true,
			"endpoints": []string{
				"/v1/chat/completions",
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

	refreshed := false
	var resp *http.Response

	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "codex-proxy/1.0")

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
			}
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

	isStreaming, _ := chatReq["stream"].(bool)
	model, _ := chatReq["model"].(string)

	codexBody, err := auth.BuildCodexRequestBody(chatReq)
	if err != nil {
		stats.errorsTotal.Add(1)
		writeError(w, 500, "internal", "failed to build upstream request")
		return
	}

	resp, err := callUpstream(r.Context(), upstreamBase+"/responses", codexBody, isStreaming)
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

	if isStreaming {
		streamChatCompletion(w, resp, model)
	} else {
		respBody, _ := io.ReadAll(resp.Body)
		converted := convertToOpenAIFormat(respBody, chatReq)
		w.Header().Set("Content-Type", "application/json")
		w.Write(converted)
	}
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

	resp, err := callUpstream(r.Context(), upstreamBase+"/responses", body, isStreaming)
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

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		streamPassthrough(w, resp)
	} else {
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
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

func streamChatCompletion(w http.ResponseWriter, resp *http.Response, model string) {
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

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	created := time.Now().Unix()
	respID := fmt.Sprintf("chatcmpl-%d", created)
	firstContent := true
	hasToolCalls := false
	toolCallIndex := -1
	var eventType string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		evType := eventType
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
				continue
			}
			chunk := buildStreamChunk(respID, model, created, firstContent, delta.Delta, "")
			firstContent = false
			if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
				return
			}
			flusher.Flush()

		case "response.function_call_arguments.delta":
			var ev struct {
				Delta  string `json:"delta"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			if ev.Name != "" {
				toolCallIndex++
				hasToolCalls = true
				chunk := buildToolCallChunk(respID, model, created, firstContent, toolCallIndex, ev.CallID, ev.Name, ev.Delta)
				firstContent = false
				if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
					return
				}
			} else {
				chunk := buildToolCallDeltaChunk(respID, model, created, toolCallIndex, ev.Delta)
				if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
					return
				}
			}
			flusher.Flush()

		case "response.completed", "response.done":
			fr := "stop"
			if hasToolCalls {
				fr = "tool_calls"
			}
			chunk := buildStreamChunk(respID, model, created, false, "", fr)
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		eventType = ""
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
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
		"usage":   codexResp["usage"],
	}

	result, _ := json.Marshal(openaiResp)
	return result
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
			// Codex: {"type":"function_call","call_id":"x","name":"f","arguments":"..."}
			// Chat:  {"id":"x","type":"function","function":{"name":"f","arguments":"..."}}
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
// Middleware
// ──────────────────────────────────────────────

func withAuth(validateKey KeyValidator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" ||
			r.URL.Path == "/health" ||
			r.URL.Path == "/metrics" ||
			r.URL.Path == "/" {
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
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")

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
