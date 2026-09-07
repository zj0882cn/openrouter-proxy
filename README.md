# ORProxy · OpenRouter 多账号代理

> 🏷️ **版本**：v1.3.0
>
> Go 单文件 · 跨平台 · 零运行时依赖（不需要 Python/Node）
>
> 多 Key 轮询 + 智能换线 + Web Dashboard

---

## ✨ 核心功能

| 功能 | 说明 |
|------|------|
| 🔌 **OpenAI 兼容 API** | 客户端只需把 `base_url` 指向 ORProxy，协议与 OpenAI 100% 兼容 |
| 🔑 **多 Key 轮询** | `OPENROUTER_API_KEY` 优先；`OPENROUTER2/3/4_API_KEY` 数字轮询 |
| 🔄 **智能换线** | 429 → 冷却 10s；401 → 冷却 24h；403 → 冷却 1h；自动轮询下一条 |
| 🌐 **OpenRouter 免费池** | `openrouter/free` 不消耗 Key 额度；失败自动 fallback 到原模型 |
| 🩺 **健康检查** | `/health` 返回 Key 列表、冷却状态、各池成功率统计 |
| 🛡️ **远程认证** | 支持 `X-Auth-Token` / `Authorization: Bearer` / `Proxy-Authorization` / `?token=` |
| 📊 **6 阶段日志** | 毫秒级时间戳，客户端 ID 跟踪，日志按级别分离到文件 |
| 🎯 **智谱 Zhipu 备选** | Key 全失败时自动 fallback 到 `glm-4-flash` / `glm-5-turbo` |

---

## 📁 文件清单

| 文件 | 说明 |
|------|------|
| `orproxy.go` | Go 源码 |
| `orproxy` | 已编译的 **Linux** 二进制 |
| `orproxy.exe` | 已编译的 **Windows** 单文件（需自行编译） |
| `orproxy.yaml` | 主配置文件 |
| `orproxy-creds.yaml` | API Key 凭据文件 |
| `orproxy-dashboard.html` | Web Dashboard（内嵌到二进制） |
| `go.mod` / `go.sum` | Go 依赖（仅 `gopkg.in/yaml.v3`） |

---

## 🚀 快速开始

### 下载或编译

```bash
# Linux
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o orproxy orproxy.go

# Windows
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o orproxy.exe orproxy.go

# macOS
GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o orproxy orproxy.go
```

### 运行

```bash
# Linux
./orproxy

# Windows
orproxy.exe
```

首次运行会自动生成 `orproxy.yaml` 和 `orproxy-creds.yaml`。

### 配置 Key

编辑 `orproxy-creds.yaml`，填入你的 OpenRouter API Key：

```yaml
OPENROUTER_API_KEY: sk-or-v1-你的key1
OPENROUTER2_API_KEY: sk-or-v1-你的key2
OPENROUTER3_API_KEY: sk-or-v1-你的key3
```

`OPENROUTER_API_KEY`（无数字）优先级最高，其他按数字顺序轮询。推荐至少 3 个 Key。

### 启动

```bash
# Linux
./orproxy

# Windows (cmd)
orproxy.exe

# Windows (PowerShell)
.\orproxy.exe
```

启动后看到类似输出：

```
[20:36:25] 🚀 启动: http://127.0.0.1:8787 → https://openrouter.ai
[20:36:25] 📄 凭据: orproxy-creds.yaml
[20:36:25] 🩺 健康: http://127.0.0.1:8787/health
```

浏览器打开 `http://127.0.0.1:8787/` 访问 Dashboard。

---

## ⚙️ 配置

### `orproxy.yaml`

```yaml
# 监听端口
port: "8787"

# 监听地址（127.0.0.1=仅本机；0.0.0.0=允许远程访问）
bind: "0.0.0.0"

# 上游服务器
upstream: "openrouter.ai"

# 免费模型列表刷新间隔（分钟）
refresh_minutes: 15

# 自动退出：Dashboard 关闭后多少秒退出（0=不自动退出）
auto_exit_sec: 0

# 远程认证 Token（留空则不认证）
auth_token: ""

# 凭据文件路径
creds_path: "orproxy-creds.yaml"

# ===== 重试/限速参数 =====
cooldown_429_sec: 10       # 429 冷却时间（秒）
cooldown_401_sec: 86400    # 401 冷却时间（24h）
cooldown_403_sec: 3600     # 403 冷却时间（1h）
per_key_delay_sec: 6       # 同一 key 最小调用间隔（秒）
wait_retry_sec: 3          # 所有 key 冷却时，等待重试间隔（秒）
wait_max_sec: 30           # 最大等待时间（秒）
between_rounds_sec: 10     # 两轮重试之间的 sleep 时间（秒）
```

> **环境变量优先于 yaml**：如 `PROXY_PORT=9000 ./orproxy` 强制用 9000 端口。

### 环境变量覆盖

