# codex-proxy

Codex OAuth API Proxy. Use your ChatGPT subscription through the Codex backend and expose local OpenAI-compatible APIs for downstream clients.

[中文文档](README.zh-CN.md)

## What it does

`codex-proxy` authenticates with ChatGPT/Codex through browser OAuth, stores and refreshes the resulting tokens locally, and forwards compatible API requests to:

```text
https://chatgpt.com/backend-api/codex/responses
```

The cost is covered by your ChatGPT subscription. It does not use OpenAI API credits.

```text
ChatGPT subscription account
        |
Browser OAuth login with PKCE
        |
Local auth.json with access/refresh tokens
        |
codex-proxy on 127.0.0.1:10531
        |
OpenAI-compatible, Anthropic-compatible, and image endpoints
        |
Any SDK or client that can set a base URL
```

## Features

- OpenAI-compatible `/v1/chat/completions`, including streaming.
- Anthropic-compatible `/v1/messages`, including streaming, images, `tools`, `tool_use`, and `tool_result`.
- Codex `/v1/responses` passthrough.
- OpenAI-compatible image generation and edits through `gpt-image-2`.
- `/v1/models`, `/usage`, `/metrics`, `/health`, and root service metadata endpoints.
- API key protection with `Authorization: Bearer ...` or `X-API-Key`.
- API keys from `~/.codex-proxy/keys.json` and optional `CODEX_PROXY_API_KEY`.
- Multi-account load balancing with `round-robin` or `random` strategy.
- Automatic token refresh, refresh-and-retry on upstream 401, and backoff retry on upstream 429/5xx.
- Linux systemd user service and macOS launchd user service.
- Linux one-command installer.
- Telegram monitoring bot with status, usage, metrics, models, key, and deployment diagnostics commands.
- Proactive Telegram alerts for auth health changes, errors, retries, token refreshes, and service exits.
- Deployment diagnostics with `codex-proxy doctor`.
- CORS enabled for browser clients.

## Requirements

- ChatGPT Plus, Pro, Team, or Enterprise subscription with Codex access.
- Go 1.22+ if building from source.
- Linux, macOS, or another platform that can run the built Go binary.

The Linux installer can install Go automatically when Go is missing or too old.

## Install

### Download from GitHub Releases

Download the matching binary from the GitHub Releases page, then put it somewhere in your `PATH`:

```bash
chmod +x codex-proxy-linux-amd64
sudo mv codex-proxy-linux-amd64 /usr/local/bin/codex-proxy
```

Release assets are built for:

- Linux amd64
- Linux arm64
- macOS amd64
- macOS arm64

### Linux one-command install

```bash
git clone https://github.com/wangyuyan666/CodexProxy.git
cd CodexProxy
./install.sh
```

The installer:

- Checks Go and installs Go 1.22 if needed.
- Builds a static binary.
- Installs `codex-proxy` to `/usr/local/bin`.
- Installs a systemd user service.
- Persists Telegram environment variables to `~/.codex-proxy/env` when they are present.

After installation:

```bash
codex-proxy login
codex-proxy key add --name main
codex-proxy start
codex-proxy status
```

### Build manually

```bash
git clone https://github.com/wangyuyan666/CodexProxy.git
cd CodexProxy
go build -o codex-proxy .
```

## Quick start

### 1. Log in

```bash
codex-proxy login
```

The command prints an OAuth URL. Open it in your browser, complete login, then copy the final redirected localhost URL back into the terminal. The localhost page may fail to load; the URL in the address bar is what matters.

By default credentials are stored in:

```text
~/.codex-proxy/auth.json
```

You can write a different auth file for multi-account setups:

```bash
codex-proxy login --auth-file ~/.codex-proxy/auth-alt.json
```

### 2. Create an API key

```bash
codex-proxy key add --name main
codex-proxy key list
```

Keys are stored in:

```text
~/.codex-proxy/keys.json
```

You can also inject one key through the environment:

```bash
export CODEX_PROXY_API_KEY="cpx-your-api-key"
```

If both are configured, keys from `keys.json` and `CODEX_PROXY_API_KEY` are all accepted.

### 3. Start the proxy

```bash
codex-proxy serve
```

Default address:

```text
http://127.0.0.1:10531/v1
```

Use another bind address or port:

```bash
codex-proxy serve --host 0.0.0.0 --port 8080
```

### 4. Use any OpenAI-compatible client

Python:

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:10531/v1",
    api_key="cpx-your-api-key",
)

resp = client.chat.completions.create(
    model="gpt-5.4",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(resp.choices[0].message.content)
```

Node.js:

```javascript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://127.0.0.1:10531/v1",
  apiKey: "cpx-your-api-key",
});

const resp = await client.chat.completions.create({
  model: "gpt-5.4",
  messages: [{ role: "user", content: "Hello!" }],
});
console.log(resp.choices[0].message.content);
```

curl:

```bash
curl http://127.0.0.1:10531/v1/chat/completions \
  -H "Authorization: Bearer cpx-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.4",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": false
  }'
