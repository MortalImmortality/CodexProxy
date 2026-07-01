package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"codex-proxy/auth"
	"codex-proxy/proxy"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version", "-v", "--version":
		printVersion()

	case "login":
		loginOpts, err := parseLoginArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid login args: %v\n", err)
			os.Exit(1)
		}
		authFile := loginOpts.authFile
		auth.SetManagerPath(authFile)
		if loginOpts.withAccessToken {
			err = auth.LoginWithAccessToken(os.Stdin)
		} else {
			err = auth.Login()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
			os.Exit(1)
		}
		if added, err := registerLoginAccount(defaultConfigPath(), loginOpts.name, authFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: login succeeded but account registration failed: %v\n", err)
		} else if added {
			fmt.Printf("Registered account in %s\n", defaultConfigPath())
		}

	case "serve":
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})))

		serveOpts, err := parseServeArgs(os.Args[2:])
		if err != nil {
			slog.Error("invalid serve args", "error", err)
			os.Exit(1)
		}
		host := serveOpts.host
		port := serveOpts.port
		proxy.SetMaxRequestBodySize(int64(serveOpts.maxBodyMB) << 20)

		configPath := initPool(&host, &port)

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		auth.Pool.StartBackgroundRefresh(ctx)
		defer auth.Pool.Stop()
		startConfigReloader(ctx, configPath)

		var extraKeys []APIKey
		ks, err := loadKeys()
		if err != nil {
			slog.Error("failed to load API keys", "error", err)
			os.Exit(1)
		}
		if envKey := os.Getenv("CODEX_PROXY_API_KEY"); envKey != "" {
			extraKeys = append(extraKeys, APIKey{Key: envKey, Name: "env"})
		}
		if len(ks.Keys)+len(extraKeys) == 0 {
			slog.Warn("no API keys configured — all requests will be rejected until keys are added via 'codex-proxy key add'")
		}
		keyValidator, err := newReloadingKeyValidator(extraKeys)
		if err != nil {
			slog.Error("failed to initialize API key validator", "error", err)
			os.Exit(1)
		}
		validateKey := proxy.KeyValidator(keyValidator.ValidKey)

		telegram := startTelegramMonitor(ctx)
		if telegram != nil {
			proxy.SetRateLimitNotifier(telegram.sendRateLimitAlert)
		} else {
			proxy.SetRateLimitNotifier(nil)
		}

		if err := proxy.Serve(ctx, host, port, validateKey); err != nil {
			telegram.sendServiceError(err)
			slog.Error("proxy server stopped", "error", err)
			os.Exit(1)
		}

	case "status":
		cfg, err := ensureConfig(defaultConfigPath())
		_, hasEnvAccessToken := envAccessTokenAccount(nil)
		if acc, ok := envAccessTokenAccount(nil); ok {
			fmt.Printf("  [%s] CODEX_ACCESS_TOKEN (env)\n", acc.Name)
		}
		if err != nil {
			if !hasEnvAccessToken {
				auth.ShowStatus()
			}
		} else {
			fmt.Printf("  Strategy:  %s\n", cfg.Strategy)
			fmt.Printf("  Accounts:  %d\n\n", len(cfg.Accounts))
			for _, acc := range cfg.Accounts {
				fmt.Printf("  [%s] %s\n", acc.Name, acc.AuthFile)
				auth.ShowStatusForFile(acc.Name, expandHome(acc.AuthFile))
			}
		}
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			fmt.Println()
			printServiceStatus()
		}

	case "key":
		if len(os.Args) < 3 {
			fmt.Println("Usage: codex-proxy key <add|delete|list>")
			os.Exit(1)
		}
		switch os.Args[2] {
		case "add":
			cmdKeyAdd(os.Args[3:])
		case "delete", "del", "rm":
			cmdKeyDelete(os.Args[3:])
		case "list", "ls":
			cmdKeyList()
		default:
			fmt.Fprintf(os.Stderr, "Unknown key command: %s\n", os.Args[2])
			os.Exit(1)
		}

	case "usage":
		if err := cmdUsage(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Invalid usage args: %v\n", err)
			os.Exit(1)
		}

	case "reset":
		if err := cmdReset(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Invalid reset args: %v\n", err)
			os.Exit(1)
		}

	case "upgrade", "update":
		if err := cmdUpgrade(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Upgrade failed: %v\n", err)
			os.Exit(1)
		}

	case "doctor":
		cmdDoctor()

	case "logout":
		logoutOpts, err := parseLogoutArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid logout args: %v\n", err)
			os.Exit(1)
		}
		auth.SetManagerPath(logoutOpts.authFile)
		auth.Logout()
		if removed, err := unregisterLoginAccount(defaultConfigPath(), logoutOpts.authFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: logout succeeded but account registration update failed: %v\n", err)
		} else if removed {
			fmt.Printf("Removed account from %s\n", defaultConfigPath())
		}

	case "install":
		serviceInstall()
	case "uninstall":
		serviceUninstall()
	case "start":
		serviceStart()
	case "stop":
		serviceStop()
	case "restart":
		serviceRestart()
	case "logs":
		serviceLogs()

	default:
		printUsage()
		os.Exit(1)
	}
}

