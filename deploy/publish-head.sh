#!/usr/bin/env bash
# Publishes today's signed tree head to the public repository.
#
# Run after each census, from the systemd unit. A log whose heads never leave
# the machine that produced them is not a transparency log: the whole claim
# rests on other people holding yesterday's head, so publishing is not a
# convenience step, it is the part that makes the rest mean anything.
#
# Failure here must never mark the census failed -- the observations are already
# archived and signed, and GitHub being unreachable at 04:00 is not a reason to
# report that the crawl did not happen. The unit calls this with a leading '-'
# for exactly that reason, and it exits 0 on anything it cannot control.
set -uo pipefail

APP_DIR=${APP_DIR:-/opt/mcp-observatory}
HEADS_DIR="$APP_DIR/heads"
CLONE="$APP_DIR/publish"

log() { echo "publish-head: $*"; }

[ -d "$CLONE/.git" ] || { log "no clone at $CLONE; skipping"; exit 0; }

# Only ever copy the public artefacts. The private key lives beside them and
# must never be handed to a command that pushes to a public remote, so this
# names the files explicitly instead of copying the directory.
mkdir -p "$CLONE/heads"
copied=0
for f in "$HEADS_DIR"/*.txt "$HEADS_DIR/key.pub"; do
  [ -e "$f" ] || continue
  case "$(basename "$f")" in
    key|*.key) continue ;;   # belt and braces
  esac
  if ! cmp -s "$f" "$CLONE/heads/$(basename "$f")"; then
    cp "$f" "$CLONE/heads/"
    copied=$((copied + 1))
  fi
done

if [ "$copied" -eq 0 ]; then
  log "nothing new to publish"
  exit 0
fi

cd "$CLONE" || exit 0

# Refuse outright if the private key somehow reached the clone. Better to
# publish nothing today than to publish the key once.
if git ls-files --error-unmatch heads/key >/dev/null 2>&1; then
  log "REFUSING: heads/key is tracked in the clone"
  exit 0
fi

git add heads/ || exit 0
if git diff --cached --quiet; then
  log "no staged changes"
  exit 0
fi

size=$(grep -h '^size ' heads/*.txt 2>/dev/null | tail -1 | awk '{print $2}')
git commit -q -m "Publish tree head $(date -u +%Y-%m-%d) (${size:-?} observations)" || exit 0

if git push -q origin HEAD 2>&1; then
  log "published $copied file(s), tree size ${size:-?}"
else
  log "push failed; the commit is local and the next run will retry"
fi
exit 0
