package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const defaultUpgradeRepo = "MortalImmortality/CodexProxy"

var githubAPIBaseURL = "https://api.github.com"

type upgradeOptions struct {
	repo    string
	version string
	yes     bool
	force   bool
}

type githubRelease struct {
	TagName string               `json:"tag_name"`
	HTMLURL string               `json:"html_url"`
	Assets  []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func parseUpgradeArgs(args []string) (upgradeOptions, error) {
	opts := upgradeOptions{repo: defaultUpgradeRepo}
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.repo, "repo", opts.repo, "GitHub repo in owner/name form")
	fs.StringVar(&opts.version, "version", "", "release tag to install")
	fs.StringVar(&opts.version, "tag", "", "release tag to install")
	fs.BoolVar(&opts.yes, "yes", false, "skip confirmation")
	fs.BoolVar(&opts.yes, "y", false, "skip confirmation")
	fs.BoolVar(&opts.force, "force", false, "reinstall even when already on the target version")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if strings.Count(opts.repo, "/") != 1 {
		return opts, fmt.Errorf("repo must be owner/name")
	}
	return opts, nil
}

func cmdUpgrade(args []string) error {
	opts, err := parseUpgradeArgs(args)
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("prebuilt upgrades are available for Linux and macOS only")
	}

	client := &http.Client{Timeout: 60 * time.Second}
	rel, err := fetchGitHubRelease(client, opts.repo, opts.version)
	if err != nil {
		return err
	}
	asset, err := findReleaseAsset(rel.Assets, releaseAssetName(runtime.GOOS, runtime.GOARCH))
	if err != nil {
		return fmt.Errorf("%w in release %s", err, rel.TagName)
	}
	if !opts.force && version == rel.TagName {
		fmt.Printf("Already on %s\n", rel.TagName)
		return nil
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("determine executable path: %w", err)
	}
	execPath, _ = filepath.EvalSymlinks(execPath)

	fmt.Printf("Current: %s\n", versionString())
	fmt.Printf("Target:  %s\n", rel.TagName)
	fmt.Printf("Asset:   %s\n", asset.Name)
	fmt.Printf("Install: %s\n", execPath)
	if !opts.yes && !confirmUpgrade(os.Stdin, os.Stdout) {
		fmt.Println("Canceled")
		return nil
	}

	tmpPath, err := downloadReleaseAsset(client, asset.BrowserDownloadURL, filepath.Dir(execPath))
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	backupPath, err := replaceExecutable(execPath, tmpPath)
	if err != nil {
		return err
	}

	fmt.Printf("Upgraded to %s\n", rel.TagName)
	fmt.Printf("Backup: %s\n", backupPath)
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		fmt.Println("If the background service is running, restart it with: codex-proxy restart")
	}
	return nil
}

func fetchGitHubRelease(client *http.Client, repo, tag string) (githubRelease, error) {
	endpoint := strings.TrimRight(githubAPIBaseURL, "/") + "/repos/" + repo + "/releases/latest"
	if tag != "" {
		tag = normalizeReleaseTag(tag)
		endpoint = strings.TrimRight(githubAPIBaseURL, "/") + "/repos/" + repo + "/releases/tags/" + tag
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return githubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "codex-proxy/"+version)

	resp, err := client.Do(req)
	if err != nil {
		return githubRelease{}, fmt.Errorf("fetch release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return githubRelease{}, fmt.Errorf("fetch release: GitHub returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return githubRelease{}, fmt.Errorf("decode release: %w", err)
	}
	if rel.TagName == "" {
		return githubRelease{}, fmt.Errorf("release response missing tag_name")
	}
	return rel, nil
}

func releaseAssetName(goos, goarch string) string {
	return "codex-proxy-" + goos + "-" + goarch
}

func normalizeReleaseTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.HasPrefix(tag, "v") {
		return tag
	}
	return "v" + tag
}

func findReleaseAsset(assets []githubReleaseAsset, name string) (githubReleaseAsset, error) {
	for _, asset := range assets {
		if asset.Name == name && asset.BrowserDownloadURL != "" {
			return asset, nil
		}
	}
	return githubReleaseAsset{}, fmt.Errorf("asset %q not found", name)
}

func downloadReleaseAsset(client *http.Client, url, dir string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "codex-proxy/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("download asset: server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	tmp, err := os.CreateTemp(dir, ".codex-proxy-upgrade-*")
	if err != nil {
		return "", fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write temporary binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("close temporary binary: %w", err)
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("make temporary binary executable: %w", err)
	}
	return tmpPath, nil
}

func replaceExecutable(execPath, tmpPath string) (string, error) {
	backupPath := execPath + ".bak"
	if _, err := os.Stat(backupPath); err == nil {
		backupPath = fmt.Sprintf("%s.bak.%d", execPath, time.Now().Unix())
	}
	if err := os.Rename(execPath, backupPath); err != nil {
		return "", fmt.Errorf("backup current binary: %w", err)
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		_ = os.Rename(backupPath, execPath)
		return "", fmt.Errorf("install new binary: %w", err)
	}
	return backupPath, nil
}

func confirmUpgrade(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Proceed with upgrade? [y/N] ")
	var answer string
	if _, err := fmt.Fscanln(in, &answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
