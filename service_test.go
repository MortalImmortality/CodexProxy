package main

import (
	"strings"
	"testing"
)

func TestBuildSystemdUnitLoadsEnvFile(t *testing.T) {
	unit := buildSystemdUnit(
		"/usr/local/bin/codex-proxy",
		"/home/alice",
		"/home/alice/.codex-proxy/env",
	)

	for _, want := range []string{
		"ExecStart=/usr/local/bin/codex-proxy serve",
		"Environment=HOME=/home/alice",
		"EnvironmentFile=-/home/alice/.codex-proxy/env",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestBuildLaunchdPlistUsesProvidedPaths(t *testing.T) {
	plist := buildLaunchdPlist(
		"/opt/codex-proxy/bin/codex-proxy",
		"/Users/alice",
		"/Users/alice/.codex-proxy/codex-proxy.log",
	)

	for _, want := range []string{
		"<string>com.local.codex-proxy</string>",
		"<string>/opt/codex-proxy/bin/codex-proxy</string>",
		"<string>/Users/alice</string>",
		"<string>/Users/alice/.codex-proxy/codex-proxy.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "/Users/peter") {
		t.Fatal("plist should not contain a hard-coded user path")
	}
}

func TestBuildLaunchdPlistEscapesXML(t *testing.T) {
	plist := buildLaunchdPlist(
		"/tmp/codex&proxy",
		"/Users/a<b",
		"/tmp/log>file",
	)

	for _, want := range []string{"/tmp/codex&amp;proxy", "/Users/a&lt;b", "/tmp/log&gt;file"} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing escaped value %q:\n%s", want, plist)
		}
	}
}
