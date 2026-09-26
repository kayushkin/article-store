// Package articlestore keeps pieces of writing saved from the web — articles,
// essays, stories — with the text pulled out of each page, so they can be read,
// searched and handed to an agent by id after the page itself is gone.
package articlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// ErrNotFound is returned when a lookup finds no row.
var ErrNotFound = errors.New("not found")

// ErrInvalidArticle marks a request the caller got wrong — no URL, a kind
// outside the vocabulary, a malformed id — so the HTTP layer answers 400.
var ErrInvalidArticle = errors.New("invalid article")

// ErrDeleted is returned when saving a URL whose article was soft-deleted.
// Saving it again silently would bring back a row someone chose to remove; the
// caller restores it, or purges it and saves afresh.
var ErrDeleted = errors.New("article is deleted")

// ArticleSummary is an article without its content: what a listing returns.
type ArticleSummary struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	FinalURL    string   `json:"final_url"`
	Title       string   `json:"title"`
	Byline      string   `json:"byline"`
	SiteName    string   `json:"site_name"`
	Language    string   `json:"language"`
	PublishedAt int64    `json:"published_at"`
	Excerpt     string   `json:"excerpt"`
	Kind        string   `json:"kind"`
	WordCount   int      `json:"word_count"`
	Note        string   `json:"note"`
	Tags        []string `json:"tags"`
	AddedBy     string   `json:"added_by"`
	ReadAt      int64    `json:"read_at"`
	FetchedAt   int64    `json:"fetched_at"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
	DeletedAt   int64    `json:"deleted_at"`
	// Snippet is set only on a search, and marks the matched words with <mark>.
	// It is computed by the read path and never stored.
	Snippet string `json:"snippet,omitempty"`
}

// Article is one saved article with its content. The page as fetched is not
// here: GET /articles/{id}/source serves it, because it is large and unsafe to
// render.
type Article struct {
	ArticleSummary
	ContentHTML     string `json:"content_html"`
	ContentMarkdown string `json:"content_markdown"`
	ContentText     string `json:"content_text"`
}

// SaveRequest is what a caller sends to save an article. Only URL is required.
// With SourceHTML the store extracts from that instead of fetching the URL —
// the way to save a page behind a login. Title, Byline, SiteName and
// PublishedAt, when set, replace what extraction found.
type SaveRequest struct {
	URL         string   `json:"url"`
	SourceHTML  string   `json:"source_html"`
	Title       string   `json:"title"`
	Byline      string   `json:"byline"`
	SiteName    string   `json:"site_name"`
	PublishedAt int64    `json:"published_at"`
	Kind        string   `json:"kind"`
	Note        string   `json:"note"`
	Tags        []string `json:"tags"`
	AddedBy     string   `json:"added_by"`
}

// ArticlePatch is a partial update. A nil field is left alone. Read sets
// read_at to now when true and back to 0 when false.
type ArticlePatch struct {
	Title       *string   `json:"title"`
	Byline      *string   `json:"byline"`
	SiteName    *string   `json:"site_name"`
	PublishedAt *int64    `json:"published_at"`
	Kind        *string   `json:"kind"`
	Note        *string   `json:"note"`
	Tags        *[]string `json:"tags"`
	AddedBy     *string   `json:"added_by"`
	Read        *bool     `json:"read"`
}

// ArticleFilter narrows a listing. A zero value lists every live article,
// newest first.
type ArticleFilter struct {
	Query          string // fts5 MATCH over title, byline, site, text, tags, note
	Tag            string
	Kind           string
	Read           *bool
	Limit          int
	Offset         int
	IncludeDeleted bool
}

// TagCount is one tag and how many live articles carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Store wraps the SQLite database and the HTTP client that fetches pages.
type Store struct {
	db         *sql.DB
	dataDir    string
	httpClient *http.Client
}

// DefaultDataDir is where the DB lives when ARTICLE_STORE_DATA_DIR is unset.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "article-store")
}

// Open opens (and migrates) the store. httpClient fetches pages.
func Open(dataDir string, httpClient *http.Client) (*Store, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "article-store.db")+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		// mattn/go-sqlite3 compiles FTS5 in only when the build asks for it.
		// The fix is a build flag, not a package, so say so.
		if strings.Contains(err.Error(), "no such module: fts5") {
			return nil, fmt.Errorf(
				"migrate: %w — this binary was built without FTS5; rebuild with: go build -tags sqlite_fts5 ./cmd/article-store", err)
		}
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, dataDir: dataDir, httpClient: httpClient}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DataDir reports where this store keeps its database.
func (s *Store) DataDir() string { return s.dataDir }

func now() int64 { return time.Now().Unix() }

// formatID renders a sequence number as the public id. The prefix lets dash's
// resolver and kanban-store's entity-type registry recognise an article id in
// prose without claiming every number or uuid.
func formatID(seq int64) string { return fmt.Sprintf("article_%06d", seq) }

var articleIDPattern = regexp.MustCompile(`^article_\d{6,}$`)

func checkID(id string) error {
	if !articleIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q is not an article id (article_000001)", ErrInvalidArticle, id)
	}
	return nil
}

// NormalizeURL is the dedupe key for a saved page: the URL trimmed, with its
// fragment dropped, since #comments and the article are one page. Nothing else
// is rewritten; two URLs that differ in path or query may be different pages.
func NormalizeURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("%w: %q is not an http or https URL", ErrInvalidArticle, raw)
	}
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String(), nil
}

func encodeTags(tags []string) string {
	// "Rationalism", "rationalism" and " rationalism " are one tag, kept in the
	// order given minus duplicates.
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		clean := strings.TrimSpace(strings.ToLower(tag))
		if clean == "" || seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		panic(fmt.Sprintf("encode tags: %v", err))
	}
	return string(raw)
}

func decodeTags(id, raw string) ([]string, error) {
	tags := []string{}
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil, fmt.Errorf("article %s has tags that are not a JSON array: %w", id, err)
	}
	return tags, nil
}

// SaveArticle fetches request.URL (or reads request.SourceHTML), extracts the
// article and stores it. A URL already saved returns the stored article and
// false, without fetching again; POST /articles/{id}/refetch is how a stored
// article's text is replaced.
func (s *Store) SaveArticle(ctx context.Context, request SaveRequest) (Article, bool, error) {
	normalized, err := NormalizeURL(request.URL)
	if err != nil {
		return Article{}, false, err
	}
	kind := request.Kind
	if kind == "" {
		kind = DefaultArticleKind
	}
	if !IsArticleKind(kind) {
		return Article{}, false, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalidArticle, kind, ArticleKinds)
	}
	if existing, err := s.getArticleByURL(normalized); err == nil {
		if existing.DeletedAt != 0 {
			return Article{}, false, fmt.Errorf("%w: %s was saved as %s and deleted; restore it or purge it first", ErrDeleted, normalized, existing.ID)
		}
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Article{}, false, err
	}

	extraction, err := s.extractFrom(ctx, normalized, request.SourceHTML)
	if err != nil {
		return Article{}, false, err
	}
	if request.Title != "" {
		extraction.Title = request.Title
	}
	if request.Byline != "" {
		extraction.Byline = request.Byline
	}
	if request.SiteName != "" {
		extraction.SiteName = request.SiteName
	}
	if request.PublishedAt != 0 {
		extraction.PublishedAt = request.PublishedAt
	}

	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Article{}, false, err
	}
	defer transaction.Rollback()
	var seq int64
	if err := transaction.QueryRow(`UPDATE id_sequences SET last = last + 1 WHERE name = 'article' RETURNING last`).Scan(&seq); err != nil {
		return Article{}, false, err
	}
	id := formatID(seq)
	timestamp := now()
	_, err = transaction.Exec(`
		INSERT INTO articles (id, seq, url, final_url, title, byline, site_name, language, published_at,
			excerpt, kind, content_html, content_markdown, content_text, word_count, source_html,
			note, tags, added_by, fetched_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, seq, normalized, extraction.FinalURL, extraction.Title, extraction.Byline, extraction.SiteName,
		extraction.Language, extraction.PublishedAt, extraction.Excerpt, kind, extraction.ContentHTML,
		extraction.ContentMarkdown, extraction.ContentText, extraction.WordCount, extraction.SourceHTML,
		request.Note, encodeTags(request.Tags), request.AddedBy, timestamp, timestamp, timestamp)
	var sqliteError sqlite3.Error
	if errors.As(err, &sqliteError) && sqliteError.ExtendedCode == sqlite3.ErrConstraintUnique {
		// Another save of the same URL landed while this one was fetching.
		transaction.Rollback()
		existing, err := s.getArticleByURL(normalized)
		return existing, false, err
	}
	if err != nil {
		return Article{}, false, err
	}
	if err := transaction.Commit(); err != nil {
		return Article{}, false, err
	}
	saved, err := s.GetArticle(id)
	return saved, true, err
}

