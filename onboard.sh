#!/bin/bash
# 简化 onboard — 只做机制 A + 最小事实拉取，避免多业务耦合
# 由系统级驱动维护；工作区自注册: AGENTS.md + .watch-state
# 不硬编码工作区；无复杂协议/规则读取
set -u
WS="${1:-}"  # 可选：传入工作区名用于标记，否则通用

# 机制 A: 先拉取世界状态（仅关键事实，不读全协议）
echo "=== 开工自检 · $WS · $(date -Iseconds) ==="
echo "[1/4] 公告栏 (git log 最近 5)"
git -C /workspace/dsh/huawei-devcontainer log --oneline -5 2>/dev/null || echo "(无 git)"

echo "[2/4] 测试服/关键状态自检（最简）"
echo "  test-tool DRIFT 开放: $(grep -oP '\d+ (?=条?开放)' /workspace/ws-hub/test-tool/DRIFT-VERIFY.md 2>/dev/null || echo 0)"
echo "  P-015 状态: $(grep -m1 '状态:' /workspace/ws-hub/mod-playerbot/PET_BAR_DESIGN-EXEC.md 2>/dev/null | sed 's/.*状态: //' || echo 无)"

echo "[3/4] 自驱动状态"
echo "  self-drive: $(cat /workspace/dsh/huawei-devcontainer/Ox-Alpha/.run/self-drive.pid 2>/dev/null || echo 无)"
echo "  proxy/monitor/waker: $(pgrep -a -f 'proxy\.py\|monitor_oxalpha\|waker_loop' | wc -l) 个存活"

echo "[4/4] 任务条件（最简判断）"
echo "  DECISIONS 开放: $(grep -c '\[ \]' /workspace/ws-hub/DECISIONS.md 2>/dev/null || echo 0)"
echo "  .watch-state: $(grep -c '=' /workspace/ws-hub/.watch-state 2>/dev/null || echo 0) 条目"

echo "=== 备忘 ==="
echo "  机制 A 已嵌入提示词首行; 刷新 = 拉取 (此输出); 不触发流程控制"
echo "  新工作区只需: 根目录 AGENTS.md + .watch-state 录入; 无需修改本脚本"
