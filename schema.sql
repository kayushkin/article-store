PRAGMA foreign_keys = ON;

-- Create-only. Every statement here is IF NOT EXISTS, so this file is what an
-- empty database gets and nothing more: a column added to a table that already
-- exists on this host will never appear by editing this file. Add such a column
-- in Open(), with an ALTER TABLE guarded by a check that it is missing.

-- id_sequences: the last number handed out, per kind of id. A number is never
-- handed out twice, even after a purge, because an id may already be written on
-- a card or in a chat, and it must never come to mean a different article.
CREATE TABLE IF NOT EXISTS id_sequences (
    name TEXT PRIMARY KEY,
    last INTEGER NOT NULL
);
INSERT OR IGNORE INTO id_sequences (name, last) VALUES ('article', 0);

-- articles: one piece of writing saved from the web, and the text we pulled out
-- of it. The page as fetched (source_html) is kept beside the extracted text, so
-- a better extractor can be run over it later without the page having to still
-- exist.
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
    source_html      TEXT NOT NULL DEFAULT '',    -- the page as fetched, unsanitized; never rendered
    note             TEXT NOT NULL DEFAULT '',    -- your own commentary, never the author's
    tags             TEXT NOT NULL DEFAULT '[]',  -- JSON array
    added_by         TEXT NOT NULL DEFAULT '',
    read_at          INTEGER NOT NULL DEFAULT 0,  -- 0 = unread
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