func initPool(host, port *string) string {
	configPath := defaultConfigPath()
	cfg, err := ensureConfig(configPath)
	if err != nil {
		accounts := defaultPoolAccounts()
		auth.Pool = auth.NewTokenPool(
			accounts,
			"round-robin",
		)
		slog.Info("single-account mode", "accounts", len(accounts), "auth_file", auth.DefaultAuthPath(), "access_token_env", envAccessTokenConfigured())
		return configPath
	}

	if cfg.Host != "" {
		*host = cfg.Host
	}
	if cfg.Port != "" {
		*port = cfg.Port
	}
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "round-robin"
	}

	accounts := accountsWithEnvAccessToken(configToAccountConfigs(cfg))
	auth.Pool = auth.NewTokenPool(accounts, strategy)
	slog.Info("multi-account mode",
		"accounts", len(accounts),
		"strategy", strategy)
	return configPath
}

func startConfigReloader(ctx context.Context, configPath string) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		lastSignature, _ := poolConfigSignature(configPath)
		lastResetEvent := time.Time{}
		for {
			select {
			case <-ctx.Done():
				slog.Info("config reloader stopped")
				return
			case <-ticker.C:
				var err error
				lastResetEvent, err = clearFailedAccountsFromResetEvents(lastResetEvent)
				if err != nil {
					slog.Warn("reset event replay skipped", "error", err)
				}

				signature, err := poolConfigSignature(configPath)
				if err != nil {
					slog.Warn("proxy config reload skipped", "config", configPath, "error", err)
					continue
				}
				if signature == lastSignature {
					continue
				}
				accounts, strategy, err := loadPoolAccounts(configPath)
				if err != nil {
					slog.Warn("proxy config reload skipped", "config", configPath, "error", err)
					continue
				}
				auth.Pool.UpdateAccounts(accounts, strategy)
				lastSignature = signature
				slog.Info("proxy config reloaded",
					"config", configPath,
					"accounts", len(accounts),
					"strategy", strategy)
			}
		}
	}()
}

func loadPoolAccounts(configPath string) ([]auth.AccountConfig, string, error) {
	cfg, err := ensureConfig(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultPoolAccounts(), "round-robin", nil
		}
		return nil, "", err
	}
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "round-robin"
	}
	return accountsWithEnvAccessToken(configToAccountConfigs(cfg)), strategy, nil
}

func poolConfigSignature(configPath string) (string, error) {
	accounts, strategy, err := loadPoolAccounts(configPath)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(strategy)
	for _, acc := range accounts {
		b.WriteByte('\n')
		b.WriteString(acc.Name)
		b.WriteByte('\t')
		b.WriteString(acc.AuthFile)
		if acc.AccessToken != "" {
			b.WriteString("\taccess-token")
		}
	}
	return b.String(), nil
}

type serveOptions struct {
	host      string
	port      string
	maxBodyMB int
}

type loginOptions struct {
	authFile        string
	name            string
	withAccessToken bool
}

type logoutOptions struct {
	authFile string
	name     string
}

func parseLoginArgs(args []string) (loginOptions, error) {
	opts := loginOptions{}
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.name, "name", "", "account name in proxy config")
	fs.BoolVar(&opts.withAccessToken, "with-access-token", false, "read a Codex access token from stdin")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	opts.authFile = accountAuthPath(opts.name)
	return opts, nil
}

