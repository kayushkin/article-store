package articlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// PublicationPlatforms is the vocabulary a publication's platform may take. A
// platform is here only when this store can backfill its archive.
var PublicationPlatforms = []string{"substack"}

// Backfill statuses. The empty status means no backfill was ever started.
const (
	BackfillStatusRunning = "running"
	BackfillStatusDone    = "done"
	BackfillStatusFailed  = "failed"
	BackfillStatusStopped = "stopped"
)

// ArchivePageSize is how many archive entries one archive request asks for.
const ArchivePageSize = 50

// Publication is a newsletter or blog whose posts are saved here.
type Publication struct {
	ID                     string `json:"id"`
	Platform               string `json:"platform"`
	BaseURL                string `json:"base_url"`
	Name                   string `json:"name"`
	PlatformPublicationRef string `json:"platform_publication_ref"`
	Backfill               struct {
		Status        string `json:"status"`
		Offset        int    `json:"offset"`
		PostsSaved    int    `json:"posts_saved"`
		PostsAlready  int    `json:"posts_already_saved"`
		PostsSkipped  int    `json:"posts_skipped"`
		PostsFailed   int    `json:"posts_failed"`
		LastError     string `json:"last_error"`
		StartedAt     int64  `json:"started_at"`
		FinishedAt    int64  `json:"finished_at"`
		NextAttemptAt int64  `json:"next_attempt_at"`
	} `json:"backfill"`
	// ArticleCount is computed by the read path and never stored.
	ArticleCount int   `json:"article_count"`
	CreatedAt    int64 `json:"created_at"`
	UpdatedAt    int64 `json:"updated_at"`
}

// PublicationRequest is what a caller sends to add a publication.
type PublicationRequest struct {
	Platform string `json:"platform"`
	BaseURL  string `json:"base_url"`
	Name     string `json:"name"`
}

func formatPublicationID(seq int64) string { return fmt.Sprintf("publication_%06d", seq) }

var publicationIDPattern = regexp.MustCompile(`^publication_\d{6,}$`)

func checkPublicationID(id string) error {
	if !publicationIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q is not a publication id (publication_000001)", ErrInvalidArticle, id)
	}
	return nil
}

// NormalizePublicationURL is the dedupe key for a publication: scheme and host,
// with no path, query or trailing slash.
func NormalizePublicationURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("%w: %q is not an http or https URL", ErrInvalidArticle, raw)
	}
	if strings.Trim(parsed.Path, "/") != "" || parsed.RawQuery != "" {
		return "", fmt.Errorf("%w: %q has a path or query; a publication is its site's root, such as https://noahpinion.substack.com", ErrInvalidArticle, raw)
	}
	return parsed.Scheme + "://" + strings.ToLower(parsed.Host), nil
}

const publicationColumns = `p.id, p.platform, p.base_url, p.name, p.platform_publication_ref,
	p.backfill_status, p.backfill_offset, p.backfill_posts_saved, p.backfill_posts_already,
	p.backfill_posts_skipped, p.backfill_posts_failed, p.backfill_last_error, p.backfill_started_at,
	p.backfill_finished_at, p.backfill_next_attempt_at, p.created_at, p.updated_at,
	(SELECT COUNT(*) FROM articles a WHERE a.publication_id = p.id AND a.deleted_at = 0)`

