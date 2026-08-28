# OpenRouter 多账号代理（Go 版本 · Windows 单文件 exe）

> 零依赖！不需要 Python、不需要 Node.js、不需要任何运行时。
> 编译好的单文件 `proxy.exe` 6.7 MB，Windows 10/11 x64 双击即用。

---

## 1. 准备凭据文件

把 `proxy.exe` 放到任意目录（比如 `C:\openrouter-proxy\`），
**同目录**放一个 `credentials.yaml`，格式：

```yaml
# 把这里 sk-or-v1-... 换成你自己的 key（去 openrouter.ai 创建）
OPENROUTER_API_KEY: sk-or-v1-你的key1
OPENROUTER1_API_KEY: sk-or-v1-你的key2
OPENROUTER2_API_KEY: sk-or-v1-你的key3
OPENROUTER4_API_KEY: sk-or-v1-你的key4
```

> 📌 **OPENROUTER_API_KEY** 是主 Key，**OPENROUTER1/2/4_API_KEY** 是备用 Key。
> 至少配 1 个就行，多账号是为了某条线 429 时自动换线。
> 推荐至少 3 个（与 Linux 服务器同款配置，方便日后对齐）。

---

## 2. 启动

**双击 `proxy.exe`** 或在 cmd 里：

```cmd
cd C:\openrouter-proxy
proxy.exe
```

看到类似输出就成功了：

```
[05:24:00] 🚀 启动: http://127.0.0.1:8787 → https://openrouter.ai
[05:24:00] 📄 凭据: credentials.yaml
[05:24:00] 🩺 健康: http://127.0.0.1:8787/health
```

⚠️ **第一次运行**可能被 Windows Defender 拦截（Go 编译的 exe 签名不常见），
点"更多信息 → 仍要运行"即可。

⚠️ **端口 8787 被占**（比如你本机已有别的代理/服务在用）：
```cmd
set PROXY_PORT=9000
proxy.exe
```
然后客户端 base URL 改成 `http://127.0.0.1:9000/v1`。

---

## 3. 自定义（环境变量）

| 变量 | 默认 | 说明 |
|------|------|------|
| `PROXY_PORT` | 8787 | 监听端口 |
| `CREDS_PATH` | `credentials.yaml` | 凭据文件路径（相对当前目录） |
| `UPSTREAM_HOST` | openrouter.ai | 上游域名 |
| `BIND_ADDR` | 127.0.0.1 | 监听地址（改为 0.0.0.0 可远程接入） |
| `AUTH_TOKEN` | （无） | 远程访问密钥（localhost 免验证） |
| `AUTO_EXIT_SEC` | 0（禁用） | Dashboard 关闭 N 秒后自动退出代理（0=不自动退出） |

PowerShell 设置示例：

```powershell
$env:PROXY_PORT=9000
$env:CREDS_PATH="D:\keys\my-creds.yaml"
.\proxy.exe
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

设置 `AUTO_EXIT_SEC` 后，Dashboard 页面会与代理保持心跳连接：

```powershell
# Dashboard 关闭后 30 秒自动退出代理
set AUTO_EXIT_SEC=30
proxy.exe
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

| 项目 | Linux（proxy.py） | Windows（proxy.exe） |
|------|-------------------|----------------------|
| 凭据路径 | `/root/.dsh/.credentials.yaml` | `proxy.exe` 同目录 `credentials.yaml` |
| 默认端口 | 8787 | 8787 |
| 路径前缀 | `/v1/...` → `/api/v1/...` | 同 |
| 429 冷却 | 90s/216min | 同 |
| 402/403 | 跳过不冷却 | 同 |
| 流式 | ✅ | ✅（标准 OpenAI 流式） |
| /health | ✅ | ✅ |

**完全兼容**，可以直接把 Linux 的凭据文件搬过来用。

---

## 7. 故障排查

- **端口被占**：改 `PROXY_PORT`（如 9000），客户端 base URL 也要改
- **连不上上游**：检查是否能 `ping openrouter.ai`；公司网络可能要设代理
- **Key 全部 429**：所有免费 key 当日额度用完，等到 UTC 0 点（北京时间 8 点）重置
- **想看请求日志**：代理 stdout 就是日志，重启会清空；想持久化用 `proxy.exe > proxy.log 2>&1`

---

## 8. 源码

`/workspace/dsh/huawei-devcontainer/Ox-Alpha/proxy.go`（一文件 ~430 行）
