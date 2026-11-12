#!/bin/sh
# Bootstrap the dev Garage node: layout, bucket and locally generated key.
# Run once after `docker compose --env-file deploy/dev.env -f deploy/docker-compose.dev.yml up -d garage`:
#   sh deploy/garage/bootstrap.sh
# Idempotent: every step is a no-op when already applied.
#
# 初始化开发用 Garage 节点：layout、桶、本地生成密钥。
# 在 compose 起 garage 后执行一次；各步均可重复执行。
set -e
ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
ENV_FILE="$ROOT/deploy/dev.env"
if [ ! -f "$ENV_FILE" ]; then
  echo "missing $ENV_FILE — run: python scripts/gen_dev_env.py" >&2
  exit 1
fi
set -a
. "$ENV_FILE"
set +a
: "${WITTYCHAT_GARAGE_ACCESS_KEY:?missing in deploy/dev.env}"
: "${WITTYCHAT_GARAGE_SECRET_KEY:?missing in deploy/dev.env}"
CT=${GARAGE_CT:-deploy-garage-1}
garage_cli() {
  # Keep Git Bash/MSYS from rewriting container paths to Windows paths.
  MSYS2_ARG_CONV_EXCL='*' docker exec -i "$CT" /garage -c /etc/garage.toml "$@"
}

echo "== layout =="
NODE=$(garage_cli status 2>/dev/null | awk 'NR>2 {print $1; exit}' | tr -d '\r')
if [ -z "$NODE" ]; then
  echo "waiting for garage..."
  for i in $(seq 1 30); do
    sleep 1
    NODE=$(garage_cli status 2>/dev/null | awk 'NR>2 {print $1; exit}' | tr -d '\r')
    [ -n "$NODE" ] && break
  done
fi
if [ -z "$NODE" ]; then
  echo "garage did not become ready" >&2
  exit 1
fi
echo "node: $NODE"
garage_cli layout assign "$NODE" -z dc1 -c 1G 2>/dev/null || true
garage_cli layout apply --version 1 2>/dev/null || true

echo "== bucket =="
garage_cli bucket create wittychat 2>/dev/null || true
garage_cli bucket info wittychat | head -3

echo "== dev key =="
garage_cli key import -n devkey "$WITTYCHAT_GARAGE_ACCESS_KEY" "$WITTYCHAT_GARAGE_SECRET_KEY" --yes 2>/dev/null || true
garage_cli bucket allow wittychat --read --write --owner --key "$WITTYCHAT_GARAGE_ACCESS_KEY" 2>/dev/null || true
garage_cli key info "$WITTYCHAT_GARAGE_ACCESS_KEY" | head -6
echo "done. S3 credentials are in deploy/dev.env (gitignored)"
