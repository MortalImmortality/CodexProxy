package main

import "testing"

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
	path, err := parseLoginArgs([]string{"--auth-file", "~/auth.json"})
	if err != nil {
		t.Fatalf("parseLoginArgs: %v", err)
	}
	if path == "~/auth.json" || path == "" {
		t.Errorf("auth path was not expanded: %q", path)
	}
	if _, err := parseLoginArgs([]string{"--unknown"}); err == nil {
		t.Fatal("expected unknown flag error")
	}
	if _, err := parseLoginArgs([]string{"extra"}); err == nil {
		t.Fatal("expected unexpected positional arg error")
	}
}
