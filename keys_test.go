package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyStoreValidKey(t *testing.T) {
	ks := &KeyStore{Keys: []APIKey{{Key: "cpx-secret", Name: "test"}}}

	if !ks.ValidKey("cpx-secret") {
		t.Fatal("expected valid key")
	}
	if ks.ValidKey("wrong") {
		t.Fatal("expected wrong key to be invalid")
	}
	if ks.ValidKey("") {
		t.Fatal("expected empty key to be invalid")
	}
}

func TestLoadKeysReturnsInvalidJSONError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "keys.json"), []byte("{"), 0600); err != nil {
		t.Fatalf("write keys file: %v", err)
	}

	if _, err := loadKeys(); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}