```

## Authentication and API keys

All endpoints except `OPTIONS`, `/health`, and `/` require an API key.

Accepted request headers:

```http
Authorization: Bearer cpx-your-api-key
```

or:

```http
X-API-Key: cpx-your-api-key
```

Manage file-backed keys:

```bash
codex-proxy key add [--name NAME] [--key KEY]
codex-proxy key list
codex-proxy key delete <key-or-name>
```

`CODEX_PROXY_API_KEY` is useful for single-key container or service deployments. `keys.json` is better when you need multiple keys.

## Endpoints

| Endpoint | Method | Auth | Description |
| --- | --- | --- | --- |
| `/` | GET | No | Service metadata |
| `/health` | GET | No | Auth-aware health check, returns 200 or 503 |
| `/v1/chat/completions` | POST | Yes | OpenAI-compatible Chat Completions |
| `/v1/messages` | POST | Yes | Anthropic-compatible Messages API |
| `/v1/images/generations` | POST | Yes | OpenAI-compatible image generation |
| `/v1/images/edits` | POST | Yes | OpenAI-compatible image edits |
| `/v1/responses` | POST | Yes | Codex Responses API passthrough |
| `/v1/models` | GET | Yes | Discovered models plus `gpt-image-2` when image generation is available |
| `/usage` | GET | Yes | Account rate-limit usage |
| `/metrics` | GET | Yes | Request, error, retry, token refresh, and uptime counters |

Request bodies are capped at 10 MB. SSE event lines are capped at 32 MB.

## OpenAI Chat Completions

`/v1/chat/completions` accepts standard OpenAI Chat Completions-style requests and translates them to Codex Responses format.

Supported behavior includes:

- Streaming and non-streaming responses.
- Text content.
- Image input through `image_url`.
- Function tools / tool calls.
- `response_format` to Responses `text.format`.
- Sampling parameters for non-reasoning models.

Some fields that the Codex backend rejects are removed or adapted before forwarding, such as unsupported `max_tokens`, `max_output_tokens`, and `stop` fields.

## Anthropic Messages

`/v1/messages` accepts Anthropic Messages-style requests and converts them through the same Codex backend.

Supported behavior includes:

- `system`
- `messages`
- text blocks
- image blocks using base64/data URL inputs
- `tools`
- `tool_choice`
- assistant `tool_use`
- user `tool_result`
- streaming Anthropic SSE responses
- Anthropic-shaped error responses for this endpoint

This is intended for clients that can speak Anthropic's Messages API but allow a custom base URL.

## Images

The image endpoints expose OpenAI-compatible request shapes and use the Codex `image_generation` tool internally.

### Generate images

```python
resp = client.images.generate(
    model="gpt-image-2",
    prompt="A shiba inu wearing sunglasses",
    size="1024x1024",
)
print(resp.data[0].b64_json)
```

```bash
curl http://127.0.0.1:10531/v1/images/generations \
  -H "Authorization: Bearer cpx-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-image-2","prompt":"A shiba inu wearing sunglasses","size":"1024x1024"}'
```

### Edit images

OpenAI SDK multipart uploads are supported:

```python
resp = client.images.edit(
    model="gpt-image-2",
    image=open("cat.png", "rb"),
    prompt="Put a hat on the cat",
)
```

JSON requests can also provide data URLs in `image`, `images`, or `image_url` objects.

Supported image request fields:

- `model`, defaults to `gpt-image-2`
- `prompt`
- `n`, from 1 to 10
- `size`, forwarded as-is to the upstream image tool
- `quality`
- `background`
- `output_format`, defaults to `png`
- `response_format`, defaults to `b64_json`; `url` returns a data URL

If `size` is omitted, the upstream model chooses its default output size. The proxy does not crop, resize, or validate image dimensions.

Image generation is unavailable when model discovery fails at startup, because the image handler needs a real discovered base model for the underlying Codex request.

You can also request image generation from Chat Completions by passing:

```json
{
  "tools": [
    {"type": "image_generation", "model": "gpt-image-2"}
  ]
}
```

Generated images are returned in markdown data URL form:

```markdown
![image](data:image/png;base64,...)
```

## Usage and metrics

CLI usage:

```bash
codex-proxy usage
```

HTTP usage:

```bash
curl http://127.0.0.1:10531/usage \
  -H "Authorization: Bearer cpx-your-api-key"
```

Metrics:

```bash
curl http://127.0.0.1:10531/metrics \
  -H "Authorization: Bearer cpx-your-api-key"
