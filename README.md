# OpenRouter 多账号代理（Go 版本 · Windows 单文件 exe）

> 零依赖！不需要 Python、不需要 Node.js、不需要任何运行时。
> 编译好的单文件 `orproxy.exe` ~10 MB，Windows 10/11 x64 双击即用。

## 文件

- `orproxy.go` — Go 源码（编译前）
- `orproxy-dashboard.html` — 嵌入的 Web Dashboard（编译进 exe，无需外部文件）
- `orproxy.exe` — 已编译的 Windows 单文件
- `orproxy.yaml` — 主配置（端口/上游/认证等，首次运行自动生成）
- `orproxy-creds.yaml` — API Key 配置（首次运行自动生成）
- `proxy.exe.README.md` — Windows 用户使用说明

## 编译

```bash
# Linux/WSL 下编译 Windows 版本
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o orproxy.exe orproxy.go
```

## 首次运行

1. 把 `orproxy.exe` 放到任意目录（建议 `C:\orproxy\`）
2. 双击运行
3. 同目录自动生成 `orproxy.yaml`（主配置）和 `orproxy-creds.yaml`（Key 列表）
4. 编辑 `orproxy-creds.yaml` 填入 API Key
5. 关闭并重新启动 `orproxy.exe`
6. 浏览器打开 `http://127.0.0.1:8787/` 使用 Dashboard

## 配置文件

### `orproxy.yaml`（主配置，首次启动自动生成）

```yaml
port: "8787"                # 监听端口
bind: "127.0.0.1"           # 监听地址（0.0.0.0 允许远程）
upstream: "openrouter.ai"   # 上游服务器
refresh_minutes: 15         # 免费模型刷新间隔
auto_exit_sec: 0            # Dashboard 关闭后多少秒自动退出（0=不退出）
auth_token: ""               # 远程认证 Token（空=不认证）
creds_path: "orproxy-creds.yaml"  # 凭据文件路径
```

> **环境变量始终优先于 yaml 配置**。如 `PROXY_PORT=9000 orproxy.exe` 会强制用 9000。

### `orproxy-creds.yaml`（API Key 配置）

```yaml
# OPENROUTER_API_KEY（无数字）优先级最高
OPENROUTER_API_KEY: sk-or-v1-xxxx

# OPENROUTER2/3/4_API_KEY 按数字顺序轮询
OPENROUTER2_API_KEY: sk-or-v1-yyyy
OPENROUTER3_API_KEY: sk-or-v1-zzzz
```

> 修改 yaml 后需重启 `orproxy.exe` 才生效。Dashboard 里新增/暂停/删除 Key 会立即更新 `orproxy-creds.yaml`，无需重启。

## 端口冲突

直接编辑 `orproxy.yaml` 的 `port:` 字段，或临时用环境变量：

```cmd
set PROXY_PORT=9000
orproxy.exe
```

## 用户文档

详见 `proxy.exe.README.md`。
