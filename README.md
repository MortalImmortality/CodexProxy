# codex-proxy

Codex OAuth API Proxy — 通过 Codex OAuth 登录，暴露 OpenAI 兼容 API 给下游客户端调用。

## 工作原理

```
用户 ChatGPT 订阅 (Plus/Pro/Team)
         ↓
  浏览器 OAuth 登录 (PKCE)
         ↓
  获取 access_token + refresh_token
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
git clone https://github.com/wangyuyan666/CodexProxy.git
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

POST 请求（chat completions、responses）需要 API key 认证。GET 请求（models、health、usage 等）无需认证。

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

## API 端点

| 端点 | 方法 | 认证 | 说明 |
|------|------|------|------|
| `/v1/chat/completions` | POST | 需要 | OpenAI 兼容的 Chat Completions API |
| `/v1/responses` | POST | 需要 | Codex Responses API 直通 |
| `/v1/models` | GET | 不需要 | 列出当前账号可用的模型 |
| `/health` | GET | 不需要 | 健康检查（含 Token 状态） |
| `/metrics` | GET | 不需要 | 运行指标（请求数、错误数、重试次数、uptime） |
| `/usage` | GET | 不需要 | 各账号 rate limit 用量 |

## CLI 命令

```bash
codex-proxy login [--auth-file PATH]              # 浏览器 OAuth 登录
codex-proxy serve [--host H] [--port P] [--config F]  # 启动代理
codex-proxy status                                 # 查看认证 + 服务状态
codex-proxy usage                                  # 查看账号用量
codex-proxy logout                                 # 删除凭证

codex-proxy key add [--name N] [--key K]           # 添加 API key
codex-proxy key list                               # 列出 API key
codex-proxy key delete <key-or-name>               # 删除 API key

codex-proxy install                                # 安装 systemd 服务
codex-proxy start / stop / restart / logs          # 服务管理
codex-proxy uninstall                              # 卸载服务
```

## Token 管理

- Token 存储在 `~/.codex-proxy/auth.json`
- Token 7 天判定为 stale，5 天时后台主动 refresh
- 遇到上游 401 自动 refresh-and-retry
- 遇到上游 429/5xx 指数退避重试（最多 2 次）
- refresh_token 轮换时自动更新 auth.json

## 多账号负载均衡

支持多个 ChatGPT 账号轮流使用，分散请求压力。

### 1. 为每个账号登录

```bash
codex-proxy login                                          # 主账号
codex-proxy login --auth-file ~/.codex-proxy/auth-alt.json # 副账号
```

### 2. 创建配置文件 `~/.codex-proxy/proxy.json`

```json
{
  "accounts": [
    {"name": "main", "auth_file": "~/.codex-proxy/auth.json"},
    {"name": "alt",  "auth_file": "~/.codex-proxy/auth-alt.json"}
  ],
  "strategy": "round-robin"
}
```

`strategy` 可选 `round-robin`（轮询）或 `random`（随机）。

### 3. 启动

```bash
codex-proxy serve                        # 自动检测 ~/.codex-proxy/proxy.json
codex-proxy serve --config /path/to.json # 指定配置文件
```

- 某账号 401 失败时自动切换到其他健康账号
- 故障账号 5 分钟后自动重试恢复
- `codex-proxy status` 和 `codex-proxy usage` 显示所有账号状态

## 安全提醒

- `auth.json` 等同于密码，不要提交到 git 或分享
- API key 保护所有 POST 请求，防止未授权调用
- 不要作为多租户公共服务部署
- 这不是 OpenAI 官方支持的用法，存在被限制的风险

## 已知同类项目

- [openai-oauth](https://github.com/EvanZhouDev/openai-oauth) — TypeScript/Node 实现，Vercel AI SDK provider
- [AI-Zero-Token](https://github.com/fchangjun/AI-Zero-Token) — 带 Web 管理面板，支持多账号
- [OpenClaw](https://docs.openclaw.ai/concepts/oauth) — 多 agent OAuth 管理