```

Metric fields:

- `requests_total`
- `requests_active`
- `errors_total`
- `retries`
- `token_refreshes`
- `uptime_seconds`

## Multi-account load balancing

Log in once per account:

```bash
codex-proxy login
codex-proxy login --auth-file ~/.codex-proxy/auth-alt.json
```

Create `~/.codex-proxy/proxy.json`:

```json
{
  "accounts": [
    {"name": "main", "auth_file": "~/.codex-proxy/auth.json"},
    {"name": "alt", "auth_file": "~/.codex-proxy/auth-alt.json"}
  ],
  "strategy": "round-robin",
  "host": "127.0.0.1",
  "port": "10531"
}
```

Start with auto-discovery:

```bash
codex-proxy serve
```

Or specify a config file:

```bash
codex-proxy serve --config /path/to/proxy.json
```

Strategies:

- `round-robin`
- `random`

If an account fails with upstream 401, the proxy fails over to another healthy account. Failed accounts are retried after a cooldown. `codex-proxy status` and `codex-proxy usage` include all configured accounts.

## Token management

- Tokens are stored in `~/.codex-proxy/auth.json` by default.
- `CODEX_HOME` changes the storage directory to `$CODEX_HOME/auth.json`, `$CODEX_HOME/keys.json`, and `$CODEX_HOME/proxy.json`.
- Token files are written with `0600` permissions.
- Tokens are treated as stale after 7 days.
- Background refresh starts proactively after 5 days.
- Upstream 401 triggers refresh-and-retry.
- Upstream 429 and 5xx responses use exponential backoff retry, up to 2 retries.
- Rotated refresh tokens are saved automatically.
- Auth requests use a `curl` subprocess to improve compatibility on servers where direct Go TLS requests are blocked by Cloudflare fingerprinting.

## Telegram monitoring

Set:

```bash
export CODEX_PROXY_TELEGRAM_BOT_TOKEN="123456:bot-token"
export CODEX_PROXY_TELEGRAM_CHAT_ID="123456789"
codex-proxy serve
```

The bot uses long polling. No inbound Telegram webhook port is required.

Commands:

```text
/status   Auth and proxy health
/usage    Account rate-limit usage
/metrics  Request, error, retry, token refresh, and uptime counters
/models   Available models
/key      API key configuration, including full key values
/doctor   Deployment diagnostics
/help     Help
```

Only the chat ID in `CODEX_PROXY_TELEGRAM_CHAT_ID` receives responses. Messages from other chats are ignored.

Telegram messages use HTML formatting with compact headings, bullets, emoji, and `<code>` blocks. Dynamic values are escaped before sending.

Proactive alerts are sent for:

- Auth health changing to degraded or recovered.
- Error counter increases.
- Upstream retry counter increases.
- Token refresh counter increases.
- Proxy server exits with an error.

Error, retry, and token-refresh alerts use cooldowns to avoid noisy bursts.

For systemd services, put Telegram environment variables in:

```text
~/.codex-proxy/env
```

Example:

```bash
mkdir -p ~/.codex-proxy
chmod 700 ~/.codex-proxy
cat > ~/.codex-proxy/env <<'EOF'
CODEX_PROXY_TELEGRAM_BOT_TOKEN=123456:bot-token
CODEX_PROXY_TELEGRAM_CHAT_ID=123456789
EOF
chmod 600 ~/.codex-proxy/env

codex-proxy install
codex-proxy restart
```

The Linux installer writes this file automatically when those variables are already present in the shell.

## Deployment diagnostics

Run:

```bash
codex-proxy doctor
```

The doctor command checks:

- Auth file
- API key configuration
- Telegram bot token reachability
- systemd or launchd service installation and running state
- Caddy binary and `/etc/caddy/Caddyfile`

It reports only. It does not change system state.

## Service management

Linux uses a user-level systemd service. macOS uses a user-level launchd agent.

```bash
codex-proxy install
codex-proxy start
codex-proxy stop
codex-proxy restart
codex-proxy logs
codex-proxy uninstall
```

On macOS, `codex-proxy install` generates:

```text
~/Library/LaunchAgents/com.local.codex-proxy.plist
```

## Caddy reverse proxy

For a public HTTPS endpoint such as `api.example.com`, keep `codex-proxy` bound to localhost and let Caddy terminate TLS:

```caddyfile
api.example.com {
    reverse_proxy 127.0.0.1:10531
}
```

Then point clients to:

```text
https://api.example.com/v1
```

Keep API key authentication enabled even behind Caddy.

## CLI reference

```bash
codex-proxy login [--auth-file PATH]
codex-proxy serve [--host H] [--port P] [--config F]
codex-proxy status
codex-proxy usage
codex-proxy doctor
codex-proxy logout

codex-proxy key add [--name NAME] [--key KEY]
codex-proxy key list
codex-proxy key delete <key-or-name>

codex-proxy install
codex-proxy start
codex-proxy stop
codex-proxy restart
codex-proxy logs
codex-proxy uninstall
```

## Development

The project uses only the Go standard library.

```bash
go test -count=1 ./...
go vet ./...
go build -o codex-proxy .
```

GitHub Actions builds Linux and macOS release binaries for amd64 and arm64 when a `v*` tag is pushed.

## Security notes

- `auth.json` is equivalent to a password for your ChatGPT account. Do not commit or share it.
- API keys protect all non-public endpoints. Treat them as secrets.
- `/key` in the Telegram bot intentionally displays full API keys. Only configure the bot for a private chat you control.
- Do not run this as an open multi-tenant public service.
- This is not an officially supported OpenAI API path. It may break or be restricted by upstream changes.
