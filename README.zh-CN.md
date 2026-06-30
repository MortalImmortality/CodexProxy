# codex-proxy

Codex OAuth API Proxy — 通过 Codex OAuth 或 Codex access token 登录，暴露 OpenAI 兼容 API 给下游客户端调用。

## 工作原理

```
用户 ChatGPT 订阅 (Plus/Pro/Team)
         ↓
  浏览器 OAuth 登录 (PKCE)
  或 CODEX_ACCESS_TOKEN / access-token 登录
         ↓
  获取 access_token + refresh_token，或直接使用静态 access token
         ↓
  本地 API Proxy (:10531)
         ↓
  转发请求到 chatgpt.com/backend-api/codex/responses
         ↓
  任何 OpenAI SDK 客户端都能用
```

费用走用户自己的 ChatGPT 订阅，不消耗 API credits。

## 前置条件

1. ChatGPT Plus / Pro / Team / Enterprise 订阅
2. Go 1.22+（Linux 一键安装脚本会自动安装）

## Linux 一键安装

```bash
git clone https://github.com/MortalImmortality/CodexProxy.git
cd CodexProxy
./install.sh
```

脚本自动完成：
- 检测 Go 环境，未安装或版本过低会自动安装 Go 1.22
- 编译静态二进制，安装到 `/usr/local/bin`
- 创建 systemd user service 并设置开机自启

安装完成后：

```bash
codex-proxy login                 # 登录（浏览器 OAuth）
codex-proxy key add --name main   # 创建 API key
codex-proxy start                 # 启动后台服务
```

以后更新已安装的 release 二进制：

```bash
codex-proxy version
sudo codex-proxy upgrade --yes
codex-proxy restart
```

## 手动编译

```bash
cd CodexProxy
go build -o codex-proxy .
```

## 快速开始

### 1. 登录

```bash
./codex-proxy login

# 输出授权链接，在浏览器中打开并登录
# 授权后浏览器跳转到 localhost（页面打不开没关系）
# 复制地址栏完整 URL 粘贴回终端即可
```

如果你的 ChatGPT 工作区账号可以创建 Codex access token，也可以跳过浏览器登录：

```bash
export CODEX_ACCESS_TOKEN="<token>"
./codex-proxy serve
```

这种方式只从环境变量读取 token，不会写入 `proxy.json`。适合可信的非交互式部署。

也可以把 access token 持久化到本地 auth 文件：

```bash
codex-proxy login --with-access-token
# 按提示粘贴 token，然后回车。
```

脚本里也可以继续通过管道传入：

```bash
echo "$CODEX_ACCESS_TOKEN" | codex-proxy login --with-access-token
```

### 2. 创建 API Key

```bash
codex-proxy key add --name peter
#   Key added: cpx-a1b2c3d4...
#   Name:      peter

codex-proxy key list
#   NAME                 KEY                                                  CREATED
#   peter                cpx-a1b2c3d4e5f6...                                 2026-06-16 18:00
```

### 3. 启动代理

```bash
./codex-proxy serve
# 或指定 host/port:
./codex-proxy serve --host 0.0.0.0 --port 8080
```

### 4. 下游客户端接入

**Python (OpenAI SDK)**:
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://YOUR_HOST:10531/v1",
    api_key="cpx-your-api-key",
)

resp = client.chat.completions.create(
    model="o3-pro",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(resp.choices[0].message.content)
```

**curl**:
```bash
curl http://YOUR_HOST:10531/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer cpx-your-api-key" \
  -d '{
    "model": "o3-pro",
    "messages": [{"role":"user","content":"Hi"}],
    "stream": false
  }'
```

**Node.js**:
```javascript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://YOUR_HOST:10531/v1",
  apiKey: "cpx-your-api-key",
});

const resp = await client.chat.completions.create({
  model: "gpt-5.4",
  messages: [{ role: "user", content: "Hello" }],
});
```

## API Key 管理

除 `OPTIONS`、`/health` 和 `/` 外，所有接口都需要 API key 认证，包括 `/v1/models`、`/metrics` 和 `/usage`。

```bash
codex-proxy key add [--name NAME] [--key KEY]    # 添加 key（不指定则自动生成）
codex-proxy key list                              # 列出所有 key
codex-proxy key delete <key-or-name>              # 按 key 或 name 删除
```

Key 存储在 `~/.codex-proxy/keys.json`。也支持 `CODEX_PROXY_API_KEY` 环境变量。

## 账号用量查询

查看各账号的 rate limit 使用情况：

```bash
codex-proxy usage