// RefetchArticle fetches the article's URL again (or reads sourceHTML) and
// replaces its content. The fields a person may have corrected — title, byline,
// site, date, kind, note, tags, read state — are left alone.
func (s *Store) RefetchArticle(ctx context.Context, id, sourceHTML string) (Article, error) {
	existing, err := s.GetArticle(id)
	if err != nil {
		return Article{}, err
	}
	extraction, err := s.extractFrom(ctx, existing.URL, sourceHTML)
	if err != nil {
		return Article{}, err
	}
	timestamp := now()
	_, err = s.db.ExecContext(ctx, `
		UPDATE articles SET final_url = ?, excerpt = ?, content_html = ?, content_markdown = ?,
			content_text = ?, word_count = ?, source_html = ?, fetched_at = ?, updated_at = ?
		WHERE id = ?`,
		extraction.FinalURL, extraction.Excerpt, extraction.ContentHTML, extraction.ContentMarkdown,
		extraction.ContentText, extraction.WordCount, extraction.SourceHTML, timestamp, timestamp, id)
	if err != nil {
		return Article{}, err
	}
	return s.GetArticle(id)
}

func (s *Store) extractFrom(ctx context.Context, pageURL, sourceHTML string) (Extraction, error) {
	finalURL := pageURL
	if sourceHTML == "" {
		var err error
		sourceHTML, finalURL, err = FetchPage(ctx, s.httpClient, pageURL)
		if err != nil {
			return Extraction{}, err
		}
	}
	return Extract(sourceHTML, finalURL)
}

