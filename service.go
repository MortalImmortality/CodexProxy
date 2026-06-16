package main

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const serviceName = "codex-proxy"
const launchdLabel = "com.local.codex-proxy"

func serviceInstall() {
	switch runtime.GOOS {
	case "linux":
		serviceInstallSystemd()
	case "darwin":
		serviceInstallLaunchd()
	default:
		fmt.Println("Service install is supported on Linux (systemd) and macOS (launchd).")
		os.Exit(1)
	}
}

func serviceInstallSystemd() {
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
			fmt.Println("  Warning: lingering not enabled (run: sudo loginctl enable-linger $USER)")
			fmt.Println("    Without lingering, service stops when you log out.")
		} else {
			fmt.Println("  Lingering enabled")
		}
	}

	fmt.Println("  Service installed and enabled")
	fmt.Println()
	fmt.Println("  Next:")
	fmt.Println("    codex-proxy login")
	fmt.Println("    codex-proxy start")
}

func serviceInstallLaunchd() {
	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot determine binary path: %v\n", err)
		os.Exit(1)
	}
	execPath, _ = filepath.EvalSymlinks(execPath)

	homeDir, _ := os.UserHomeDir()
	agentDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	plistPath := launchAgentPath()
	logPath := launchdLogPath()
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot create %s: %v\n", agentDir, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot create log directory: %v\n", err)
		os.Exit(1)
	}
	plist := buildLaunchdPlist(execPath, homeDir, logPath)
	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot write plist file: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  LaunchAgent: %s\n", plistPath)
	fmt.Println("  Service installed")
	fmt.Println()
	fmt.Println("  Next:")
	fmt.Println("    codex-proxy login")
	fmt.Println("    codex-proxy start")
}

func serviceUninstall() {
	switch runtime.GOOS {
	case "linux":
		serviceUninstallSystemd()
	case "darwin":
		serviceUninstallLaunchd()
	default:
		fmt.Println("This command is supported on Linux and macOS.")
		os.Exit(1)
	}
}

func serviceUninstallSystemd() {
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
	fmt.Println("  Service uninstalled")
	fmt.Printf("  Binary still at %s; remove manually if desired\n", execPath)
}

func serviceUninstallLaunchd() {
	plistPath := launchAgentPath()
	if err := runLaunchctl("unload", plistPath); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot unload service: %v\n", err)
	}
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "  Cannot remove plist file: %v\n", err)
	} else {
		fmt.Printf("  Removed %s\n", plistPath)
	}
	fmt.Println("  Service uninstalled")
}

func serviceStart() {
	switch runtime.GOOS {
	case "linux":
		requireInstalled()
		mustRunSystemctl("start", serviceName)
	case "darwin":
		requireLaunchAgentInstalled()
		mustRunLaunchctl("load", "-w", launchAgentPath())
	default:
		fmt.Println("This command is supported on Linux and macOS.")
		os.Exit(1)
	}
	fmt.Println("  Started")
	fmt.Println()
	printServiceStatus()
}

func serviceStop() {
	switch runtime.GOOS {
	case "linux":
		mustRunSystemctl("stop", serviceName)
	case "darwin":
		requireLaunchAgentInstalled()
		mustRunLaunchctl("unload", "-w", launchAgentPath())
	default:
		fmt.Println("This command is supported on Linux and macOS.")
		os.Exit(1)
	}
	fmt.Println("  Stopped")
}

func serviceRestart() {
	switch runtime.GOOS {
	case "linux":
		requireInstalled()
		mustRunSystemctl("restart", serviceName)
	case "darwin":
		requireLaunchAgentInstalled()
		if err := runLaunchctl("unload", "-w", launchAgentPath()); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: cannot unload service: %v\n", err)
		}
		mustRunLaunchctl("load", "-w", launchAgentPath())
	default:
		fmt.Println("This command is supported on Linux and macOS.")
		os.Exit(1)
	}
	fmt.Println("  Restarted")
	fmt.Println()
	printServiceStatus()
}

func serviceLogs() {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("journalctl", "--user", "-u", serviceName, "-f", "--no-pager", "-o", "cat")
	case "darwin":
		cmd = exec.Command("tail", "-f", launchdLogPath())
	default:
		fmt.Println("This command is supported on Linux and macOS.")
		os.Exit(1)
	}
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

func launchAgentPath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, "Library", "LaunchAgents", launchdLabel+".plist")
}

func launchdLogPath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".codex-proxy", "codex-proxy.log")
}

func buildLaunchdPlist(execPath, homeDir, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>serve</string>
    </array>
    <key>KeepAlive</key>
    <true/>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>%s</string>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
</dict>
</plist>
`, html.EscapeString(launchdLabel), html.EscapeString(execPath), html.EscapeString(logPath), html.EscapeString(logPath), html.EscapeString(homeDir))
}

func runLaunchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func mustRunLaunchctl(args ...string) {
	if err := runLaunchctl(args...); err != nil {
		fmt.Fprintf(os.Stderr, "launchctl %v failed: %v\n", args, err)
		os.Exit(1)
	}
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

func requireLaunchAgentInstalled() {
	if _, err := os.Stat(launchAgentPath()); err != nil {
		fmt.Println("Service not installed. Run: codex-proxy install")
		os.Exit(1)
	}
}
