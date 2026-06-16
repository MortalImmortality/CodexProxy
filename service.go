package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const serviceName = "codex-proxy"

func serviceInstall() {
	if runtime.GOOS != "linux" {
		fmt.Println("Service install is Linux-only (systemd).")
		fmt.Println("For macOS, see codex-proxy.plist with launchctl.")
		os.Exit(1)
	}

	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot determine binary path: %v\n", err)
		os.Exit(1)
	}
	execPath, _ = filepath.EvalSymlinks(execPath)

	homeDir, _ := os.UserHomeDir()
	unitDir := filepath.Join(homeDir, ".config", "systemd", "user")
	unitPath := filepath.Join(unitDir, serviceName+".service")

	if err := os.MkdirAll(unitDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot create %s: %v\n", unitDir, err)
		os.Exit(1)
	}

	unit := fmt.Sprintf(`[Unit]
Description=Codex OAuth API Proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s serve
Restart=on-failure
RestartSec=5
Environment=HOME=%s

[Install]
WantedBy=default.target
`, execPath, homeDir)

	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot write unit file: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Unit file: %s\n", unitPath)

	mustRunSystemctl("daemon-reload")
	mustRunSystemctl("enable", serviceName)

	if user := os.Getenv("USER"); user != "" {
		cmd := exec.Command("loginctl", "enable-linger", user)
		if err := cmd.Run(); err != nil {
			fmt.Println("  ⚠ Lingering not enabled (run: sudo loginctl enable-linger $USER)")
			fmt.Println("    Without lingering, service stops when you log out.")
		} else {
			fmt.Println("  ✓ Lingering enabled")
		}
	}

	fmt.Println("  ✓ Service installed and enabled")
	fmt.Println()
	fmt.Println("  Next:")
	fmt.Println("    codex-proxy login")
	fmt.Println("    codex-proxy start")
}

func serviceUninstall() {
	requireLinux()

	if err := runSystemctl("stop", serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot stop service: %v\n", err)
	}
	if err := runSystemctl("disable", serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot disable service: %v\n", err)
	}

	homeDir, _ := os.UserHomeDir()
	unitPath := filepath.Join(homeDir, ".config", "systemd", "user", serviceName+".service")
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "  Cannot remove unit file: %v\n", err)
	} else {
		fmt.Printf("  Removed %s\n", unitPath)
	}

	mustRunSystemctl("daemon-reload")

	execPath, _ := os.Executable()
	fmt.Println("  ✓ Service uninstalled")
	fmt.Printf("  Binary still at %s — remove manually if desired\n", execPath)
}

func serviceStart() {
	requireLinux()
	requireInstalled()
	mustRunSystemctl("start", serviceName)
	fmt.Println("  ✓ Started")
	fmt.Println()
	printServiceStatus()
}

func serviceStop() {
	requireLinux()
	mustRunSystemctl("stop", serviceName)
	fmt.Println("  ✓ Stopped")
}

func serviceRestart() {
	requireLinux()
	requireInstalled()
	mustRunSystemctl("restart", serviceName)
	fmt.Println("  ✓ Restarted")
	fmt.Println()
	printServiceStatus()
}

func serviceLogs() {
	requireLinux()
	cmd := exec.Command("journalctl", "--user", "-u", serviceName, "-f", "--no-pager", "-o", "cat")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func printServiceStatus() {
	if runtime.GOOS != "linux" {
		return
	}
	if !unitFileExists() {
		fmt.Println("  Service: not installed (run: codex-proxy install)")
		return
	}
	cmd := exec.Command("systemctl", "--user", "status", serviceName, "--no-pager", "-l")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func runSystemctl(args ...string) error {
	allArgs := append([]string{"--user"}, args...)
	cmd := exec.Command("systemctl", allArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func mustRunSystemctl(args ...string) {
	if err := runSystemctl(args...); err != nil {
		fmt.Fprintf(os.Stderr, "systemctl --user %v failed: %v\n", args, err)
		os.Exit(1)
	}
}

func unitFileExists() bool {
	homeDir, _ := os.UserHomeDir()
	unitPath := filepath.Join(homeDir, ".config", "systemd", "user", serviceName+".service")
	_, err := os.Stat(unitPath)
	return err == nil
}

func requireLinux() {
	if runtime.GOOS != "linux" {
		fmt.Println("This command is Linux-only (systemd).")
		os.Exit(1)
	}
}

func requireInstalled() {
	if !unitFileExists() {
		fmt.Println("Service not installed. Run: codex-proxy install")
		os.Exit(1)
	}
}
