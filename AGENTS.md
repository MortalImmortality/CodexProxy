# AGENTS.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o codex-proxy .
./codex-proxy login                 # authenticate via browser OAuth
./codex-proxy serve                 # start proxy on 127.0.0.1:10531
./codex-proxy serve --host 0.0.0.0 --port 8080
./codex-proxy status                # show auth + service status
./codex-proxy doctor                # diagnose auth/key/telegram/service/caddy setup
./codex-proxy logout                # remove ~/.codex-proxy/auth.json
```

Linux one-click install: `./install.sh` (builds, installs to /usr/local/bin, sets up systemd).

Service management (Linux/macOS, after install):
```bash
codex-proxy install     # set up systemd/launchd user service
codex-proxy start       # start background service
codex-proxy stop / restart / logs / uninstall
```

No external dependencies — stdlib only (Go 1.22+, uses log/slog and builtin min). Tests live under `auth/*_test.go` and `proxy/*_test.go`; run `go test ./...` before committing.

## What This Is

Local HTTP proxy that lets any OpenAI-compatible SDK hit ChatGPT's Codex backend API using the user's ChatGPT subscription (no API credits). The flow:

1. User authenticates via browser OAuth with PKCE
2. Proxy holds/refreshes tokens and listens on `:10531`
3. Incoming `/v1/chat/completions` requests get translated to Codex `/responses` format and forwarded to `chatgpt.com/backend-api/codex/responses`
4. Responses get converted back to OpenAI chat completion shape (both streaming SSE and non-streaming)
5. `/v1/messages` accepts Anthropic Messages-style clients and translates text/image requests plus tools/tool_use/tool_result through the same Codex backend

## Architecture

```
main.go                  CLI entrypoint, signal handling, graceful shutdown
service.go               systemd service management (install/start/stop/logs)
auth/auth.go             OAuth, token lifecycle, request body translation
proxy/proxy.go           HTTP server, format conversion, retry, metrics
install.sh               Linux one-click installer
codex-proxy.plist        macOS launchd template; install command generates the real file
```

- **`main.go`** + **`service.go`** — Manual arg parsing, dispatches to auth/proxy/service. `serve` sets up `signal.NotifyContext` for SIGINT/SIGTERM, starts background token refresh, optionally starts Telegram monitoring when `CODEX_PROXY_TELEGRAM_BOT_TOKEN` and `CODEX_PROXY_TELEGRAM_CHAT_ID` are set, and does graceful shutdown. `service.go` wraps `systemctl --user` and `journalctl --user` for the install/start/stop/restart/logs/uninstall subcommands. `install` writes the systemd unit file using the current binary path via `os.Executable()`.

- **`doctor.go`** — Deployment diagnostics for auth file, API keys, Telegram bot reachability, service install/running state, and Caddy presence. It reports only; it does not mutate system state.

- **`auth/auth.go`** — Browser-based OAuth with PKCE, token persistence (`~/.codex-proxy/auth.json`, shared with Codex CLI), thread-safe `TokenManager` with auto-refresh (7-day staleness, 5-day proactive refresh via background goroutine). `IsHealthy()` reports token usability for health checks. Auth requests use `curl` subprocess to avoid Cloudflare TLS fingerprint blocking on VPS.

- **`proxy/proxy.go`** — HTTP server with OpenAI-compatible endpoints. Two HTTP clients: `normalClient` (60s timeout) and `streamClient` (no overall timeout, 30s response header timeout). `callUpstream` handles 401→refresh-and-retry plus 429/5xx→exponential backoff (max 2 retries). Streaming `/v1/chat/completions` converts Codex SSE events (`response.output_text.delta`, `response.completed`) into OpenAI chat completion chunk format. `/v1/responses` does raw SSE passthrough. JSON and multipart request bodies default to a 100 MiB cap via `http.MaxBytesReader`, configurable with `--max-body-mb` or `CODEX_PROXY_MAX_REQUEST_BODY_MB`.

- **`telegram.go`** — Optional Telegram long-polling monitor. It only starts when bot token and allowed chat id env vars are present. Supports `/status`, `/usage`, `/metrics`, `/models`, `/key`, `/doctor`, `/help`; unauthorized chats are ignored and Telegram failures never stop the proxy. Also sends proactive alerts for auth health degradation/recovery, rising error/retry/token-refresh counters, and proxy server exit. Telegram messages use HTML parse mode with light emoji, grouped bold headings, bullet rows, and `<code>` for commands/models. Any future Telegram message should follow this format and escape dynamic values with `tgEscape`; noisy alerts should use cooldown.

### Endpoints

| Path | Purpose |
|------|---------|
| `/v1/chat/completions` | OpenAI-compatible, converts to/from Codex format |
| `/v1/messages` | Anthropic Messages API compatibility, converts text/images/tools to/from Codex format |
| `/v1/responses` | Codex API passthrough |
| `/v1/models` | Lists available models (discovered at startup) |
| `/health` | Returns 200/503 based on token state |
| `/metrics` | JSON counters: requests, errors, retries, uptime |

### Key design details

- `auth.Manager` is a package-level singleton (`*TokenManager`) initialized in `init()`. All token access goes through it.
- Token file path: `$CODEX_HOME/auth.json` or `~/.codex-proxy/auth.json`. Written with 0600 perms.
- `BuildCodexRequestBody` maps `messages` → `input`, drops Codex-rejected chat params (`max_tokens`, `max_output_tokens`, `stop`), drops sampling params for reasoning models, flattens Chat Completions function tools, and maps `response_format` to `text.format`.
- `convertToOpenAIFormat` / `extractMessage` navigate the Codex response structure (`output[].content[].text`, function calls, image calls, refusals) back into `choices[].message`.
- `logWriter` wraps `http.ResponseWriter` and implements `http.Flusher` so streaming works through the logging middleware.
- Structured JSON logging (slog) only in `serve` mode; interactive commands use plain fmt.

### Deployment

**Linux** — one-click:
```bash
./install.sh   # builds, installs binary, sets up systemd user service
```
Or manually: `go build`, copy binary, `codex-proxy install`.

**macOS** — `codex-proxy install` generates a launchd user agent:
```bash
go build -o /usr/local/bin/codex-proxy .
codex-proxy install
codex-proxy start
```
