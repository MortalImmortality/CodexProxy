package auth

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestBuildCodexRequestBody(t *testing.T) {
	chatReq := map[string]interface{}{
		"model": "o3-pro",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
		"stream":      true,
		"temperature": 0.7,
		"max_tokens":  100,
	}

	body, err := BuildCodexRequestBody(chatReq)
	if err != nil {
		t.Fatalf("BuildCodexRequestBody: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result["model"] != "o3-pro" {
		t.Errorf("model = %v, want o3-pro", result["model"])
	}
	if result["stream"] != true {
		t.Errorf("stream = %v, want true", result["stream"])
	}
	if result["input"] == nil {
		t.Error("input is nil, want messages mapped to input")
	}
	if _, ok := result["temperature"]; ok {
		t.Error("temperature should be dropped for reasoning models")
	}
	if _, ok := result["max_output_tokens"]; ok {
		t.Error("max_tokens should not be mapped to max_output_tokens for Codex backend")
	}
	if result["messages"] != nil {
		t.Error("messages should not be in codex request")
	}
}

func TestBuildCodexRequestBody_NonReasoningSamplingParams(t *testing.T) {
	chatReq := map[string]interface{}{
		"model":       "gpt-4.1",
		"temperature": 0.7,
		"top_p":       0.9,
	}
	body, _ := BuildCodexRequestBody(chatReq)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	if result["temperature"] != 0.7 {
		t.Errorf("temperature = %v, want 0.7", result["temperature"])
	}
	if result["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want 0.9", result["top_p"])
	}
}

func TestBuildCodexRequestBody_ReasoningEffort(t *testing.T) {
	tests := []struct {
		name    string
		chatReq map[string]interface{}
		want    interface{}
	}{
		{
			name: "reasoning object",
			chatReq: map[string]interface{}{
				"model": "gpt-5",
				"reasoning": map[string]interface{}{
					"effort":  "high",
					"summary": "auto",
				},
			},
			want: "high",
		},
		{
			name: "reasoning_effort alias",
			chatReq: map[string]interface{}{
				"model":            "gpt-5",
				"reasoning_effort": "medium",
			},
			want: "medium",
		},
		{
			name: "top level effort alias",
			chatReq: map[string]interface{}{
				"model":  "gpt-5",
				"effort": "low",
			},
			want: "low",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := BuildCodexRequestBody(tt.chatReq)
			if err != nil {
				t.Fatalf("BuildCodexRequestBody: %v", err)
			}
			var result map[string]interface{}
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}

			reasoning, ok := result["reasoning"].(map[string]interface{})
			if !ok {
				t.Fatalf("reasoning = %T, want object", result["reasoning"])
			}
			if reasoning["effort"] != tt.want {
				t.Errorf("reasoning.effort = %v, want %v", reasoning["effort"], tt.want)
			}
			if _, ok := result["effort"]; ok {
				t.Error("top-level effort should be mapped to reasoning.effort")
			}
			if _, ok := result["reasoning_effort"]; ok {
				t.Error("reasoning_effort should be mapped to reasoning.effort")
			}
		})
	}
}

func TestBuildCodexRequestBody_ConvertsToolsAndResponseFormat(t *testing.T) {
	chatReq := map[string]interface{}{
		"model": "gpt-4.1",
		"tool_choice": map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": "lookup",
			},
		},
		"tools": []interface{}{
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "lookup",
					"description": "Lookup a value",
					"parameters": map[string]interface{}{
						"type": "object",
					},
					"strict": true,
				},
			},
		},
		"response_format": map[string]interface{}{
			"type": "json_schema",
			"json_schema": map[string]interface{}{
				"name":   "answer",
				"schema": map[string]interface{}{"type": "object"},
				"strict": true,
			},
		},
	}
	body, _ := BuildCodexRequestBody(chatReq)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	toolChoice := result["tool_choice"].(map[string]interface{})
	if toolChoice["name"] != "lookup" {
		t.Errorf("tool_choice = %v, want flattened lookup", toolChoice)
	}
	tools := result["tools"].([]interface{})
	tool := tools[0].(map[string]interface{})
	if tool["name"] != "lookup" {
		t.Errorf("tool name = %v, want lookup", tool["name"])
	}
	if _, ok := tool["function"]; ok {
		t.Error("function tool should be flattened")
	}
	text := result["text"].(map[string]interface{})
	format := text["format"].(map[string]interface{})
	if format["type"] != "json_schema" || format["name"] != "answer" {
		t.Errorf("format = %v, want flattened json_schema", format)
	}
}

func TestTokenManagerIsHealthy(t *testing.T) {
	tm := &TokenManager{
		name:     "test",
		filePath: "/tmp/test-auth.json",
	}

	healthy, _ := tm.IsHealthy()
	if healthy {
		t.Error("nil authFile should not be healthy")
	}

	tm.authFile = &AuthFile{
		Tokens:      Tokens{AccessToken: "tok", RefreshToken: "ref"},
		LastRefresh: time.Now(),
	}

	healthy, _ = tm.IsHealthy()
	if !healthy {
		t.Error("fresh token should be healthy")
	}

	tm.authFile.LastRefresh = time.Now().Add(-8 * 24 * time.Hour)
	healthy, _ = tm.IsHealthy()
	if healthy {
		t.Error("stale token should not be healthy")
	}
}

func TestTokenManagerFailTracking(t *testing.T) {
	tm := NewTokenManager("test", "/tmp/test.json")

	if tm.IsFailed() {
		t.Error("new manager should not be failed")
	}

	tm.MarkFailed(nil)
	// MarkFailed with nil error still sets failedAt
	// but lastError is nil so IsFailed checks lastError
	tm.lastError = fmt.Errorf("test error")
	if !tm.IsFailed() {
		t.Error("should be failed after MarkFailed")
	}

	tm.ClearFailed()
	if tm.IsFailed() {
		t.Error("should not be failed after ClearFailed")
	}
}
