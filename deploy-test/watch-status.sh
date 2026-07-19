#!/bin/bash
# 真机验证：触发 echo_server 部署，逐秒采样 deploy 状态直到终态，
# 打印每次状态变化（时间偏移 + 状态），验证 queued→dispatching→running→succeeded 全程可见。
set -euo pipefail
cd /opt/apps/hookploy_test
resp=$(curl -sS -X POST http://127.0.0.1:9180/hooks/echo_server \
  -H "Authorization: Bearer $(cat .echo_token)" \
  -H "Content-Type: application/json" -d '{}')
id=$(echo "$resp" | sed -n 's/.*"deploy_id":"\([^"]*\)".*/\1/p')
echo "deploy_id: $id"

export HOOKPLOY_URL=http://127.0.0.1:9180 HOOKPLOY_ADMIN_TOKEN=$(cat .admin_token)
start=$(date +%s)
last=""
while true; do
    st=$(./hookploy deploys echo_server | head -1 | awk '{print $2}')
    if [ "$st" != "$last" ]; then
        echo "+$(( $(date +%s) - start ))s  $st"
        last="$st"
    fi
    case "$st" in succeeded|failed|unreachable) break;; esac
    sleep 1
done
