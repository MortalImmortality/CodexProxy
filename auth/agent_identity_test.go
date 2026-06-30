package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// makeAgentToken builds a synthetic agent-identity JWT (header.payload.sig) for
// the given claims. Only the payload segment is read by the code under test.
func makeAgentToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	seg := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return seg([]byte(`{"alg":"none"}`)) + "." + seg(payload) + "." + seg([]byte("sig"))
}

func newTestAgentKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return priv, base64.StdEncoding.EncodeToString(der)
}

func TestIsAgentIdentityToken(t *testing.T) {
	_, pk := newTestAgentKey(t)
	agentTok := makeAgentToken(t, map[string]any{
		"aud":               "codex-app-server",
		"iss":               "https://chatgpt.com/codex-backend/agent-identity",
		"account_id":        "acct-1",
		"agent_runtime_id":  "agent-1",
		"agent_private_key": pk,
	})
	if !isAgentIdentityToken(agentTok) {
		t.Fatal("expected agent identity token to be detected")
	}

	bearerTok := makeAgentToken(t, map[string]any{
		"aud":                "https://api.openai.com/v1",
		"chatgpt_account_id": "acct-2",
	})
	if isAgentIdentityToken(bearerTok) {
		t.Fatal("ordinary bearer token misclassified as agent identity")
	}
	if isAgentIdentityToken("not-a-jwt") {
		t.Fatal("non-jwt misclassified as agent identity")
	}
}

func TestNewAgentIdentity(t *testing.T) {
	priv, pk := newTestAgentKey(t)
	tok := makeAgentToken(t, map[string]any{
		"aud":                        "codex-app-server",
		"account_id":                 "acct-xyz",
		"agent_runtime_id":           "agent-xyz",
		"agent_private_key":          pk,
		"email":                      "user@example.com",
		"chatgpt_account_is_fedramp": true,
	})
	ai, err := newAgentIdentity(tok)
	if err != nil {
		t.Fatalf("newAgentIdentity: %v", err)
	}
	if ai == nil {
		t.Fatal("expected non-nil agent identity")
	}
	if ai.runtimeID != "agent-xyz" || ai.accountID != "acct-xyz" || ai.email != "user@example.com" || !ai.fedramp {
		t.Fatalf("unexpected fields: %+v", ai)
	}
	if !ai.signer.Equal(priv) {
		t.Fatal("parsed signer does not match original key")
	}

	// Ordinary bearer tokens yield (nil, nil).
	ai2, err := newAgentIdentity(makeAgentToken(t, map[string]any{"aud": "https://api.openai.com/v1"}))
	if err != nil || ai2 != nil {
		t.Fatalf("expected nil agent identity for bearer token, got %v / %v", ai2, err)
	}
}

