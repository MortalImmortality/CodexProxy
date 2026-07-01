package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		_, _ = w.Write([]byte(`{
			"email":"user@example.com",
			"chatgpt_user_id":"user_123",
			"chatgpt_account_id":"acct_123",
			"chatgpt_plan_type":"team",
			"chatgpt_account_is_fedramp":false
		}`))
	}))
	defer server.Close()
	oldWhoamiURL := AccessTokenWhoamiURL
	AccessTokenWhoamiURL = server.URL
	t.Cleanup(func() { AccessTokenWhoamiURL = oldWhoamiURL })

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
	if af.Tokens.AccountID != "acct_123" {
		t.Fatalf("AccountID = %q, want acct_123", af.Tokens.AccountID)
	}
}

func TestAccountIDFallsBackToAccessTokenJWT(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct_jwt"}}`))
	accessToken := "header." + payload + ".sig"

	tm := &TokenManager{
		authFile: &AuthFile{
			AuthMode: "access_token",
			Tokens:   Tokens{AccessToken: accessToken},
		},
	}
	if got := tm.AccountID(); got != "acct_jwt" {
		t.Fatalf("AccountID = %q, want acct_jwt", got)
	}

	// Stored id wins over the JWT claim.
	tm.authFile.Tokens.AccountID = "acct_stored"
	if got := tm.AccountID(); got != "acct_stored" {
		t.Fatalf("AccountID = %q, want acct_stored", got)
	}
}

func TestAccountIDFallsBackToDirectAccessTokenClaim(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"chatgpt_account_id":"acct_direct"}`))
	accessToken := "header." + payload + ".sig"

	tm := &TokenManager{
		authFile: &AuthFile{
			AuthMode: "access_token",
			Tokens:   Tokens{AccessToken: accessToken},
		},
	}
	if got := tm.AccountID(); got != "acct_direct" {
		t.Fatalf("AccountID = %q, want acct_direct", got)
	}
}

func TestLoginWithAccessTokenFallsBackToJWTWhenWhoamiRejectsTokenType(t *testing.T) {
	oldManager := Manager
	t.Cleanup(func() { Manager = oldManager })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"auth_token_type_not_allowed"}}`))
	}))
	defer server.Close()
	oldWhoamiURL := AccessTokenWhoamiURL
	AccessTokenWhoamiURL = server.URL
	t.Cleanup(func() { AccessTokenWhoamiURL = oldWhoamiURL })

	path := filepath.Join(t.TempDir(), "auth.json")
	Manager = NewTokenManager("default", path)
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"email":"user@example.com","chatgpt_account_id":"acct_jwt"}`))
	token := "header." + payload + ".sig"

	if err := LoginWithAccessToken(bytes.NewBufferString(token + "\n")); err != nil {
		t.Fatalf("LoginWithAccessToken: %v", err)
	}

	af, err := loadAuthFile(path)
	if err != nil {
		t.Fatalf("loadAuthFile: %v", err)
	}
	if af.Tokens.AccountID != "acct_jwt" {
		t.Fatalf("AccountID = %q, want acct_jwt", af.Tokens.AccountID)
	}
}

func TestQueryUsageForAccessTokenManagerHydratesAccountID(t *testing.T) {
	var usageSawAccountID, profileSawAccountID bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
			t.Fatalf("%s Authorization = %q, want bearer token", r.URL.Path, got)
		}
		switch r.URL.Path {
		case "/whoami":
			_, _ = w.Write([]byte(`{
				"email":"user@example.com",
				"chatgpt_user_id":"user_123",
				"chatgpt_account_id":"acct_123",
				"chatgpt_plan_type":"team",
				"chatgpt_account_is_fedramp":false
			}`))
		case "/usage":
			usageSawAccountID = r.Header.Get("ChatGPT-Account-Id") == "acct_123"
			_, _ = w.Write([]byte(`{
				"plan_type":"team",
				"email":"user@example.com",
				"rateLimitResetCredits":{"availableCount":2},
				"rate_limit":{
					"allowed":true,
					"limit_reached":false,
					"primary_window":{
						"used_percent":25,
						"limit_window_seconds":3600,
						"reset_after_seconds":1800
					}
				}
			}`))
		case "/profile":
			profileSawAccountID = r.Header.Get("ChatGPT-Account-Id") == "acct_123"
			_, _ = w.Write([]byte(`{
				"stats":{
					"lifetime_tokens":12345,
					"current_streak_days":7,
					"daily_usage_buckets":[{"start_date":"2026-06-30","tokens":42}]
				}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldWhoamiURL, oldUsageURL, oldProfileURL := AccessTokenWhoamiURL, UsageURL, UsageProfileURL
	AccessTokenWhoamiURL = server.URL + "/whoami"
	UsageURL = server.URL + "/usage"
	UsageProfileURL = server.URL + "/profile"
	t.Cleanup(func() {
		AccessTokenWhoamiURL = oldWhoamiURL
		UsageURL = oldUsageURL
		UsageProfileURL = oldProfileURL
	})

	path := filepath.Join(t.TempDir(), "auth.json")
	tm := NewTokenManager("team", path)
	tm.authFile = &AuthFile{
		AuthMode:    "access_token",
		Tokens:      Tokens{AccessToken: "codex-token"},
		LastRefresh: time.Now(),
	}

	info, err := QueryUsageForManager(context.Background(), tm)
	if err != nil {
		t.Fatalf("QueryUsageForManager: %v", err)
	}
	if !usageSawAccountID {
		t.Fatal("usage request did not include ChatGPT-Account-Id")
	}
	if !profileSawAccountID {
		t.Fatal("profile request did not include ChatGPT-Account-Id")
	}
	if tm.AccountID() != "acct_123" {
		t.Fatalf("AccountID = %q, want acct_123", tm.AccountID())
	}
	if info.PlanType != "team" || len(info.Windows) != 1 || info.Windows[0].UsedPercent != 25 {
		t.Fatalf("usage info = %#v", info)
	}
	if info.TokenActivity == nil || info.TokenActivity.Summary.LifetimeTokens == nil || *info.TokenActivity.Summary.LifetimeTokens != 12345 {
		t.Fatalf("TokenActivity = %#v", info.TokenActivity)
	}
	if info.ResetCredits == nil || *info.ResetCredits != 2 {
		t.Fatalf("ResetCredits = %v, want 2", info.ResetCredits)
	}
}

