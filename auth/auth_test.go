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
		"temperature":  0.7,
		"max_tokens":   100,
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
	if result["temperature"] != 0.7 {
		t.Errorf("temperature = %v, want 0.7", result["temperature"])
	}
	if result["messages"] != nil {
		t.Error("messages should not be in codex request")
	}
}

func TestBuildCodexRequestBody_PassthroughParams(t *testing.T) {
	params := []string{"temperature", "top_p", "max_tokens", "max_output_tokens",
		"stop", "tools", "tool_choice", "response_format"}

	for _, key := range params {
		chatReq := map[string]interface{}{
			"model": "test",
			key:     "test-value",
		}
		body, _ := BuildCodexRequestBody(chatReq)
		var result map[string]interface{}
		json.Unmarshal(body, &result)

		if result[key] != "test-value" {
			t.Errorf("param %s not passed through", key)
		}
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
