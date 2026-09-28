#!/usr/bin/env bash
# article-radar dispatcher: work the DUE research and scout sources of
# article-store with one capped agent session. Runs as a scheduler shell job.
# Per-source cadence is kept by the store's next_run_at, so on most ticks
# nothing is due.
#
# Watch sources are not handled here. They need no model: the store's own
# Watcher checks each watched publication for new posts. This script handles
# only the two kinds that need an agent, and it is the one place where what
# those kinds mean is written down. The store never branches on them.
#
# Cost control: a session starts only when at least one research or scout
# source is due, and that session is capped with --max-budget-usd.
set -euo pipefail

export PATH="$HOME/.local/share/mise/shims:$HOME/.local/share/mise/installs/node/25.8.2/bin:$PATH"

# The store's address comes from its unit, the one place it is set, unless a
# caller (the smoke test) names another store.
UNIT="$HOME/.config/systemd/user/article-store.service"
if [[ -n "${ARTICLE_RADAR_STORE_URL:-}" ]]; then
  STORE="$ARTICLE_RADAR_STORE_URL"
else
  ADDR="$(sed -n 's/^Environment=ARTICLE_STORE_ADDR=//p' "$UNIT" 2>/dev/null || true)"
  if [[ -z "$ADDR" ]]; then
    echo "article-radar: $UNIT sets no ARTICLE_STORE_ADDR, so there is no store address — no dispatch this run" >&2
    exit 1
  fi
  case "$ADDR" in :*) ADDR="localhost$ADDR" ;; esac
  STORE="http://$ADDR"
fi
BUDGET="${ARTICLE_RADAR_MAX_BUDGET_USD:-10}"
MODEL="${ARTICLE_RADAR_MODEL:-sonnet}"
# The session runner is a variable so the grading at the bottom can be tested
# without paying for a model call: scripts/e2e-smoke.sh drives this script with
# stub runners that mark none, one, and all of the sources they are handed.
CLAUDE_BIN="${ARTICLE_RADAR_CLAUDE_BIN:-claude}"

# The kinds this script works. A due watch is the store's own business.
AGENT_KINDS="research,scout"

due_agent_sources() { # $1 = JSON from GET /sources?due=1; prints the agent-kind rows as a JSON object
  ARTICLE_RADAR_AGENT_KINDS="$AGENT_KINDS" python3 -c '
import json, os, sys
kinds = set(os.environ["ARTICLE_RADAR_AGENT_KINDS"].split(","))
sources = [s for s in json.load(sys.stdin).get("sources") or [] if s["kind"] in kinds]
print(json.dumps({"sources": sources}))
' <<<"$1"
}

if ! all_due=$(curl -sfS "$STORE/sources?due=1"); then
  echo "article-radar: cannot reach article-store at $STORE — no dispatch this run" >&2
  exit 1
fi

# Count with python, never `grep -o | wc -l`: an empty list makes grep exit 1,
# and under `set -eo pipefail` the quiet tick would then die with a bare exit 1
# that looks exactly like the store being down.
if ! due=$(due_agent_sources "$all_due") || ! read -r count due_ids <<<"$(python3 -c '
import json, sys
sources = json.load(sys.stdin)["sources"]
print(len(sources), ",".join(source["id"] for source in sources))
' <<<"$due")"; then
  echo "article-radar: article-store returned a source list this script cannot read: ${all_due:0:300}" >&2
  exit 1
fi

# This gate is the cost control: nothing due means no session and no spend.
if [[ "$count" == "0" ]]; then
  echo "article-radar: no research or scout sources due — skipping (no Claude session launched)"
  exit 0
fi

echo "article-radar: $count source(s) due ($due_ids) — launching capped session (\$$BUDGET, $MODEL)"

read -r -d '' INSTRUCTIONS <<'EOF' || true
You are the article-radar dispatcher. You find reading worth the user's time and
record it in article-store at STORE_URL. Do NOT save articles, do NOT accept
suggestions, do NOT approve, enable or reject sources, do NOT email or message
anyone, and do NOT ask questions. Work autonomously, then stop.

Everything you write is a PROPOSAL that the user reviews on the dash Articles
page. A suggestion becomes an article only when the user accepts it; a source
runs only when the user approves it. You cannot give yourself work and you must
not try.

Steps:

1. The sources you are working are listed at the END of this prompt, under
   "The source(s) due right now". The dispatcher read them from the store and
   only started you because that read was non-empty, so do not fetch that list
   again and never finish on the grounds that nothing is due.

2. First read what the user already has, so you do not offer it again:
     curl -s STORE_URL/publications
     curl -s "STORE_URL/articles?order=saved&limit=100"
     curl -s "STORE_URL/suggestions?limit=200"
   Every suggestion there — proposed, accepted or DISMISSED — is a URL already
   decided. A dismissed one is a "no" from the user: do not offer it again, nor
   the same piece under another URL.

3. A source of kind "research": its "prompt" is a topic. Find a handful (three
   to six) of GOOD, RECENT, SUBSTANTIVE articles on it:
   - Use WebSearch, then open each candidate with WebFetch and read it. Never
     suggest from a search snippet alone.
   - Good means: an essay, analysis, long-form report or careful blog post by
     someone who knows the subject. Not a press release, not a listicle, not SEO
     filler, not a news brief that restates a headline, not a paywalled page
     whose text you could not read, not a video or podcast page.
   - Recent means published in the last few months, unless the prompt asks for
     something else. Prefer primary writers over aggregators that summarize them.
   - POST each one:
       curl -s -X POST STORE_URL/suggestions -H 'Content-Type: application/json' -d '{
         "url": "https://…/the-article",          // the article itself, not a search or index page
         "title": "The article's own title",
         "byline": "The author as the page names them, or empty",
         "site_name": "The publication or blog",
         "reason": "One or two sentences: what it argues or reports and why it is worth reading for THIS topic.",
         "source_id": "source_000123"               // THIS source's id, from the list below
       }'
     201 means suggested. 200 means that URL was suggested before (the answer is
     the old suggestion, unchanged — look at its status; if it is dismissed, the
     user already said no). 409 means the user already saved it as an article.
     400 names what you got wrong; fix it and retry once.
   - The reason is for the user, deciding in a few seconds whether to read it.
     Say what the piece actually says, not that it "offers insights".