#   [main]
#     Plan:     plus
#     Usage:    [████████░░░░░░░░░░░░] 40%
#     Status:   ✓
#     Reset in: 120m
```

也可通过 HTTP 查询：`GET /usage`

## Telegram 监控

可选启用 Telegram Bot 查询运行状态。配置后，`serve` 启动时会开启 long polling，不需要额外开放端口。

```bash
export CODEX_PROXY_TELEGRAM_BOT_TOKEN="123456:bot-token"
export CODEX_PROXY_TELEGRAM_CHAT_ID="123456789"
codex-proxy serve
```

支持命令：

```text
/status   查看 token / account 健康状态
/usage    查看账号 rate limit 用量
/metrics  查看请求数、错误数、重试数、token refresh 次数
/models   查看可用模型
/key      查看 API key 配置状态（显示完整 key）
/doctor   查看部署诊断
/help
```

Bot 只响应 `CODEX_PROXY_TELEGRAM_CHAT_ID` 指定的 chat，其他 chat 会被忽略。Telegram 网络失败只记录日志，不会影响代理服务。

消息使用 Telegram HTML 格式化，包含轻量 emoji、分组标题和等宽命令，便于手机端快速扫读。

主动告警会在以下情况推送：

- Auth 健康状态变为 degraded 或恢复 healthy
- 代理错误计数增加
- 上游重试计数增加
- Token refresh 计数增加
- 代理服务异常退出

错误/重试告警带 5 分钟冷却，避免短时间内刷屏。

如果通过 systemd / launchd 运行，需要把这两个环境变量配置到服务进程环境中；只在当前 shell 里 `export` 后再 `codex-proxy start`，服务进程不一定能继承。

Linux systemd 服务会自动加载 `~/.codex-proxy/env`：

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

如果运行 `install.sh` 时当前 shell 已经设置了 `CODEX_PROXY_TELEGRAM_BOT_TOKEN` 和 `CODEX_PROXY_TELEGRAM_CHAT_ID`，安装脚本会自动写入 `~/.codex-proxy/env`。

## 文生图 / 图生图

Codex 后端支持图片生成（`gpt-image-2`）。代理暴露 OpenAI 兼容的 images 接口。

**文生图** (`/v1/images/generations`)：
```python
resp = client.images.generate(
    model="gpt-image-2",
    prompt="一只戴墨镜的柴犬",
    size="1024x1024",
)
# resp.data[0].b64_json
```

```bash
curl http://YOUR_HOST:10531/v1/images/generations \
  -H "Authorization: Bearer cpx-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"一只戴墨镜的柴犬","size":"1024x1024"}'
```

**图生图** (`/v1/images/edits`)：在原图基础上修改。支持 multipart 文件上传（OpenAI SDK 标准）或 JSON 传 data URL。
```python
resp = client.images.edit(
    model="gpt-image-2",
    image=open("cat.png", "rb"),
    prompt="给猫戴上一顶帽子",
)
```

也可在 chat completions 里直接用：传 `tools: [{"type":"image_generation","model":"gpt-image-2"}]`，生成的图以 markdown `![image](data:...)` 形式返回。

> 默认图片模型 `gpt-image-2`。模型发现失败时图片接口返回 503。

## API 端点

| 端点 | 方法 | 认证 | 说明 |
|------|------|------|------|
| `/v1/chat/completions` | POST | 需要 | OpenAI 兼容的 Chat Completions API（支持文生图工具） |
| `/v1/messages` | POST | 需要 | Anthropic Messages API 兼容（文本、图片 data URL、tools/tool_use/tool_result、流式） |
| `/v1/images/generations` | POST | 需要 | 文生图（OpenAI images API 兼容） |
| `/v1/images/edits` | POST | 需要 | 图生图（multipart 或 JSON data URL） |
| `/v1/responses` | POST | 需要 | Codex Responses API 直通 |
| `/v1/models` | GET | 需要 | 列出可用模型（含 `gpt-image-2`） |
| `/health` | GET | 不需要 | 健康检查（含 Token 状态） |
| `/metrics` | GET | 需要 | 运行指标（请求数、错误数、重试次数、uptime） |
| `/usage` | GET | 需要 | 各账号 rate limit 用量 |

请求体默认限制为 100 MiB。可通过 `codex-proxy serve --max-body-mb N` 或 `CODEX_PROXY_MAX_REQUEST_BODY_MB=N` 调整。SSE 单个事件行限制为 32 MiB。

## CLI 命令

```bash
codex-proxy version                                 # 查看当前版本
codex-proxy login [--name NAME] [--with-access-token]  # 浏览器 OAuth 或 stdin access token 登录
codex-proxy serve [--host H] [--port P] [--max-body-mb N]  # 启动代理
codex-proxy status                                 # 查看认证 + 服务状态
codex-proxy usage                                  # 查看账号用量
codex-proxy upgrade [--version TAG] [--yes]        # 从 GitHub Releases 升级二进制
codex-proxy doctor                                 # 诊断部署配置
codex-proxy logout [--name NAME]                   # 删除凭证