func TestDecryptTaskIDRoundTrip(t *testing.T) {
	priv, pk := newTestAgentKey(t)
	ai, err := newAgentIdentity(makeAgentToken(t, map[string]any{
		"aud":               "codex-app-server",
		"agent_runtime_id":  "agent-1",
		"agent_private_key": pk,
	}))
	if err != nil || ai == nil {
		t.Fatalf("newAgentIdentity: %v", err)
	}

	// Seal a task id to the X25519 public key derived from the signer, the way
	// the backend does, then confirm decryptTaskID recovers it.
	secret := curve25519SecretFromEd25519(priv)
	var pub [32]byte
	pk32, err := curve25519.X25519(secret[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}
	copy(pub[:], pk32)
	sealed, err := box.SealAnonymous(nil, []byte("task-secret-42"), &pub, rand.Reader)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := ai.decryptTaskID(base64.StdEncoding.EncodeToString(sealed))
	if err != nil {
		t.Fatalf("decryptTaskID: %v", err)
	}
	if got != "task-secret-42" {
		t.Fatalf("round trip mismatch: %q", got)
	}
}

func TestAgentAssertionHeader(t *testing.T) {
	priv, pk := newTestAgentKey(t)
	ai, err := newAgentIdentity(makeAgentToken(t, map[string]any{
		"aud":               "codex-app-server",
		"account_id":        "acct-h",
		"agent_runtime_id":  "agent-h",
		"agent_private_key": pk,
	}))
	if err != nil || ai == nil {
		t.Fatalf("newAgentIdentity: %v", err)
	}
	ai.taskID = "task-h" // skip network registration

	req, _ := http.NewRequest("POST", "https://example.com", nil)
	if err := ai.apply(context.Background(), req, ""); err != nil {
		t.Fatalf("apply: %v", err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AgentAssertion ") {
		t.Fatalf("expected AgentAssertion header, got %q", auth)
	}
	if req.Header.Get("ChatGPT-Account-Id") != "acct-h" {
		t.Fatalf("missing account header: %q", req.Header.Get("ChatGPT-Account-Id"))
	}

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(auth, "AgentAssertion "))
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var env struct {
		RuntimeID string `json:"agent_runtime_id"`
		Signature string `json:"signature"`
		TaskID    string `json:"task_id"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.RuntimeID != "agent-h" || env.TaskID != "task-h" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	payload := env.RuntimeID + ":" + env.TaskID + ":" + env.Timestamp
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), []byte(payload), sig) {
		t.Fatal("agent assertion signature failed to verify")
	}
}

func TestQueryUsageContextWithAccountIDUsesAgentAssertion(t *testing.T) {
	_, pk := newTestAgentKey(t)
	tok := makeAgentToken(t, map[string]any{
		"aud":               "codex-app-server",
		"iss":               "https://chatgpt.com/codex-backend/agent-identity",
		"account_id":        "acct-usage",
		"agent_runtime_id":  "agent-usage",
		"agent_private_key": pk,
		"email":             "teacher@example.com",
		"plan_type":         "k12",
	})

	oldClient := httpClient
	defer func() { httpClient = oldClient }()

	var sawRegister bool
	var sawUsage bool
	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/task/register"):
			sawRegister = true
			return jsonResponse(200, `{"task_id":"task-usage"}`), nil
		case req.URL.String() == UsageURL:
			sawUsage = true
			if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "AgentAssertion ") {
				t.Fatalf("expected AgentAssertion usage auth, got %q", got)
			}
			if got := req.Header.Get("ChatGPT-Account-Id"); got != "acct-usage" {
				t.Fatalf("account header = %q, want acct-usage", got)
			}
			return jsonResponse(200, `{"plan_type":"k12","email":"teacher@example.com","rate_limit":{"allowed":true}}`), nil
		case req.URL.String() == UsageProfileURL:
			if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "AgentAssertion ") {
				t.Fatalf("expected AgentAssertion profile auth, got %q", got)
			}
			return jsonResponse(401, `{"detail":"Unauthorized"}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})}

	info, err := QueryUsageContextWithAccountID(context.Background(), tok, "")
	if err != nil {
		t.Fatalf("QueryUsageContextWithAccountID: %v", err)
	}
	if !sawRegister || !sawUsage {
		t.Fatalf("expected register and usage requests, register=%v usage=%v", sawRegister, sawUsage)
	}
	if info.Email != "teacher@example.com" || info.PlanType != "k12" {
		t.Fatalf("unexpected usage info: %+v", info)
	}
}

func TestQueryUsageContextWithAccountIDKeepsBearerForOrdinaryToken(t *testing.T) {
	tok := makeAgentToken(t, map[string]any{
		"aud":                "https://api.openai.com/v1",
		"chatgpt_account_id": "acct-bearer",
	})

	oldClient := httpClient
	defer func() { httpClient = oldClient }()

	httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/task/register") {
			t.Fatal("ordinary bearer token should not register an agent task")
		}
		switch req.URL.String() {
		case UsageURL:
			if got := req.Header.Get("Authorization"); got != "Bearer "+tok {
				t.Fatalf("expected bearer usage auth, got %q", got)
			}
			if got := req.Header.Get("ChatGPT-Account-Id"); got != "acct-bearer" {
				t.Fatalf("account header = %q, want acct-bearer", got)
			}
			return jsonResponse(200, `{"plan_type":"plus","email":"user@example.com","rate_limit":{"allowed":true}}`), nil
		case UsageProfileURL:
			return jsonResponse(401, `{"detail":"Unauthorized"}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})}

	info, err := QueryUsageContextWithAccountID(context.Background(), tok, "acct-bearer")
	if err != nil {
		t.Fatalf("QueryUsageContextWithAccountID: %v", err)
	}
	if info.Email != "user@example.com" || info.PlanType != "plus" {
		t.Fatalf("unexpected usage info: %+v", info)
	}
}
