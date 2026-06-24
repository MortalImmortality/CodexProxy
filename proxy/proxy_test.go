package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-proxy/auth"
)

func TestConvertToOpenAIFormat(t *testing.T) {
	codexResp := map[string]interface{}{
		"id": "resp_123",
		"output": []interface{}{
			map[string]interface{}{
				"type": "message",
				"content": []interface{}{
					map[string]interface{}{
						"type": "output_text",
						"text": "Hello world",
					},
				},
			},
		},
		"usage": map[string]interface{}{
			"input_tokens":  10,
			"output_tokens": 5,
		},
	}

	respBody, _ := json.Marshal(codexResp)
	chatReq := map[string]interface{}{"model": "o3-pro"}

	result := convertToOpenAIFormat(respBody, chatReq)

	var openaiResp map[string]interface{}
	if err := json.Unmarshal(result, &openaiResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if openaiResp["id"] != "resp_123" {
		t.Errorf("id = %v, want resp_123", openaiResp["id"])
	}
	if openaiResp["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", openaiResp["object"])
	}
	if openaiResp["model"] != "o3-pro" {
		t.Errorf("model = %v, want o3-pro", openaiResp["model"])
	}

	choices, ok := openaiResp["choices"].([]interface{})
	if !ok || len(choices) != 1 {
		t.Fatalf("choices: got %v", openaiResp["choices"])
	}

	choice := choices[0].(map[string]interface{})
	msg := choice["message"].(map[string]interface{})
	if msg["content"] != "Hello world" {
		t.Errorf("content = %v, want Hello world", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msg["role"])
	}
}

func TestSnapshotMetricsIncludesLastError(t *testing.T) {
	recordLastError(http.StatusBadGateway, "upstream_error", "bad <upstream>")

	snapshot := SnapshotMetrics()
	if snapshot.LastError == nil {
		t.Fatal("LastError is nil")
	}
	if snapshot.LastError.Status != http.StatusBadGateway {
		t.Fatalf("LastError.Status = %d, want %d", snapshot.LastError.Status, http.StatusBadGateway)
	}
	if snapshot.LastError.Type != "upstream_error" || snapshot.LastError.Message != "bad <upstream>" {
		t.Fatalf("LastError = %#v", snapshot.LastError)
	}
}

func TestUntrackedErrorDoesNotOverwriteLastError(t *testing.T) {
	recordLastError(http.StatusBadGateway, "upstream_error", "counted failure")

	w := httptest.NewRecorder()
	writeUntrackedError(w, http.StatusUnauthorized, "unauthorized", "invalid key")

	snapshot := SnapshotMetrics()
	if snapshot.LastError == nil {
		t.Fatal("LastError is nil")
	}
	if snapshot.LastError.Status != http.StatusBadGateway || snapshot.LastError.Message != "counted failure" {
		t.Fatalf("LastError = %#v", snapshot.LastError)
	}
}

func TestWriteErrorIncrementsMetricsWithDetails(t *testing.T) {
	before := SnapshotMetrics()

	w := httptest.NewRecorder()
	writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")

	after := SnapshotMetrics()
	if after.ErrorsTotal != before.ErrorsTotal+1 {
		t.Fatalf("ErrorsTotal = %d, want %d", after.ErrorsTotal, before.ErrorsTotal+1)
	}
	if after.LastError == nil {
		t.Fatal("LastError is nil")
	}
	if after.LastError.Status != http.StatusBadRequest || after.LastError.Message != "invalid JSON" {
		t.Fatalf("LastError = %#v", after.LastError)
	}
}

func TestRequestReasoningForLog(t *testing.T) {
	got := requestReasoningForLog(map[string]interface{}{
		"model":            "gpt-5",
		"reasoning_effort": "high",
	})

	reasoning, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("reasoning = %#v, want map", got)
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("effort = %v, want high", reasoning["effort"])
	}
}

