package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type APIKey struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type KeyStore struct {
	Keys []APIKey `json:"keys"`
}

type reloadingKeyValidator struct {
	mu        sync.RWMutex
	path      string
	modTime   time.Time
	size      int64
	keys      *KeyStore
	extraKeys []APIKey
}

func keysFilePath() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "keys.json")
	}
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".codex-proxy", "keys.json")
}

func loadKeys() (*KeyStore, error) {
	data, err := os.ReadFile(keysFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &KeyStore{}, nil
		}
		return nil, err
	}
	var ks KeyStore
	if err := json.Unmarshal(data, &ks); err != nil {
		return nil, err
	}
	return &ks, nil
}

func saveKeys(ks *KeyStore) error {
	path := keysFilePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func generateKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "cpx-" + hex.EncodeToString(b)
}

func (ks *KeyStore) ValidKey(key string) bool {
	if key == "" {
		return false
	}
	keyHash := sha256.Sum256([]byte(key))
	for _, k := range ks.Keys {
		storedHash := sha256.Sum256([]byte(k.Key))
		if subtle.ConstantTimeCompare(keyHash[:], storedHash[:]) == 1 {
			return true
		}
	}
	return false
}

func newReloadingKeyValidator(extraKeys []APIKey) (*reloadingKeyValidator, error) {
	v := &reloadingKeyValidator{
		path:      keysFilePath(),
		extraKeys: append([]APIKey(nil), extraKeys...),
	}
	if err := v.reload(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *reloadingKeyValidator) ValidKey(key string) bool {
	if key == "" {
		return false
	}
	if err := v.reloadIfChanged(); err != nil {
		slog.Warn("failed to reload API keys", "file", v.path, "error", err)
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.keys == nil {
		return false
	}
	return v.keys.ValidKey(key)
}

func (v *reloadingKeyValidator) reloadIfChanged() error {
	info, err := os.Stat(v.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	var modTime time.Time
	var size int64
	if err == nil {
		modTime = info.ModTime()
		size = info.Size()
	}

	v.mu.RLock()
	unchanged := v.keys != nil && v.modTime.Equal(modTime) && v.size == size
	v.mu.RUnlock()
	if unchanged {
		return nil
	}
	return v.reload()
}

func (v *reloadingKeyValidator) reload() error {
	ks, err := loadKeys()
	if err != nil {
		return err
	}
	if len(v.extraKeys) > 0 {
		ks.Keys = append(ks.Keys, v.extraKeys...)
	}

	info, err := os.Stat(v.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	var modTime time.Time
	var size int64
	if err == nil {
		modTime = info.ModTime()
		size = info.Size()
	}

	v.mu.Lock()
	v.keys = ks
	v.modTime = modTime
	v.size = size
	v.mu.Unlock()
	return nil
}

func cmdKeyAdd(args []string) {
	name, customKey, err := parseKeyAddArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid key add args: %v\n", err)
		fmt.Fprintf(os.Stderr, "Usage: codex-proxy key add [--name NAME] [--key KEY]\n")
		os.Exit(1)
	}

	ks, err := loadKeys()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load keys: %v\n", err)
		os.Exit(1)
	}

	key := customKey
	if key == "" {
		key = generateKey()
	}

	for _, k := range ks.Keys {
		if k.Key == key {
			fmt.Fprintf(os.Stderr, "Key already exists\n")
			os.Exit(1)
		}
	}

	ks.Keys = append(ks.Keys, APIKey{
		Key:       key,
		Name:      name,
		CreatedAt: time.Now(),
	})

	if err := saveKeys(ks); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save keys: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("  Key added: %s\n", key)
	if name != "" {
		fmt.Printf("  Name:      %s\n", name)
	}
	fmt.Printf("  File:      %s\n", keysFilePath())
}

func parseKeyAddArgs(args []string) (string, string, error) {
	name := ""
	customKey := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			value, next, err := nextKeyFlagValue(args, i)
			if err != nil {
				return "", "", fmt.Errorf("--name requires a value")
			}
			name = strings.TrimSpace(value)
			if name == "" {
				return "", "", fmt.Errorf("--name requires a non-empty value")
			}
			i = next
		case "--key":
			value, next, err := nextKeyFlagValue(args, i)
			if err != nil {
				return "", "", fmt.Errorf("--key requires a value")
			}
			customKey = strings.TrimSpace(value)
			if customKey == "" {
				return "", "", fmt.Errorf("--key requires a non-empty value")
			}
			i = next
		default:
			return "", "", fmt.Errorf("unknown argument %q", args[i])
		}
	}
	return name, customKey, nil
}

func nextKeyFlagValue(args []string, flagIndex int) (string, int, error) {
	valueIndex := flagIndex + 1
	if valueIndex >= len(args) || strings.HasPrefix(args[valueIndex], "--") {
		return "", flagIndex, fmt.Errorf("missing value")
	}
	return args[valueIndex], valueIndex, nil
}

func cmdKeyDelete(args []string) {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "Usage: codex-proxy key delete <key-or-name|--empty-name>\n")
		os.Exit(1)
	}
	target := args[0]

	ks, err := loadKeys()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load keys: %v\n", err)
		os.Exit(1)
	}

	found := false
	var remaining []APIKey
	for _, k := range ks.Keys {
		if keyMatchesDeleteTarget(k, target) {
			found = true
			fmt.Printf("  Deleted: %s", k.Key)
			if k.Name != "" {
				fmt.Printf(" (%s)", k.Name)
			} else {
				fmt.Printf(" (empty name)")
			}
			fmt.Println()
		} else {
			remaining = append(remaining, k)
		}
	}

	if !found {
		fmt.Fprintf(os.Stderr, "Key not found: %s\n", target)
		os.Exit(1)
	}

	ks.Keys = remaining
	if err := saveKeys(ks); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save keys: %v\n", err)
		os.Exit(1)
	}
}

func keyMatchesDeleteTarget(k APIKey, target string) bool {
	if k.Key == target || k.Name == target {
		return true
	}
	return k.Name == "" && (target == "-" || target == "--empty-name")
}

func cmdKeyList() {
	ks, err := loadKeys()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load keys: %v\n", err)
		os.Exit(1)
	}

	if len(ks.Keys) == 0 {
		fmt.Println("  No API keys configured (proxy rejects authenticated endpoints)")
		return
	}

	fmt.Printf("  %-20s %-52s %s\n", "NAME", "KEY", "CREATED")
	fmt.Printf("  %-20s %-52s %s\n", "----", "---", "-------")
	for _, k := range ks.Keys {
		name := k.Name
		if name == "" {
			name = "(empty)"
		}
		fmt.Printf("  %-20s %-52s %s\n", name, k.Key, k.CreatedAt.Format("2006-01-02 15:04"))
	}
	fmt.Printf("\n  Total: %d key(s)\n", len(ks.Keys))
}
