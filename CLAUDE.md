# About article-store

## What it owns

`127.0.0.1:8318`, unit `article-store.service`. Articles, essays and stories saved from the web: the URL, the text pulled out of the page as sanitized HTML, markdown and plain text, what was fetched, and your tags, note, read state and favorites. Also the publications they come from — a Substack, say — with an import of each one's whole archive. Ids are `article_000001` and `publication_000001`. Routes are rooted at `/`, not `/api`. `CONTRACT.md` is the route table and `README.md` the reasoning.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Saving is fetch, extract, sanitize

`POST /articles {"url":…}` fetches the page (`extract.go`: http and https only, 30 s, 10 MB, HTML only), runs readeck's go-readability over it, sanitizes the result with bluemonday's UGC policy, and converts that to markdown. A URL already saved answers 200 with the stored row and is **not** fetched again; `POST /articles/{id}/refetch` replaces the content and keeps every field a person may have corrected. `source_html` in the request body skips the fetch — the way to save a page behind a login. A page with no article text is 502, and nothing is stored.

## Publications and archive backfills

`POST /publications/{id}/backfill` sets a flag; the work is the `Backfiller` goroutine `main` starts (`publication.go`). Each step takes the running backfill that has waited longest, reads one archive page of 50 from Substack's `/api/v1/archive`, and saves each post through `/api/v1/posts/<slug>` (`substack.go`), pausing `ARTICLE_STORE_BACKFILL_REQUEST_INTERVAL` between requests. `recordEntry` moves the offset and counts the outcome in one statement after every post, so a restart or deploy carries on without counting twice; a URL already saved costs no request. A 429 sets `backfill_next_attempt_at` from `Retry-After` and leaves the backfill running. An article's `source_kind` says what `fetched_source` holds (a page's HTML or Substack's JSON), and refetch goes back the same way. Paid posts are saved with only their preview and the tag `paid-only`.

## The byline is display text

readability's byline is often wrong ("Posted on" on Slate Star Codex). A caller may send `byline`, `title`, `site_name` or `published_at` on save to replace what extraction found, or `PATCH` them later. The byline is never a key: nothing joins on it.

## Search, ids and deleting

`GET /articles?q=…` is an external-content fts5 index keyed on `seq`, over title, byline, site, text, tags and note; three triggers in `schema.sql` keep it true, and a new write path that changes an indexed column needs no trigger of its own as long as it is an `UPDATE` on `articles`. A malformed query is 400. `id_sequences` hands out each number once, even after `DELETE ?hard=true`, so an id written on a card never comes to mean a different article. `DELETE` without `hard` is reversible with `POST /articles/{id}/restore`, and saving a deleted URL is 409 rather than a silent restore.

# Access and operations

## Who may call it

No authentication, and it listens on **localhost only**: it fetches any URL it is handed, so on every interface it would fetch internal addresses for anyone on the network. Do not widen `ARTICLE_STORE_ADDR`. `GET /articles/{id}/source` serves what was fetched, unsanitized, as `text/plain` with `nosniff` and a sandbox CSP; never serve it as HTML. Agents read an article with `GET /articles/{id}?format=markdown`. Unit `article-store.service`, binary `~/bin/article-store`, database `~/.config/article-store/article-store.db`.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/article-store`, root package `articlestore`, server in `cmd/article-store`. ⚠️ **Build and test with `-tags sqlite_fts5`** (`make test`, `make build`): without it the binary dies at boot with "no such module: fts5". The three variables it reads, `ARTICLE_STORE_ADDR`, `ARTICLE_STORE_DATA_DIR` and `ARTICLE_STORE_BACKFILL_REQUEST_INTERVAL`, are declared once in `settings.go` with llm-bridge `servicesettings` (hence `replace ../llm-bridge` in `go.mod`) and served read-only at `GET /settings`; a new variable is declared there or `TestEveryEnvironmentVariableTheServiceReadsIsDeclared` fails. Tests fetch from an `httptest` server — a fake Substack for the backfill — and never touch the network. A column added to a table that exists goes in `migrate()` in `store.go`, never only in `schema.sql`; `TestOpenMigratesADatabaseFromTheFirstSchema` holds that. `./deploy.sh` runs the shared deploy gate, tests, builds, checks provenance and the live environment, restarts the unit, smoke-checks search at the address the unit sets, and records the deploy.
