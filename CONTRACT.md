# article-store routes

Rooted at `/`. JSON in and out unless a route says otherwise. Ids are
`article_000001`; a malformed id is 400, an unknown one 404. A request body with
a field the route does not take is 400.

| Method | Path | What it does |
|---|---|---|
| `GET` | `/health` | `{"status":"ok"}` |
| `GET` | `/settings` | The service's settings, read-only |
| `GET` | `/vocabulary` | `{"kinds":[…],"default_kind":"article","source_kinds":[…],"publication_platforms":[…]}` |
| `GET` | `/tags` | `{"tags":[{"tag":…,"count":…}]}` over live articles, most used first |
| `GET` | `/articles` | `{"articles":[summary…],"total":n}`. Query: `q` (fts5 over title, byline, site, text, tags, note; adds `snippet`), `tag`, `kind`, `publication_id`, `read=true\|false`, `limit` (default 100, max 500), `offset`, `include_deleted=true`. Newest first; by relevance with `q`. Summaries carry no content |
| `POST` | `/articles` | Save. Body: `url` (required), and optionally `source_html`, `title`, `byline`, `site_name`, `published_at`, `kind`, `note`, `tags`, `added_by`. **201** new, **200** the URL was already saved (not fetched again), **409** it was saved and deleted, **502** the page could not be fetched or held no article text |
| `GET` | `/articles/{id}` | The article with `content_html`, `content_markdown`, `content_text`. `?format=markdown` answers `text/markdown`: title, byline, source, id, note, then the text |
| `GET` | `/articles/{id}/source` | What was fetched — the page's HTML, or Substack's JSON for the post, as `source_kind` says — as `text/plain` with `nosniff` and a sandbox CSP |
| `PATCH` | `/articles/{id}` | Any of `title`, `byline`, `site_name`, `published_at`, `kind`, `note`, `tags`, `added_by`, and `read` (`true` stamps `read_at`, `false` clears it) |
| `POST` | `/articles/{id}/refetch` | Fetch again the way the article was first fetched, or extract from `{"source_html":…}` as a page, and replace the content. Title, byline, site, date, kind, note, tags and read state are kept |
| `DELETE` | `/articles/{id}` | Soft delete: gone from listings and search, still readable by id. `?hard=true` purges the row and frees the URL |
| `POST` | `/articles/{id}/restore` | Undo a soft delete |

| `GET` | `/publications` | `{"publications":[…]}` by name, each with its `backfill` progress and `article_count` |
| `POST` | `/publications` | `{"platform":"substack","base_url":"https://noahpinion.substack.com","name":…}`. **201** new, **200** that base URL is already stored. `base_url` is the site's root: a path is 400 |
| `GET` | `/publications/{id}` | One publication |
| `PATCH` | `/publications/{id}` | `{"name":…}` |
| `POST` | `/publications/{id}/backfill` | **202**: start saving the publication's whole archive in the background. A stopped or failed backfill carries on from its offset; a done one starts again; `?restart=true` always starts again at the newest post. Posts already saved are counted, not fetched |
| `POST` | `/publications/{id}/backfill/stop` | Stop a running backfill where it is |

## Fields

`url` is the URL as saved with its fragment dropped, and is unique. `final_url`
is where the fetch ended after redirects. `published_at`, `read_at`,
`fetched_at`, `created_at`, `updated_at` and `deleted_at` are unix seconds; 0
means unknown, unread, or not deleted. `byline` is the author as the page prints
it — display text, never a key. `content_html` is sanitized and safe to render;
`source_html` is not.

A publication's `backfill.status` is empty (never run), `running`, `done`,
`failed` (the archive itself could not be read; `last_error` says why) or
`stopped`. `offset` counts archive entries handled, newest first.
`posts_saved`, `posts_already_saved`, `posts_skipped` (not an article, such as a
chat thread) and `posts_failed` count what happened to them; `last_error` holds
the latest failure. After a 429 the backfill asks that site nothing before
`next_attempt_at`.

An article from a backfill has `publication_id`, `kind` `post`, `source_kind`
`substack_post_api`, `added_by` `backfill publication_…`, and the tag
`paid-only` when a reader without a subscription sees only a preview: its text
then stops where the preview does.
