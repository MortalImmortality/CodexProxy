package main

import (
	"os"
	"path/filepath"
	"testing"

	"codex-proxy/auth"
)

func TestParseServeArgs(t *testing.T) {
	opts, err := parseServeArgs([]string{"--host", "0.0.0.0", "--port", "8080", "--config", "~/proxy.json"})
	if err != nil {
		t.Fatalf("parseServeArgs: %v", err)
	}
	if opts.host != "0.0.0.0" {
		t.Errorf("host = %q, want 0.0.0.0", opts.host)
	}
	if opts.port != "8080" {
		t.Errorf("port = %q, want 8080", opts.port)
	}
	if opts.configPath == "~/proxy.json" || opts.configPath == "" {
		t.Errorf("configPath was not expanded: %q", opts.configPath)
	}
}

func TestParseServeArgsDefaultsAndRejectsUnknown(t *testing.T) {
	opts, err := parseServeArgs(nil)
	if err != nil {
		t.Fatalf("parseServeArgs defaults: %v", err)
	}
	if opts.host != "127.0.0.1" || opts.port != "10531" {
		t.Fatalf("defaults = %s:%s, want 127.0.0.1:10531", opts.host, opts.port)
	}
	if _, err := parseServeArgs([]string{"--bad"}); err == nil {
		t.Fatal("expected unknown flag error")
	}
	if _, err := parseServeArgs([]string{"extra"}); err == nil {
		t.Fatal("expected unexpected positional arg error")
	}
}

func TestParseLoginArgs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	opts, err := parseLoginArgs([]string{"--name", "Alt Account"})
	if err != nil {
		t.Fatalf("parseLoginArgs: %v", err)
	}
	wantAuthFile := filepath.Join(home, "auth-alt-account.json")
	if opts.authFile != wantAuthFile {
		t.Errorf("authFile = %q, want %q", opts.authFile, wantAuthFile)
	}
	if opts.name != "Alt Account" {
		t.Errorf("name = %q, want Alt Account", opts.name)
	}

	if _, err := parseLoginArgs([]string{"--unknown"}); err == nil {
		t.Fatal("expected unknown flag error")
	}
	if _, err := parseLoginArgs([]string{"extra"}); err == nil {
		t.Fatal("expected unexpected positional arg error")
	}
}

func TestAccountAuthPathDefaultsAndSlugifiesName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	if got := accountAuthPath(""); got != filepath.Join(home, "auth.json") {
		t.Fatalf("default auth path = %q, want auth.json", got)
	}
	if got := accountAuthPath("Work Account!"); got != filepath.Join(home, "auth-work-account.json") {
		t.Fatalf("named auth path = %q, want slugged auth path", got)
	}
}

func TestParseLogoutArgs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	opts, err := parseLogoutArgs([]string{"--name", "Alt Account"})
	if err != nil {
		t.Fatalf("parseLogoutArgs: %v", err)
	}
	if opts.authFile != filepath.Join(home, "auth-alt-account.json") {
		t.Fatalf("authFile = %q, want named auth path", opts.authFile)
	}
	if opts.name != "Alt Account" {
		t.Fatalf("name = %q, want Alt Account", opts.name)
	}
	if _, err := parseLogoutArgs([]string{"--unknown"}); err == nil {
		t.Fatal("expected unknown flag error")
	}
	if _, err := parseLogoutArgs([]string{"extra"}); err == nil {
		t.Fatal("expected unexpected positional arg error")
	}
}

