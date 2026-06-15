# codex-proxy

Codex OAuth API Proxy — 通过 Codex OAuth 登录，暴露 OpenAI 兼容 API 给下游客户端调用。

## 工作原理

```
用户 ChatGPT 订阅 (Plus/Pro/Team)
         ↓
  Device Code OAuth 登录
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
2. 在 ChatGPT 设置中开启 Device Code Login:
   - 个人账号: ChatGPT → Settings → Security → "Allow device code login"
   - 工作区: Workspace Settings → Permissions → "Allow device code login"
3. Go 1.22+（Linux 一键安装脚本会自动安装）

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

安装完成后通过 CLI 管理：

```bash
codex-proxy login --device-auth   # 登录
codex-proxy start                 # 启动后台服务
codex-proxy stop                  # 停止
codex-proxy restart               # 重启
codex-proxy status                # 查看认证 + 服务状态
codex-proxy logs                  # 查看日志（实时）
codex-proxy uninstall             # 卸载服务
```

## 手动编译

```bash
cd CodexProxy
go build -o codex-proxy .
```

## 使用

### 1. 登录（Device Code 方式）

```bash
./codex-proxy login --device-auth

# 输出:
#   ╭─────────────────────────────────────────────╮
#   │  Open:  https://auth.openai.com/codex/device│
#   │  Code:  ABCD-1234                           │
#   ╰─────────────────────────────────────────────╯
#   Waiting for authorization...
#   ✓ Authenticated successfully!
```

在任意设备的浏览器中打开 URL，输入 Code，用 ChatGPT 账号登录。

### 2. 启动 API 代理

```bash
./codex-proxy serve
# 或指定 host/port:
./codex-proxy serve --host 0.0.0.0 --port 8080
```

### 3. 下游客户端接入

**Python (OpenAI SDK)**:
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:10531/v1",
    api_key="unused",  # 任意非空字符串
)

resp = client.chat.completions.create(
    model="o3-pro",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(resp.choices[0].message.content)
```

**curl**:
```bash
curl http://127.0.0.1:10531/v1/chat/completions \
  -H "Content-Type: application/json" \
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
  baseURL: "http://127.0.0.1:10531/v1",
  apiKey: "unused",
});

const resp = await client.chat.completions.create({
  model: "gpt-5.4",
  messages: [{ role: "user", content: "Hello" }],
});
```

### 4. 查看状态

```bash
./codex-proxy status

#   Auth mode:       chatgptDeviceCode
#   Last refresh:    2026-06-15T10:30:00+08:00
#   Token staleness: 2h30m
#   Access token:    eyJhbGciO...abc123
#   Has refresh:     true
#   ✓ Token is fresh
```

### 5. 登出

```bash
./codex-proxy logout
```

## API 端点

| 端点 | 方法 | 说明 |
|------|------|------|
| `/v1/chat/completions` | POST | OpenAI 兼容的 Chat Completions API |
| `/v1/responses` | POST | Codex Responses API 直通 |
| `/v1/models` | GET | 列出当前账号可用的模型 |
| `/health` | GET | 健康检查 |

## Token 管理

- Token 存储在 `~/.codex/auth.json`（与 Codex CLI 共享）
- 如果你已经通过 `codex login` 登录过，可以直接 `codex-proxy serve`
- Token 大约 8 天后过期，proxy 会自动 refresh
- 遇到上游 401 时会自动 refresh-and-retry
- refresh_token 可能在刷新时被轮换，proxy 会自动更新 auth.json

## auth.json 结构

```json
{
  "auth_mode": "chatgptDeviceCode",
  "tokens": {
    "id_token": "eyJ...",
    "access_token": "eyJ...",
    "refresh_token": "GrD...",
    "account_id": "user-xxx"
  },
  "last_refresh": "2026-06-15T10:30:00Z"
}
```

## 集成到 hermes-rhyme / 盯盘工具

在你的 Hermes Agent 配置中，设置 LLM provider:

```yaml
# hermes agent config
llm:
  provider: openai
  base_url: http://127.0.0.1:10531/v1
  api_key: unused
  model: o3-pro
```

Telegram Bot 中:
```python
# bot.py
OPENAI_BASE_URL = "http://127.0.0.1:10531/v1"
```

## 安全提醒

- `auth.json` 等同于密码，不要提交到 git 或分享
- 仅在受信任的本地机器上运行
- 不要作为多租户公共服务部署
- 这不是 OpenAI 官方支持的用法，存在被限制的风险
- Anthropic 已在 2026 年 4 月关闭了 Claude 的等效通道

## 已知同类项目

- [openai-oauth](https://github.com/EvanZhouDev/openai-oauth) — TypeScript/Node 实现，Vercel AI SDK provider
- [AI-Zero-Token](https://github.com/fchangjun/AI-Zero-Token) — 带 Web 管理面板，支持多账号
- [OpenClaw](https://docs.openclaw.ai/concepts/oauth) — 多 agent OAuth 管理
