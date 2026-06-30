package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseUpgradeArgs(t *testing.T) {
	opts, err := parseUpgradeArgs([]string{"--version", "v1.2.3", "--yes", "--force"})
	if err != nil {
		t.Fatalf("parseUpgradeArgs: %v", err)
	}
	if opts.repo != defaultUpgradeRepo {
		t.Fatalf("repo = %q, want default", opts.repo)
	}
	if opts.version != "v1.2.3" || !opts.yes || !opts.force {
		t.Fatalf("opts = %#v, want version/yes/force", opts)
	}

	opts, err = parseUpgradeArgs([]string{"--repo", "owner/project", "--tag", "v2"})
	if err != nil {
		t.Fatalf("parseUpgradeArgs repo/tag: %v", err)
	}
	if opts.repo != "owner/project" || opts.version != "v2" {
		t.Fatalf("opts = %#v, want custom repo/tag", opts)
	}

	if _, err := parseUpgradeArgs([]string{"--repo", "bad"}); err == nil {
		t.Fatal("expected invalid repo error")
	}
	if _, err := parseUpgradeArgs([]string{"extra"}); err == nil {
		t.Fatal("expected unexpected positional arg error")
	}
}

func TestReleaseAssetName(t *testing.T) {
	if got := releaseAssetName("darwin", "arm64"); got != "codex-proxy-darwin-arm64" {
		t.Fatalf("asset = %q, want darwin arm64 asset", got)
	}
}

func TestNormalizeReleaseTag(t *testing.T) {
	if got := normalizeReleaseTag("1.2.3"); got != "v1.2.3" {
		t.Fatalf("tag = %q, want v1.2.3", got)
	}
	if got := normalizeReleaseTag("v1.2.3"); got != "v1.2.3" {
		t.Fatalf("tag = %q, want unchanged v tag", got)
	}
}

func TestFindReleaseAsset(t *testing.T) {
	asset, err := findReleaseAsset([]githubReleaseAsset{
		{Name: "codex-proxy-linux-amd64", BrowserDownloadURL: "https://example.com/linux"},
		{Name: "codex-proxy-darwin-arm64", BrowserDownloadURL: "https://example.com/darwin"},
	}, "codex-proxy-darwin-arm64")
	if err != nil {
		t.Fatalf("findReleaseAsset: %v", err)
	}
	if asset.BrowserDownloadURL != "https://example.com/darwin" {
		t.Fatalf("asset URL = %q, want darwin URL", asset.BrowserDownloadURL)
	}

	if _, err := findReleaseAsset(nil, "missing"); err == nil {
		t.Fatal("expected missing asset error")
	}
}

func TestFetchGitHubRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/project/releases/tags/v1.2.3" {
			t.Fatalf("path = %q, want tag release path", r.URL.Path)
		}
		fmt.Fprint(w, `{"tag_name":"v1.2.3","assets":[{"name":"codex-proxy-linux-amd64","browser_download_url":"https://example.com/bin"}]}`)
	}))
	defer server.Close()

	oldBase := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = oldBase })

	rel, err := fetchGitHubRelease(server.Client(), "owner/project", "v1.2.3")
	if err != nil {
		t.Fatalf("fetchGitHubRelease: %v", err)
	}
	if rel.TagName != "v1.2.3" || len(rel.Assets) != 1 {
		t.Fatalf("release = %#v, want tag and asset", rel)
	}
}

func TestDownloadReleaseAssetAndReplaceExecutable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "new-binary")
	}))
	defer server.Close()

	dir := t.TempDir()
	tmpPath, err := downloadReleaseAsset(server.Client(), server.URL, dir)
	if err != nil {
		t.Fatalf("downloadReleaseAsset: %v", err)
	}
	defer os.Remove(tmpPath)
	info, err := os.Stat(tmpPath)
	if err != nil {
		t.Fatalf("stat downloaded binary: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("downloaded mode = %v, want 0755", info.Mode().Perm())
	}

	execPath := filepath.Join(dir, "codex-proxy")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	backupPath, err := replaceExecutable(execPath, tmpPath)
	if err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}
	data, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("read new executable: %v", err)
	}
	if string(data) != "new-binary" {
		t.Fatalf("new executable = %q, want downloaded binary", data)
	}
	data, err = os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(data) != "old-binary" {
		t.Fatalf("backup = %q, want old binary", data)
	}
}

func TestConfirmUpgrade(t *testing.T) {
	if !confirmUpgrade(strings.NewReader("yes\n"), ioDiscard{}) {
		t.Fatal("yes should confirm")
	}
	if confirmUpgrade(strings.NewReader("\n"), ioDiscard{}) {
		t.Fatal("empty answer should decline")
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
