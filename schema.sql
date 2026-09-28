PRAGMA foreign_keys = ON;

-- Create-only. Every statement here is IF NOT EXISTS, so this file is what an
-- empty database gets and nothing more: a column added to a table that already
-- exists on this host will never appear by editing this file. Such a column is
-- added in migrate() in Open(), and so is any index that names it.

-- id_sequences: the last number handed out, per kind of id. A number is never
-- handed out twice, even after a purge, because an id may already be written on
-- a card or in a chat, and it must never come to mean a different article.
CREATE TABLE IF NOT EXISTS id_sequences (
    name TEXT PRIMARY KEY,
    last INTEGER NOT NULL
);
INSERT OR IGNORE INTO id_sequences (name, last) VALUES ('article', 0);
INSERT OR IGNORE INTO id_sequences (name, last) VALUES ('publication', 0);
INSERT OR IGNORE INTO id_sequences (name, last) VALUES ('source', 0);
INSERT OR IGNORE INTO id_sequences (name, last) VALUES ('suggestion', 0);

-- publications: a newsletter or blog whose posts are saved here, such as one
-- Substack. An article from it carries its id in articles.publication_id.
--
-- The backfill_ columns are the one archive import a publication can have
-- running: where it has got to, what it has done, and when it may next ask the
-- site for anything. The worker saves them after every post, so a restart
-- carries on where it stopped.
CREATE TABLE IF NOT EXISTS publications (
    id                        TEXT PRIMARY KEY,           -- publication_000001
    seq                       INTEGER NOT NULL UNIQUE,    -- from id_sequences
    platform                  TEXT NOT NULL,              -- see publication.go for the vocabulary
    base_url                  TEXT NOT NULL UNIQUE,       -- https://noahpinion.substack.com, no trailing slash
    name                      TEXT NOT NULL DEFAULT '',
    platform_publication_ref  TEXT NOT NULL DEFAULT '',   -- the platform's own id for it, once seen
    backfill_status           TEXT NOT NULL DEFAULT '',   -- '' never run | running | done | failed | stopped
    backfill_offset           INTEGER NOT NULL DEFAULT 0, -- archive entries fully handled, newest first
    backfill_posts_saved      INTEGER NOT NULL DEFAULT 0,
    backfill_posts_already    INTEGER NOT NULL DEFAULT 0, -- in the archive and already saved
    backfill_posts_skipped    INTEGER NOT NULL DEFAULT 0, -- not an article: a chat thread, say
    backfill_posts_failed     INTEGER NOT NULL DEFAULT 0,
    backfill_last_error       TEXT NOT NULL DEFAULT '',
    backfill_started_at       INTEGER NOT NULL DEFAULT 0,
    backfill_finished_at      INTEGER NOT NULL DEFAULT 0,
    backfill_next_attempt_at  INTEGER NOT NULL DEFAULT 0, -- after a 429, not before this
    created_at                INTEGER NOT NULL,
    updated_at                INTEGER NOT NULL
);

-- articles: one piece of writing saved from the web, and the text we pulled out
-- of it. What was fetched (fetched_source) is kept beside the extracted text,
-- so a better extractor can be run over it later without the page having to
-- still exist. source_kind says what fetched_source is and how to fetch again.
CREATE TABLE IF NOT EXISTS articles (
    id               TEXT PRIMARY KEY,            -- article_000001
    seq              INTEGER NOT NULL UNIQUE,     -- from id_sequences; generates id; the fts rowid
    url              TEXT NOT NULL UNIQUE,        -- as saved, fragment dropped; the dedupe key
    final_url        TEXT NOT NULL DEFAULT '',    -- where the fetch ended after redirects
    title            TEXT NOT NULL DEFAULT '',
    byline           TEXT NOT NULL DEFAULT '',    -- the author as the page prints it; display only
    site_name        TEXT NOT NULL DEFAULT '',
    language         TEXT NOT NULL DEFAULT '',
    published_at     INTEGER NOT NULL DEFAULT 0,  -- unix seconds; 0 = unknown
    excerpt          TEXT NOT NULL DEFAULT '',
    kind             TEXT NOT NULL DEFAULT 'article', -- see kind.go for the vocabulary
    content_html     TEXT NOT NULL DEFAULT '',    -- extracted and sanitized; safe to render
    content_markdown TEXT NOT NULL DEFAULT '',    -- the same content as markdown, for agents
    content_text     TEXT NOT NULL DEFAULT '',    -- plain text, for search and word count
    word_count       INTEGER NOT NULL DEFAULT 0,
    fetched_source   TEXT NOT NULL DEFAULT '',    -- as fetched, unsanitized; never rendered
    source_kind      TEXT NOT NULL DEFAULT 'web_page', -- see source_kind.go
    publication_id   TEXT NOT NULL DEFAULT '',    -- publications.id; '' = none
    note             TEXT NOT NULL DEFAULT '',    -- your own commentary, never the author's
    tags             TEXT NOT NULL DEFAULT '[]',  -- JSON array
    added_by         TEXT NOT NULL DEFAULT '',
    read_at          INTEGER NOT NULL DEFAULT 0,  -- 0 = unread
    favorited_at     INTEGER NOT NULL DEFAULT 0,  -- 0 = not a favorite
    fetched_at       INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    deleted_at       INTEGER NOT NULL DEFAULT 0   -- soft delete: 0 = live
);
CREATE INDEX IF NOT EXISTS idx_articles_created ON articles(created_at);
CREATE INDEX IF NOT EXISTS idx_articles_kind    ON articles(kind, deleted_at);
CREATE INDEX IF NOT EXISTS idx_articles_read    ON articles(read_at, deleted_at);