func parseLogoutArgs(args []string) (logoutOptions, error) {
	opts := logoutOptions{}
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.name, "name", "", "account name in proxy config")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	opts.authFile = accountAuthPath(opts.name)
	return opts, nil
}

func accountAuthPath(name string) string {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(name) == "default" {
		return auth.DefaultAuthPath()
	}
	return filepath.Join(authStorageDir(), "auth-"+slugifyAccountName(name)+".json")
}

func authStorageDir() string {
	return filepath.Dir(auth.DefaultAuthPath())
}

func slugifyAccountName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" || slug == "default" {
		return "account"
	}
	return slug
}

func registerLoginAccount(configPath, name, authFile string) (bool, error) {
	cfg, err := loadConfigForWrite(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, err
		}
		cfg = &ProxyConfig{Strategy: "round-robin"}
		defaultAuthPath := auth.DefaultAuthPath()
		if cleanPath(authFile) != cleanPath(defaultAuthPath) && fileExists(defaultAuthPath) {
			cfg.Accounts = append(cfg.Accounts, ProxyAccount{
				Name:     uniqueAccountName(cfg, "default", ""),
				AuthFile: defaultAuthPath,
			})
		}
	}

	for i := range cfg.Accounts {
		if cleanPath(cfg.Accounts[i].AuthFile) == cleanPath(authFile) {
			if name != "" && cfg.Accounts[i].Name != name {
				cfg.Accounts[i].Name = name
				return true, saveConfig(configPath, cfg)
			}
			return false, nil
		}
	}

	if name == "" {
		name = inferAccountName(authFile)
	}
	name = uniqueAccountName(cfg, name, authFile)
	cfg.Accounts = append(cfg.Accounts, ProxyAccount{Name: name, AuthFile: authFile})
	return true, saveConfig(configPath, cfg)
}

func unregisterLoginAccount(configPath, authFile string) (bool, error) {
	cfg, err := loadConfigForWrite(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	accounts := cfg.Accounts[:0]
	removed := false
	for _, acc := range cfg.Accounts {
		if cleanPath(acc.AuthFile) == cleanPath(authFile) {
			removed = true
			continue
		}
		accounts = append(accounts, acc)
	}
	if !removed {
		return false, nil
	}
	cfg.Accounts = accounts
	return true, saveConfig(configPath, cfg)
}

func inferAccountName(authFile string) string {
	if cleanPath(authFile) == cleanPath(auth.DefaultAuthPath()) {
		return "default"
	}
	base := strings.TrimSuffix(filepath.Base(authFile), filepath.Ext(authFile))
	base = strings.TrimPrefix(base, "auth-")
	if base == "" || base == "auth" {
		return "account"
	}
	return base
}

func uniqueAccountName(cfg *ProxyConfig, preferred, sameAuthFile string) string {
	if preferred == "" {
		preferred = "account"
	}
	used := map[string]bool{}
	for _, acc := range cfg.Accounts {
		if sameAuthFile != "" && cleanPath(acc.AuthFile) == cleanPath(sameAuthFile) {
			continue
		}
		used[acc.Name] = true
	}
	if !used[preferred] {
		return preferred
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", preferred, i)
		if !used[candidate] {
			return candidate
		}
	}
}

func cleanPath(path string) string {
	return filepath.Clean(expandHome(path))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func defaultPoolAccounts() []auth.AccountConfig {
	if acc, ok := envAccessTokenAccount(nil); ok {
		return []auth.AccountConfig{acc}
	}
	return []auth.AccountConfig{{Name: "default", AuthFile: auth.DefaultAuthPath()}}
}

func accountsWithEnvAccessToken(accounts []auth.AccountConfig) []auth.AccountConfig {
	if acc, ok := envAccessTokenAccount(accounts); ok {
		return append([]auth.AccountConfig{acc}, accounts...)
	}
	return accounts
}

func envAccessTokenAccount(existing []auth.AccountConfig) (auth.AccountConfig, bool) {
	token := strings.TrimSpace(os.Getenv("CODEX_ACCESS_TOKEN"))
	if token == "" {
		return auth.AccountConfig{}, false
	}
	return auth.AccountConfig{
		Name:        uniqueAccountConfigName(existing, "codex-access-token"),
		AccessToken: token,
	}, true
}

func envAccessTokenConfigured() bool {
	return strings.TrimSpace(os.Getenv("CODEX_ACCESS_TOKEN")) != ""
}

func uniqueAccountConfigName(accounts []auth.AccountConfig, preferred string) string {
	used := map[string]bool{}
	for _, acc := range accounts {
		used[acc.Name] = true
	}
	if !used[preferred] {
		return preferred
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", preferred, i)
		if !used[candidate] {
			return candidate
		}
	}
}

func parseServeArgs(args []string) (serveOptions, error) {
	maxBodyMB, err := defaultMaxBodyMB()
	if err != nil {
		return serveOptions{}, err
	}
	opts := serveOptions{
		host:      "127.0.0.1",
		port:      "10531",
		maxBodyMB: maxBodyMB,
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.host, "host", opts.host, "listen host")
	fs.StringVar(&opts.port, "port", opts.port, "listen port")
	fs.IntVar(&opts.maxBodyMB, "max-body-mb", opts.maxBodyMB, "maximum request body size in MiB")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if opts.maxBodyMB <= 0 {
		return opts, fmt.Errorf("max-body-mb must be positive")
	}
	return opts, nil
}

func defaultMaxBodyMB() (int, error) {
	const mib = int64(1 << 20)
	defaultMB := int(proxy.MaxRequestBodySize() / mib)
	env := strings.TrimSpace(os.Getenv("CODEX_PROXY_MAX_REQUEST_BODY_MB"))
	if env == "" {
		return defaultMB, nil
	}
	n, err := strconv.Atoi(env)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("CODEX_PROXY_MAX_REQUEST_BODY_MB must be a positive integer")
	}
	return n, nil
}

type usageOptions struct {
	raw bool
}

type resetOptions struct {
	target string
}

func parseUsageArgs(args []string) (usageOptions, error) {
	opts := usageOptions{}
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.raw, "raw", false, "print raw usage JSON")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return opts, nil
}