func TestQueryUsageForAccessTokenManagerContinuesWhenWhoamiRejectsTokenType(t *testing.T) {
	var usageCalled, profileCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
			t.Fatalf("%s Authorization = %q, want bearer token", r.URL.Path, got)
		}
		switch r.URL.Path {
		case "/whoami":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"auth_token_type_not_allowed"}}`))
		case "/usage":
			usageCalled = true
			_, _ = w.Write([]byte(`{
				"plan_type":"team",
				"email":"user@example.com",
				"rateLimitResetCredits":{"available_count":1},
				"rate_limit":{
					"allowed":true,
					"limit_reached":false,
					"primary_window":{
						"used_percent":25,
						"limit_window_seconds":3600,
						"reset_after_seconds":1800
					}
				}
			}`))
		case "/profile":
			profileCalled = true
			_, _ = w.Write([]byte(`{"stats":{"lifetime_tokens":12345}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldWhoamiURL, oldUsageURL, oldProfileURL := AccessTokenWhoamiURL, UsageURL, UsageProfileURL
	AccessTokenWhoamiURL = server.URL + "/whoami"
	UsageURL = server.URL + "/usage"
	UsageProfileURL = server.URL + "/profile"
	t.Cleanup(func() {
		AccessTokenWhoamiURL = oldWhoamiURL
		UsageURL = oldUsageURL
		UsageProfileURL = oldProfileURL
	})

	tm := NewAccessTokenManager("team", "codex-token", "")
	info, err := QueryUsageForManager(context.Background(), tm)
	if err != nil {
		t.Fatalf("QueryUsageForManager: %v", err)
	}
	if !usageCalled {
		t.Fatal("usage endpoint was not called")
	}
	if !profileCalled {
		t.Fatal("profile endpoint was not called")
	}
	if info.PlanType != "team" || len(info.Windows) != 1 || info.Windows[0].UsedPercent != 25 {
		t.Fatalf("usage info = %#v", info)
	}
	if info.ResetCredits == nil || *info.ResetCredits != 1 {
		t.Fatalf("ResetCredits = %v, want 1", info.ResetCredits)
	}
}

