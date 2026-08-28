# OpenRouter 多账号代理（Go 版本 · Windows 单文件 exe）

> 零依赖！不需要 Python、不需要 Node.js、不需要任何运行时。
> 编译好的单文件 `proxy.exe` 6.7 MB，Windows 10/11 x64 双击即用。

## 文件

- `proxy.go` — Go 源码（编译前）
- `proxy-dashboard.html` — 嵌入的 Web Dashboard
- `proxy-windows-amd64.exe` — 已编译的 Windows 单文件
- `proxy.exe.README.md` — Windows 用户使用说明

## 编译

```bash
# Linux/WSL 下编译 Windows 版本
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o proxy-windows-amd64.exe proxy.go
```

## 用户文档

详见 `proxy.exe.README.md`。
