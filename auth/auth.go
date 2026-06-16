package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	OAuthAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	OAuthTokenURL     = "https://auth.openai.com/oauth/token"

	OAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexBaseURL  = "https://chatgpt.com/backend-api/codex"

	RefreshInterval          = 7 * 24 * time.Hour
	ProactiveRefreshInterval = 5 * 24 * time.Hour
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

func curlPostForm(endpoint string, data url.Values) ([]byte, int, error) {
	curlPath, err := exec.LookPath("curl")
	if err != nil {
		return nil, 0, fmt.Errorf("curl not found: %w", err)
	}
	cmd := exec.Command(curlPath,
		"-sL", "-w", "\n%{http_code}",
		"-X", "POST",
		"-H", "Content-Type: application/x-www-form-urlencoded",
		"-H", "Accept: application/json",
		"--data", data.Encode(),
		endpoint,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, 0, fmt.Errorf("curl failed: %w", err)
	}
	// Last line is HTTP status code
	parts := strings.SplitN(string(out), "\n", -1)
	if len(parts) < 2 {
		return nil, 0, fmt.Errorf("unexpected curl output")
	}
	statusStr := strings.TrimSpace(parts[len(parts)-1])
	body := strings.Join(parts[:len(parts)-1], "\n")
	var status int
	fmt.Sscanf(statusStr, "%d", &status)
	return []byte(body), status, nil
}

// ──────────────────────────────────────────────
// Token storage types (matches ~/.codex/auth.json)
// ──────────────────────────────────────────────

type AuthFile struct {
	AuthMode    string    `json:"auth_mode"`
	Tokens      Tokens    `json:"tokens"`
	LastRefresh time.Time `json:"last_refresh"`
}

type Tokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error,omitempty"`
}

// ──────────────────────────────────────────────
// TokenManager: thread-safe token lifecycle
// ──────────────────────────────────────────────

type TokenManager struct {
	mu       sync.RWMutex
	authFile *AuthFile
	filePath string
	name     string
	cancel   context.CancelFunc

	failMu    sync.RWMutex
	lastError error
	failedAt  time.Time
}

var Manager *TokenManager

func init() {
	Manager = &TokenManager{
		name:     "default",
		filePath: defaultAuthPath(),
	}
}

func NewTokenManager(name, filePath string) *TokenManager {
	return &TokenManager{
		name:     name,
		filePath: filePath,
	}
}

func (tm *TokenManager) Name() string     { return tm.name }
func (tm *TokenManager) FilePath() string  { return tm.filePath }

func (tm *TokenManager) MarkFailed(err error) {
	tm.failMu.Lock()
	defer tm.failMu.Unlock()
	tm.lastError = err
	tm.failedAt = time.Now()
	slog.Warn("account marked failed", "account", tm.name, "error", err)
}

func (tm *TokenManager) ClearFailed() {
	tm.failMu.Lock()
	defer tm.failMu.Unlock()
	tm.lastError = nil
	tm.failedAt = time.Time{}
}

func (tm *TokenManager) IsFailed() bool {
	tm.failMu.RLock()
	defer tm.failMu.RUnlock()
	if tm.lastError == nil {
		return false
	}
	// Auto-clear after 5 minutes
	return time.Since(tm.failedAt) <= 5*time.Minute
}

func DefaultAuthPath() string {
	return defaultAuthPath()
}

func SetManagerPath(path string) {
	Manager.filePath = path
}

func defaultAuthPath() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "auth.json")
	}
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".codex", "auth.json")
}

// ──────────────────────────────────────────────
// Background proactive refresh
// ──────────────────────────────────────────────

func (tm *TokenManager) StartBackgroundRefresh(ctx context.Context) {
	ctx, tm.cancel = context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				slog.Info("background token refresh stopped")
				return
			case <-ticker.C:
				tm.mu.RLock()
				needsRefresh := tm.authFile != nil &&
					time.Since(tm.authFile.LastRefresh) > ProactiveRefreshInterval
				tm.mu.RUnlock()

				if needsRefresh {
					slog.Info("proactive token refresh")
					if _, err := tm.RefreshNow(); err != nil {
						slog.Error("proactive refresh failed", "error", err)
					}
				}
			}
		}
	}()
}

