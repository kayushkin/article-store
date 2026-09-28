#!/usr/bin/env bash
# Boot-and-answer smoke test for the article radar: builds, starts the store on
# a throwaway port and data dir, walks the source and suggestion routes, then
# drives the real scripts/article-radar-dispatch.sh against that store with
# stub session runners, and tears down. No model call, no network, and the
# real ~/.config/article-store is never touched.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
PORT="${SMOKE_PORT:-18318}"
DATA_DIR="$(mktemp -d)"
DISPATCH_DIR="$(mktemp -d)"
BINARY="$DISPATCH_DIR/article-store-smoke"
BASE="http://127.0.0.1:${PORT}"

cleanup() {
  [[ -n "${SERVER_PID:-}" ]] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$DATA_DIR" "$DISPATCH_DIR"
}
trap cleanup EXIT

pass() { echo "    ok: $1"; }
fail() { echo "FAIL: $1"; exit 1; }
field() { # $1 = json, $2 = field
  python3 -c "import json,sys; print(json.load(sys.stdin)['$2'])" <<<"$1"
}
post() { # $1 = path, $2 = body; prints the body, then the status on its own line
  curl -sS -X POST "$BASE$1" -H 'Content-Type: application/json' -d "$2" -w '\n%{http_code}'
}

echo "==> build"
go build -tags sqlite_fts5 -o "$BINARY" ./cmd/article-store

echo "==> start on $PORT (data=$DATA_DIR)"
ARTICLE_STORE_ADDR="127.0.0.1:${PORT}" ARTICLE_STORE_DATA_DIR="$DATA_DIR" "$BINARY" &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl -sf "$BASE/health" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -sf "$BASE/health" | grep -q '"status":"ok"' && pass health || fail health

echo "==> sources"
OUT=$(post /sources '{"kind":"research","prompt":"housing supply economics"}')
[[ "${OUT##*$'\n'}" == "201" ]] || fail "create research source: $OUT"
RESEARCH=$(field "${OUT%$'\n'*}" id)
[[ "$(field "${OUT%$'\n'*}" status)" == "proposed" ]] && pass "a blank status is proposed" || fail "status: $OUT"
curl -sf "$BASE/sources?due=1" | grep -q "$RESEARCH" && fail "a proposed source is due" || pass "a proposed source is not due"
OUT=$(post /sources '{"kind":"scout"}')
[[ "${OUT##*$'\n'}" == "400" ]] && pass "a scout with no prompt is 400" || fail "scout without prompt: $OUT"
OUT=$(post /sources '{"kind":"watch","platform":"substack","base_url":"https://radar-smoke.invalid","name":"Example"}')
WATCH=$(field "${OUT%$'\n'*}" id)
OUT=$(post /sources '{"kind":"scout","prompt":"economics blogs","status":"active"}')
SCOUT=$(field "${OUT%$'\n'*}" id)
curl -sfS -X PATCH "$BASE/sources/$RESEARCH" -H 'Content-Type: application/json' -d '{"status":"active"}' >/dev/null

echo "==> suggestions"
OUT=$(post /suggestions "{\"url\":\"https://example.com/essay\",\"reason\":\"a careful essay\",\"source_id\":\"$RESEARCH\"}")
[[ "${OUT##*$'\n'}" == "201" ]] && pass "suggest" || fail "suggest: $OUT"
SUGGESTION=$(field "${OUT%$'\n'*}" id)
OUT=$(post /suggestions '{"url":"https://example.com/essay#comments","reason":"again"}')
[[ "${OUT##*$'\n'}" == "200" && "$(field "${OUT%$'\n'*}" id)" == "$SUGGESTION" ]] && pass "the same URL is not suggested twice" || fail "second suggestion: $OUT"
OUT=$(post "/suggestions/$SUGGESTION/dismiss" '')
[[ "$(field "${OUT%$'\n'*}" status)" == "dismissed" ]] && pass "dismiss" || fail "dismiss: $OUT"

