package auth

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

type AccountConfig struct {
	Name        string
	AuthFile    string
	AccessToken string
	AccountID   string
}

type TokenHandle struct {
	Manager   *TokenManager
	Token     string
	AccountID string
}

func (h *TokenHandle) Refresh() (string, error) {
	token, err := h.Manager.RefreshNow()
	if err != nil {
		h.Manager.MarkFailed(err)
		return "", err
	}
	h.Token = token
	h.AccountID = h.Manager.AccountID()
	return token, nil
}

type TokenPool struct {
	managers []*TokenManager
	strategy string
	counter  atomic.Uint64
	mu       sync.RWMutex
	cancel   context.CancelFunc
}

var Pool *TokenPool

func NewTokenPool(accounts []AccountConfig, strategy string) *TokenPool {
	if strategy == "" {
		strategy = "round-robin"
	}
	p := &TokenPool{strategy: strategy}
	for _, acc := range accounts {
		tm := newManagerFromConfig(acc)
		p.managers = append(p.managers, tm)
	}
	return p
}

func (p *TokenPool) UpdateAccounts(accounts []AccountConfig, strategy string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing := make(map[string]*TokenManager, len(p.managers))
	for _, tm := range p.managers {
		existing[managerKey(tm)] = tm
	}

	managers := make([]*TokenManager, 0, len(accounts))
	for _, acc := range accounts {
		key := accountKey(acc)
		if tm := existing[key]; tm != nil {
			managers = append(managers, tm)
			continue
		}
		managers = append(managers, newManagerFromConfig(acc))
	}
	p.managers = managers
	if strategy == "" {
		strategy = "round-robin"
	}
	p.strategy = strategy
}

func newManagerFromConfig(acc AccountConfig) *TokenManager {
	if acc.AccessToken != "" {
		return NewAccessTokenManager(acc.Name, acc.AccessToken, acc.AccountID)
	}
	return NewTokenManager(acc.Name, acc.AuthFile)
}

func accountKey(acc AccountConfig) string {
	return acc.Name + "\x00" + acc.AuthFile + "\x00" + acc.AccessToken + "\x00" + acc.AccountID
}

func managerKey(tm *TokenManager) string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	accessToken := ""
	accountID := ""
	if tm.authFile != nil && isAccessTokenAuth(tm.authFile) {
		accessToken = tm.authFile.Tokens.AccessToken
		accountID = tm.authFile.Tokens.AccountID
	}
	return tm.name + "\x00" + tm.filePath + "\x00" + accessToken + "\x00" + accountID
}

func (p *TokenPool) Acquire() (*TokenHandle, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.managers) == 0 {
		return nil, fmt.Errorf("no accounts configured")
	}

	var healthy []*TokenManager
	for _, tm := range p.managers {
		if !tm.IsFailed() {
			healthy = append(healthy, tm)
		}
	}

	if len(healthy) == 0 {
		healthy = p.managers
	}

	tm := p.pick(healthy)

	token, err := tm.EnsureFreshToken()
	if err != nil {
		tm.MarkFailed(err)
		for _, alt := range healthy {
			if alt != tm {
				token, err = alt.EnsureFreshToken()
				if err == nil {
					return &TokenHandle{Manager: alt, Token: token, AccountID: alt.AccountID()}, nil
				}
				alt.MarkFailed(err)
			}
		}
		return nil, fmt.Errorf("all accounts failed, last: %w", err)
	}

	return &TokenHandle{Manager: tm, Token: token, AccountID: tm.AccountID()}, nil
}

func (p *TokenPool) pick(candidates []*TokenManager) *TokenManager {
	if len(candidates) == 1 {
		return candidates[0]
	}
	switch p.strategy {
	case "random":
		return candidates[rand.Intn(len(candidates))]
	default:
		idx := p.counter.Add(1) - 1
		return candidates[idx%uint64(len(candidates))]
	}
}

func (p *TokenPool) Managers() []*TokenManager {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*TokenManager, len(p.managers))
	copy(result, p.managers)
	return result
}

func (p *TokenPool) Strategy() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strategy
}

func (p *TokenPool) StartBackgroundRefresh(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				slog.Info("pool background refresh stopped")
				return
			case <-ticker.C:
				for _, tm := range p.Managers() {
					tm.mu.RLock()
					needsRefresh := tm.authFile != nil &&
						!isAccessTokenAuth(tm.authFile) &&
						time.Since(tm.authFile.LastRefresh) > ProactiveRefreshInterval
					tm.mu.RUnlock()

					if needsRefresh {
						slog.Info("proactive token refresh", "account", tm.name)
						if _, err := tm.RefreshNow(); err != nil {
							slog.Error("proactive refresh failed",
								"account", tm.name, "error", err)
						}
					}
				}

				for _, tm := range p.Managers() {
					if tm.IsFailed() {
						if until := tm.FailedUntil(); !until.IsZero() && time.Now().Before(until) {
							continue
						}
						slog.Info("retrying failed account", "account", tm.name)
						if _, err := tm.EnsureFreshToken(); err == nil {
							tm.ClearFailed()
							slog.Info("account recovered", "account", tm.name)
						}
					}
				}
			}
		}
	}()
}

func (p *TokenPool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *TokenPool) IsHealthy() (bool, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	total := len(p.managers)
	healthy := 0
	for _, tm := range p.managers {
		if h, _ := tm.IsHealthy(); h && !tm.IsFailed() {
			healthy++
		}
	}

	if healthy == 0 {
		return false, fmt.Sprintf("0/%d accounts healthy", total)
	}
	if healthy < total {
		return true, fmt.Sprintf("%d/%d accounts healthy", healthy, total)
	}
	return true, fmt.Sprintf("all %d accounts healthy", total)
}
