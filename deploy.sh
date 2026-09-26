#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="article-store.service"
BINARY="article-store"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"

# schema.sql creates an FTS5 virtual table and mattn/go-sqlite3 only compiles
# FTS5 in when asked. Without this tag the build succeeds and the service dies at
# boot with "no such module: fts5".
GO_TAGS="sqlite_fts5"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test -tags "$GO_TAGS" ./...

echo "==> Building $BINARY (tags: $GO_TAGS)..."
go build -tags "$GO_TAGS" -o "$BINARY" ./cmd/article-store
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"

# Provenance. Asserted here, BEFORE the unit install and before the service is
# stopped -- not after the binary lands. `go build` writes no VCS stamp when it
# cannot find a .git DIRECTORY, and it does not fail when that happens, not even
# with -buildvcs=true; the usual cause is building from a git worktree, whose
# .git is a pointer file. Such a binary compiles perfectly and reads clean in the
# log, so checking later would stop the live service, install an untraceable
# binary, and only then tell us it cannot be traced to a commit.
echo "==> Checking provenance..."
buildinfo="$(go version -m "$BINARY")"
vcs_revision="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
vcs_modified="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.modified$/ {print $2}')"
if [ -z "$vcs_revision" ]; then
  echo "    REFUSING TO INSTALL: this binary carries no vcs.revision, so nothing can tie" >&2
  echo "    it back to a commit. Build from a real clone or checkout, not a worktree." >&2
  exit 1
fi
echo "    vcs.revision=$vcs_revision"
if [ "$vcs_modified" = "true" ]; then
  echo "    WARNING: built from a DIRTY tree (vcs.modified=true). $vcs_revision names the" >&2
  echo "    commit this binary was built NEAR, not the source it was built FROM, and that" >&2
  echo "    source is not recoverable from any commit. Commit first for a reproducible build." >&2
fi

# A set ARTICLE_STORE_ variable that settings.go does not declare stops the new
# binary at boot. Ask before the old one is stopped: build the registry from the
# running service's own environment. The test prints a verdict, never a value.
echo "==> Checking the running service's environment against the declared settings..."
live_pid="$(systemctl --user show -p MainPID --value "$SERVICE")"
if [ -n "$live_pid" ] && [ "$live_pid" != "0" ]; then
  go test -tags "$GO_TAGS" -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' . -args -live-environment-file="/proc/$live_pid/environ"
else
  echo "    $SERVICE is not running, so there is no environment to check"
fi

echo "==> Installing systemd unit..."
mkdir -p "$(dirname "$UNIT_DEST")"
cp "$UNIT_SRC" "$UNIT_DEST"

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binary to $BIN_DIR..."
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"

echo "==> Starting $SERVICE..."
systemctl --user daemon-reload
systemctl --user enable "$SERVICE" >/dev/null
systemctl --user start "$SERVICE"

echo "==> Verifying..."
sleep 2
if systemctl --user is-active --quiet "$SERVICE"; then
  echo "    $SERVICE is running"
  journalctl --user -u "$SERVICE" -n 5 --no-pager 2>&1 | grep -v '^--' || true
else
  echo "ERROR: $SERVICE failed to start"
  journalctl --user -u "$SERVICE" -n 20 --no-pager 2>&1
  exit 1
fi

# A running process is not a working one. FTS5 is the thing most likely to be
# missing from a binary that otherwise starts, so prove the search path answers
# before calling the deploy done.
echo "==> Smoke-checking the API..."
# The address comes from the unit, the one place it is set.
ADDR="$(sed -n 's/^Environment=ARTICLE_STORE_ADDR=//p' "$UNIT_SRC")"
[ -n "$ADDR" ] || { echo "ERROR: $UNIT_SRC sets no ARTICLE_STORE_ADDR"; exit 1; }
case "$ADDR" in :*) ADDR="localhost$ADDR" ;; esac
BASE="http://$ADDR"
curl -sfS "$BASE/health" >/dev/null || { echo "ERROR: /health did not answer"; exit 1; }
curl -sfS "$BASE/articles?q=article" >/dev/null || {
  echo "ERROR: full-text search did not answer — was this built with -tags $GO_TAGS?"
  exit 1
}
echo "    /health and full-text search both answered"

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
