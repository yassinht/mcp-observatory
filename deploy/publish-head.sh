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

# A transient GitHub outage is not worth marking a census failed -- the
# observations are archived and signed either way. Being stuck for days is a
# different thing entirely, and it has now happened twice: heads kept being
# signed, pushes kept failing, and nothing anywhere said so. Silence is the
# failure mode that costs the most, because it is indistinguishable from
# working. So: tolerate a bad night, shout after two.
stuck_or_exit() {
  local oldest age days
  oldest=$(git log --format=%ct "@{u}..HEAD" 2>/dev/null | tail -1)
  [ -n "$oldest" ] || exit 0
  age=$(( $(date +%s) - oldest ))
  days=$(( age / 86400 ))
  if [ "$days" -ge 2 ]; then
    log "STUCK: signed heads have been unpublished for ${days} days"
    log "the log is still being written but nobody outside can verify it"
    exit 1   # surfaces as a failed unit in systemctl / journalctl
  fi
  log "will retry tomorrow (unpublished for ${days}d)"
  exit 0
}

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

cd "$CLONE" || exit 0

# Two writers push to this branch: the server every night, and a human from a
# laptop whenever the code changes. Without a rebase first, the very next
# nightly push is rejected as non-fast-forward and stays rejected until someone
# notices. Rebase rather than merge, so the published head history stays a
# straight line that is easy to audit.
git fetch -q origin 2>/dev/null && git rebase -q origin/HEAD 2>/dev/null || git rebase --abort 2>/dev/null || true

# Unpushed commits from a previous run whose push failed. The earlier version
# exited here whenever there was nothing new to copy, which meant a failed push
# was never retried -- it logged "the next run will retry" and then made that
# impossible. Four days of signed heads sat committed and unpublished.
unpushed=$(git rev-list --count @{u}..HEAD 2>/dev/null || echo 0)

if [ "$copied" -eq 0 ] && [ "$unpushed" -eq 0 ]; then
  log "nothing new to publish"
  exit 0
fi
if [ "$copied" -eq 0 ]; then
  log "no new heads, but $unpushed unpushed commit(s); retrying push"
  if git push -q origin HEAD 2>&1; then
    log "pushed $unpushed pending commit(s)"
    exit 0
  fi
  log "push still failing"
  stuck_or_exit
fi

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
  exit 0
fi
log "push failed; the commit is local"
stuck_or_exit