func (tm *TokenManager) Stop() {
	if tm.cancel != nil {
		tm.cancel()
	}
}

// IsHealthy reports whether the token state is usable.
func (tm *TokenManager) IsHealthy() (bool, string) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if tm.authFile == nil {
		return false, "auth not loaded"
	}
	if tm.authFile.Tokens.AccessToken == "" {
		return false, "no access token"
	}
	if tm.authFile.Tokens.RefreshToken == "" {
		return false, "no refresh token"
	}
	staleness := time.Since(tm.authFile.LastRefresh)
	if staleness > RefreshInterval {
		return false, fmt.Sprintf("stale (%s)", staleness.Round(time.Minute))
	}
	return true, fmt.Sprintf("fresh (%s)", staleness.Round(time.Minute))
}

// ──────────────────────────────────────────────
// Login - Browser-based OAuth with PKCE
// ──────────────────────────────────────────────

func Login() error {
	return loginBrowser()
}

func loginBrowser() error {
	verifier := generateCodeVerifier()
	challenge := generateCodeChallenge(verifier)
	state := generateState()
	redirectURI := "http://localhost:1455/auth/callback"

	params := url.Values{
		"response_type":              {"code"},
		"client_id":                  {OAuthClientID},
		"redirect_uri":              {redirectURI},
		"code_challenge":            {challenge},
		"code_challenge_method":     {"S256"},
		"scope":                     {"openid profile email offline_access"},
		"state":                     {state},
		"codex_cli_simplified_flow": {"true"},
		"id_token_add_organizations": {"true"},
	}
	authorizeURL := OAuthAuthorizeURL + "?" + params.Encode()

	fmt.Println()
	fmt.Println("  Open this link in your browser to log in:")
	fmt.Println()
	fmt.Println("  " + authorizeURL)
	fmt.Println()
	fmt.Println("  After authorization, browser redirects to localhost (page won't load — that's OK).")
	fmt.Println("  Copy the full URL from the address bar and paste it here:")
	fmt.Print("\n> ")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 4096), 4096)
	if !scanner.Scan() {
		return fmt.Errorf("no input received")
	}
	callbackURL := strings.TrimSpace(scanner.Text())

	parsed, err := url.Parse(callbackURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	code := parsed.Query().Get("code")
	if code == "" {
		return fmt.Errorf("no authorization code found in URL")
	}
	if parsed.Query().Get("state") != state {
		return fmt.Errorf("state mismatch")
	}

	fmt.Println("  Exchanging code for tokens...")

	body, status, err := curlPostForm(OAuthTokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {OAuthClientID},
		"code_verifier": {verifier},
	})
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("token exchange returned %d: %s", status, body)
	}

	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("failed to parse token response: %w", err)
	}

	authFile := &AuthFile{
		AuthMode: "browser",
		Tokens: Tokens{
			IDToken:      tr.IDToken,
			AccessToken:  tr.AccessToken,
			RefreshToken: tr.RefreshToken,
		},
		LastRefresh: time.Now(),
	}
	if err := saveAuthFile(authFile, Manager.filePath); err != nil {
		return err
	}
	Manager.mu.Lock()
	Manager.authFile = authFile
	Manager.mu.Unlock()

	fmt.Println("  ✓ Authenticated successfully!")
	fmt.Printf("  Token saved to %s\n", Manager.filePath)
	return nil
}

func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ──────────────────────────────────────────────
// Token refresh
// ──────────────────────────────────────────────