func scanPublication(row scanner) (Publication, error) {
	var publication Publication
	backfill := &publication.Backfill
	err := row.Scan(&publication.ID, &publication.Platform, &publication.BaseURL, &publication.Name,
		&publication.PlatformPublicationRef, &backfill.Status, &backfill.Offset, &backfill.PostsSaved,
		&backfill.PostsAlready, &backfill.PostsSkipped, &backfill.PostsFailed, &backfill.LastError,
		&backfill.StartedAt, &backfill.FinishedAt, &backfill.NextAttemptAt, &publication.CreatedAt,
		&publication.UpdatedAt, &publication.ArticleCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	return publication, err
}

// AddPublication stores a publication, or returns the one already stored at
// the same base URL and false.
func (s *Store) AddPublication(request PublicationRequest) (Publication, bool, error) {
	if !contains(PublicationPlatforms, request.Platform) {
		return Publication{}, false, fmt.Errorf("%w: platform %q is not one of %v", ErrInvalidArticle, request.Platform, PublicationPlatforms)
	}
	baseURL, err := NormalizePublicationURL(request.BaseURL)
	if err != nil {
		return Publication{}, false, err
	}
	existing, err := s.getPublicationWhere(`p.base_url = ?`, baseURL)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Publication{}, false, err
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return Publication{}, false, err
	}
	defer transaction.Rollback()
	var seq int64
	if err := transaction.QueryRow(`UPDATE id_sequences SET last = last + 1 WHERE name = 'publication' RETURNING last`).Scan(&seq); err != nil {
		return Publication{}, false, err
	}
	id := formatPublicationID(seq)
	timestamp := now()
	if _, err := transaction.Exec(`INSERT INTO publications (id, seq, platform, base_url, name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, seq, request.Platform, baseURL, strings.TrimSpace(request.Name), timestamp, timestamp); err != nil {
		return Publication{}, false, err
	}
	if err := transaction.Commit(); err != nil {
		return Publication{}, false, err
	}
	created, err := s.GetPublication(id)
	return created, true, err
}

func (s *Store) getPublicationWhere(where string, argument any) (Publication, error) {
	return scanPublication(s.db.QueryRow(`SELECT `+publicationColumns+` FROM publications p WHERE `+where, argument))
}

// GetPublication returns one publication.
func (s *Store) GetPublication(id string) (Publication, error) {
	if err := checkPublicationID(id); err != nil {
		return Publication{}, err
	}
	publication, err := s.getPublicationWhere(`p.id = ?`, id)
	if errors.Is(err, ErrNotFound) {
		return Publication{}, fmt.Errorf("%w: publication %s", ErrNotFound, id)
	}
	return publication, err
}

// ListPublications lists every publication by name.
func (s *Store) ListPublications() ([]Publication, error) {
	rows, err := s.db.Query(`SELECT ` + publicationColumns + ` FROM publications p ORDER BY p.name COLLATE NOCASE, p.seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	publications := []Publication{}
	for rows.Next() {
		publication, err := scanPublication(rows)
		if err != nil {
			return nil, err
		}
		publications = append(publications, publication)
	}
	return publications, rows.Err()
}

// PatchPublicationName renames a publication.
func (s *Store) PatchPublicationName(id, name string) (Publication, error) {
	if err := checkPublicationID(id); err != nil {
		return Publication{}, err
	}
	result, err := s.db.Exec(`UPDATE publications SET name = ?, updated_at = ? WHERE id = ?`, strings.TrimSpace(name), now(), id)
	if err != nil {
		return Publication{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		if err != nil {
			return Publication{}, err
		}
		return Publication{}, fmt.Errorf("%w: publication %s", ErrNotFound, id)
	}
	return s.GetPublication(id)
}

// StartBackfill sets the publication's archive backfill running. A backfill
// that stopped part way carries on from its offset; restart begins again at
// the newest post, and posts already saved are counted, not fetched again.
func (s *Store) StartBackfill(id string, restart bool) (Publication, error) {
	publication, err := s.GetPublication(id)
	if err != nil {
		return Publication{}, err
	}
	if publication.Backfill.Status == BackfillStatusRunning && !restart {
		return publication, nil
	}
	timestamp := now()
	statement := `UPDATE publications SET backfill_status = 'running', backfill_last_error = '',
		backfill_finished_at = 0, backfill_next_attempt_at = 0, backfill_started_at = ?, updated_at = ?`
	if restart || publication.Backfill.Status == BackfillStatusDone {
		statement += `, backfill_offset = 0, backfill_posts_saved = 0, backfill_posts_already = 0,
			backfill_posts_skipped = 0, backfill_posts_failed = 0`
	}
	if _, err := s.db.Exec(statement+` WHERE id = ?`, timestamp, timestamp, id); err != nil {
		return Publication{}, err
	}
	return s.GetPublication(id)
}

// StopBackfill stops a running backfill where it is. StartBackfill carries on
// from there.
func (s *Store) StopBackfill(id string) (Publication, error) {
	if err := checkPublicationID(id); err != nil {
		return Publication{}, err
	}
	if _, err := s.db.Exec(`UPDATE publications SET backfill_status = 'stopped', updated_at = ?
		WHERE id = ? AND backfill_status = 'running'`, now(), id); err != nil {
		return Publication{}, err
	}
	return s.GetPublication(id)
}

// nextBackfillDue returns the running backfill that may ask its site for
// something now and has waited longest, or found false.
func (s *Store) nextBackfillDue() (Publication, bool, error) {
	publication, err := scanPublication(s.db.QueryRow(`SELECT `+publicationColumns+` FROM publications p
		WHERE p.backfill_status = 'running' AND p.backfill_next_attempt_at <= ?
		ORDER BY p.backfill_next_attempt_at, p.updated_at, p.seq LIMIT 1`, now()))
	if errors.Is(err, ErrNotFound) {
		return Publication{}, false, nil
	}
	return publication, err == nil, err
}

// backfillOutcome is what happened to one archive entry.
type backfillOutcome string

const (
	outcomeSaved   backfillOutcome = "backfill_posts_saved"
	outcomeAlready backfillOutcome = "backfill_posts_already"
	outcomeSkipped backfillOutcome = "backfill_posts_skipped"
	outcomeFailed  backfillOutcome = "backfill_posts_failed"
)

// recordEntry moves the offset past one archive entry and counts what happened
// to it, in one statement, so a restart never counts an entry twice.
func (s *Store) recordEntry(id string, outcome backfillOutcome, lastError string) error {
	statement := `UPDATE publications SET backfill_offset = backfill_offset + 1, ` + string(outcome) + ` = ` + string(outcome) + ` + 1, updated_at = ?`
	arguments := []any{now()}
	if lastError != "" {
		statement += `, backfill_last_error = ?`
		arguments = append(arguments, lastError)
	}
	_, err := s.db.Exec(statement+` WHERE id = ?`, append(arguments, id)...)
	return err
}

func (s *Store) setBackfillWait(id string, until time.Time, lastError string) error {
	_, err := s.db.Exec(`UPDATE publications SET backfill_next_attempt_at = ?, backfill_last_error = ?, updated_at = ? WHERE id = ?`,
		until.Unix(), lastError, now(), id)
	return err
}

func (s *Store) finishBackfill(id, status, lastError string) error {
	_, err := s.db.Exec(`UPDATE publications SET backfill_status = ?, backfill_last_error = ?, backfill_finished_at = ?, updated_at = ? WHERE id = ?`,
		status, lastError, now(), now(), id)
	return err
}

// Backfiller works through running archive backfills, one request at a time.
type Backfiller struct {
	Store           *Store
	RequestInterval time.Duration
	// IdleInterval is how long to wait before looking again when no backfill
	// is due.
	IdleInterval time.Duration
}

// Run works until ctx ends. An error it cannot pin on one publication — the
// database failing — is logged and retried after IdleInterval.
func (b *Backfiller) Run(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := b.Step(ctx)
		if err != nil {
			log.Printf("article-store: backfill: %v", err)
		}
		if !worked || err != nil {
			sleep(ctx, b.IdleInterval)
		}
	}
}

// Step handles one archive page of the backfill most due, and reports whether
// there was one.
func (b *Backfiller) Step(ctx context.Context) (bool, error) {
	publication, found, err := b.Store.nextBackfillDue()
	if err != nil || !found {
		return false, err
	}
	switch publication.Platform {
	case "substack":
		return true, b.stepSubstack(ctx, publication)
	default:
		return true, b.Store.finishBackfill(publication.ID, BackfillStatusFailed,
			fmt.Sprintf("platform %q has no backfill in this build", publication.Platform))
	}
}

func (b *Backfiller) stepSubstack(ctx context.Context, publication Publication) error {
	store := b.Store
	entries, err := FetchSubstackArchivePage(ctx, store.httpClient, publication.BaseURL, publication.Backfill.Offset, ArchivePageSize)
	if rateLimited, is := isRateLimited(err); is {
		return store.setBackfillWait(publication.ID, time.Now().Add(rateLimited.RetryAfter), rateLimited.Error())
	}
	if err != nil {
		return store.finishBackfill(publication.ID, BackfillStatusFailed, err.Error())
	}
	sleep(ctx, b.RequestInterval)
	if len(entries) == 0 {
		return store.finishBackfill(publication.ID, BackfillStatusDone, publication.Backfill.LastError)
	}
	if publication.PlatformPublicationRef == "" && entries[0].PublicationID != 0 {
		if _, err := store.db.Exec(`UPDATE publications SET platform_publication_ref = ? WHERE id = ?`,
			fmt.Sprint(entries[0].PublicationID), publication.ID); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil
		}
		// A stop request between entries ends the page here.
		current, err := store.GetPublication(publication.ID)
		if err != nil {
			return err
		}
		if current.Backfill.Status != BackfillStatusRunning {
			return nil
		}
		outcome, fetched, err := b.saveSubstackEntry(ctx, publication, entry)
		if rateLimited, is := isRateLimited(err); is {
			return store.setBackfillWait(publication.ID, time.Now().Add(rateLimited.RetryAfter), rateLimited.Error())
		}
		lastError := ""
		if err != nil {
			lastError = fmt.Sprintf("%s: %v", entry.CanonicalURL, err)
		}
		if err := store.recordEntry(publication.ID, outcome, lastError); err != nil {
			return err
		}
		if fetched {
			sleep(ctx, b.RequestInterval)
		}
	}
	return nil
}