func TestChatCompletionsRejectsImageOnlyModel(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model": "gpt-image-2",
		"messages": [{"role": "user", "content": "draw a square"}]
	}`))
	w := httptest.NewRecorder()

	handleChatCompletions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj := got["error"].(map[string]interface{})
	if errObj["type"] != "unsupported_model" {
		t.Fatalf("error = %#v", errObj)
	}
	if !strings.Contains(errObj["message"].(string), "/v1/images/generations") {
		t.Fatalf("message = %q, want images endpoint guidance", errObj["message"])
	}
}

func TestAnthropicToChatRequest(t *testing.T) {
	req := map[string]interface{}{
		"model":  "claude-3-5-sonnet-latest",
		"system": "be concise",
		"tools": []interface{}{
			map[string]interface{}{
				"name":        "get_weather",
				"description": "Get weather",
				"input_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"city": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
		"tool_choice": map[string]interface{}{"type": "tool", "name": "get_weather"},
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "hello"},
					map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "abc",
						},
					},
				},
			},
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{
						"type":  "tool_use",
						"id":    "toolu_1",
						"name":  "get_weather",
						"input": map[string]interface{}{"city": "Beijing"},
					},
				},
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "toolu_1",
						"content":     "sunny",
					},
				},
			},
		},
		"max_tokens": float64(100),
	}

	chatReq := anthropicToChatRequest(req)
	if chatReq["model"] != "gpt-5.4" {
		t.Fatalf("model = %v, want gpt-5.4", chatReq["model"])
	}
	messages := chatReq["messages"].([]interface{})
	if len(messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(messages))
	}
	system := messages[0].(map[string]interface{})
	if system["role"] != "system" || system["content"] != "be concise" {
		t.Fatalf("system message = %#v", system)
	}
	user := messages[1].(map[string]interface{})
	parts := user["content"].([]interface{})
	image := parts[1].(map[string]interface{})
	imageURL := image["image_url"].(map[string]interface{})
	if imageURL["url"] != "data:image/png;base64,abc" {
		t.Fatalf("image url = %v", imageURL["url"])
	}
	assistant := messages[2].(map[string]interface{})
	toolCalls := assistant["tool_calls"].([]interface{})
	toolCall := toolCalls[0].(map[string]interface{})
	if toolCall["id"] != "toolu_1" {
		t.Fatalf("tool call = %#v", toolCall)
	}
	toolMsg := messages[3].(map[string]interface{})
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" || toolMsg["content"] != "sunny" {
		t.Fatalf("tool result message = %#v", toolMsg)
	}
	tools := chatReq["tools"].([]interface{})
	fn := tools[0].(map[string]interface{})["function"].(map[string]interface{})
	if fn["name"] != "get_weather" || fn["parameters"] == nil {
		t.Fatalf("tool conversion = %#v", tools[0])
	}
	choice := chatReq["tool_choice"].(map[string]interface{})
	if choice["type"] != "function" {
		t.Fatalf("tool_choice = %#v", choice)
	}
}

func TestConvertToAnthropicFormat(t *testing.T) {
	codexResp := map[string]interface{}{
		"id": "resp_123",
		"output": []interface{}{
			map[string]interface{}{
				"type": "message",
				"content": []interface{}{
					map[string]interface{}{"type": "output_text", "text": "Hello"},
				},
			},
		},
		"usage": map[string]interface{}{"input_tokens": 3, "output_tokens": 2},
	}
	body, _ := json.Marshal(codexResp)

	result := convertToAnthropicFormat(body, "claude-test")
	var got map[string]interface{}
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != "message" || got["role"] != "assistant" || got["model"] != "claude-test" {
		t.Fatalf("anthropic response = %#v", got)
	}
	content := got["content"].([]interface{})
	text := content[0].(map[string]interface{})
	if text["type"] != "text" || text["text"] != "Hello" {
		t.Fatalf("content = %#v", content)
	}
	usage := got["usage"].(map[string]interface{})
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(2) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestConvertToAnthropicFormatWithToolUse(t *testing.T) {
	codexResp := map[string]interface{}{
		"id": "resp_123",
		"output": []interface{}{
			map[string]interface{}{
				"type":      "function_call",
				"call_id":   "toolu_1",
				"name":      "get_weather",
				"arguments": `{"city":"Beijing"}`,
			},
		},
		"usage": map[string]interface{}{"input_tokens": 3, "output_tokens": 2},
	}
	body, _ := json.Marshal(codexResp)

	result := convertToAnthropicFormat(body, "claude-test")
	var got map[string]interface{}
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use", got["stop_reason"])
	}
	content := got["content"].([]interface{})
	toolUse := content[0].(map[string]interface{})
	if toolUse["type"] != "tool_use" || toolUse["id"] != "toolu_1" || toolUse["name"] != "get_weather" {
		t.Fatalf("tool_use = %#v", toolUse)
	}
	input := toolUse["input"].(map[string]interface{})
	if input["city"] != "Beijing" {
		t.Fatalf("input = %#v", input)
	}
}

func TestStreamAnthropicMessages(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"pong"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":1}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(sse))}
	w := httptest.NewRecorder()

	streamAnthropicMessages(w, resp, "claude-test")

	body := w.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		`"text":"pong"`,
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("stream should be Anthropic SSE, got OpenAI chunk:\n%s", body)
	}
}

func TestStreamAnthropicMessagesWithToolUse(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"toolu_1","name":"get_weather"},"output_index":0}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"city\":\"Beijing\"}","output_index":0}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call"},"output_index":0}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":1}}}`,
		``,
	}, "\n")
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(sse))}
	w := httptest.NewRecorder()

	streamAnthropicMessages(w, resp, "claude-test")

	body := w.Body.String()
	for _, want := range []string{
		"event: content_block_start",
		`"type":"tool_use"`,
		`"id":"toolu_1"`,
		`"name":"get_weather"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"city\":\"Beijing\"}"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
}

func TestAnthropicMessagesInvalidJSONReturnsAnthropicError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{"))
	w := httptest.NewRecorder()

	handleAnthropicMessages(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != "error" {
		t.Fatalf("type = %v, want error", got["type"])
	}
	errObj := got["error"].(map[string]interface{})
	if errObj["type"] != "invalid_request_error" || errObj["message"] != "invalid JSON" {
		t.Fatalf("error = %#v", errObj)
	}
}

func TestAnthropicMessagesAuthErrorShape(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unauthorized request should not reach next handler")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	w := httptest.NewRecorder()

	withAuth(func(string) bool { return false }, next).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != "error" {
		t.Fatalf("type = %v, want error", got["type"])
	}
	errObj := got["error"].(map[string]interface{})
	if errObj["type"] != "authentication_error" {
		t.Fatalf("error = %#v", errObj)
	}
}

func TestAnthropicErrorHelpers(t *testing.T) {
	if got := anthropicErrorType(http.StatusTooManyRequests); got != "rate_limit_error" {
		t.Fatalf("429 error type = %q", got)
	}
	body := []byte(`{"error":{"message":"bad upstream"}}`)
	if got := upstreamErrorMessage(body); got != "bad upstream" {
		t.Fatalf("upstreamErrorMessage = %q", got)
	}
	body = []byte(`{"detail":"unsupported upstream model"}`)
	if got := upstreamErrorMessage(body); got != "unsupported upstream model" {
		t.Fatalf("upstreamErrorMessage detail = %q", got)
	}
}

func TestExtractContent(t *testing.T) {
	tests := []struct {
		name string
		resp map[string]interface{}
		want string
	}{
		{
			name: "standard output",
			resp: map[string]interface{}{
				"output": []interface{}{
					map[string]interface{}{
						"type": "message",
						"content": []interface{}{
							map[string]interface{}{"type": "output_text", "text": "part1"},
							map[string]interface{}{"type": "output_text", "text": "part2"},
						},
					},
				},
			},
			want: "part1part2",
		},
		{
			name: "text field fallback",
			resp: map[string]interface{}{"text": "direct text"},
			want: "direct text",
		},
		{
			name: "empty response",
			resp: map[string]interface{}{},
			want: "",
		},
		{
			name: "text type variant",
			resp: map[string]interface{}{
				"output": []interface{}{
					map[string]interface{}{
						"type": "message",
						"content": []interface{}{
							map[string]interface{}{"type": "text", "text": "variant"},
						},
					},
				},
			},
			want: "variant",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, _ := extractMessage(tt.resp)
			got, _ := msg["content"].(string)
			if got != tt.want {
				t.Errorf("extractMessage content = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAggregateCodexResponseUsesDeltasWhenCompletedHasNoOutput(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"pong"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_123","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	respBody, err := aggregateCodexResponse(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateCodexResponse: %v", err)
	}
	chatReq := map[string]interface{}{"model": "gpt-5.4-mini"}
	result := convertToOpenAIFormat(respBody, chatReq)

	var openaiResp map[string]interface{}
	if err := json.Unmarshal(result, &openaiResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices := openaiResp["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "pong" {
		t.Errorf("content = %v, want pong", msg["content"])
	}
	usage := openaiResp["usage"].(map[string]interface{})
	if usage["total_tokens"] != float64(4) {
		t.Errorf("total_tokens = %v, want 4", usage["total_tokens"])
	}
}

func TestAggregateCodexResponseUsesFunctionCallDone(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","status":"completed","arguments":"{\"city\":\"Beijing\"}","call_id":"call_1","name":"get_weather"},"output_index":0}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_123","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4},"output":[]}}`,
		``,
	}, "\n")

	respBody, err := aggregateCodexResponse(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateCodexResponse: %v", err)
	}
	result := convertToOpenAIFormat(respBody, map[string]interface{}{"model": "gpt-5.4-mini"})

	var openaiResp map[string]interface{}
	if err := json.Unmarshal(result, &openaiResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices := openaiResp["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]interface{})
	toolCalls := msg["tool_calls"].([]interface{})
	call := toolCalls[0].(map[string]interface{})
	if call["id"] != "call_1" {
		t.Errorf("call id = %v, want call_1", call["id"])
	}
	fn := call["function"].(map[string]interface{})
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Beijing"}` {
		t.Errorf("function = %v", fn)
	}
}