| 变量 | 对应 yaml 字段 |
|------|----------------|
| `PROXY_PORT` | port |
| `BIND_ADDR` | bind |
| `UPSTREAM_HOST` | upstream |
| `AUTO_EXIT_SEC` | auto_exit_sec |
| `AUTH_TOKEN` | auth_token |
| `CREDS_PATH` | creds_path |
| `CONFIG_PATH` | 指定 yaml 配置文件路径 |

### `orproxy-creds.yaml`

```yaml
# OPENROUTER_API_KEY（无数字）优先级最高
OPENROUTER_API_KEY: sk-or-v1-你的key1
OPENROUTER2_API_KEY: sk-or-v1-你的key2
OPENROUTER3_API_KEY: sk-or-v1-你的key3
```

---

## 🔐 认证方式

远程访问时（`bind: 0.0.0.0` 且 `auth_token` 非空），支持以下方式：

| 方式 | 示例 |
|------|------|
| `X-Auth-Token` 头 | `X-Auth-Token: your-token` |
| URL Query 参数 | `?token=your-token` |
| `Authorization: Bearer` | `Authorization: Bearer your-token` |
| `Authorization: Basic` | `Authorization: Basic <base64(token)>` |
| `Proxy-Authorization: Bearer` | `Proxy-Authorization: Bearer your-token` |
| `Proxy-Authorization: Basic` | `Proxy-Authorization: Basic <base64(token)>` |

> 本机访问（`127.0.0.1` / `localhost`）始终免认证。

---

## 🩺 健康检查

```bash
curl http://127.0.0.1:8787/health
```

```json
{
  "status": "ok",
  "upstream": "openrouter.ai",
  "version": "v1.3.0",
  "keys": [
    {"key":"...abc123","cooling_until":"-"},
    {"key":"...def456","cooling_until":"2026-09-07 22:30:00"}
  ],
  "daily_total": 42,
  "daily_success": 38,
  "daily_fail": 4,
  "daily_retry": 7
}
```

`cooling_until` 为冷却到期时间，`-` 表示可用。

---

## 📊 请求处理流程

```
客户端请求 → ORProxy
  ├─ 1️⃣ OpenRouter Free（需 X-ORProxy-Free: 1）
  ├─ 2️⃣ OpenRouter Key 轮询（按优先级 + 冷却状态）
  └─ 3️⃣ Zhipu 备选（glm-4-flash / glm-5-turbo）
         ↓
  ┌─ 成功 → 返回响应
  └─ 失败 → 重试（最多 3 轮，间隔递增 2s/4s/8s...）
```

---

## 🖥 Dashboard

浏览器打开 `http://127.0.0.1:8787/`：

- 🔑 **Key 状态卡片** — 实时显示各 Key 是否在线、冷却中
- 🎁 **免费模型列表** — 自动拉取 OpenRouter 定价为 $0 的模型
- 💬 **请求测试台** — 选模型直接发请求测试
- 📋 **实时日志** — SSE 推送，每次请求一行
- ⚙️ **配置编辑** — 在线编辑 `orproxy.yaml`

---

## 🧩 客户端接入

把客户端的 base URL 改为 `http://127.0.0.1:8787/v1`，其它不变：

| 客户端 | 配置 |
|--------|------|
| OpenAI Python | `openai.base_url = "http://127.0.0.1:8787/v1"` |
| OpenAI Node.js | `openai.baseURL = "http://127.0.0.1:8787/v1"` |
| Cursor / Cline / Continue | API Base URL: `http://127.0.0.1:8787/v1` |
| ChatBox / Cherry Studio | 自定义 OpenAI 兼容端点 |

---

## 🛠 故障排查

| 问题 | 解决方法 |
|------|---------|
| 端口被占 | 改 `orproxy.yaml` 的 `port`，或设 `PROXY_PORT=9000` |
| Key 全部 429 | 当日额度用完，UTC 0 点（北京时间 8 点）重置 |
| 连不上上游 | 检查 `ping openrouter.ai`；内网需配代理 |
| 想看日志 | 启动时 `./orproxy 2>&1 \| tee proxy.log` |

---

## 📜 版本历史

- **v1.3.0**（2026-09-07）
  - 6 阶段日志（毫秒级时间戳 + 客户端 ID 跟踪）
  - 修复 IPv6 双栈导致的 `ERR_CONNECTION_RESET`（改用 `tcp4` 监听）
  - 新增 `Authorization: Bearer` / `Proxy-Authorization` 认证
  - 日志文件分离：`info.log` / `error.log` / `debug.log`
  - Zhipu 超时 20s → 60s，冷却 1s → 6s
  - 完整请求/响应包调试日志

- **v0.1**（2026-08-28）
  - 多 Key 轮询 + 429/402/403 智能换线
  - Web Dashboard + SSE 实时日志
  - `openrouter/free` 自动集成 + 失败 fallback
  - 健康检查 / 远程认证