// saveSubstackEntry saves one archive entry and reports what happened and
// whether it asked the site for anything.
func (b *Backfiller) saveSubstackEntry(ctx context.Context, publication Publication, entry SubstackArchiveEntry) (backfillOutcome, bool, error) {
	if !SubstackPostTypesSaved[entry.Type] {
		return outcomeSkipped, false, nil
	}
	normalized, err := NormalizeURL(entry.CanonicalURL)
	if err != nil {
		return outcomeFailed, false, err
	}
	_, found, err := b.Store.findSaved(normalized)
	if found || errors.Is(err, ErrDeleted) {
		// A deleted article was removed by someone; the backfill leaves it so.
		return outcomeAlready, false, nil
	}
	if err != nil {
		return outcomeFailed, false, err
	}
	post, err := FetchSubstackPost(ctx, b.Store.httpClient, normalized)
	if err != nil {
		return outcomeFailed, true, err
	}
	extraction, err := post.Extraction(normalized)
	if err != nil {
		return outcomeFailed, true, err
	}
	extraction.SiteName = publication.Name
	tags := []string{}
	if entry.IsPaidOnly() {
		tags = append(tags, SubstackPaidOnlyTag)
	}
	_, _, err = b.Store.insertArticle(ctx, normalized, extraction, savedFields{
		Kind: "post", Tags: tags, AddedBy: "backfill " + publication.ID, PublicationID: publication.ID,
	})
	if err != nil {
		return outcomeFailed, true, err
	}
	return outcomeSaved, true, nil
}

func sleep(ctx context.Context, duration time.Duration) {
	if duration <= 0 {
		return
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
