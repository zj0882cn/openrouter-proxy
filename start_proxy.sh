#!/bin/bash
# Ollama 本地代理启动脚本
export PROXY_PORT=${PROXY_PORT:-8787}
export OLLAMA_HOST=${OLLAMA_HOST:-http://127.0.0.1:11434}
export DEFAULT_MODEL=${DEFAULT_MODEL:-qwen2.5-coder:3b}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 检查 Ollama 是否运行
if ! curl -s "$OLLAMA_HOST/api/tags" > /dev/null 2>&1; then
    echo "⚠️  Ollama 未运行，先启动 Ollama..."
    /workspace/ollama/start.sh
    sleep 3
fi

# 检查端口是否已被占用
if ss -tlnp 2>/dev/null | grep -q ":$PROXY_PORT "; then
    echo "⚠️  端口 $PROXY_PORT 已被占用，先停掉旧进程..."
    pkill -f "ollama_proxy.py.*$PROXY_PORT" 2>/dev/null
    sleep 2
fi

echo "🚀 启动 Ollama 代理 (端口 $PROXY_PORT)..."
cd "$SCRIPT_DIR"
python3 ollama_proxy.py