const summaryColumns = `id, url, final_url, title, byline, site_name, language, published_at, excerpt,
	kind, word_count, note, tags, added_by, read_at, fetched_at, created_at, updated_at, deleted_at`

type scanner interface{ Scan(...any) error }

func scanSummary(row scanner, extra ...any) (ArticleSummary, error) {
	var summary ArticleSummary
	var tags string
	destinations := append([]any{&summary.ID, &summary.URL, &summary.FinalURL, &summary.Title, &summary.Byline,
		&summary.SiteName, &summary.Language, &summary.PublishedAt, &summary.Excerpt, &summary.Kind,
		&summary.WordCount, &summary.Note, &tags, &summary.AddedBy, &summary.ReadAt, &summary.FetchedAt,
		&summary.CreatedAt, &summary.UpdatedAt, &summary.DeletedAt}, extra...)
	if err := row.Scan(destinations...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ArticleSummary{}, ErrNotFound
		}
		return ArticleSummary{}, err
	}
	decoded, err := decodeTags(summary.ID, tags)
	if err != nil {
		return ArticleSummary{}, err
	}
	summary.Tags = decoded
	return summary, nil
}

func (s *Store) getArticleWhere(where string, argument any) (Article, error) {
	var article Article
	summary, err := scanSummary(s.db.QueryRow(
		`SELECT `+summaryColumns+`, content_html, content_markdown, content_text FROM articles WHERE `+where, argument),
		&article.ContentHTML, &article.ContentMarkdown, &article.ContentText)
	if err != nil {
		return Article{}, err
	}
	article.ArticleSummary = summary
	return article, nil
}

