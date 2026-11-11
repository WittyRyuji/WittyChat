#!/bin/sh
# Bootstrap the dev Garage node: layout, bucket, fixed dev key.
# Run once after `docker compose -f deploy/docker-compose.dev.yml up -d garage`:
#   sh deploy/garage/bootstrap.sh
# Idempotent: every step is a no-op when already applied.
#
# 初始化开发用 Garage 节点：layout、桶、固定开发密钥。
# 在 compose 起 garage 后执行一次；各步均可重复执行。
set -e
CT=${GARAGE_CT:-deploy-garage-1}
G="docker exec -i $CT /garage -c /etc/garage.toml"

echo "== layout =="
NODE=$($G status 2>/dev/null | awk 'NR>2 {print $1; exit}' | tr -d '\r')
if [ -z "$NODE" ]; then
  echo "waiting for garage..."
  for i in $(seq 1 30); do
    sleep 1
    NODE=$($G status 2>/dev/null | awk 'NR>2 {print $1; exit}' | tr -d '\r')
    [ -n "$NODE" ] && break
  done
fi
echo "node: $NODE"
$G layout assign "$NODE" -z dc1 -c 1G 2>/dev/null || true
$G layout apply --version 1 2>/dev/null || true

echo "== bucket =="
$G bucket create wittychat 2>/dev/null || true
$G bucket info wittychat | head -3

echo "== dev key =="
$G key import -n devkey GKDEV0DEV0DEV0DEV0DEV0 devsecret_devsecret_devsecret_012345 --yes 2>/dev/null || true
$G bucket allow wittychat --read --write --owner --key GKDEV0DEV0DEV0DEV0DEV0 2>/dev/null || true
$G key info GKDEV0DEV0DEV0DEV0DEV0 | head -6
echo "done. Access key: GKDEV0DEV0DEV0DEV0DEV0 / devsecret_devsecret_devsecret_012345"