-- Full-text search over what someone would remember an article by: its title,
-- who wrote it, where it ran, its text, and the tags and note you gave it.
--
-- An external-content index over the one table, keyed on seq. It stores no copy
-- of the text, so the triggers below must send it the fts5 'delete' command with
-- the OLD values: an external-content index cannot read its own rows back.
--
-- Requires a driver built with FTS5 compiled in. See Open(): a build without it
-- fails here, at boot, with a message naming the build tag.
CREATE VIRTUAL TABLE IF NOT EXISTS articles_fts USING fts5(
    title, byline, site_name, content_text, tags, note,
    content='articles', content_rowid='seq'
);

CREATE TRIGGER IF NOT EXISTS articles_fts_after_insert AFTER INSERT ON articles BEGIN
    INSERT INTO articles_fts(rowid, title, byline, site_name, content_text, tags, note)
    VALUES (new.seq, new.title, new.byline, new.site_name, new.content_text, new.tags, new.note);
END;

CREATE TRIGGER IF NOT EXISTS articles_fts_after_delete AFTER DELETE ON articles BEGIN
    INSERT INTO articles_fts(articles_fts, rowid, title, byline, site_name, content_text, tags, note)
    VALUES ('delete', old.seq, old.title, old.byline, old.site_name, old.content_text, old.tags, old.note);
END;

CREATE TRIGGER IF NOT EXISTS articles_fts_after_update AFTER UPDATE ON articles BEGIN
    INSERT INTO articles_fts(articles_fts, rowid, title, byline, site_name, content_text, tags, note)
    VALUES ('delete', old.seq, old.title, old.byline, old.site_name, old.content_text, old.tags, old.note);
    INSERT INTO articles_fts(rowid, title, byline, site_name, content_text, tags, note)
    VALUES (new.seq, new.title, new.byline, new.site_name, new.content_text, new.tags, new.note);
END;

-- sources: the article radar's list of where to look for new reading. A watch
-- is one publication, checked for new posts by the store itself; research and
-- scout are prompts a scheduled agent session works (scripts/
-- article-radar-dispatch.sh). A proposed source never runs: the due query
-- asks for status 'active', and only a person moves a source there.
CREATE TABLE IF NOT EXISTS sources (
    id              TEXT PRIMARY KEY,               -- source_000001
    seq             INTEGER NOT NULL UNIQUE,        -- from id_sequences
    kind            TEXT NOT NULL,                  -- watch | research | scout
    name            TEXT NOT NULL DEFAULT '',       -- display only
    status          TEXT NOT NULL DEFAULT 'proposed', -- active | proposed | rejected
    enabled         INTEGER NOT NULL DEFAULT 1,     -- pause switch, apart from status
    cadence_hours   INTEGER NOT NULL,
    next_run_at     INTEGER NOT NULL DEFAULT 0,
    last_run_at     INTEGER NOT NULL DEFAULT 0,
    last_result     TEXT NOT NULL DEFAULT '',
    publication_id  TEXT NOT NULL DEFAULT '',       -- watch: publications.id, once there is one
    platform        TEXT NOT NULL DEFAULT '',       -- watch: the platform of a blog not yet a publication
    base_url        TEXT NOT NULL DEFAULT '',       -- watch: its root URL, normalized
    prompt          TEXT NOT NULL DEFAULT '',       -- research: the topic; scout: what to look for
    notes           TEXT NOT NULL DEFAULT '',       -- what the proposer checked, and why
    proposed_by     TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sources_due ON sources(status, enabled, next_run_at);

-- suggestions: an article an agent thinks is worth reading, not yet saved.
-- Accepting one saves the article through the ordinary save path; a URL is
-- suggested once, and a dismissed one is never suggested again.
CREATE TABLE IF NOT EXISTS suggestions (
    id          TEXT PRIMARY KEY,                   -- suggestion_000001
    seq         INTEGER NOT NULL UNIQUE,            -- from id_sequences
    url         TEXT NOT NULL UNIQUE,               -- normalized as articles.url
    title       TEXT NOT NULL DEFAULT '',
    byline      TEXT NOT NULL DEFAULT '',
    site_name   TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL,                      -- why the agent picked it
    source_id   TEXT NOT NULL DEFAULT '',           -- sources.id of what found it; '' = none
    status      TEXT NOT NULL DEFAULT 'proposed',   -- proposed | accepted | dismissed
    article_id  TEXT NOT NULL DEFAULT '',           -- articles.id, once accepted
    decided_at  INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_suggestions_status ON suggestions(status, created_at);
