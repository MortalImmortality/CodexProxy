package auth

import (
	"fmt"
	"testing"
	"time"
)

func newTestManager(name, token string) *TokenManager {
	return &TokenManager{
		name:     name,
		filePath: "/tmp/test-" + name + ".json",
		authFile: &AuthFile{
			Tokens:      Tokens{AccessToken: token, RefreshToken: "ref-" + token},
			LastRefresh: time.Now(),
		},
	}
}

func TestPoolRoundRobin(t *testing.T) {
	pool := &TokenPool{strategy: "round-robin"}
	pool.managers = []*TokenManager{
		newTestManager("a", "token-a"),
		newTestManager("b", "token-b"),
		newTestManager("c", "token-c"),
	}

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		h, err := pool.Acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		seen[h.Manager.Name()]++
	}

	for _, name := range []string{"a", "b", "c"} {
		if seen[name] != 2 {
			t.Errorf("account %s got %d requests, want 2", name, seen[name])
		}
	}
}

func TestPoolSkipsFailedAccounts(t *testing.T) {
	pool := &TokenPool{strategy: "round-robin"}
	pool.managers = []*TokenManager{
		newTestManager("a", "token-a"),
		newTestManager("b", "token-b"),
	}

	pool.managers[0].MarkFailed(nil)
	pool.managers[0].lastError = fmt.Errorf("broken")

	for i := 0; i < 3; i++ {
		h, err := pool.Acquire()
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if h.Manager.Name() == "a" {
			t.Error("should not pick failed account a")
		}
	}
}

func TestPoolAcquireIncludesAccountID(t *testing.T) {
	pool := &TokenPool{strategy: "round-robin"}
	tm := newTestManager("a", "token-a")
	tm.authFile.Tokens.AccountID = "acct_123"
	pool.managers = []*TokenManager{tm}

	h, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if h.AccountID != "acct_123" {
		t.Errorf("AccountID = %q, want acct_123", h.AccountID)
	}
}

func TestPoolAcquireAccessTokenAccount(t *testing.T) {
	pool := NewTokenPool([]AccountConfig{{
		Name:        "team",
		AccessToken: "codex-token",
		AccountID:   "acct_123",
	}}, "round-robin")

	h, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if h.Token != "codex-token" {
		t.Fatalf("token = %q, want codex-token", h.Token)
	}
	if h.AccountID != "acct_123" {
		t.Fatalf("AccountID = %q, want acct_123", h.AccountID)
	}
	if !h.Manager.IsAccessTokenAuth() {
		t.Fatal("manager is not access-token auth")
	}
}

func TestPoolUpdateAccountsReplacesChangedAccessToken(t *testing.T) {
	pool := NewTokenPool([]AccountConfig{{Name: "team", AccessToken: "old-token"}}, "round-robin")
	oldManager := pool.Managers()[0]

	pool.UpdateAccounts([]AccountConfig{{Name: "team", AccessToken: "new-token"}}, "round-robin")
	newManager := pool.Managers()[0]
	if newManager == oldManager {
		t.Fatal("changed access token reused old manager")
	}

	h, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if h.Token != "new-token" {
		t.Fatalf("token = %q, want new-token", h.Token)
	}
}

func TestPoolUpdateAccountsPreservesAddsAndRemovesManagers(t *testing.T) {
	pool := &TokenPool{strategy: "round-robin"}
	a := newTestManager("a", "token-a")
	pool.managers = []*TokenManager{a}

	pool.UpdateAccounts([]AccountConfig{
		{Name: "a", AuthFile: a.FilePath()},
		{Name: "b", AuthFile: "/tmp/test-b.json"},
	}, "random")

	managers := pool.Managers()
	if len(managers) != 2 {
		t.Fatalf("managers = %d, want 2", len(managers))
	}
	if managers[0] != a {
		t.Fatal("existing manager was not preserved")
	}
	if managers[1].Name() != "b" {
		t.Fatalf("new manager = %s, want b", managers[1].Name())
	}
	if pool.Strategy() != "random" {
		t.Fatalf("strategy = %q, want random", pool.Strategy())
	}

	pool.UpdateAccounts([]AccountConfig{{Name: "b", AuthFile: "/tmp/test-b.json"}}, "")

	managers = pool.Managers()
	if len(managers) != 1 || managers[0].Name() != "b" {
		t.Fatalf("managers after delete = %#v, want only b", managers)
	}
	if pool.Strategy() != "round-robin" {
		t.Fatalf("empty strategy = %q, want round-robin", pool.Strategy())
	}
}

func TestPoolUpdateAccountsRenamedAccountUsesNewManager(t *testing.T) {
	pool := &TokenPool{strategy: "round-robin"}
	a := newTestManager("a", "token-a")
	pool.managers = []*TokenManager{a}

	pool.UpdateAccounts([]AccountConfig{{Name: "renamed", AuthFile: a.FilePath()}}, "round-robin")

	managers := pool.Managers()
	if len(managers) != 1 {
		t.Fatalf("managers = %d, want 1", len(managers))
	}
	if managers[0] == a {
		t.Fatal("renamed account reused old manager")
	}
	if managers[0].Name() != "renamed" {
		t.Fatalf("manager name = %q, want renamed", managers[0].Name())
	}
}

func TestPoolIsHealthy(t *testing.T) {
	pool := &TokenPool{strategy: "round-robin"}
	pool.managers = []*TokenManager{
		newTestManager("a", "token-a"),
		newTestManager("b", "token-b"),
	}

	healthy, msg := pool.IsHealthy()
	if !healthy {
		t.Errorf("all accounts healthy but got unhealthy: %s", msg)
	}

	pool.managers[0].authFile.Tokens.AccessToken = ""
	healthy, msg = pool.IsHealthy()
	if !healthy {
		t.Errorf("one healthy account should keep pool healthy: %s", msg)
	}

	pool.managers[1].authFile.Tokens.AccessToken = ""
	healthy, _ = pool.IsHealthy()
	if healthy {
		t.Error("no healthy accounts but pool reports healthy")
	}
}
