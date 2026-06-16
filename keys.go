package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func cmdKeyAdd(args []string) {
	name := ""
	customKey := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "--key":
			if i+1 < len(args) {
				customKey = args[i+1]
				i++
			}
		}
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

func cmdKeyDelete(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: codex-proxy key delete <key-or-name>\n")
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
		if k.Key == target || k.Name == target {
			found = true
			fmt.Printf("  Deleted: %s", k.Key)
			if k.Name != "" {
				fmt.Printf(" (%s)", k.Name)
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
			name = "-"
		}
		fmt.Printf("  %-20s %-52s %s\n", name, k.Key, k.CreatedAt.Format("2006-01-02 15:04"))
	}
	fmt.Printf("\n  Total: %d key(s)\n", len(ks.Keys))
}
