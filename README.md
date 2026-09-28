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
- **WordPress archives come through the REST API too.** `/wp-json/wp/v2/posts`
  pages by number, not offset, and its list already carries each post's
  `content.rendered`, so one request saves 50 posts; `_embed=author` puts the
  author's name in the same answer. A page past the last is a 400 with the code
  `rest_post_invalid_page_number`, which ends the backfill. The backfill turns
  its offset into a page and a count to skip, so it carries on part way through
  a page the same way it does on Substack.
- **Each platform is one archive reader.** `publicationArchives` maps a
  platform to what reads a page of its archive (`substack.go`,
  `wordpress.go`); the backfill loop in `archive.go` is shared. A new platform
  is a reader and a word in `PublicationPlatforms`, not a branch in another
  platform's code.
- **A post saved by hand is not saved twice.** Before saving a backfilled post
  the backfill looks for its URL with either scheme and with or without the
  trailing slash, since a person may have pasted any of them.
- **Ids are never reused.** `id_sequences` hands out each number once, even
  after a purge, because an id may already be written on a card or in a chat.
- **The radar copies event-store's and job-store's.** A source has a kind, a
  cadence and a status, `GET /sources?due=1` holds the approval gate in one
  clause, and `PATCH` is where a person decides. Two differences: a blank
  status here means `proposed`, not `active`, so a source nobody approved
  cannot run; and a watch needs no model, so the store runs it itself, like
  the backfill, instead of paying for an agent session to read a JSON archive.
  Research and scout do need one, and only the scheduled
  `article-radar-dispatch` starts it, only when one of them is due.
- **A watch reads as little as it can.** Posts come newest first, so the first
  one already saved means the rest are older. A watch reads the next archive
  page only when every post on this one was new, and stops at two pages: a
  whole archive is the backfill's job, which a person starts on purpose.
- **Suggestions are not articles.** An agent's pick is a proposal until a
  person accepts it, and only then is it fetched and saved. A dismissed URL
  stays dismissed: posting it again answers the old row.
- **It listens on localhost only.** It fetches any URL it is handed, so open on
  every interface it would fetch internal addresses for anyone on the network.
  dash proxies it, behind dash's own login.
