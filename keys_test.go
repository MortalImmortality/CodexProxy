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

func TestParseKeyAddArgsRejectsIncompleteOrUnknownArgs(t *testing.T) {
	cases := [][]string{
		{"main"},
		{"--name"},
		{"--name", "--key"},
		{"--key", ""},
	}
	for _, args := range cases {
		if _, _, err := parseKeyAddArgs(args); err == nil {
			t.Fatalf("parseKeyAddArgs(%v) expected error", args)
		}
	}
}

func TestParseKeyAddArgsAcceptsNameAndCustomKey(t *testing.T) {
	name, key, err := parseKeyAddArgs([]string{"--name", "main", "--key", "cpx-custom"})
	if err != nil {
		t.Fatalf("parseKeyAddArgs returned error: %v", err)
	}
	if name != "main" || key != "cpx-custom" {
		t.Fatalf("unexpected args: name=%q key=%q", name, key)
	}
}

func TestKeyMatchesDeleteTargetSupportsEmptyName(t *testing.T) {
	emptyNameKey := APIKey{Key: "cpx-empty"}
	if !keyMatchesDeleteTarget(emptyNameKey, "-") {
		t.Fatal("expected '-' to match empty-name key")
	}
	if !keyMatchesDeleteTarget(emptyNameKey, "--empty-name") {
		t.Fatal("expected --empty-name to match empty-name key")
	}
	if keyMatchesDeleteTarget(APIKey{Key: "cpx-named", Name: "main"}, "--empty-name") {
		t.Fatal("did not expect --empty-name to match named key")
	}
}

func TestReloadingKeyValidatorReloadsChangedKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	if err := saveKeys(&KeyStore{Keys: []APIKey{{Key: "cpx-old", Name: "old"}}}); err != nil {
		t.Fatalf("save initial keys: %v", err)
	}
	validator, err := newReloadingKeyValidator(nil)
	if err != nil {
		t.Fatalf("newReloadingKeyValidator: %v", err)
	}
	if !validator.ValidKey("cpx-old") {
		t.Fatal("expected initial key to be valid")
	}
	if validator.ValidKey("cpx-new-longer") {
		t.Fatal("did not expect new key before reload")
	}

	if err := saveKeys(&KeyStore{Keys: []APIKey{{Key: "cpx-new-longer", Name: "new"}}}); err != nil {
		t.Fatalf("save changed keys: %v", err)
	}
	if !validator.ValidKey("cpx-new-longer") {
		t.Fatal("expected changed key to be valid after reload")
	}
	if validator.ValidKey("cpx-old") {
		t.Fatal("did not expect removed key to remain valid")
	}
}
