package main

import (
	"strings"
	"testing"
)

func TestFormatDoctorCheck(t *testing.T) {
	ok := formatDoctorCheck(doctorCheck{Name: "Auth file", OK: true, Detail: "/tmp/auth.json"})
	if !strings.Contains(ok, "✓") || !strings.Contains(ok, "Auth file:") {
		t.Fatalf("ok check = %q", ok)
	}

	bad := formatDoctorCheck(doctorCheck{Name: "Telegram", OK: false, Detail: "not configured"})
	if !strings.Contains(bad, "✗") || !strings.Contains(bad, "not configured") {
		t.Fatalf("bad check = %q", bad)
	}
}