// GetArticle returns one article with its content, deleted or not; deleted_at
// says which.
func (s *Store) GetArticle(id string) (Article, error) {
	if err := checkID(id); err != nil {
		return Article{}, err
	}
	article, err := s.getArticleWhere(`id = ?`, id)
	if errors.Is(err, ErrNotFound) {
		return Article{}, fmt.Errorf("%w: article %s", ErrNotFound, id)
	}
	return article, err
}

func (s *Store) getArticleByURL(normalizedURL string) (Article, error) {
	return s.getArticleWhere(`url = ?`, normalizedURL)
}

// GetSource returns the page as it was fetched.
func (s *Store) GetSource(id string) (string, error) {
	if err := checkID(id); err != nil {
		return "", err
	}
	var source string
	err := s.db.QueryRow(`SELECT source_html FROM articles WHERE id = ?`, id).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: article %s", ErrNotFound, id)
	}
	return source, err
}

func (filter ArticleFilter) where() (string, []any) {
	clauses := []string{}
	arguments := []any{}
	if !filter.IncludeDeleted {
		clauses = append(clauses, `a.deleted_at = 0`)
	}
	if filter.Query != "" {
		clauses = append(clauses, `articles_fts MATCH ?`)
		arguments = append(arguments, filter.Query)
	}
	if filter.Tag != "" {
		clauses = append(clauses, `EXISTS (SELECT 1 FROM json_each(a.tags) WHERE json_each.value = ?)`)
		arguments = append(arguments, strings.ToLower(strings.TrimSpace(filter.Tag)))
	}
	if filter.Kind != "" {
		clauses = append(clauses, `a.kind = ?`)
		arguments = append(arguments, filter.Kind)
	}
	if filter.Read != nil {
		if *filter.Read {
			clauses = append(clauses, `a.read_at <> 0`)
		} else {
			clauses = append(clauses, `a.read_at = 0`)
		}
	}
	if len(clauses) == 0 {
		return "", arguments
	}
	return " WHERE " + strings.Join(clauses, " AND "), arguments
}

func (filter ArticleFilter) from() string {
	if filter.Query != "" {
		return ` FROM articles a JOIN articles_fts ON articles_fts.rowid = a.seq`
	}
	return ` FROM articles a`
}

