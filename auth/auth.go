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
// Token storage types (matches ~/.codex-proxy/auth.json)
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
func (tm *TokenManager) FilePath() string { return tm.filePath }

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
	return filepath.Join(homeDir, ".codex-proxy", "auth.json")
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
		"redirect_uri":               {redirectURI},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"scope":                      {"openid profile email offline_access"},
		"state":                      {state},
		"codex_cli_simplified_flow":  {"true"},
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

// AccountID returns the ChatGPT account id sent as the `chatgpt-account-id`
// header. Prefers the stored value, falling back to the id_token JWT claim.
func (tm *TokenManager) AccountID() string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if tm.authFile == nil {
		return ""
	}
	if id := tm.authFile.Tokens.AccountID; id != "" {
		return id
	}
	return accountIDFromJWT(tm.authFile.Tokens.IDToken)
}

// accountIDFromJWT decodes an OpenAI id_token and extracts
// auth["chatgpt_account_id"]. Returns "" on any parse failure.
func accountIDFromJWT(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
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

const UsageURL = "https://chatgpt.com/backend-api/wham/usage"

type UsageWindow struct {
	Name        string `json:"name"`
	UsedPercent int    `json:"used_percent"`
	ResetSecs   int    `json:"reset_after_seconds"`
	WindowSecs  int    `json:"limit_window_seconds"`
}

type UsageInfo struct {
	PlanType string        `json:"plan_type"`
	Email    string        `json:"email"`
	Allowed  bool          `json:"allowed"`
	LimitHit bool          `json:"limit_reached"`
	Windows  []UsageWindow `json:"windows"`
	RawJSON  string        `json:"-"`
}

type usageRawWindow struct {
	UsedPercent     int `json:"used_percent"`
	LimitWindowSecs int `json:"limit_window_seconds"`
	ResetAfterSecs  int `json:"reset_after_seconds"`
}

func windowLabel(windowSecs int, fallback string) string {
	switch {
	case windowSecs <= 0:
		return fallback
	case windowSecs < 3600:
		return fmt.Sprintf("%dm", windowSecs/60)
	case windowSecs < 86400:
		return fmt.Sprintf("%dh", windowSecs/3600)
	case windowSecs < 604800:
		return fmt.Sprintf("%dd", windowSecs/86400)
	default:
		return "weekly"
	}
}

func QueryUsage(accessToken string) (*UsageInfo, error) {
	req, _ := http.NewRequest("GET", UsageURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "codex-proxy/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("usage query returned %d: %s", resp.StatusCode, string(body[:min(200, len(body))]))
	}

	var raw struct {
		PlanType  string `json:"plan_type"`
		Email     string `json:"email"`
		RateLimit *struct {
			Allowed         bool            `json:"allowed"`
			LimitReached    bool            `json:"limit_reached"`
			PrimaryWindow   *usageRawWindow `json:"primary_window"`
			SecondaryWindow *usageRawWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("cannot parse usage: %w", err)
	}

	info := &UsageInfo{PlanType: raw.PlanType, Email: raw.Email, RawJSON: string(body)}
	if raw.RateLimit != nil {
		info.Allowed = raw.RateLimit.Allowed
		info.LimitHit = raw.RateLimit.LimitReached
		if w := raw.RateLimit.PrimaryWindow; w != nil {
			info.Windows = append(info.Windows, UsageWindow{
				Name:        windowLabel(w.LimitWindowSecs, "session"),
				UsedPercent: w.UsedPercent,
				ResetSecs:   w.ResetAfterSecs,
				WindowSecs:  w.LimitWindowSecs,
			})
		}
		if w := raw.RateLimit.SecondaryWindow; w != nil {
			info.Windows = append(info.Windows, UsageWindow{
				Name:        windowLabel(w.LimitWindowSecs, "weekly"),
				UsedPercent: w.UsedPercent,
				ResetSecs:   w.ResetAfterSecs,
				WindowSecs:  w.LimitWindowSecs,
			})
		}
	}
	return info, nil
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
				role, _ := msg["role"].(string)

				switch role {
				case "system":
					if content, ok := msg["content"].(string); ok {
						instructions = append(instructions, content)
					}

				case "tool":
					// Chat: {"role":"tool","tool_call_id":"x","content":"result"}
					// Codex: {"type":"function_call_output","call_id":"x","output":"result"}
					output, _ := msg["content"].(string)
					input = append(input, map[string]interface{}{
						"type":    "function_call_output",
						"call_id": msg["tool_call_id"],
						"output":  output,
					})

				case "assistant":
					// Handle text/refusal content as message
					hasContent := false
					assistantMsg := map[string]interface{}{"role": "assistant"}

					if refusal, ok := msg["refusal"].(string); ok && refusal != "" {
						assistantMsg["content"] = []interface{}{
							map[string]interface{}{"type": "refusal", "refusal": refusal},
						}
						hasContent = true
					} else if msg["content"] != nil {
						content := msg["content"]
						if s, ok := content.(string); ok && s != "" {
							assistantMsg["content"] = []interface{}{
								map[string]interface{}{"type": "output_text", "text": s},
							}
							hasContent = true
						} else if parts, ok := content.([]interface{}); ok && len(parts) > 0 {
							convertContentParts(parts, "assistant")
							assistantMsg["content"] = parts
							hasContent = true
						}
					}

					if hasContent {
						input = append(input, assistantMsg)
					}

					// Chat: {"tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"..."}}]}
					// Codex: separate {"type":"function_call","call_id":"x","name":"f","arguments":"..."}
					if toolCalls, ok := msg["tool_calls"].([]interface{}); ok {
						for _, tc := range toolCalls {
							tcMap, ok := tc.(map[string]interface{})
							if !ok {
								continue
							}
							callID, _ := tcMap["id"].(string)
							fn, _ := tcMap["function"].(map[string]interface{})
							if fn == nil {
								continue
							}
							name, _ := fn["name"].(string)
							args, _ := fn["arguments"].(string)
							input = append(input, map[string]interface{}{
								"type":      "function_call",
								"call_id":   callID,
								"name":      name,
								"arguments": args,
							})
						}
					}

				default:
					// user, developer, etc.
					converted := map[string]interface{}{"role": role}
					if name, ok := msg["name"].(string); ok {
						converted["name"] = name
					}
					content := msg["content"]
					if s, ok := content.(string); ok {
						converted["content"] = []interface{}{
							map[string]interface{}{"type": "input_text", "text": s},
						}
					} else if parts, ok := content.([]interface{}); ok {
						convertContentParts(parts, role)
						converted["content"] = parts
					}
					input = append(input, converted)
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

	// Params the Codex backend accepts verbatim.
	if v, ok := chatReq["tool_choice"]; ok {
		codexReq["tool_choice"] = convertToolChoice(v)
	}
	if reasoning := convertReasoning(chatReq); reasoning != nil {
		codexReq["reasoning"] = reasoning
	}

	// temperature/top_p are rejected by reasoning models (gpt-5*, o-series,
	// codex). Codex CLI never sends them for those. Forward only otherwise.
	model, _ := chatReq["model"].(string)
	if !isReasoningModel(model) {
		for _, key := range []string{"temperature", "top_p"} {
			if v, ok := chatReq[key]; ok {
				codexReq[key] = v
			}
		}
	}

	// `max_tokens`, `max_output_tokens`, and `stop` are rejected by the Codex
	// backend for chat completions, so drop them instead of returning upstream
	// 400s for otherwise valid OpenAI-compatible requests.

	// Chat tools are nested under `function`; Responses wants them flattened.
	if tools, ok := chatReq["tools"].([]interface{}); ok {
		codexReq["tools"] = convertTools(tools)
	}

	// Chat `response_format` → Responses `text.format`.
	if rf, ok := chatReq["response_format"].(map[string]interface{}); ok {
		if format := convertResponseFormat(rf); format != nil {
			codexReq["text"] = map[string]interface{}{"format": format}
		}
	}

	return json.Marshal(codexReq)
}

func convertReasoning(chatReq map[string]interface{}) map[string]interface{} {
	if reasoning, ok := chatReq["reasoning"].(map[string]interface{}); ok {
		out := make(map[string]interface{}, len(reasoning)+1)
		for k, v := range reasoning {
			out[k] = v
		}
		if _, ok := out["effort"]; !ok {
			if effort, ok := reasoningEffort(chatReq); ok {
				out["effort"] = effort
			}
		}
		return out
	}
	if effort, ok := reasoningEffort(chatReq); ok {
		return map[string]interface{}{"effort": effort}
	}
	return nil
}

func reasoningEffort(chatReq map[string]interface{}) (interface{}, bool) {
	if effort, ok := chatReq["reasoning_effort"]; ok {
		return effort, true
	}
	if effort, ok := chatReq["effort"]; ok {
		return effort, true
	}
	return nil, false
}

func convertToolChoice(v interface{}) interface{} {
	choice, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	if choice["type"] != "function" {
		return choice
	}
	fn, ok := choice["function"].(map[string]interface{})
	if !ok {
		return choice
	}
	name, _ := fn["name"].(string)
	if name == "" {
		return choice
	}
	return map[string]interface{}{
		"type": "function",
		"name": name,
	}
}

// convertTools flattens Chat-Completions function tools into Responses shape.
// Chat:      {"type":"function","function":{"name","description","parameters","strict"}}
// Responses: {"type":"function","name","description","parameters","strict"}
// Non-function tools (e.g. web_search) pass through unchanged.
func convertTools(tools []interface{}) []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, t := range tools {
		tool, ok := t.(map[string]interface{})
		if !ok {
			out = append(out, t)
			continue
		}
		fn, ok := tool["function"].(map[string]interface{})
		if tool["type"] != "function" || !ok {
			out = append(out, tool)
			continue
		}
		flat := map[string]interface{}{"type": "function"}
		for _, k := range []string{"name", "description", "parameters", "strict"} {
			if v, ok := fn[k]; ok {
				flat[k] = v
			}
		}
		out = append(out, flat)
	}
	return out
}

// convertResponseFormat maps a Chat `response_format` object to the Responses
// `text.format` object. json_schema is flattened (no nested json_schema key).
func convertResponseFormat(rf map[string]interface{}) map[string]interface{} {
	t, _ := rf["type"].(string)
	switch t {
	case "json_schema":
		format := map[string]interface{}{"type": "json_schema"}
		if js, ok := rf["json_schema"].(map[string]interface{}); ok {
			for _, k := range []string{"name", "schema", "strict", "description"} {
				if v, ok := js[k]; ok {
					format[k] = v
				}
			}
		}
		return format
	case "json_object", "text":
		return map[string]interface{}{"type": t}
	default:
		return nil
	}
}

// isReasoningModel reports whether the model rejects sampling params
// (temperature/top_p) — the gpt-5 family, o-series, and codex models.
func isReasoningModel(model string) bool {
	return strings.HasPrefix(model, "gpt-5") ||
		strings.HasPrefix(model, "o1") ||
		strings.HasPrefix(model, "o3") ||
		strings.HasPrefix(model, "o4") ||
		strings.Contains(model, "codex")
}

func convertContentParts(parts []interface{}, role string) {
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
		case "image_file":
			// OpenAI: {"type":"image_file","image_file":{"file_id":"..."}}
			// Codex:  {"type":"input_file","file_id":"..."}
			fileID := ""
			if obj, ok := part["image_file"].(map[string]interface{}); ok {
				fileID, _ = obj["file_id"].(string)
			}
			parts[i] = map[string]interface{}{
				"type":    "input_file",
				"file_id": fileID,
			}
		case "file":
			fileID := ""
			if obj, ok := part["file"].(map[string]interface{}); ok {
				fileID, _ = obj["file_id"].(string)
			}
			parts[i] = map[string]interface{}{
				"type":    "input_file",
				"file_id": fileID,
			}
		}
	}
}