func parseResetArgs(args []string) (resetOptions, error) {
	opts := resetOptions{}
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	switch fs.NArg() {
	case 0:
		return opts, nil
	case 1:
		opts.target = fs.Arg(0)
		return opts, nil
	default:
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
}

func cmdUsage(args []string) error {
	opts, err := parseUsageArgs(args)
	if err != nil {
		return err
	}
	return printUsageForAccounts(opts, usageAccounts())
}

func cmdReset(args []string) error {
	opts, err := parseResetArgs(args)
	if err != nil {
		return err
	}
	accounts := usageAccounts()
	if opts.target != "" {
		return resetUsageForAccount(opts.target, accounts)
	}
	return printResetCreditsForAccounts(opts, accounts)
}

func usageAccounts() []auth.AccountConfig {
	cfg, err := ensureConfig(defaultConfigPath())
	if err != nil {
		return defaultPoolAccounts()
	}
	return accountsWithEnvAccessToken(configToAccountConfigs(cfg))
}

func printUsageForAccounts(opts usageOptions, accounts []auth.AccountConfig) error {
	for _, acc := range accounts {
		tm := tokenManagerForAccount(acc)
		info, err := auth.QueryUsageForManager(context.Background(), tm)
		if err != nil {
			fmt.Printf("  [%s] usage query failed: %v\n\n", acc.Name, err)
			continue
		}
		if opts.raw {
			fmt.Printf("  [%s]\n%s\n\n", acc.Name, info.RawJSON)
			continue
		}
		printAccountUsage(acc.Name, info)
	}
	return nil
}

func printResetCreditsForAccounts(opts resetOptions, accounts []auth.AccountConfig) error {
	found := false
	for _, acc := range accounts {
		if opts.target != "" && acc.Name != opts.target {
			continue
		}
		found = true
		tm := tokenManagerForAccount(acc)
		info, err := auth.QueryUsageForManager(context.Background(), tm)
		if err != nil {
			fmt.Printf("  [%s] reset credits query failed: %v\n\n", acc.Name, err)
			continue
		}
		label := acc.Name
		if info.Email != "" {
			label = info.Email
		}
		fmt.Printf("  [%s]\n", label)
		fmt.Printf("    Reset credits: %s\n\n", formatResetCredits(info.ResetCredits))
	}
	if opts.target != "" && !found {
		return fmt.Errorf("account not found: %s", opts.target)
	}
	return nil
}

func resetUsageForAccount(target string, accounts []auth.AccountConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	acc, label, err := findResetAccount(ctx, target, accounts)
	if err != nil {
		return err
	}
	tm := tokenManagerForAccount(acc)
	result, err := auth.ResetUsageForManager(ctx, tm)
	if err != nil {
		return fmt.Errorf("[%s] usage reset failed: %w", label, err)
	}
	fmt.Printf("  [%s] reset outcome: %s\n", label, usageResetOutcomeText(result.Outcome))
	if result.Message != "" {
		fmt.Printf("    %s\n", result.Message)
	}
	if err := recordResetEvent(acc, label); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: reset succeeded but failed to notify running service: %v\n", err)
	}
	return nil
}