// ListArticles lists summaries: by relevance on a search, newest first
// otherwise.
func (s *Store) ListArticles(filter ArticleFilter) ([]ArticleSummary, error) {
	if filter.Kind != "" && !IsArticleKind(filter.Kind) {
		return nil, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalidArticle, filter.Kind, ArticleKinds)
	}
	if err := s.checkSearchQuery(filter.Query); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	columns := "a." + strings.ReplaceAll(summaryColumns, ", ", ", a.")
	order := ` ORDER BY a.created_at DESC, a.seq DESC`
	if filter.Query != "" {
		// Column 3 of articles_fts is content_text.
		columns += `, snippet(articles_fts, 3, '<mark>', '</mark>', '…', 24)`
		order = ` ORDER BY rank`
	}
	where, arguments := filter.where()
	rows, err := s.db.Query(`SELECT `+columns+filter.from()+where+order+` LIMIT ? OFFSET ?`,
		append(arguments, limit, filter.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	summaries := []ArticleSummary{}
	for rows.Next() {
		var snippet string
		extra := []any{}
		if filter.Query != "" {
			extra = append(extra, &snippet)
		}
		summary, err := scanSummary(rows, extra...)
		if err != nil {
			return nil, err
		}
		summary.Snippet = snippet
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

// CountArticles counts what ListArticles would list, ignoring limit and offset.
func (s *Store) CountArticles(filter ArticleFilter) (int, error) {
	if err := s.checkSearchQuery(filter.Query); err != nil {
		return 0, err
	}
	where, arguments := filter.where()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*)`+filter.from()+where, arguments...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// checkSearchQuery runs the query alone against the index. A query fts5 cannot
// parse fails here, and that is the caller's mistake, not this service's.
func (s *Store) checkSearchQuery(query string) error {
	if query == "" {
		return nil
	}
	// fts5 parses the query when the statement first steps, so the row must be
	// read for a bad query to fail.
	var matches int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM articles_fts WHERE articles_fts MATCH ?`, query).Scan(&matches); err != nil {
		return fmt.Errorf("%w: search query %q: %v", ErrInvalidArticle, query, err)
	}
	return nil
}

// PatchArticle applies a partial update.
func (s *Store) PatchArticle(id string, patch ArticlePatch) (Article, error) {
	existing, err := s.GetArticle(id)
	if err != nil {
		return Article{}, err
	}
	sets := []string{}
	arguments := []any{}
	set := func(column string, value any) {
		sets = append(sets, column+" = ?")
		arguments = append(arguments, value)
	}
	if patch.Title != nil {
		set("title", *patch.Title)
	}
	if patch.Byline != nil {
		set("byline", *patch.Byline)
	}
	if patch.SiteName != nil {
		set("site_name", *patch.SiteName)
	}
	if patch.PublishedAt != nil {
		set("published_at", *patch.PublishedAt)
	}
	if patch.Kind != nil {
		if !IsArticleKind(*patch.Kind) {
			return Article{}, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalidArticle, *patch.Kind, ArticleKinds)
		}
		set("kind", *patch.Kind)
	}
	if patch.Note != nil {
		set("note", *patch.Note)
	}
	if patch.Tags != nil {
		set("tags", encodeTags(*patch.Tags))
	}
	if patch.AddedBy != nil {
		set("added_by", *patch.AddedBy)
	}
	if patch.Read != nil {
		switch {
		case *patch.Read && existing.ReadAt == 0:
			set("read_at", now())
		case !*patch.Read:
			set("read_at", 0)
		}
	}
	if len(sets) == 0 {
		return existing, nil
	}
	set("updated_at", now())
	if _, err := s.db.Exec(`UPDATE articles SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(arguments, id)...); err != nil {
		return Article{}, err
	}
	return s.GetArticle(id)
}

// SoftDeleteArticle hides an article from listings and search; it stays
// readable by id and RestoreArticle brings it back.
func (s *Store) SoftDeleteArticle(id string) error {
	return s.setDeletedAt(id, now())
}

// RestoreArticle undoes SoftDeleteArticle.
func (s *Store) RestoreArticle(id string) (Article, error) {
	if err := s.setDeletedAt(id, 0); err != nil {
		return Article{}, err
	}
	return s.GetArticle(id)
}

func (s *Store) setDeletedAt(id string, deletedAt int64) error {
	if err := checkID(id); err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE articles SET deleted_at = ?, updated_at = ? WHERE id = ?`, deletedAt, now(), id)
	if err != nil {
		return err
	}
	return requireOneRow(result, id)
}

// PurgeArticle removes the row outright, freeing its URL to be saved again.
func (s *Store) PurgeArticle(id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	result, err := s.db.Exec(`DELETE FROM articles WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return requireOneRow(result, id)
}

func requireOneRow(result sql.Result, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: article %s", ErrNotFound, id)
	}
	return nil
}

// ListTags counts the tags on live articles, most used first.
func (s *Store) ListTags() ([]TagCount, error) {
	rows, err := s.db.Query(`
		SELECT json_each.value, COUNT(*) FROM articles, json_each(articles.tags)
		WHERE articles.deleted_at = 0
		GROUP BY json_each.value ORDER BY COUNT(*) DESC, json_each.value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := []TagCount{}
	for rows.Next() {
		var tag TagCount
		if err := rows.Scan(&tag.Tag, &tag.Count); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}