func (tm *TokenManager) EnsureFreshToken() (string, error) {
	// Fast path: read lock only when token is loaded and fresh
	tm.mu.RLock()
	if tm.authFile != nil && time.Since(tm.authFile.LastRefresh) <= RefreshInterval {
		token := tm.authFile.Tokens.AccessToken
		tm.mu.RUnlock()
		return token, nil
	}
	tm.mu.RUnlock()

	// Slow path: need to load or refresh
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.authFile == nil {
		af, err := loadAuthFile(tm.filePath)
		if err != nil {
			return "", fmt.Errorf("no auth file at %s — run 'codex-proxy login' first: %w",
				tm.filePath, err)
		}
		tm.authFile = af
	}

	if time.Since(tm.authFile.LastRefresh) > RefreshInterval {
		slog.Info("refreshing stale access token")
		if err := tm.refreshLocked(); err != nil {
			return "", fmt.Errorf("token refresh failed: %w", err)
		}
	}

	return tm.authFile.Tokens.AccessToken, nil
}

func (tm *TokenManager) RefreshNow() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.authFile == nil {
		return "", fmt.Errorf("no auth loaded")
	}
	if err := tm.refreshLocked(); err != nil {
		return "", err
	}
	return tm.authFile.Tokens.AccessToken, nil
}

func (tm *TokenManager) refreshLocked() error {
	if tm.authFile.Tokens.RefreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	body, status, err := curlPostForm(OAuthTokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tm.authFile.Tokens.RefreshToken},
		"client_id":     {OAuthClientID},
	})
	if err != nil {
		return fmt.Errorf("refresh request failed: %w", err)
	}

	if status != 200 {
		return fmt.Errorf("refresh returned %d: %s", status, body)
	}

	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("failed to parse refresh response: %w", err)
	}

	tm.authFile.Tokens.AccessToken = tr.AccessToken
	if tr.IDToken != "" {
		tm.authFile.Tokens.IDToken = tr.IDToken
	}
	if tr.RefreshToken != "" {
		tm.authFile.Tokens.RefreshToken = tr.RefreshToken
	}
	tm.authFile.LastRefresh = time.Now()

	slog.Info("token refreshed successfully")
	return saveAuthFile(tm.authFile, tm.filePath)
}

func (tm *TokenManager) GetAccessToken() string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if tm.authFile == nil {
		return ""
	}
	return tm.authFile.Tokens.AccessToken
}

// ──────────────────────────────────────────────
// File I/O
// ──────────────────────────────────────────────

func loadAuthFile(path string) (*AuthFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var af AuthFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, err
	}
	return &af, nil
}

func saveAuthFile(af *AuthFile, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

// ──────────────────────────────────────────────
// Status / Logout
// ──────────────────────────────────────────────

func ShowStatus() {
	af, err := loadAuthFile(Manager.filePath)
	if err != nil {
		fmt.Printf("Not logged in (no auth file at %s)\n", Manager.filePath)
		return
	}

	fmt.Println("  Auth mode:      ", af.AuthMode)
	fmt.Println("  Last refresh:   ", af.LastRefresh.Format(time.RFC3339))
	fmt.Println("  Token staleness:", time.Since(af.LastRefresh).Round(time.Minute))

	tokenPreview := af.Tokens.AccessToken
	if len(tokenPreview) > 20 {
		tokenPreview = tokenPreview[:10] + "..." + tokenPreview[len(tokenPreview)-6:]
	}
	fmt.Println("  Access token:   ", tokenPreview)
	fmt.Println("  Has refresh:    ", af.Tokens.RefreshToken != "")
	fmt.Println("  File:           ", Manager.filePath)

	if time.Since(af.LastRefresh) > RefreshInterval {
		fmt.Println("  ⚠ Token is stale - will refresh on next API call")
	} else {
		fmt.Println("  ✓ Token is fresh")
	}
}

func ShowStatusForFile(name, path string) {
	af, err := loadAuthFile(path)
	if err != nil {
		fmt.Printf("    ✗ Not logged in (%s)\n", path)
		return
	}

	tokenPreview := af.Tokens.AccessToken
	if len(tokenPreview) > 20 {
		tokenPreview = tokenPreview[:10] + "..." + tokenPreview[len(tokenPreview)-6:]
	}

	staleness := time.Since(af.LastRefresh).Round(time.Minute)
	status := "✓ fresh"
	if time.Since(af.LastRefresh) > RefreshInterval {
		status = "⚠ stale"
	}

	fmt.Printf("    %s  token:%s  staleness:%s  refresh:%v\n",
		status, tokenPreview, staleness, af.Tokens.RefreshToken != "")
}

func Logout() {
	if err := os.Remove(Manager.filePath); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("Already logged out")
		} else {
			fmt.Printf("Failed to remove auth file: %v\n", err)
		}
		return
	}
	fmt.Println("Logged out, credentials removed")
}

