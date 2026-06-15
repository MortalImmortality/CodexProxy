package auth

import (
	"context"
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
	"sync"
	"time"
)

const (
	OAuthAuthorizeURL = "https://auth.openai.com/authorize"
	OAuthTokenURL     = "https://auth.openai.com/oauth/token"
	OAuthDeviceURL    = "https://auth.openai.com/codex/device"

	OAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexBaseURL  = "https://chatgpt.com/backend-api/codex"

	RefreshInterval          = 7 * 24 * time.Hour
	ProactiveRefreshInterval = 5 * 24 * time.Hour
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

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

// ──────────────────────────────────────────────
// Device Code flow types
// ──────────────────────────────────────────────

type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
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
// Login - Device Code flow implementation
// ──────────────────────────────────────────────

func Login(deviceAuth bool) error {
	if deviceAuth {
		return loginDeviceCode()
	}
	return loginBrowser()
}

func loginDeviceCode() error {
	fmt.Println("Requesting device code from OpenAI...")

	resp, err := httpClient.PostForm(OAuthDeviceURL, url.Values{
		"client_id": {OAuthClientID},
		"scope":     {"openid profile email offline_access"},
	})
	if err != nil {
		return fmt.Errorf("device code request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("device code request returned %d: %s", resp.StatusCode, body)
	}

	var dc DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return fmt.Errorf("failed to parse device code response: %w", err)
	}

	fmt.Println()
	fmt.Println("  ╭─────────────────────────────────────────────╮")
	fmt.Printf("  │  Open:  %-36s │\n", dc.VerificationURI)
	fmt.Printf("  │  Code:  %-36s │\n", dc.UserCode)
	fmt.Println("  ╰─────────────────────────────────────────────╯")
	fmt.Println()
	fmt.Println("  Waiting for authorization...")

	openBrowser(dc.VerificationURI)

	interval := dc.Interval
	if interval < 5 {
		interval = 5
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(interval) * time.Second)

		tokenResp, err := pollDeviceToken(dc.DeviceCode)
		if err != nil {
			return err
		}

		switch tokenResp.Error {
		case "":
			authFile := &AuthFile{
				AuthMode: "chatgptDeviceCode",
				Tokens: Tokens{
					IDToken:      tokenResp.IDToken,
					AccessToken:  tokenResp.AccessToken,
					RefreshToken: tokenResp.RefreshToken,
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

		case "authorization_pending":
			fmt.Print(".")
			continue
		case "slow_down":
			interval += 5
			continue
		case "expired_token":
			return fmt.Errorf("device code expired, please try again")
		default:
			return fmt.Errorf("unexpected error: %s", tokenResp.Error)
		}
	}

	return fmt.Errorf("timed out waiting for authorization")
}

func pollDeviceToken(deviceCode string) (*TokenResponse, error) {
	resp, err := httpClient.PostForm(OAuthTokenURL, url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {OAuthClientID},
	})
	if err != nil {
		return nil, fmt.Errorf("token poll failed: %w", err)
	}
	defer resp.Body.Close()

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}
	return &tr, nil
}

func loginBrowser() error {
	fmt.Println("Browser-based login: run 'codex login' or use --device-auth for headless")
	fmt.Println("If you already have ~/.codex/auth.json, just run 'codex-proxy serve'")
	return nil
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

	resp, err := httpClient.PostForm(OAuthTokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tm.authFile.Tokens.RefreshToken},
		"client_id":     {OAuthClientID},
	})
	if err != nil {
		return fmt.Errorf("refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("refresh returned %d: %s", resp.StatusCode, body)
	}

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
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
	req, _ := http.NewRequest("GET", CodexBaseURL+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

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
	}

	if messages, ok := chatReq["messages"]; ok {
		codexReq["input"] = messages
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
