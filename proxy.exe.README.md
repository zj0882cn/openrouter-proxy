# OpenRouter 多账号代理（Go 版本 · Windows 单文件 exe）

> 零依赖！不需要 Python、不需要 Node.js、不需要任何运行时。
> 编译好的单文件 `orproxy.exe` ~10 MB，Windows 10/11 x64 双击即用。

---

## 1. 首次运行（自动生成配置）

把 `orproxy.exe` 放到任意目录（比如 `C:\orproxy\`），双击运行。

**首次运行会自动生成两个 yaml**：
- `orproxy.yaml` — 主配置（端口/上游/认证等）
- `orproxy-creds.yaml` — API Key 列表

先编辑 `orproxy-creds.yaml` 填入你的 Key，再重启 `orproxy.exe` 即可。`orproxy.yaml` 默认配置开箱即用。

### Key 配置文件示例

```yaml
# OPENROUTER_API_KEY（无数字）优先级最高，会优先使用
OPENROUTER_API_KEY: sk-or-v1-你的key1

# OPENROUTER2/3/4_API_KEY 按数字顺序轮询
OPENROUTER2_API_KEY: sk-or-v1-你的key2
OPENROUTER3_API_KEY: sk-or-v1-你的key3
OPENROUTER4_API_KEY: sk-or-v1-你的key4
```

> 📌 **OPENROUTER_API_KEY** 优先级最高，其他按数字轮询。
> 至少配 1 个就行，多账号是为了某条线 429 时自动换线。
> 推荐至少 3 个（与 Linux 服务器同款配置，方便日后对齐）。

---

## 2. 启动

**双击 `orproxy.exe`** 或在 cmd 里：

```cmd
cd C:\orproxy
orproxy.exe
```

看到类似输出就成功了：

```
[05:24:00] 🚀 启动: http://127.0.0.1:8787 → https://openrouter.ai
[05:24:00] 📄 凭据: C:\orproxy\orproxy-creds.yaml
[05:24:00] 🩺 健康: http://127.0.0.1:8787/health
[05:24:00] 📊 面板: http://127.0.0.1:8787/
```

⚠️ **第一次运行**可能被 Windows Defender 拦截（Go 编译的 exe 签名不常见），
点"更多信息 → 仍要运行"即可。

⚠️ **端口 8787 被占**（比如你本机已有别的代理/服务在用）：
```cmd
set PROXY_PORT=9000
orproxy.exe
```
然后客户端 base URL 改成 `http://127.0.0.1:9000/v1`。

---

## 3. 自定义配置

所有配置项都在 `orproxy.yaml` 里，首次运行自动生成。也可以用环境变量临时覆盖（**环境变量优先于 yaml**）。

### orproxy.yaml 字段说明

| 字段 | 默认 | 说明 |
|------|------|------|
| `port` | "8787" | 监听端口 |
| `bind` | "127.0.0.1" | 监听地址（改为 "0.0.0.0" 可远程接入） |
| `upstream` | "openrouter.ai" | 上游服务器 |
| `refresh_minutes` | 15 | 免费模型刷新间隔（分钟） |
| `auto_exit_sec` | 0 | Dashboard 关闭后多少秒退出（0=不退出） |
| `auth_token` | "" | 远程认证 Token（空=不认证） |
| `creds_path` | "orproxy-creds.yaml" | 凭据文件路径（相对 exe 目录） |

### 环境变量覆盖

| 变量 | 对应 yaml 字段 |
|------|----------------|
| `PROXY_PORT` | port |
| `BIND_ADDR` | bind |
| `UPSTREAM_HOST` | upstream |
| `AUTO_EXIT_SEC` | auto_exit_sec |
| `AUTH_TOKEN` | auth_token |
| `CREDS_PATH` | creds_path |
| `CONFIG_PATH` | - （指定 yaml 配置文件本身） |

PowerShell 设置示例：

```powershell
$env:PROXY_PORT=9000
$env:CREDS_PATH="D:\keys\my-creds.yaml"
.\orproxy.exe
```

---

## 4. 可视化 Dashboard

浏览器直接打开代理地址：

```
http://127.0.0.1:8787/
```