func TestRegisterLoginAccountCreatesAndDeduplicatesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	defaultAuth := auth.DefaultAuthPath()
	if err := os.WriteFile(defaultAuth, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write default auth: %v", err)
	}
	configPath := filepath.Join(home, "proxy.json")
	altAuth := filepath.Join(home, "auth-alt.json")

	added, err := registerLoginAccount(configPath, "alt", altAuth)
	if err != nil {
		t.Fatalf("registerLoginAccount: %v", err)
	}
	if !added {
		t.Fatal("added = false, want true")
	}

	cfg, err := loadConfigForWrite(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Strategy != "round-robin" {
		t.Fatalf("strategy = %q, want round-robin", cfg.Strategy)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts = %#v, want default and alt", cfg.Accounts)
	}
	if cfg.Accounts[0].Name != "default" || cfg.Accounts[0].AuthFile != defaultAuth {
		t.Fatalf("first account = %#v, want default auth", cfg.Accounts[0])
	}
	if cfg.Accounts[1].Name != "alt" || cfg.Accounts[1].AuthFile != altAuth {
		t.Fatalf("second account = %#v, want alt auth", cfg.Accounts[1])
	}

	added, err = registerLoginAccount(configPath, "alt", altAuth)
	if err != nil {
		t.Fatalf("register duplicate: %v", err)
	}
	if added {
		t.Fatal("duplicate added = true, want false")
	}
}

func TestUnregisterLoginAccountRemovesMatchingAuthFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	configPath := filepath.Join(home, "proxy.json")
	defaultAuth := filepath.Join(home, "auth.json")
	altAuth := filepath.Join(home, "auth-alt.json")
	if err := saveConfig(configPath, &ProxyConfig{
		Accounts: []ProxyAccount{
			{Name: "default", AuthFile: defaultAuth},
			{Name: "alt", AuthFile: altAuth},
		},
		Strategy: "round-robin",
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	removed, err := unregisterLoginAccount(configPath, altAuth)
	if err != nil {
		t.Fatalf("unregisterLoginAccount: %v", err)
	}
	if !removed {
		t.Fatal("removed = false, want true")
	}

	cfg, err := loadConfigForWrite(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Name != "default" {
		t.Fatalf("accounts = %#v, want only default", cfg.Accounts)
	}
}

func TestLoadPoolAccountsFallsBackToDefaultWhenConfigMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	accounts, strategy, err := loadPoolAccounts(filepath.Join(home, "missing-proxy.json"))
	if err != nil {
		t.Fatalf("loadPoolAccounts: %v", err)
	}
	if strategy != "round-robin" {
		t.Fatalf("strategy = %q, want round-robin", strategy)
	}
	if len(accounts) != 1 || accounts[0].Name != "default" || accounts[0].AuthFile != auth.DefaultAuthPath() {
		t.Fatalf("accounts = %#v, want default auth account", accounts)
	}
}

func TestEnsureConfigBootstrapsExistingAuthFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	defaultAuth := filepath.Join(home, "auth.json")
	altAuth := filepath.Join(home, "auth-alt.json")
	if err := os.WriteFile(defaultAuth, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write default auth: %v", err)
	}
	if err := os.WriteFile(altAuth, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write alt auth: %v", err)
	}

	cfg, err := ensureConfig(defaultConfigPath())
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts = %#v, want default and alt", cfg.Accounts)
	}
	if cfg.Accounts[0].Name != "default" || cfg.Accounts[0].AuthFile != defaultAuth {
		t.Fatalf("first account = %#v, want default", cfg.Accounts[0])
	}
	if cfg.Accounts[1].Name != "alt" || cfg.Accounts[1].AuthFile != altAuth {
		t.Fatalf("second account = %#v, want alt", cfg.Accounts[1])
	}
	if _, err := os.Stat(defaultConfigPath()); err != nil {
		t.Fatalf("proxy config was not written: %v", err)
	}
}

func TestParseUsageArgs(t *testing.T) {
	opts, err := parseUsageArgs([]string{"--raw", "--config", "~/proxy.json"})
	if err != nil {
		t.Fatalf("parseUsageArgs: %v", err)
	}
	if !opts.raw {
		t.Fatal("raw = false, want true")
	}
	if opts.configPath == "~/proxy.json" || opts.configPath == "" {
		t.Errorf("configPath was not expanded: %q", opts.configPath)
	}
	if _, err := parseUsageArgs([]string{"--unknown"}); err == nil {
		t.Fatal("expected unknown flag error")
	}
	if _, err := parseUsageArgs([]string{"extra"}); err == nil {
		t.Fatal("expected unexpected positional arg error")
	}
}
