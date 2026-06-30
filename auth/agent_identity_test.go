package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

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
		"aud":              "codex-app-server",
		"iss":              "https://chatgpt.com/codex-backend/agent-identity",
		"account_id":       "acct-1",
		"agent_runtime_id": "agent-1",
		"agent_private_key": pk,
	})
	if !isAgentIdentityToken(agentTok) {
		t.Fatal("expected agent identity token to be detected")
	}

	bearerTok := makeAgentToken(t, map[string]any{
		"aud":               "https://api.openai.com/v1",
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
		"aud":               "codex-app-server",
		"account_id":        "acct-xyz",
		"agent_runtime_id":  "agent-xyz",
		"agent_private_key": pk,
		"email":             "user@example.com",
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
