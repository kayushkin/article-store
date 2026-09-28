# article-store

Articles, essays and stories saved from the web, with the text pulled out of
each page, so they can be read, searched, and handed to an agent by id long
after the page has moved or gone.

```bash
curl -s -X POST localhost:8318/articles -H 'Content-Type: application/json' \
  -d '{"url":"https://slatestarcodex.com/2015/08/17/the-goddess-of-everything-else-2/","byline":"Scott Alexander","kind":"story","tags":["ssc"]}'
curl -s "localhost:8318/articles?q=goddess"
curl -s "localhost:8318/articles/article_000001?format=markdown"
```

`CONTRACT.md` is the route table.

## Why these choices

- **A store of its own, not noteboard notes.** A note is your own writing. An
  article is someone else's, and carries a source URL, an author, a site and a
  date that a note has nowhere to put.
- **Keep the page as fetched.** `source_html` is stored beside the extracted
  text, so a better extractor can be run over old saves with
  `POST /articles/{id}/refetch {"source_html":…}` even after the page is gone.
- **Extraction** is readeck's go-readability (the maintained fork of
  go-shiori's port of Mozilla Readability), then bluemonday's UGC policy, then
  html-to-markdown. The byline readability finds is sometimes wrong — on Slate
  Star Codex it reads "Posted on" — so a caller may send its own on save, and
  fix it later with `PATCH`.
- **Substack archives come through Substack's API, not its pages.** A
  publication's `/api/v1/archive` lists its posts and `/api/v1/posts/<slug>`
  serves one post's `body_html`: the article and nothing else, a sixth the size
  of the page, with the real authors and date. The archive list leaves
  `body_html` empty, so it is one request per post. Substack answers 429 to
  quick requests, so a backfill is a job inside the service, not a script: one
  request per `ARTICLE_STORE_BACKFILL_REQUEST_INTERVAL` (5 s), a wait of the
  named `Retry-After` on a 429, and progress saved after every post.
- **Ids are never reused.** `id_sequences` hands out each number once, even
  after a purge, because an id may already be written on a card or in a chat.
- **It listens on localhost only.** It fetches any URL it is handed, so open on
  every interface it would fetch internal addresses for anyone on the network.
  dash proxies it, behind dash's own login.