codex-proxy key add [--name N] [--key K]           # 添加 API key
codex-proxy key list                               # 列出 API key
codex-proxy key delete <key-or-name>               # 删除 API key

codex-proxy install                                # 安装用户服务（Linux systemd / macOS launchd）
codex-proxy start / stop / restart / logs          # 服务管理
codex-proxy uninstall                              # 卸载服务
```

macOS 下 `codex-proxy install` 会为当前用户生成 `~/Library/LaunchAgents/com.local.codex-proxy.plist`，自动写入当前 HOME、二进制路径和日志路径。

`codex-proxy doctor` 会检查 auth 文件、API key、Telegram bot 连通性、systemd/launchd 服务状态，以及 Caddy 是否安装并存在 `/etc/caddy/Caddyfile`。

## Token 管理

- Token 存储在 `~/.codex-proxy/auth.json`
- `CODEX_ACCESS_TOKEN` 直接从环境变量使用，不写入磁盘
- `codex-proxy login --with-access-token` 会持久化一个没有 refresh_token 的静态 access-token auth 文件
- 静态 Codex access-token 凭证无法查询 ChatGPT usage，因为 usage 接口需要浏览器 OAuth 凭证
- Token 7 天判定为 stale，5 天时后台主动 refresh
- 遇到上游 401 自动 refresh-and-retry
- 静态 access-token 凭证无法 refresh；过期或撤销后需要替换 token
- 遇到上游 429/5xx 指数退避重试（最多 2 次）
- refresh_token 轮换时自动更新 auth.json

## 多账号负载均衡

支持多个 ChatGPT 账号轮流使用，分散请求压力。

### 1. 为每个账号登录

```bash
codex-proxy login                                          # 主账号
codex-proxy login --name alt                              # 副账号
codex-proxy logout --name alt                             # 删除副账号
```

### 2. 自动生成配置文件 `~/.codex-proxy/proxy.json`

登录成功后会自动注册到默认配置文件：

```json
{
  "accounts": [
    {"name": "default", "auth_file": "~/.codex-proxy/auth.json"},
    {"name": "alt",  "auth_file": "~/.codex-proxy/auth-alt.json"}
  ],
  "strategy": "round-robin"
}
```

auth 文件始终保存在 codex-proxy 配置目录中。默认 `codex-proxy login` 写入 `~/.codex-proxy/auth.json`；`codex-proxy login --name alt` 写入 `~/.codex-proxy/auth-alt.json`。

如果升级前已经有 `auth.json` / `auth-*.json`，但没有 `proxy.json`，默认配置会自动从这些 auth 文件生成。

服务已运行时，新增/删除账号以及修改 `proxy.json` 里的 `strategy` 会自动重新加载。正在处理的请求继续使用当前账号，新请求会使用更新后的负载均衡账号池。

如果设置了 `CODEX_ACCESS_TOKEN`，它会作为名为 `codex-access-token` 的内存账号加入账号池，并和 `proxy.json` 里的账号一起参与负载均衡；该 token 不会保存到 `proxy.json`。

只有需要改 `strategy`、`host`、`port` 等高级设置时，才需要手动编辑。`strategy` 可选 `round-robin`（轮询）或 `random`（随机）。

### 3. 启动

```bash
codex-proxy serve                        # 自动检测 ~/.codex-proxy/proxy.json
```

- 某账号 401 失败时自动切换到其他健康账号
- 故障账号 5 分钟后自动重试恢复
- `codex-proxy status` 和 `codex-proxy usage` 显示所有账号状态

## 安全提醒

- `auth.json` 等同于密码，不要提交到 git 或分享
- Codex access token 也等同于密码。自动化部署建议通过环境变量或 secret manager 注入，并定期轮换
- API key 保护所有 POST 请求，防止未授权调用
- 不要作为多租户公共服务部署
- 这不是 OpenAI 官方支持的用法，存在被限制的风险
