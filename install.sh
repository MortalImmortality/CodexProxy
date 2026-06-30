#!/usr/bin/env bash
set -euo pipefail

# ──────────────────────────────────────────────
# codex-proxy one-click installer for Linux
#
# Usage:
#   ./install.sh
#
# What it does:
#   1. Installs Go if missing or too old
#   2. Builds codex-proxy from source
#   3. Installs binary to /usr/local/bin
#   4. Sets up systemd user service
# ──────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
error() { echo -e "${RED}[-]${NC} $*"; exit 1; }

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_DIR="/usr/local/bin"
MIN_GO_MAJOR=1
MIN_GO_MINOR=22

# ── Preflight ──────────────────────────────────

[[ "$(uname -s)" == "Linux" ]] || error "This installer is for Linux only"

command -v curl &>/dev/null || error "curl is required but not installed"

# ── Go ─────────────────────────────────────────

install_go() {
    local version="1.22.10"
    local arch
    case "$(uname -m)" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        armv7l)  arch="armv6l" ;;
        *) error "Unsupported architecture: $(uname -m)" ;;
    esac

    info "Installing Go ${version} (${arch})..."
    local tmp
    tmp=$(mktemp -d)
    curl -fsSL "https://go.dev/dl/go${version}.linux-${arch}.tar.gz" -o "$tmp/go.tar.gz"
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$tmp/go.tar.gz"
    rm -rf "$tmp"
    export PATH="/usr/local/go/bin:$PATH"
    info "Go ${version} installed to /usr/local/go"
}

check_go() {
    if ! command -v go &>/dev/null; then
        install_go
        return
    fi

    local ver
    ver=$(go version | awk '{print $3}' | sed 's/go//')
    local major minor
    major=$(echo "$ver" | cut -d. -f1)
    minor=$(echo "$ver" | cut -d. -f2)

    if [[ "$major" -lt "$MIN_GO_MAJOR" ]] || \
       [[ "$major" -eq "$MIN_GO_MAJOR" && "$minor" -lt "$MIN_GO_MINOR" ]]; then
        warn "Go ${ver} is too old (need >= ${MIN_GO_MAJOR}.${MIN_GO_MINOR})"
        install_go
    fi
}

check_go
info "Using $(go version)"

# ── Build ──────────────────────────────────────

cd "$REPO_DIR"
info "Building codex-proxy..."
BUILD_VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
BUILD_COMMIT="$(git rev-parse HEAD 2>/dev/null || true)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X main.version=${BUILD_VERSION} -X main.commit=${BUILD_COMMIT} -X main.date=${BUILD_DATE}" \
    -o codex-proxy .

# ── Install binary ─────────────────────────────

info "Installing to ${INSTALL_DIR}/codex-proxy..."
sudo mkdir -p "$INSTALL_DIR"
sudo install -m 755 codex-proxy "$INSTALL_DIR/codex-proxy"
rm -f codex-proxy

# Verify it's in PATH
if ! command -v codex-proxy &>/dev/null; then
    warn "${INSTALL_DIR} is not in your PATH"
    warn "Add to your shell profile:  export PATH=\"${INSTALL_DIR}:\$PATH\""
fi

# ── Persist optional service environment ───────

ENV_FILE="${HOME}/.codex-proxy/env"
persist_env_var() {
    local name="$1"
    local value="${!name:-}"
    [[ -n "$value" ]] || return 0

    mkdir -p "$(dirname "$ENV_FILE")"
    touch "$ENV_FILE"
    chmod 600 "$ENV_FILE"

    local tmp
    tmp=$(mktemp)
    grep -v "^${name}=" "$ENV_FILE" > "$tmp" || true
    printf '%s=%s\n' "$name" "$value" >> "$tmp"
    mv "$tmp" "$ENV_FILE"
    chmod 600 "$ENV_FILE"
}

if [[ -n "${CODEX_PROXY_TELEGRAM_BOT_TOKEN:-}" || -n "${CODEX_PROXY_TELEGRAM_CHAT_ID:-}" ]]; then
    info "Persisting Telegram service environment to ${ENV_FILE}..."
    persist_env_var CODEX_PROXY_TELEGRAM_BOT_TOKEN
    persist_env_var CODEX_PROXY_TELEGRAM_CHAT_ID
fi

# ── Install systemd service ───────────────────

info "Setting up systemd service..."
"$INSTALL_DIR/codex-proxy" install

# ── Done ───────────────────────────────────────

echo ""
info "Installation complete!"
echo ""
echo "  Quick start:"
echo "    codex-proxy login                 # authenticate"
echo "    codex-proxy start                 # start background service"
echo "    codex-proxy status                # check everything"
echo "    codex-proxy logs                  # tail logs"
echo ""
echo "  Other commands:"
echo "    codex-proxy stop                  # stop service"
echo "    codex-proxy restart               # restart service"
echo "    codex-proxy serve                 # run foreground (debug)"
echo "    codex-proxy uninstall             # remove service"
echo ""
