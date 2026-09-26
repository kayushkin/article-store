# article-store routes

Rooted at `/`. JSON in and out unless a route says otherwise. Ids are
`article_000001`; a malformed id is 400, an unknown one 404. A request body with
a field the route does not take is 400.

| Method | Path | What it does |
|---|---|---|
| `GET` | `/health` | `{"status":"ok"}` |
| `GET` | `/settings` | The service's settings, read-only |
| `GET` | `/vocabulary` | `{"kinds":[…],"default_kind":"article"}` |
| `GET` | `/tags` | `{"tags":[{"tag":…,"count":…}]}` over live articles, most used first |
| `GET` | `/articles` | `{"articles":[summary…],"total":n}`. Query: `q` (fts5 over title, byline, site, text, tags, note; adds `snippet`), `tag`, `kind`, `read=true\|false`, `limit` (default 100, max 500), `offset`, `include_deleted=true`. Newest first; by relevance with `q`. Summaries carry no content |
| `POST` | `/articles` | Save. Body: `url` (required), and optionally `source_html`, `title`, `byline`, `site_name`, `published_at`, `kind`, `note`, `tags`, `added_by`. **201** new, **200** the URL was already saved (not fetched again), **409** it was saved and deleted, **502** the page could not be fetched or held no article text |
| `GET` | `/articles/{id}` | The article with `content_html`, `content_markdown`, `content_text`. `?format=markdown` answers `text/markdown`: title, byline, source, id, note, then the text |
| `GET` | `/articles/{id}/source` | The page as fetched, as `text/plain` with `nosniff` and a sandbox CSP |
| `PATCH` | `/articles/{id}` | Any of `title`, `byline`, `site_name`, `published_at`, `kind`, `note`, `tags`, `added_by`, and `read` (`true` stamps `read_at`, `false` clears it) |
| `POST` | `/articles/{id}/refetch` | Fetch again, or extract from `{"source_html":…}`, and replace the content. Title, byline, site, date, kind, note, tags and read state are kept |
| `DELETE` | `/articles/{id}` | Soft delete: gone from listings and search, still readable by id. `?hard=true` purges the row and frees the URL |
| `POST` | `/articles/{id}/restore` | Undo a soft delete |

## Fields

`url` is the URL as saved with its fragment dropped, and is unique. `final_url`
is where the fetch ended after redirects. `published_at`, `read_at`,
`fetched_at`, `created_at`, `updated_at` and `deleted_at` are unix seconds; 0
means unknown, unread, or not deleted. `byline` is the author as the page prints
it — display text, never a key. `content_html` is sanitized and safe to render;
`source_html` is not.