功能：
- 🔑 **Key 状态卡片**：实时显示 Key1/Key2/Key4 是否在线、是否冷却中
- 🎁 **免费模型列表**：自动从 OpenRouter `/v1/models` 拉取定价为 $0 的模型，下拉选择
- 💬 **请求测试台**：在页面上直接发送对话请求，测试 Key 是否正常
- 📋 **实时日志**：每次请求自动记录（最多 200 条）
- 🚪 **自动退出**：Dashboard 关闭时自动退出代理（需设置 `AUTO_EXIT_SEC`，见下文）

> Dashboard 无需额外端口，和代理同端口、同地址。

### 自动退出功能

在 `orproxy.yaml` 里设置 `auto_exit_sec: 30` 即可（推荐写在 yaml 里更直观），也可用环境变量：

```powershell
# Dashboard 关闭后 30 秒自动退出代理
$env:AUTO_EXIT_SEC=30
.\orproxy.exe
```

- 代理启动时会显示 `💓 心跳检测已开启，Dashboard 关闭后 30 秒自动退出`
- 页面左上角会显示 `#OPEN` 心跳指示灯（绿=在线）
- Dashboard 关闭或浏览器离开页面时，代理会在指定秒数后自动退出
- 不想自动退出？`AUTO_EXIT_SEC` 留空或设为 0 即可

---

## 5. 健康检查

浏览器打开 [http://127.0.0.1:8787/health](http://127.0.0.1:8787/health)：

```json
{
  "status": "ok",
  "upstream": "openrouter.ai",
  "keys": [
    {"key":"Key(abc123)","cooling_until":"-"},
    {"key":"Key1(def456)","cooling_until":"-"},
    {"key":"Key2(ghi789)","cooling_until":"-"}
  ]
}
```

`cooling_until` 是冷却到几点（北京时间），`-` 表示可用。

---

## 6. 客户端接入

把客户端的 base URL 从 `https://openrouter.ai/api/v1` 改成：

```
http://127.0.0.1:8787/v1
```

**其它都不变**（路径、headers、API key 参数都透传）：

| 客户端 | 改哪里 |
|--------|--------|
| OpenAI Python | `openai.base_url = "http://127.0.0.1:8787/v1"` |
| OpenAI Node | `openai.baseURL = "http://127.0.0.1:8787/v1"` |
| LangChain | `base_url="http://127.0.0.1:8787/v1"` |
| Cursor / Cline / Continue | API Base URL: `http://127.0.0.1:8787/v1` |
| ChatBox / Cherry Studio | 自定义 OpenAI 兼容端点 |

---

## 7. 与 Linux 服务版对齐

| 项目 | Linux（proxy.py） | Windows（orproxy.exe） |
|------|-------------------|----------------------|
| 配置文件 | `/root/.dsh/credentials.yaml` | `orproxy.yaml` + `orproxy-creds.yaml` |
| Key 文件 | `credentials.yaml` | `orproxy-creds.yaml` |
| 默认端口 | 8787 | 8787 |
| 路径前缀 | `/v1/...` → `/api/v1/...` | 同 |
| 429 冷却 | 90s/216min | 同 |
| 402/403 | 跳过不冷却 | 同 |
| 流式 | ✅ | ✅（标准 OpenAI 流式） |
| /health | ✅ | ✅ |

**完全兼容**，Linux 凭据文件的 Key 可以直接复制到 `orproxy-creds.yaml`。

---

## 8. 故障排查

- **端口被占**：编辑 `orproxy.yaml` 的 `port:` 字段，或临时用 `set PROXY_PORT=9000`；客户端 base URL 也要改
- **连不上上游**：检查是否能 `ping openrouter.ai`；公司网络可能要设代理
- **Key 全部 429**：所有免费 key 当日额度用完，等到 UTC 0 点（北京时间 8 点）重置
- **想看请求日志**：代理 stdout 就是日志，重启会清空；想持久化用 `orproxy.exe > orproxy.log 2>&1`

---

## 9. 源码

`/workspace/dsh/huawei-devcontainer/Ox-Alpha/proxy.go`（一文件 ~430 行）
