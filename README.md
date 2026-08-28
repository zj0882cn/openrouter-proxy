# ORProxy · OpenRouter 多账号代理

> 🏷️ **版本**：v0.1-demo（Demo 第一版）
>
> Go 1.x · 单文件可执行 · 跨平台 · 内嵌 Web Dashboard
>
> 零运行时依赖（不需要 Python/Node）。

---

## ✨ 核心功能

| 功能 | 说明 |
|------|------|
| 🔌 **OpenAI 兼容 API** | 客户端只需把 `base_url` 指向 ORProxy，协议与 OpenAI 100% 兼容 |
| 🔑 **多 Key 轮询** | `OPENROUTER_API_KEY` 优先；`OPENROUTER2/3/4_API_KEY` 数字轮询 |
| 🔄 **智能换线** | 429 → 冷却该 Key（90s~数小时）；402/403 → 立即跳过；自动轮询下一条 |
| ⏸️ **Key 暂停/恢复** | Dashboard 一键暂停某条线，不改文件，重启后恢复 |
| ➕ **可视化增删 Key** | Dashboard 粘贴 → 自动写入 `orproxy-creds.yaml`，立即生效 |
| 🌐 **OpenRouter 免费 router** | `openrouter/free` 不消耗 Key 额度；失败自动 fallback 到原模型 |
| 🎁 **免费模型列表** | 自动从 OpenRouter `/v1/models` 抓取 0 价模型（381 个） |
| 💬 **在线测试台** | Dashboard 内置 Chat 测试面板，下拉选模型直接发请求 |
| 📋 **实时日志** | SSE 推送，浏览器直接看每次请求的 Key/状态/耗时/Token |
| ⚙️ **可视化配置编辑** | Dashboard 里直接改 `orproxy.yaml`，部分字段热更新 |
| 🩺 **健康检查** | `/health` 返回 Key 列表与冷却状态 |
| 🚪 **自动退出** | Dashboard 关闭后 N 秒自动退出代理（`auto_exit_sec`） |
| 🛡️ **远程认证** | `auth_token` 字段启用 `X-Auth-Token` 鉴权（远程访问） |

---

## 📁 文件清单

| 文件 | 说明 |
|------|------|
| `orproxy.go` | Go 源码（编译前） |
| `orproxy-dashboard.html` | 内嵌的 Web Dashboard（编译进 exe） |
| `orproxy.exe` | 已编译的 **Windows** 单文件（~10 MB） |
| `orproxy` | 已编译的 **Linux** 二进制 |
| `orproxy.yaml` | 主配置（首次运行自动生成） |
| `orproxy-creds.yaml` | API Key 配置（首次运行自动生成） |
| `proxy.exe.README.md` | Windows 用户使用说明（含 Key 申请、客户端配置示例） |
| `go.mod` / `go.sum` | Go 依赖（仅 `gopkg.in/yaml.v3`） |

---

## 🛠 编译

```bash
# Windows 单文件
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o orproxy.exe orproxy.go

# Linux
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o orproxy orproxy.go

# macOS（ARM64）
GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o orproxy orproxy.go
```

`-ldflags="-s -w"` 去掉符号表，文件更小。

---

## 🚀 快速开始

1. **双击 `orproxy.exe`**（或 `./orproxy`）
2. 同目录自动生成 `orproxy.yaml` + `orproxy-creds.yaml`
3. 编辑 `orproxy-creds.yaml`，填入你的 OpenRouter Key
4. 关闭并重新启动 `orproxy.exe`
5. 浏览器打开 `http://127.0.0.1:8787/`

启动后控制台输出类似：

```
[20:36:25] 🚀 启动: http://127.0.0.1:8787 → https://openrouter.ai
[20:36:25] 📄 凭据: C:\orproxy\orproxy-creds.yaml
[20:36:25] 📄 配置: C:\orproxy\orproxy.yaml
[20:36:25] 🩺 健康: http://127.0.0.1:8787/health
[20:36:25] 📊 面板: http://127.0.0.1:8787/
```

---

## ⚙️ 配置（`orproxy.yaml`）