func TestParseModernUsageBodyIncludesResetCredits(t *testing.T) {
	resetAt := time.Now().Add(time.Hour).Unix()
	body := []byte(fmt.Sprintf(`{
		"rateLimitsByLimitId":{
			"codex":{
				"planType":"plus",
				"primary":{"usedPercent":42,"windowDurationMins":300,"resetsAt":%d}
			}
		},
		"rateLimitResetCredits":{"availableCount":3}
	}`, resetAt))

	info, err := parseUsageBody(body)
	if err != nil {
		t.Fatalf("parseUsageBody: %v", err)
	}
	if info.PlanType != "plus" || len(info.Windows) != 1 || info.Windows[0].UsedPercent != 42 {
		t.Fatalf("usage info = %#v", info)
	}
	if info.ResetCredits == nil || *info.ResetCredits != 3 {
		t.Fatalf("ResetCredits = %v, want 3", info.ResetCredits)
	}
}

func TestResetUsageForManagerClearsFailedState(t *testing.T) {
	var sawAccountID, sawRedeemRequestID bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		sawAccountID = r.Header.Get("ChatGPT-Account-Id") == "acct_123"
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := body["idempotencyKey"]; ok {
			t.Fatal("reset request included obsolete idempotencyKey field")
		}
		sawRedeemRequestID = body["redeem_request_id"] != ""
		_, _ = w.Write([]byte(`{"outcome":"reset"}`))
	}))
	defer server.Close()
	oldResetURL := UsageResetURL
	UsageResetURL = server.URL
	t.Cleanup(func() { UsageResetURL = oldResetURL })

	tm := NewAccessTokenManager("team", "codex-token", "acct_123")
	tm.MarkFailedUntil(fmt.Errorf("rate limited"), time.Now().Add(time.Hour))

	result, err := ResetUsageForManager(context.Background(), tm)
	if err != nil {
		t.Fatalf("ResetUsageForManager: %v", err)
	}
	if result.Outcome != "reset" {
		t.Fatalf("Outcome = %q, want reset", result.Outcome)
	}
	if !sawAccountID {
		t.Fatal("reset request did not include ChatGPT-Account-Id")
	}
	if !sawRedeemRequestID {
		t.Fatal("reset request did not include redeem_request_id")
	}
	if tm.IsFailed() {
		t.Fatal("manager is still failed after reset")
	}
}

func TestResetUsageForOAuthManager(t *testing.T) {
	var sawBearer, sawRedeemRequestID bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer = r.Header.Get("Authorization") == "Bearer oauth-token"
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		sawRedeemRequestID = body["redeem_request_id"] != ""
		_, _ = w.Write([]byte(`{"outcome":"reset"}`))
	}))
	defer server.Close()
	oldResetURL := UsageResetURL
	UsageResetURL = server.URL
	t.Cleanup(func() { UsageResetURL = oldResetURL })

	tm := NewTokenManager("default", filepath.Join(t.TempDir(), "auth.json"))
	tm.authFile = &AuthFile{
		Tokens:      Tokens{AccessToken: "oauth-token"},
		LastRefresh: time.Now(),
	}

	result, err := ResetUsageForManager(context.Background(), tm)
	if err != nil {
		t.Fatalf("ResetUsageForManager: %v", err)
	}
	if result.Outcome != "reset" {
		t.Fatalf("Outcome = %q, want reset", result.Outcome)
	}
	if !sawBearer {
		t.Fatal("reset request did not use OAuth bearer token")
	}
	if !sawRedeemRequestID {
		t.Fatal("reset request did not include redeem_request_id")
	}
}

func TestParseUsageResetBodyDefaultsToResetOnSuccessfulResponse(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "empty json", body: []byte(`{}`)},
		{name: "empty body", body: nil},
		{name: "plain body", body: []byte(`ok`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseUsageResetBody(tt.body)
			if result.Outcome != "reset" {
				t.Fatalf("Outcome = %q, want reset", result.Outcome)
			}
		})
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
