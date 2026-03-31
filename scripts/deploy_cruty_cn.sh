#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist/linux-x64"
REMOTE_HOST="cruty.cn"
REMOTE_DIR="/www/wwwroot/go-server"
REMOTE_SERVICE_FILE="/etc/systemd/system/pr-agent-go.service"
REMOTE_SERVICE_NAME="pr-agent-go"

"$ROOT_DIR/scripts/build_linux_x64.sh"

echo "[deploy] uploading release files to ${REMOTE_HOST}:${REMOTE_DIR}"
tar -C "$DIST_DIR" -cf - pr-agent-go .env.example README.md SHA256SUMS | \
  ssh "$REMOTE_HOST" "mkdir -p '$REMOTE_DIR' && tar -C '$REMOTE_DIR' -xf -"

echo "[deploy] installing systemd unit"
cat "$ROOT_DIR/deploy/pr-agent-go.service" | \
  ssh "$REMOTE_HOST" "cat > '$REMOTE_SERVICE_FILE'"

echo "[deploy] ensuring runtime env exists"
ssh "$REMOTE_HOST" "
  mkdir -p '$REMOTE_DIR/data' &&
  if [ ! -f '$REMOTE_DIR/.env' ]; then
    cp '$REMOTE_DIR/.env.example' '$REMOTE_DIR/.env'
  fi &&
  sed -i 's/^GITHUB_WEBHOOK_SECRET=replace-me$/GITHUB_WEBHOOK_SECRET=/' '$REMOTE_DIR/.env' &&
  sed -i 's/^GITHUB_TOKEN=ghp_replace_me$/GITHUB_TOKEN=/' '$REMOTE_DIR/.env' &&
  sed -i 's/^OPENAI_API_KEY=sk-replace-me$/OPENAI_API_KEY=/' '$REMOTE_DIR/.env' &&
  if grep -q '^PORT=' '$REMOTE_DIR/.env'; then
    sed -i 's/^PORT=.*/PORT=9000/' '$REMOTE_DIR/.env'
  else
    printf '\nPORT=9000\n' >> '$REMOTE_DIR/.env'
  fi
"

echo "[deploy] normalizing ownership and permissions"
ssh "$REMOTE_HOST" "
  chown -R root:root '$REMOTE_DIR' &&
  chmod 755 '$REMOTE_DIR' '$REMOTE_DIR/data' &&
  chmod 600 '$REMOTE_DIR/.env' &&
  chmod 644 '$REMOTE_DIR/.env.example' '$REMOTE_DIR/README.md' '$REMOTE_DIR/SHA256SUMS' &&
  chmod 755 '$REMOTE_DIR/pr-agent-go'
"

echo "[deploy] switching service state"
ssh "$REMOTE_HOST" "
  systemctl stop '$REMOTE_SERVICE_NAME' 2>/dev/null || true &&
  systemctl daemon-reload &&
  systemctl enable '$REMOTE_SERVICE_NAME' &&
  systemctl restart '$REMOTE_SERVICE_NAME' &&
  systemctl status '$REMOTE_SERVICE_NAME' --no-pager
"

echo "[deploy] done"