```yaml
# 监听端口
port: "8787"

# 监听地址（127.0.0.1=仅本机；0.0.0.0=允许远程）
bind: "127.0.0.1"

# 上游服务器
upstream: "openrouter.ai"

# 免费模型刷新间隔（分钟）
refresh_minutes: 15

# Dashboard 关闭后多少秒自动退出（0=不退出）
auto_exit_sec: 0

# 远程认证 Token（空=不认证）
auth_token: ""

# 凭据文件路径
creds_path: "orproxy-creds.yaml"
```

> **环境变量始终优先于 yaml**：如 `PROXY_PORT=9000 ./orproxy` 强制用 9000。

### 配置热更新（v0.1 新增）

通过 Dashboard「⚙️ 配置 → 编辑配置文件」修改 yaml，**部分字段保存即生效**：

| 字段 | 热更新 |
|------|--------|
| `upstream` | ✅ 立即生效 |
| `bind` | ✅ 立即生效 |
| `auth_token` | ✅ 立即生效 |
| `port` | ❌ 需重启 |
| `refresh_minutes` | ❌ 需重启 |
| `auto_exit_sec` | ❌ 需重启 |
| `creds_path` | ❌ 需重启 |

每次保存会自动备份原文件为 `orproxy.yaml.bak`。

---

## 🌐 OpenRouter 免费 router（v0.1 新增）

OpenRouter 官方提供 `openrouter/free` 这个特殊 model id：

- **不消耗 Key 自己的额度**（OpenRouter 承担费用）
- 任何有效 Key 都能用
- 走的是 OpenRouter 自己精选的免费模型

### 自动触发

Dashboard 测试台里，从「免费模型」下拉选任何模型（自动包括 `openrouter/free`），请求**自动**带 `X-ORProxy-Free: 1` 头，代理自动把 model 重写到免费路径，**无需手动开关**。

### 自动 fallback

如果 `openrouter/free` 全部 Key 都失败（网络/限速），代理**自动用原始 model 再走一轮 key 轮询**，保证请求不丢。日志：

```
🌐 走免费 router（不消耗 key 额度）: openrouter/free
🔄 免费路由全部失败，fallback 到原始模型: openai/gpt-4o-mini
```

### 第三方客户端

客户端只需把 `model` 设为列表里的免费模型名称，ORProxy 自动识别为免费请求。也可以手动加头：

```http
POST /v1/chat/completions
X-ORProxy-Free: 1
{ "model": "openai/gpt-4o-mini", "messages": [...] }
```

→ ORProxy 把 `model` 改写为 `openrouter/free`，失败再 fallback。

---

## 🖥 Dashboard 功能

- **菜单**（左上，按钮）→ 打开侧边栏
- **🔑 Key 管理**：实时状态、暂停/恢复、删除、添加新 Key
- **⚙️ 配置**：在线编辑 `orproxy.yaml`（带热更新提示）
- **📖 轮询规则说明**
- **🔗 接入说明**：API 地址、健康检查、Python SDK 示例
- **💬 发送测试**：选模型 + 输问题 → 直接看响应（自动走免费 router）
- **📋 实时日志**：SSE 流，每次请求一行

---

## 🩺 健康检查

```bash
curl http://127.0.0.1:8787/health
```

```json
{
  "status": "ok",
  "upstream": "openrouter.ai",
  "keys": [
    {"key":"Key(abc123)","cooling_until":"-"},
    {"key":"Key1(def456)","cooling_until":"2026-08-28 22:30:00"}
  ]
}
```

---

## 📜 版本历史

- **v0.1-demo**（2026-08-28）— Demo 第一版
  - 多 Key 轮询 + 429/402/403 智能换线
  - 可视化增删/暂停 Key
  - Web Dashboard + SSE 实时日志
  - 在线测试台
  - `openrouter/free` 自动集成 + 失败 fallback
  - 配置文件可视化编辑 + 字段热更新
  - 健康检查 / 自动退出 / 远程认证

---

## 📚 客户端集成详细说明

详见 `proxy.exe.README.md`（Windows 用户视角，含 Key 申请、Python/JavaScript/curl 示例）。