// ──────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	}
	if cmd != nil {
		_ = cmd.Start()
	}
}

func DiscoverModels(accessToken string) ([]string, error) {
	req, _ := http.NewRequest("GET", CodexBaseURL+"/models?client_version=1.0.0", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-proxy/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("model discovery returned %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}

	body, _ := io.ReadAll(resp.Body)

	if err := json.Unmarshal(body, &result); err != nil {
		var slugs []string
		if err2 := json.Unmarshal(body, &slugs); err2 != nil {
			return nil, fmt.Errorf("cannot parse models response: %s", string(body[:min(200, len(body))]))
		}
		return slugs, nil
	}

	models := make([]string, len(result.Models))
	for i, m := range result.Models {
		models[i] = m.Slug
	}
	return models, nil
}

func BuildCodexRequestBody(chatReq map[string]interface{}) ([]byte, error) {
	codexReq := map[string]interface{}{
		"model":  chatReq["model"],
		"stream": chatReq["stream"],
		"store":  false,
	}

	if messages, ok := chatReq["messages"]; ok {
		if msgList, ok := messages.([]interface{}); ok {
			var instructions []string
			var input []interface{}
			for _, m := range msgList {
				msg, ok := m.(map[string]interface{})
				if !ok {
					input = append(input, m)
					continue
				}
				if msg["role"] == "system" {
					if content, ok := msg["content"].(string); ok {
						instructions = append(instructions, content)
					}
				} else {
					convertContentTypes(msg)
					input = append(input, msg)
				}
			}
			if len(instructions) > 0 {
				codexReq["instructions"] = strings.Join(instructions, "\n")
			} else {
				codexReq["instructions"] = "You are a helpful assistant."
			}
			codexReq["input"] = input
		} else {
			codexReq["input"] = messages
			codexReq["instructions"] = "You are a helpful assistant."
		}
	} else {
		codexReq["instructions"] = "You are a helpful assistant."
	}

	for _, key := range []string{
		"temperature", "top_p", "max_tokens", "max_output_tokens",
		"stop", "tools", "tool_choice", "response_format",
	} {
		if v, ok := chatReq[key]; ok {
			codexReq[key] = v
		}
	}

	return json.Marshal(codexReq)
}

func convertContentTypes(msg map[string]interface{}) {
	role, _ := msg["role"].(string)
	content, ok := msg["content"]
	if !ok {
		return
	}

	// String content → wrap as single content part
	if text, ok := content.(string); ok {
		typeName := "input_text"
		if role == "assistant" {
			typeName = "output_text"
		}
		msg["content"] = []interface{}{
			map[string]interface{}{"type": typeName, "text": text},
		}
		return
	}

	parts, ok := content.([]interface{})
	if !ok {
		return
	}
	for i, p := range parts {
		part, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := part["type"].(string)
		switch t {
		case "text":
			if role == "assistant" {
				part["type"] = "output_text"
			} else {
				part["type"] = "input_text"
			}
		case "image_url":
			// OpenAI: {"type":"image_url","image_url":{"url":"...","detail":"..."}}
			// Codex:  {"type":"input_image","image_url":"..."}
			imgURL := ""
			if obj, ok := part["image_url"].(map[string]interface{}); ok {
				imgURL, _ = obj["url"].(string)
			} else if s, ok := part["image_url"].(string); ok {
				imgURL = s
			}
			parts[i] = map[string]interface{}{
				"type":      "input_image",
				"image_url": imgURL,
			}
		}
	}
}
