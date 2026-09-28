# article-store routes

Rooted at `/`. JSON in and out unless a route says otherwise. Ids are
`article_000001`, `publication_000001`, `source_000001` and `suggestion_000001`;
a malformed id is 400, an unknown one 404. A request body with
a field the route does not take is 400.

| Method | Path | What it does |
|---|---|---|
| `GET` | `/health` | `{"status":"ok"}` |
| `GET` | `/settings` | The service's settings, read-only |
| `GET` | `/vocabulary` | `{"kinds":[…],"default_kind":"article","source_kinds":[…],"publication_platforms":[…],"orders":[…],"radar_source_kinds":[…],"radar_source_statuses":[…],"suggestion_statuses":[…]}`. `source_kinds` says where an article's text came from; `radar_source_kinds` are the kinds of `/sources` |
| `GET` | `/tags` | `{"tags":[{"tag":…,"count":…}]}` over live articles, most used first |
| `GET` | `/articles` | `{"articles":[summary…],"total":n}`. Query: `q` (fts5 over title, byline, site, text, tags, note; adds `snippet`), `tag`, `kind`, `publication_id`, `read=true\|false`, `favorite=true\|false`, `order` (`published`: most recently published first, the default; `saved`: most recently saved first; `relevance`: the default with `q`, and 400 without it), `limit` (default 100, max 500), `offset`, `include_deleted=true`. Summaries carry no content |
| `POST` | `/articles` | Save. Body: `url` (required), and optionally `source_html`, `title`, `byline`, `site_name`, `published_at`, `kind`, `note`, `tags`, `added_by`. **201** new, **200** the URL was already saved (not fetched again), **409** it was saved and deleted, **502** the page could not be fetched or held no article text |
| `GET` | `/articles/{id}` | The article with `content_html`, `content_markdown`, `content_text`. `?format=markdown` answers `text/markdown`: title, byline, source, id, note, then the text |
| `GET` | `/articles/{id}/source` | What was fetched — the page's HTML, or Substack's or WordPress's JSON for the post, as `source_kind` says — as `text/plain` with `nosniff` and a sandbox CSP |
| `PATCH` | `/articles/{id}` | Any of `title`, `byline`, `site_name`, `published_at`, `kind`, `note`, `tags`, `added_by`, `read` (`true` stamps `read_at`, `false` clears it) and `favorite` (the same for `favorited_at`) |
| `POST` | `/articles/{id}/refetch` | Fetch again the way the article was first fetched, or extract from `{"source_html":…}` as a page, and replace the content. Title, byline, site, date, kind, note, tags and read state are kept |
| `DELETE` | `/articles/{id}` | Soft delete: gone from listings and search, still readable by id. `?hard=true` purges the row and frees the URL |
| `POST` | `/articles/{id}/restore` | Undo a soft delete |
| `GET` | `/publications` | `{"publications":[…]}` by name, each with its `backfill` progress and `article_count` |
| `POST` | `/publications` | `{"platform":"substack","base_url":"https://noahpinion.substack.com","name":…}`; `platform` is one of `publication_platforms` (`substack`, `wordpress`). **201** new, **200** that base URL is already stored. `base_url` is the site's root: a path is 400 |
| `GET` | `/publications/{id}` | One publication |
| `PATCH` | `/publications/{id}` | `{"name":…}` |
| `POST` | `/publications/{id}/backfill` | **202**: start saving the publication's whole archive in the background. A stopped or failed backfill carries on from its offset; a done one starts again; `?restart=true` always starts again at the newest post. Posts already saved are counted, not fetched |
| `POST` | `/publications/{id}/backfill/stop` | Stop a running backfill where it is |
| `GET` | `/sources` | `{"sources":[…]}` oldest first. Query: `kind`, `status`, `due=1` (active, enabled and `next_run_at` passed — one clause, so the approval gate cannot be left out) |
| `POST` | `/sources` | Add a radar source; see *The article radar*. **201** new, **200** a watch of that publication already exists (answered unchanged, whatever its status), **400** a field the kind does not take or lacks |
| `GET` | `/sources/{id}` | One source |
| `PATCH` | `/sources/{id}` | Any of `name`, `status`, `enabled`, `cadence_hours`, `prompt`, `notes`. The decision channel: a move to `active` approves the source |
| `DELETE` | `/sources/{id}` | Remove it. Its suggestions keep its id |
| `POST` | `/sources/{id}/ran` | Stamp `last_run_at`, set `next_run_at` a cadence from now, and store `{"result":…}` (optional) as `last_result` |
| `GET` | `/suggestions` | `{"suggestions":[…]}` newest first. Query: `status`, `source_id`, `limit` (default 100, max 500) |
| `POST` | `/suggestions` | `{"url","reason"` (both required)`,"title","byline","site_name","source_id"}`. Nothing is fetched. **201** new; **200** that URL was suggested before, answered unchanged whatever its status — so a dismissed URL stays dismissed; **409** that URL is already an article, live or deleted, and the error names it. URLs match as a backfill matches posts: fragment dropped, either scheme, with or without the trailing slash |
| `GET` | `/suggestions/{id}` | One suggestion |
| `POST` | `/suggestions/{id}/accept` | Save the article through `POST /articles`'s path, with the suggestion's title, byline and site name (when set) replacing what extraction finds and `added_by` `suggestion suggestion_…`, and answer `{"suggestion":…,"article":…}`. Accepting again answers the same article. **502** the page could not be fetched, and the suggestion stays proposed; **409** the URL was saved and deleted; **400** it was dismissed |
| `POST` | `/suggestions/{id}/dismiss` | Mark it dismissed; the URL is never suggested again. **400** it was accepted |

