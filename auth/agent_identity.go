package auth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// ──────────────────────────────────────────────
// Codex agent-identity auth (aligns with codex-rs/agent-identity)
//
// `codex login --with-access-token` stores an "agent identity" credential: a
// JWT with aud=codex-app-server carrying an embedded ed25519 private key. Such
// tokens are NOT usable as a Bearer credential — chatgpt.com/backend-api
// rejects them with 401 "Could not parse your authentication token". Instead
// every request must carry an `AgentAssertion` header signed with the embedded
// key, and the assertion references a task id obtained by registering the
// agent runtime with auth.openai.com first.
// ──────────────────────────────────────────────

const (
	agentIdentityAudience       = "codex-app-server"
	agentIdentityIssuerFragment = "codex-backend/agent-identity"

	// Task registration + JWKS live on the accounts auth API. We only need the
	// production base URL (codex uses a staging variant for the org tenant).
	agentIdentityAuthAPIBase = "https://auth.openai.com/api/accounts"
)

// agentIdentityClaims are the JWT payload fields codex embeds in the stored
// credential. Only the agent runtime id, private key, and account routing
// fields are required to mint AgentAssertion headers.
type agentIdentityClaims struct {
	Audience      string `json:"aud"`
	Issuer        string `json:"iss"`
	AccountID     string `json:"account_id"`
	RuntimeID     string `json:"agent_runtime_id"`
	PrivateKey    string `json:"agent_private_key"`
	ChatGPTUserID string `json:"chatgpt_user_id"`
	Email         string `json:"email"`
	PlanType      string `json:"plan_type"`
	IsFedRAMP     bool   `json:"chatgpt_account_is_fedramp"`
}

// agentIdentity holds the parsed credential plus a cached task id. A task id is
// minted once via registration and reused across requests; a 401 invalidates it
// so the next request re-registers.
type agentIdentity struct {
	runtimeID string
	signer    ed25519.PrivateKey
	accountID string
	email     string
	planType  string
	fedramp   bool

	mu     sync.Mutex
	taskID string
}

// isAgentIdentityToken reports whether an access token is a codex agent-identity
// credential (and therefore must use AgentAssertion auth, not Bearer).
func isAgentIdentityToken(accessToken string) bool {
	claims, err := decodeAgentIdentityClaims(accessToken)
	if err != nil {
		return false
	}
	return claims.isAgentIdentity()
}

func (c *agentIdentityClaims) isAgentIdentity() bool {
	if c == nil {
		return false
	}
	if c.Audience == agentIdentityAudience {
		return true
	}
	return c.PrivateKey != "" && c.RuntimeID != "" &&
		bytes.Contains([]byte(c.Issuer), []byte(agentIdentityIssuerFragment))
}

func decodeAgentIdentityClaims(accessToken string) (*agentIdentityClaims, error) {
	var claims agentIdentityClaims
	if !decodeJWTClaims(accessToken, &claims) {
		return nil, fmt.Errorf("not a JWT")
	}
	return &claims, nil
}

// newAgentIdentity parses an agent-identity JWT into a usable signer. Returns
// nil (without error) when the token is not an agent-identity credential.
func newAgentIdentity(accessToken string) (*agentIdentity, error) {
	claims, err := decodeAgentIdentityClaims(accessToken)
	if err != nil || !claims.isAgentIdentity() {
		return nil, nil
	}
	if claims.RuntimeID == "" || claims.PrivateKey == "" {
		return nil, fmt.Errorf("agent identity token missing runtime id or private key")
	}
	signer, err := ed25519SignerFromPKCS8Base64(claims.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("agent identity private key: %w", err)
	}
	return &agentIdentity{
		runtimeID: claims.RuntimeID,
		signer:    signer,
		accountID: claims.AccountID,
		email:     claims.Email,
		planType:  claims.PlanType,
		fedramp:   claims.IsFedRAMP,
	}, nil
}

func ed25519SignerFromPKCS8Base64(pkcs8Base64 string) (ed25519.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(pkcs8Base64)
	if err != nil {
		return nil, fmt.Errorf("private key is not valid base64: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("private key is not valid PKCS#8: %w", err)
	}
	signer, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not ed25519")
	}
	return signer, nil
}

func agentTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

func (a *agentIdentity) sign(payload string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(a.signer, []byte(payload)))
}

// apply sets the auth headers for an upstream Codex request, registering a task
// id on first use. accountID overrides the embedded account id when non-empty.
func (a *agentIdentity) apply(ctx context.Context, req *http.Request, accountID string) error {
	taskID, err := a.ensureTaskID(ctx)
	if err != nil {
		return err
	}
	ts := agentTimestamp()
	envelope, err := json.Marshal(map[string]string{
		"agent_runtime_id": a.runtimeID,
		"signature":        a.sign(a.runtimeID + ":" + taskID + ":" + ts),
		"task_id":          taskID,
		"timestamp":        ts,
	})
	if err != nil {
		return fmt.Errorf("agent assertion encode: %w", err)
	}
	req.Header.Set("Authorization", "AgentAssertion "+base64.RawURLEncoding.EncodeToString(envelope))
	if accountID == "" {
		accountID = a.accountID
	}
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	if a.fedramp {
		req.Header.Set("X-OpenAI-Fedramp", "true")
	}
	return nil
}

// invalidateTask drops the cached task id so the next request re-registers.
func (a *agentIdentity) invalidateTask() {
	a.mu.Lock()
	a.taskID = ""
	a.mu.Unlock()
}

func (a *agentIdentity) ensureTaskID(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.taskID != "" {
		return a.taskID, nil
	}
	taskID, err := a.registerTask(ctx)
	if err != nil {
		return "", err
	}
	a.taskID = taskID
	return taskID, nil
}

// registerTask registers the agent runtime for a task and returns the task id.
// The response returns the id either in cleartext or sealed (curve25519) — codex
// decrypts the latter with the X25519 key derived from the ed25519 signer.
func (a *agentIdentity) registerTask(ctx context.Context) (string, error) {
	ts := agentTimestamp()
	body, err := json.Marshal(map[string]string{
		"timestamp": ts,
		"signature": a.sign(a.runtimeID + ":" + ts),
	})
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/v1/agent/%s/task/register", agentIdentityAuthAPIBase, a.runtimeID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-proxy/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent task registration failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("agent task registration returned %d: %s",
			resp.StatusCode, string(respBody[:min(200, len(respBody))]))
	}

	var parsed struct {
		TaskID          string `json:"task_id"`
		TaskIDCamel     string `json:"taskId"`
		EncryptedTaskID string `json:"encrypted_task_id"`
		EncryptedCamel  string `json:"encryptedTaskId"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("agent task registration parse: %w", err)
	}
	if id := firstNonEmpty(parsed.TaskID, parsed.TaskIDCamel); id != "" {
		return id, nil
	}
	encrypted := firstNonEmpty(parsed.EncryptedTaskID, parsed.EncryptedCamel)
	if encrypted == "" {
		return "", fmt.Errorf("agent task registration omitted task id")
	}
	return a.decryptTaskID(encrypted)
}

// decryptTaskID opens a libsodium sealed box (curve25519 + XSalsa20-Poly1305)
// using the X25519 key derived from the ed25519 signer, matching codex.
func (a *agentIdentity) decryptTaskID(encrypted string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("encrypted task id is not valid base64: %w", err)
	}
	priv := curve25519SecretFromEd25519(a.signer)
	var pub [32]byte
	pk, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("derive curve25519 public key: %w", err)
	}
	copy(pub[:], pk)

	plaintext, ok := box.OpenAnonymous(nil, ciphertext, &pub, &priv)
	if !ok {
		return "", fmt.Errorf("failed to decrypt agent task id")
	}
	return string(plaintext), nil
}

// curve25519SecretFromEd25519 converts an ed25519 signing key to the clamped
// X25519 secret used by codex: sha512(seed)[:32] with the standard clamp.
func curve25519SecretFromEd25519(signer ed25519.PrivateKey) [32]byte {
	digest := sha512.Sum512(signer.Seed())
	var secret [32]byte
	copy(secret[:], digest[:32])
	secret[0] &= 248
	secret[31] &= 127
	secret[31] |= 64
	return secret
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