func findResetAccount(ctx context.Context, target string, accounts []auth.AccountConfig) (auth.AccountConfig, string, error) {
	for _, acc := range accounts {
		if strings.EqualFold(target, acc.Name) {
			return acc, acc.Name, nil
		}
	}

	var matches []struct {
		acc   auth.AccountConfig
		label string
	}
	for _, acc := range accounts {
		tm := tokenManagerForAccount(acc)
		info, err := auth.QueryUsageForManager(ctx, tm)
		if err != nil || info.Email == "" || !strings.EqualFold(target, info.Email) {
			continue
		}
		matches = append(matches, struct {
			acc   auth.AccountConfig
			label string
		}{acc: acc, label: info.Email})
	}
	if len(matches) == 0 {
		return auth.AccountConfig{}, "", fmt.Errorf("account not found: %s", target)
	}
	if len(matches) > 1 {
		return auth.AccountConfig{}, "", fmt.Errorf("account match is ambiguous: %s", target)
	}
	return matches[0].acc, matches[0].label, nil
}

type resetEvent struct {
	AccountName string    `json:"account_name"`
	Email       string    `json:"email,omitempty"`
	AuthFile    string    `json:"auth_file,omitempty"`
	Time        time.Time `json:"time"`
}

func resetEventsPath() string {
	return filepath.Join(authStorageDir(), "reset-events.jsonl")
}

