package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "edit this image"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	part, err := writer.CreateFormFile("image", "large.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("x"), maxRequestBodySize+1)); err != nil {
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