echo "==> the dispatcher works only research and scout, and grades on what ran"
# The watch is approved too, so a due watch is in the store's due list; the
# dispatcher must leave it to the store's own watcher.
curl -sfS -X PATCH "$BASE/sources/$WATCH" -H 'Content-Type: application/json' -d '{"status":"active"}' >/dev/null
# Stop the in-process watcher from running the watch in the middle of the
# dispatch checks: it would ask radar-smoke.invalid, a name that never
# resolves, and the smoke should not depend on that.
curl -sfS -X PATCH "$BASE/sources/$WATCH" -H 'Content-Type: application/json' -d '{"enabled":false}' >/dev/null
STUB_LOG="$DISPATCH_DIR/prompt.txt"
write_stub() { # $1 = ids to mark ran, space separated, may be empty
  cat >"$DISPATCH_DIR/claude-stub" <<STUB
#!/usr/bin/env bash
# Stub session runner, invoked as \`-p <prompt> --model …\`: record the prompt,
# mark the ids this stub was built to mark.
printf '%s' "\$2" >"$STUB_LOG"
for id in $1; do curl -sfS -X POST "$BASE/sources/\$id/ran" -H 'Content-Type: application/json' -d '{"result":"stub"}' >/dev/null; done
STUB
  chmod +x "$DISPATCH_DIR/claude-stub"
}
run_dispatch() {
  ARTICLE_RADAR_STORE_URL="$BASE" ARTICLE_RADAR_CLAUDE_BIN="$DISPATCH_DIR/claude-stub" \
    bash "$REPO_DIR/scripts/article-radar-dispatch.sh" 2>&1
}

write_stub ""
OUT=$(run_dispatch) && CODE=0 || CODE=$?
[[ "$CODE" != "0" ]] && pass "a session that marked nothing fails the run" || fail "reported success after marking nothing: $OUT"
grep -q 'marked NOTHING' <<<"$OUT" && pass "the failure says what went wrong" || fail "failure text: $OUT"
grep -q "\"id\": \"$RESEARCH\"" "$STUB_LOG" && pass "the research row reaches the prompt" || fail "prompt lacks $RESEARCH"
grep -q "\"id\": \"$SCOUT\"" "$STUB_LOG" && pass "the scout row reaches the prompt" || fail "prompt lacks $SCOUT"
grep -q "\"id\": \"$WATCH\"" "$STUB_LOG" && fail "the watch reached the prompt" || pass "the watch is left to the store"
grep -q 'this list IS the work' "$STUB_LOG" && pass "the prompt says the list is the work" || fail "prompt header missing"
grep -q "$BASE/suggestions" "$STUB_LOG" && pass "the prompt names this store's address" || fail "prompt does not name $BASE"
grep -q 'Never "active"' "$STUB_LOG" && pass "the prompt forbids approving sources" || fail "prompt lost the approval rule"

write_stub "$RESEARCH"
OUT=$(run_dispatch) && CODE=0 || CODE=$?
[[ "$CODE" == "0" ]] && grep -q "still due: $SCOUT" <<<"$OUT" && pass "a partial dispatch succeeds and names what is left" || fail "partial: $OUT"

write_stub "$SCOUT"
OUT=$(run_dispatch) && CODE=0 || CODE=$?
[[ "$CODE" == "0" ]] && grep -q 'dispatch complete' <<<"$OUT" && pass "a full dispatch succeeds" || fail "full: $OUT"

curl -sfS -X PATCH "$BASE/sources/$WATCH" -H 'Content-Type: application/json' -d '{"enabled":true}' >/dev/null
OUT=$(run_dispatch) && CODE=0 || CODE=$?
[[ "$CODE" == "0" ]] && grep -q 'no research or scout sources due' <<<"$OUT" && pass "a quiet tick skips without a session, even with a watch due" || fail "quiet tick: $OUT"
echo "    quiet tick said: $OUT"

echo "==> smoke passed"