func TestAggregateCodexResponseReturnsScannerError(t *testing.T) {
	oversizedLine := "data: " + strings.Repeat("x", maxSSEEventSize+1)
	respBody, err := aggregateCodexResponse(strings.NewReader(oversizedLine))
	if err == nil {
		t.Fatal("expected scanner error for oversized SSE line")
	}
	if respBody != nil {
		t.Fatalf("response body = %s, want nil", respBody)
	}
}

func TestScanSSESupportsMultilineData(t *testing.T) {
	raw := strings.Join([]string{
		"event: joined",
		"data: first",
		"data: second",
		"",
	}, "\n")

	var got []sseEvent
	if err := scanSSE(strings.NewReader(raw), maxSSEEventSize, func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("scanSSE: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].event != "joined" {
		t.Errorf("event = %q, want joined", got[0].event)
	}
	if got[0].data != "first\nsecond" {
		t.Errorf("data = %q, want joined data", got[0].data)
	}
}

func TestStreamChatCompletionInitializesToolCallFromAdded(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"get_weather"},"output_index":0}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"city\":\"Beijing\"}","output_index":0}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{}}`,
		``,
	}, "\n")
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(sse))}
	w := httptest.NewRecorder()

	streamChatCompletion(w, resp, "gpt-5.4-mini", false)
	body := w.Body.String()
	if !strings.Contains(body, `"index":0`) {
		t.Fatalf("stream missing tool index 0:\n%s", body)
	}
	if strings.Contains(body, `"index":-1`) {
		t.Fatalf("stream contains invalid tool index -1:\n%s", body)
	}
	if !strings.Contains(body, `"id":"call_1"`) || !strings.Contains(body, `"name":"get_weather"`) {
		t.Fatalf("stream missing initial tool call metadata:\n%s", body)
	}
}

func TestBuildStreamChunk(t *testing.T) {
	chunk := buildStreamChunk("id-1", "o3-pro", 1000, true, "hello", "")

	var result map[string]interface{}
	if err := json.Unmarshal(chunk, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if result["id"] != "id-1" {
		t.Errorf("id = %v", result["id"])
	}
	if result["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v", result["object"])
	}

	choices := result["choices"].([]interface{})
	delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})

	if delta["role"] != "assistant" {
		t.Error("first chunk should include role")
	}
	if delta["content"] != "hello" {
		t.Errorf("content = %v", delta["content"])
	}

	// Non-first chunk without role
	chunk2 := buildStreamChunk("id-1", "o3-pro", 1000, false, "world", "")
	var result2 map[string]interface{}
	json.Unmarshal(chunk2, &result2)
	choices2 := result2["choices"].([]interface{})
	delta2 := choices2[0].(map[string]interface{})["delta"].(map[string]interface{})
	if _, hasRole := delta2["role"]; hasRole {
		t.Error("subsequent chunk should not include role")
	}

	// Final chunk with finish_reason
	chunk3 := buildStreamChunk("id-1", "o3-pro", 1000, false, "", "stop")
	var result3 map[string]interface{}
	json.Unmarshal(chunk3, &result3)
	choices3 := result3["choices"].([]interface{})
	fr := choices3[0].(map[string]interface{})["finish_reason"]
	if fr != "stop" {
		t.Errorf("finish_reason = %v, want stop", fr)
	}
}

func TestHandleImageRejectsOversizedMultipart(t *testing.T) {
	oldMax := MaxRequestBodySize()
	SetMaxRequestBodySize(1024)
	defer SetMaxRequestBodySize(oldMax)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "edit this image"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	part, err := writer.CreateFormFile("image", "large.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("x"), int(MaxRequestBodySize())+1)); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	handleImage(w, req, "gpt-5.4-mini", true)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleImageRejectsTooManyImages(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"prompt": "draw a square",
		"n": 11
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handleImage(w, req, "gpt-5.4-mini", false)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCORSAllowsXAPIKey(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Access-Control-Request-Headers", "X-API-Key, Content-Type")
	w := httptest.NewRecorder()

	withCORS(next).ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	allow := w.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(allow, "X-API-Key") {
		t.Fatalf("allowed headers = %q, want X-API-Key", allow)
	}
}

func TestCallUpstreamRefreshesOn401AndRetriesWithAccountID(t *testing.T) {
	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth-a.json")
	writeTestAuthFile(t, authFile, "old-token", "refresh-a", "acct-a")
	writeFakeCurl(t, dir, `{"access_token":"new-token","refresh_token":"new-refresh","token_type":"Bearer"}`)

	oldPool := auth.Pool
	oldNormalClient := normalClient
	oldStreamClient := streamClient
	t.Cleanup(func() {
		auth.Pool = oldPool
		normalClient = oldNormalClient
		streamClient = oldStreamClient
	})
	auth.Pool = auth.NewTokenPool([]auth.AccountConfig{{Name: "a", AuthFile: authFile}}, "round-robin")

	type seenRequest struct {
		authHeader string
		accountID  string
	}
	var seen []seenRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, seenRequest{
			authHeader: r.Header.Get("Authorization"),
			accountID:  r.Header.Get("chatgpt-account-id"),
		})
		if len(seen) == 1 {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	normalClient = upstream.Client()
	streamClient = upstream.Client()

	resp, err := callUpstream(context.Background(), upstream.URL, []byte(`{"model":"gpt-5"}`), false)
	if err != nil {
		t.Fatalf("callUpstream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if len(seen) != 2 {
		t.Fatalf("upstream calls = %d, want 2: %#v", len(seen), seen)
	}
	if seen[0].authHeader != "Bearer old-token" || seen[0].accountID != "acct-a" {
		t.Fatalf("first request = %#v, want old token and acct-a", seen[0])
	}
	if seen[1].authHeader != "Bearer new-token" || seen[1].accountID != "acct-a" {
		t.Fatalf("retry request = %#v, want refreshed token and acct-a", seen[1])
	}
}

func TestCallUpstreamFallsBackToAnotherAccountWhenRefreshFails(t *testing.T) {
	dir := t.TempDir()
	authFileA := filepath.Join(dir, "auth-a.json")
	authFileB := filepath.Join(dir, "auth-b.json")
	writeTestAuthFile(t, authFileA, "token-a", "", "acct-a")
	writeTestAuthFile(t, authFileB, "token-b", "refresh-b", "acct-b")

	oldPool := auth.Pool
	oldNormalClient := normalClient
	oldStreamClient := streamClient
	t.Cleanup(func() {
		auth.Pool = oldPool
		normalClient = oldNormalClient
		streamClient = oldStreamClient
	})
	auth.Pool = auth.NewTokenPool([]auth.AccountConfig{
		{Name: "a", AuthFile: authFileA},
		{Name: "b", AuthFile: authFileB},
	}, "round-robin")

	type seenRequest struct {
		authHeader string
		accountID  string
	}
	var seen []seenRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, seenRequest{
			authHeader: r.Header.Get("Authorization"),
			accountID:  r.Header.Get("chatgpt-account-id"),
		})
		if len(seen) == 1 {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	normalClient = upstream.Client()
	streamClient = upstream.Client()

	resp, err := callUpstream(context.Background(), upstream.URL, []byte(`{"model":"gpt-5"}`), false)
	if err != nil {
		t.Fatalf("callUpstream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if len(seen) != 2 {
		t.Fatalf("upstream calls = %d, want 2: %#v", len(seen), seen)
	}
	if seen[0].authHeader != "Bearer token-a" || seen[0].accountID != "acct-a" {
		t.Fatalf("first request = %#v, want account a", seen[0])
	}
	if seen[1].authHeader != "Bearer token-b" || seen[1].accountID != "acct-b" {
		t.Fatalf("fallback request = %#v, want account b", seen[1])
	}
}

func TestNotifyUpstreamRateLimitIncludesAccount(t *testing.T) {
	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth-a.json")
	writeTestAuthFile(t, authFile, "token-a", "refresh-a", "acct-a")

	oldPool := auth.Pool
	oldNormalClient := normalClient
	oldStreamClient := streamClient
	t.Cleanup(func() {
		auth.Pool = oldPool
		normalClient = oldNormalClient
		streamClient = oldStreamClient
		SetRateLimitNotifier(nil)
	})
	auth.Pool = auth.NewTokenPool([]auth.AccountConfig{{Name: "a", AuthFile: authFile}}, "round-robin")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"usage limit reached"}}`))
	}))
	defer upstream.Close()
	normalClient = upstream.Client()
	streamClient = upstream.Client()

	events := make(chan UpstreamRateLimitEvent, 1)
	SetRateLimitNotifier(func(ctx context.Context, event UpstreamRateLimitEvent) {
		events <- event
	})

	resp, err := callUpstream(context.Background(), upstream.URL, []byte(`{"model":"gpt-5"}`), false)
	if err != nil {
		t.Fatalf("callUpstream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	notifyUpstreamRateLimit(context.Background(), resp, upstreamErrorMessage(respBody))

	select {
	case event := <-events:
		if event.AccountName != "a" || event.AccountID != "acct-a" || event.Status != http.StatusTooManyRequests {
			t.Fatalf("event = %#v", event)
		}
		if event.Message != "usage limit reached" {
			t.Fatalf("message = %q", event.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for rate limit event")
	}
}

func writeTestAuthFile(t *testing.T, path, accessToken, refreshToken, accountID string) {
	t.Helper()
	authFile := auth.AuthFile{
		AuthMode: "browser",
		Tokens: auth.Tokens{
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			AccountID:    accountID,
		},
		LastRefresh: time.Now(),
	}
	body, err := json.Marshal(authFile)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
}

func writeFakeCurl(t *testing.T, dir, responseJSON string) {
	t.Helper()
	scriptPath := filepath.Join(dir, "curl")
	script := "#!/bin/sh\nprintf '%s\\n' '" + responseJSON + "'\nprintf '200'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake curl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