func recordResetEvent(acc auth.AccountConfig, label string) error {
	event := resetEvent{
		AccountName: acc.Name,
		Time:        time.Now().UTC(),
	}
	if acc.AuthFile != "" {
		event.AuthFile = cleanPath(acc.AuthFile)
	}
	if strings.Contains(label, "@") {
		event.Email = label
	}
	if err := os.MkdirAll(authStorageDir(), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(resetEventsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func clearFailedAccountsFromResetEvents(after time.Time) (time.Time, error) {
	if auth.Pool == nil {
		return after, nil
	}
	data, err := os.ReadFile(resetEventsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return after, nil
		}
		return after, err
	}

	latest := after
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event resetEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if !event.Time.After(after) {
			continue
		}
		if event.Time.After(latest) {
			latest = event.Time
		}
		for _, tm := range auth.Pool.Managers() {
			if resetEventMatchesManager(event, tm) {
				tm.ClearFailed()
				slog.Info("account restored after reset event", "account", tm.Name())
			}
		}
	}
	return latest, nil
}

func resetEventMatchesManager(event resetEvent, tm *auth.TokenManager) bool {
	if tm == nil {
		return false
	}
	if event.AccountName != "" && strings.EqualFold(event.AccountName, tm.Name()) {
		return true
	}
	if event.AuthFile != "" && tm.FilePath() != "" && cleanPath(event.AuthFile) == cleanPath(tm.FilePath()) {
		return true
	}
	return false
}

func tokenManagerForAccount(acc auth.AccountConfig) *auth.TokenManager {
	if acc.AccessToken != "" {
		return auth.NewAccessTokenManager(acc.Name, acc.AccessToken, acc.AccountID)
	}
	return auth.NewTokenManager(acc.Name, expandHome(acc.AuthFile))
}

func usageResetOutcomeText(outcome string) string {
	switch outcome {
	case "reset":
		return "reset"
	case "alreadyRedeemed":
		return "already redeemed"
	case "nothingToReset":
		return "nothing to reset"
	case "noCredit":
		return "no reset credit"
	default:
		if outcome == "" {
			return "unknown"
		}
		return outcome
	}
}

func printAccountUsage(name string, info *auth.UsageInfo) {
	status := "✓"
	if info.LimitHit {
		status = "✗ LIMIT HIT"
	}
	label := name
	if info.Email != "" {
		label = info.Email
	}
	fmt.Printf("  [%s]\n", label)
	fmt.Printf("    Plan:     %s\n", info.PlanType)
	fmt.Printf("    Status:   %s\n", status)
	fmt.Printf("    Resets:   %s available\n", formatResetCredits(info.ResetCredits))
	if len(info.Windows) == 0 {
		fmt.Printf("    (no rate limit windows)\n")
	}
	for _, w := range info.Windows {
		bar := ""
		filled := w.UsedPercent / 5
		for i := 0; i < 20; i++ {
			if i < filled {
				bar += "█"
			} else {
				bar += "░"
			}
		}
		resetStr := formatReset(w.ResetSecs)
		fmt.Printf("    %-8s  [%s] %d%%  (reset: %s)\n", w.Name+":", bar, w.UsedPercent, resetStr)
	}
	if info.TokenActivity != nil {
		if info.TokenActivity.Summary.LifetimeTokens != nil {
			fmt.Printf("    Tokens:   %d lifetime\n", *info.TokenActivity.Summary.LifetimeTokens)
		}
		if info.TokenActivity.Summary.CurrentStreakDays != nil {
			fmt.Printf("    Streak:   %d days\n", *info.TokenActivity.Summary.CurrentStreakDays)
		}
	} else if info.TokenUsageError != "" {
		fmt.Printf("    Token activity: unavailable (%s)\n", info.TokenUsageError)
	}
	fmt.Println()
}

func formatResetCredits(credits *int) string {
	if credits == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *credits)
}

func formatReset(secs int) string {
	if secs <= 0 {
		return "-"
	}
	h := secs / 3600
	m := (secs % 3600) / 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func printUsage() {
	fmt.Println(`codex-proxy - Codex OAuth API Proxy

Usage:
  codex-proxy version                                   Show current version
  codex-proxy login [--name NAME] [--with-access-token]  Login via browser OAuth or stdin token
  codex-proxy serve [--host H] [--port P] [--max-body-mb N]  Start proxy (foreground)
  codex-proxy status                                     Show auth & service status
  codex-proxy usage [--raw]                             Show account rate limit usage
  codex-proxy reset [ACCOUNT_OR_EMAIL]                  Show or use rate-limit reset credits
  codex-proxy upgrade [--version TAG] [--yes]           Upgrade binary from GitHub Releases
  codex-proxy doctor                                     Diagnose deployment configuration
  codex-proxy logout [--name NAME]                       Remove stored credentials

API key management:
  codex-proxy key add [--name NAME] [--key KEY]          Add API key (auto-generate if no --key)
  codex-proxy key list                                   List all API keys
  codex-proxy key delete <key-or-name|--empty-name>      Delete an API key

Service management:
  codex-proxy install                  Install user service (systemd/launchd)
  codex-proxy uninstall                Remove user service
  codex-proxy start                    Start background service
  codex-proxy stop                     Stop background service
  codex-proxy restart                  Restart background service
  codex-proxy logs                     Tail service logs

Multi-account:
  Login with different --name values; proxy.json is updated automatically.
  See proxy.example.json for format.

Codex access token:
  export CODEX_ACCESS_TOKEN=<token>
  codex-proxy serve

  Or persist one locally:
  echo "$CODEX_ACCESS_TOKEN" | codex-proxy login --with-access-token

After login, any OpenAI-compatible client can use:
  base_url = http://127.0.0.1:10531/v1

Example:
  export OPENAI_BASE_URL=http://127.0.0.1:10531/v1
  export OPENAI_API_KEY=unused
  python -c "from openai import OpenAI; print(OpenAI().chat.completions.create(model='o3-pro', messages=[{'role':'user','content':'hi'}]))"`)
}
