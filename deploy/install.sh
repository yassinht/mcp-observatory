#!/usr/bin/env bash
# Installs the MCP Observatory crawler as a daily systemd timer.
#
# Run as root on the server, from the directory holding the uploaded files:
#   sudo bash install.sh
#
# Idempotent: safe to re-run after uploading a new binary.
set -euo pipefail

APP_DIR=/opt/mcp-observatory
SVC_USER=mcpobs

need() { [ -f "$1" ] || { echo "missing file: $1" >&2; exit 1; }; }
need crawler
need mcpobs
need mcpobs.service
need mcpobs.timer

# This box may already be running things that matter. Refuse to install rather
# than collide with anything: every name used here must be unused.
for unit in mcpobs.service mcpobs.timer; do
  if [ -e "/etc/systemd/system/$unit" ] && [ "${FORCE:-}" != "1" ]; then
    echo "refusing: /etc/systemd/system/$unit already exists." >&2
    echo "inspect it first; re-run with FORCE=1 only if it is this project's." >&2
    exit 1
  fi
done
if [ -e "$APP_DIR" ] && [ ! -f "$APP_DIR/crawler" ] && [ "${FORCE:-}" != "1" ]; then
  echo "refusing: $APP_DIR exists but holds no crawler -- something else owns it." >&2
  exit 1
fi
if id "$SVC_USER" >/dev/null 2>&1 && [ "$(id -u "$SVC_USER")" -ge 1000 ]; then
  echo "refusing: user '$SVC_USER' exists as a regular login account." >&2
  exit 1
fi

# A service account with no shell and no home: the crawler never needs to log in.
if ! id "$SVC_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
  echo "created service user $SVC_USER"
fi

install -d -o "$SVC_USER" -g "$SVC_USER" "$APP_DIR" "$APP_DIR/data"
install -m 0755 crawler mcpobs "$APP_DIR/"
install -m 0644 mcpobs.service mcpobs.timer /etc/systemd/system/

systemctl daemon-reload
systemctl enable --now mcpobs.timer

echo
echo "installed. next scheduled run:"
systemctl list-timers mcpobs.timer --no-pager || true
echo
echo "run one now:      sudo systemctl start mcpobs.service"
echo "watch it:         sudo journalctl -u mcpobs.service -f"
echo "read the log:     sudo -u $SVC_USER $APP_DIR/mcpobs --data $APP_DIR/data runs"
