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
