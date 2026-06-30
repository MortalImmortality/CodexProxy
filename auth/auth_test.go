package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
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

func TestAccessTokenManagerUsesStaticToken(t *testing.T) {
	tm := NewAccessTokenManager("team", "codex-token", "")

	token, err := tm.EnsureFreshToken()
	if err != nil {
		t.Fatalf("EnsureFreshToken: %v", err)
	}
	if token != "codex-token" {
		t.Fatalf("token = %q, want codex-token", token)
	}

	healthy, reason := tm.IsHealthy()
	if !healthy {
		t.Fatalf("static access token should be healthy: %s", reason)
	}

	if _, err := tm.RefreshNow(); err == nil {
		t.Fatal("RefreshNow succeeded for static access token, want error")
	}
}

func TestLoginWithAccessTokenPersistsStaticAuthFile(t *testing.T) {
	oldManager := Manager
	t.Cleanup(func() { Manager = oldManager })

	path := filepath.Join(t.TempDir(), "auth.json")
	Manager = NewTokenManager("default", path)

	if err := LoginWithAccessToken(bytes.NewBufferString("  codex-token\n")); err != nil {
		t.Fatalf("LoginWithAccessToken: %v", err)
	}

	af, err := loadAuthFile(path)
	if err != nil {
		t.Fatalf("loadAuthFile: %v", err)
	}
	if af.AuthMode != "access_token" {
		t.Fatalf("AuthMode = %q, want access_token", af.AuthMode)
	}
	if af.Tokens.AccessToken != "codex-token" {
		t.Fatalf("AccessToken = %q, want codex-token", af.Tokens.AccessToken)
	}
	if af.Tokens.RefreshToken != "" {
		t.Fatalf("RefreshToken = %q, want empty", af.Tokens.RefreshToken)
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

func TestBuildCodexRequestBody_ConvertsObjectToolChoiceAuto(t *testing.T) {
	chatReq := map[string]interface{}{
		"model":       "gpt-5",
		"tool_choice": map[string]interface{}{"type": "auto"},
	}

	body, err := BuildCodexRequestBody(chatReq)
	if err != nil {
		t.Fatalf("BuildCodexRequestBody: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", result["tool_choice"])
	}
}

func TestBuildCodexRequestBody_ForwardsCompatibleChatParams(t *testing.T) {
	chatReq := map[string]interface{}{
		"model":                 "gpt-5",
		"max_completion_tokens": 512,
		"parallel_tool_calls":   false,
		"verbosity":             "low",
		"metadata":              map[string]interface{}{"request_id": "req-123"},
		"response_format":       map[string]interface{}{"type": "json_object"},
		"reasoning_effort":      "medium",
		"max_tokens":            100,
		"max_output_tokens":     200,
		"temperature":           0.7,
		"top_p":                 0.9,
		"stop":                  []interface{}{"END"},
	}

	body, err := BuildCodexRequestBody(chatReq)
	if err != nil {
		t.Fatalf("BuildCodexRequestBody: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if _, ok := result["max_output_tokens"]; ok {
		t.Error("max_completion_tokens should be dropped for Codex backend")
	}
	if result["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false", result["parallel_tool_calls"])
	}
	if metadata, ok := result["metadata"].(map[string]interface{}); !ok || metadata["request_id"] != "req-123" {
		t.Errorf("metadata = %v, want request_id", result["metadata"])
	}
	text, ok := result["text"].(map[string]interface{})
	if !ok {
		t.Fatalf("text = %T, want object", result["text"])
	}
	if text["verbosity"] != "low" {
		t.Errorf("text.verbosity = %v, want low", text["verbosity"])
	}
	format := text["format"].(map[string]interface{})
	if format["type"] != "json_object" {
		t.Errorf("text.format = %v, want json_object", format)
	}
	if _, ok := result["max_tokens"]; ok {
		t.Error("max_tokens should still be dropped")
	}
	if _, ok := result["temperature"]; ok {
		t.Error("temperature should still be dropped for reasoning models")
	}
	if _, ok := result["top_p"]; ok {
		t.Error("top_p should still be dropped for reasoning models")
	}
	if _, ok := result["stop"]; ok {
		t.Error("stop should still be dropped")
	}
}

func TestBuildCodexRequestBody_ConvertsAnthropicStyleTools(t *testing.T) {
	chatReq := map[string]interface{}{
		"model": "gpt-5",
		"tools": []interface{}{
			map[string]interface{}{
				"name":        "search_files",
				"description": "Search files by pattern",
				"input_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{"type": "string"},
					},
					"required": []interface{}{"query"},
				},
			},
		},
	}

	body, err := BuildCodexRequestBody(chatReq)
	if err != nil {
		t.Fatalf("BuildCodexRequestBody: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	tools := result["tools"].([]interface{})
	tool := tools[0].(map[string]interface{})
	if tool["type"] != "function" {
		t.Errorf("tool type = %v, want function", tool["type"])
	}
	if tool["name"] != "search_files" {
		t.Errorf("tool name = %v, want search_files", tool["name"])
	}
	if _, ok := tool["parameters"].(map[string]interface{}); !ok {
		t.Errorf("parameters = %T, want object", tool["parameters"])
	}
	if _, ok := tool["input_schema"]; ok {
		t.Error("input_schema should be mapped to parameters")
	}
}

func TestBuildCodexRequestBody_ConvertsDeprecatedFunctions(t *testing.T) {
	chatReq := map[string]interface{}{
		"model": "gpt-4.1",
		"function_call": map[string]interface{}{
			"name": "lookup",
		},
		"functions": []interface{}{
			map[string]interface{}{
				"name":        "lookup",
				"description": "Lookup a value",
				"parameters":  map[string]interface{}{"type": "object"},
			},
		},
	}

	body, err := BuildCodexRequestBody(chatReq)
	if err != nil {
		t.Fatalf("BuildCodexRequestBody: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	toolChoice := result["tool_choice"].(map[string]interface{})
	if toolChoice["type"] != "function" || toolChoice["name"] != "lookup" {
		t.Errorf("tool_choice = %v, want function lookup", toolChoice)
	}
	tools := result["tools"].([]interface{})
	tool := tools[0].(map[string]interface{})
	if tool["type"] != "function" || tool["name"] != "lookup" {
		t.Errorf("tool = %v, want function lookup", tool)
	}
	if _, ok := result["functions"]; ok {
		t.Error("functions should be mapped to tools")
	}
	if _, ok := result["function_call"]; ok {
		t.Error("function_call should be mapped to tool_choice")
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

	until := time.Now().Add(time.Hour)
	tm.MarkFailedUntil(fmt.Errorf("limited"), until)
	if !tm.IsFailed() {
		t.Error("should be failed before failedUntil")
	}
	if got := tm.FailedUntil(); !got.Equal(until) {
		t.Fatalf("FailedUntil = %s, want %s", got, until)
	}

	tm.MarkFailedUntil(fmt.Errorf("expired"), time.Now().Add(-time.Second))
	if tm.IsFailed() {
		t.Error("should not be failed after failedUntil")
	}
}