4. A source of kind "scout": its "prompt" says what kind of writers to look
   for. Find NEW blogs and newsletters worth following, given the publications
   already followed (step 2) — similar in subject and quality, not copies of
   what is there.
   - First GET STORE_URL/sources (no filter: you need every status, rejected
     included). A blog already there under any status is not new; a rejected
     one is a "no" from the user, and proposing it again answers the old row
     unchanged.
   - Only two platforms can be followed: "substack" and "wordpress". VERIFY the
     archive API answers before proposing, and use the site's root URL:
       substack:  curl -s "<root>/api/v1/archive?sort=new&offset=0&limit=3"
                  must answer a JSON array of posts with recent post_date values.
                  A custom domain works if this answers.
       wordpress: curl -s "<root>/wp-json/wp/v2/posts?per_page=3"
                  must answer a JSON array of posts with recent "date" values.
     A blog on any other platform, or whose API does not answer, cannot be
     followed: leave it out and say so in your summary.
   - Prefer blogs that post at least monthly and have posted in the last two
     months. A dormant blog is not worth a watch.
   - POST each one as a proposed watch:
       curl -s -X POST STORE_URL/sources -H 'Content-Type: application/json' -d '{
         "kind": "watch",
         "status": "proposed",
         "platform": "substack",                    // or "wordpress"
         "base_url": "https://example.substack.com", // the root: no path, no trailing slash
         "name": "The blog's own name",
         "proposed_by": "source_000124",            // THIS scout's id
         "notes": "Who writes it, what about, how often it posts, the date of its latest post, which API you checked, and why it fits what is already followed."
       }'
     Always send "status": "proposed". Never "active".
   - Be selective: three or four good proposals beat fifteen that need triage.

5. When you finish a source — every kind — mark it ran, with one line saying
   what you did:
     curl -s -X POST STORE_URL/sources/<id>/ran -H 'Content-Type: application/json' \
       -d '{"result":"5 suggested, 2 already known"}'
   The dispatcher grades this run by which sources got that mark, so a source
   you worked but did not mark reads as a source that never ran.

6. Finish with a short plain-text summary: per source, what you suggested or
   proposed (titles or blog names), and what you looked at and left out, and why.
EOF

INSTRUCTIONS="${INSTRUCTIONS//STORE_URL/$STORE}"

# Hand the session the list the gate already measured instead of telling it to
# fetch it again: job-store's and event-store's dispatchers each lost a day to a
# session that re-fetched its worklist and read another command's empty output.
due_rows=$(python3 -c '
import json, sys
for source in json.load(sys.stdin)["sources"]:
    print(json.dumps(source, sort_keys=True))
' <<<"$due")

PROMPT="$INSTRUCTIONS

## The $count source(s) due right now — this list IS the work

The dispatcher read these from GET $STORE/sources?due=1 immediately before
starting you, and that same read is why you were started at all. Work every row
below. The list is never empty, so \"nothing was due\" is never the right answer:
if you think it is empty, you are reading some other command's output.

$due_rows"

session_status=0
"$CLAUDE_BIN" -p "$PROMPT" \
  --model "$MODEL" \
  --max-budget-usd "$BUDGET" \
  --dangerously-skip-permissions || session_status=$?

# A dispatch that marked nothing did nothing, and reporting that as success is
# how a lost day comes to look like a quiet one. Compare last_run_at per id,
# not list membership: a short cadence can make a source due again at once.
if ! after_all=$(curl -sfS "$STORE/sources?due=1"); then
  echo "article-radar: session finished (exit $session_status) but article-store is unreachable now — cannot tell whether any source ran" >&2
  exit 1
fi

if ! read -r ran_count still_ids <<<"$(ARTICLE_RADAR_DUE_BEFORE="$due" python3 -c '
import json, os, sys

def marks(payload):
    return {source["id"]: source.get("last_run_at") for source in payload.get("sources") or []}

before = marks(json.loads(os.environ["ARTICLE_RADAR_DUE_BEFORE"]))
after = marks(json.load(sys.stdin))
ran = [i for i in before if i not in after or after[i] != before[i]]
still = [i for i in sorted(before) if i in after and after[i] == before[i]]
print(len(ran), ",".join(still))
' <<<"$after_all")"; then
  echo "article-radar: cannot compare the due list before and after the session — treating this dispatch as failed" >&2
  exit 1
fi

if [[ "$ran_count" == "0" ]]; then
  echo "article-radar: the session marked NOTHING ran — all $count source(s) it was handed are still due ($due_ids), session exit $session_status. Refusing to report this run as a success." >&2
  exit 1
fi

if [[ -n "$still_ids" ]]; then
  echo "article-radar: partial dispatch — $ran_count source(s) ran, still due: $still_ids"
fi

if [[ "$session_status" != "0" ]]; then
  echo "article-radar: session exited $session_status" >&2
  exit "$session_status"
fi

echo "article-radar: dispatch complete"