## Fields

`url` is the URL as saved with its fragment dropped, and is unique. `final_url`
is where the fetch ended after redirects. `published_at`, `read_at`, `favorited_at`,
`fetched_at`, `created_at`, `updated_at` and `deleted_at` are unix seconds; 0
means unknown, unread, not a favorite, or not deleted. `byline` is the author as the page prints
it — display text, never a key. `content_html` is sanitized and safe to render;
what `/source` serves is not.

A publication's `backfill.status` is empty (never run), `running`, `done`,
`failed` (the archive itself could not be read; `last_error` says why) or
`stopped`. `offset` counts archive entries handled, newest first.
`posts_saved`, `posts_already_saved` (saved before, under the post's URL or the
same URL with the other of http and https, or with or without its trailing
slash), `posts_skipped` (not an article, such as a Substack chat thread or a
password-protected WordPress post) and `posts_failed` count what happened to them; `last_error` holds
the latest failure. After a 429 the backfill asks that site nothing before
`next_attempt_at`.

An article from a backfill has `publication_id`, `kind` `post`, `added_by`
`backfill publication_…`, and `source_kind` `substack_post_api` or
`wordpress_post_api`, by platform. A Substack post also has the tag `paid-only`
when a reader without a subscription sees only a preview: its text then stops
where the preview does.

## The article radar

A **source** is a place the radar looks for new reading. `kind` is one of:

- **`watch`** — one publication, checked for new posts. It needs
  `publication_id`, or `platform` and `base_url` for a blog that is not yet a
  publication (a scout proposes those); naming a `base_url` already stored
  fills in its `publication_id`. It takes no `prompt`. The store's own
  `Watcher` runs a due watch — no model, no scheduler job — reading the archive
  from the newest end through the same platform readers, pause and 429 handling
  as the backfill, saving what is new with `added_by` `watch source_…`. It reads
  the next archive page only when every post on this one was new, and at most
  2 pages a run; `last_result` counts what it did. It does not run while its
  publication's backfill is running, or inside a 429 wait, which it keeps on the
  publication (`backfill.next_attempt_at`) so the backfill honours it too.
- **`research`** — `prompt` is a topic. `article-radar-dispatch` hands it to an
  agent session, which posts `/suggestions` with a reason.
- **`scout`** — `prompt` says what writers to look for. The session proposes
  watch sources (`status` `proposed`, platform `substack` or `wordpress`,
  after checking the site's archive API answers).

The store never branches on `research` or `scout`; what they mean is in the
dispatcher's prompt.

`status` is `active`, `proposed` or `rejected`, and **a blank status is
`proposed`**: nothing runs until a person approves it. `enabled` is a pause
switch apart from status. `cadence_hours` defaults to 24 for a watch and 168
for research and scout. A source added `active` is due at once; a proposed one
has `next_run_at` 0 and is not due. **A move to `active` by `PATCH`** from any
other status enables the source and makes it due now, and a watch with no
`publication_id` gets one — the publication at its `base_url`, or a new one. No
archive backfill starts: `POST /publications/{id}/backfill` is a separate
decision. Changing `cadence_hours` on an active source that has run moves
`next_run_at` to a cadence after `last_run_at`.

A **suggestion** is an article an agent thinks worth reading, not yet saved:
`url`, `title`, `byline`, `site_name`, `reason` (why it was picked),
`source_id`, `status` (`proposed`, `accepted`, `dismissed`), `article_id` once
accepted, and `decided_at`.

### The dispatcher

`~/bin/article-radar-dispatch`, installed by `deploy.sh` from
`scripts/article-radar-dispatch.sh` and run by a scheduler shell job. It reads
the store's address from the installed unit.

1. `GET /sources?due=1` and keep the `research` and `scout` rows. Store
   unreachable → stderr and exit 1.
2. **None due → print `no research or scout sources due` and exit 0 with no
   session.** This gate is the cost control.
3. Otherwise run `claude -p` with the due rows inlined in the prompt,
   `--model` (`ARTICLE_RADAR_MODEL`, default `sonnet`) and `--max-budget-usd`
   (`ARTICLE_RADAR_MAX_BUDGET_USD`, default 10). The session suggests, proposes
   and marks each source ran; it never accepts a suggestion or approves a source.
4. Grade on `last_run_at` per source id before and after. Nothing marked →
   exit 1.

`ARTICLE_RADAR_STORE_URL` and `ARTICLE_RADAR_CLAUDE_BIN` point it at another
store and another runner; `scripts/e2e-smoke.sh` uses both to test it with stub
runners and no model call.
