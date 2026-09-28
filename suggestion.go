package articlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Suggestion statuses. A suggestion is proposed until a person accepts it,
// which saves the article, or dismisses it, which keeps its URL from ever
// being suggested again.
const (
	SuggestionStatusProposed  = "proposed"
	SuggestionStatusAccepted  = "accepted"
	SuggestionStatusDismissed = "dismissed"
)

// SuggestionStatuses is the vocabulary, served by GET /vocabulary.
var SuggestionStatuses = []string{SuggestionStatusProposed, SuggestionStatusAccepted, SuggestionStatusDismissed}

// ErrAlreadySaved is returned when a suggested URL is already an article,
// live or deleted. It answers 409.
var ErrAlreadySaved = errors.New("already saved")

// Suggestion is an article an agent thinks is worth reading, not yet saved.
type Suggestion struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Byline   string `json:"byline"`
	SiteName string `json:"site_name"`
	// Reason is why the agent picked it.
	Reason string `json:"reason"`
	// SourceID is the source that found it, or empty.
	SourceID string `json:"source_id"`
	Status   string `json:"status"`
	// ArticleID is the article it was saved as, once accepted.
	ArticleID string `json:"article_id"`
	DecidedAt int64  `json:"decided_at"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// SuggestionRequest is what a caller sends to suggest an article.
type SuggestionRequest struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Byline   string `json:"byline"`
	SiteName string `json:"site_name"`
	Reason   string `json:"reason"`
	SourceID string `json:"source_id"`
}

// SuggestionFilter narrows a listing of suggestions.
type SuggestionFilter struct {
	Status   string
	SourceID string
	Limit    int
}

func formatSuggestionID(seq int64) string { return fmt.Sprintf("suggestion_%06d", seq) }

var suggestionIDPattern = regexp.MustCompile(`^suggestion_\d{6,}$`)

func checkSuggestionID(id string) error {
	if !suggestionIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q is not a suggestion id (suggestion_000001)", ErrInvalidArticle, id)
	}
	return nil
}

const suggestionColumns = `id, url, title, byline, site_name, reason, source_id, status, article_id, decided_at, created_at, updated_at`

func scanSuggestion(row scanner) (Suggestion, error) {
	var suggestion Suggestion
	err := row.Scan(&suggestion.ID, &suggestion.URL, &suggestion.Title, &suggestion.Byline, &suggestion.SiteName,
		&suggestion.Reason, &suggestion.SourceID, &suggestion.Status, &suggestion.ArticleID, &suggestion.DecidedAt,
		&suggestion.CreatedAt, &suggestion.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Suggestion{}, ErrNotFound
	}
	return suggestion, err
}

// findSuggested looks a URL up among suggestions under the same equivalent
// forms a backfill treats as one post.
func (s *Store) findSuggested(normalizedURL string) (Suggestion, bool, error) {
	for _, candidate := range equivalentPostURLs(normalizedURL) {
		suggestion, err := scanSuggestion(s.db.QueryRow(`SELECT `+suggestionColumns+` FROM suggestions WHERE url = ?`, candidate))
		if err == nil {
			return suggestion, true, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Suggestion{}, false, err
		}
	}
	return Suggestion{}, false, nil
}

// AddSuggestion stores a suggestion and returns it with true. A URL already
// suggested — proposed, accepted or dismissed — returns that suggestion
// unchanged and false. A URL already saved as an article, even a deleted one,
// is ErrAlreadySaved. Nothing is fetched.
func (s *Store) AddSuggestion(request SuggestionRequest) (Suggestion, bool, error) {
	normalized, err := NormalizeURL(request.URL)
	if err != nil {
		return Suggestion{}, false, err
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Reason == "" {
		return Suggestion{}, false, fmt.Errorf("%w: a suggestion needs a reason: why this article is worth reading", ErrInvalidArticle)
	}
	if request.SourceID != "" {
		if _, err := s.GetRadarSource(request.SourceID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return Suggestion{}, false, fmt.Errorf("%w: source_id %s is not a source", ErrInvalidArticle, request.SourceID)
			}
			return Suggestion{}, false, err
		}
	}
	if existing, found, err := s.findSuggested(normalized); err != nil || found {
		return existing, false, err
	}
	article, found, err := s.findSavedPost(normalized)
	if errors.Is(err, ErrDeleted) {
		return Suggestion{}, false, fmt.Errorf("%w: %v", ErrAlreadySaved, err)
	}
	if err != nil {
		return Suggestion{}, false, err
	}
	if found {
		return Suggestion{}, false, fmt.Errorf("%w: %s is saved as %s", ErrAlreadySaved, normalized, article.ID)
	}

	transaction, err := s.db.Begin()
	if err != nil {
		return Suggestion{}, false, err
	}
	defer transaction.Rollback()
	var seq int64
	if err := transaction.QueryRow(`UPDATE id_sequences SET last = last + 1 WHERE name = 'suggestion' RETURNING last`).Scan(&seq); err != nil {
		return Suggestion{}, false, err
	}
	id := formatSuggestionID(seq)
	timestamp := now()
	if _, err := transaction.Exec(`INSERT INTO suggestions (id, seq, url, title, byline, site_name, reason, source_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, seq, normalized, strings.TrimSpace(request.Title),
		strings.TrimSpace(request.Byline), strings.TrimSpace(request.SiteName), request.Reason, request.SourceID,
		timestamp, timestamp); err != nil {
		return Suggestion{}, false, err
	}
	if err := transaction.Commit(); err != nil {
		return Suggestion{}, false, err
	}
	created, err := s.GetSuggestion(id)
	return created, true, err
}

// GetSuggestion returns one suggestion.
func (s *Store) GetSuggestion(id string) (Suggestion, error) {
	if err := checkSuggestionID(id); err != nil {
		return Suggestion{}, err
	}
	suggestion, err := scanSuggestion(s.db.QueryRow(`SELECT `+suggestionColumns+` FROM suggestions WHERE id = ?`, id))
	if errors.Is(err, ErrNotFound) {
		return Suggestion{}, fmt.Errorf("%w: suggestion %s", ErrNotFound, id)
	}
	return suggestion, err
}

// ListSuggestions lists suggestions newest first.
func (s *Store) ListSuggestions(filter SuggestionFilter) ([]Suggestion, error) {
	clauses := []string{}
	arguments := []any{}
	if filter.Status != "" {
		if !contains(SuggestionStatuses, filter.Status) {
			return nil, fmt.Errorf("%w: status %q is not one of %v", ErrInvalidArticle, filter.Status, SuggestionStatuses)
		}
		clauses = append(clauses, `status = ?`)
		arguments = append(arguments, filter.Status)
	}
	if filter.SourceID != "" {
		if err := checkRadarSourceID(filter.SourceID); err != nil {
			return nil, err
		}
		clauses = append(clauses, `source_id = ?`)
		arguments = append(arguments, filter.SourceID)
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := ""
	if len(clauses) > 0 {
		where = ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	rows, err := s.db.Query(`SELECT `+suggestionColumns+` FROM suggestions`+where+` ORDER BY seq DESC LIMIT ?`, append(arguments, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	suggestions := []Suggestion{}
	for rows.Next() {
		suggestion, err := scanSuggestion(rows)
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, suggestion)
	}
	return suggestions, rows.Err()
}

// AcceptSuggestion saves the suggested article through the ordinary save path,
// with added_by naming the suggestion, and marks the suggestion accepted. The
// suggestion's title, byline and site name, when set, replace what extraction
// finds, as a caller's would. Accepting one already accepted returns its
// article again. A failed fetch leaves the suggestion proposed.
func (s *Store) AcceptSuggestion(ctx context.Context, id string) (Suggestion, Article, error) {
	suggestion, err := s.GetSuggestion(id)
	if err != nil {
		return Suggestion{}, Article{}, err
	}
	switch suggestion.Status {
	case SuggestionStatusAccepted:
		article, err := s.GetArticle(suggestion.ArticleID)
		return suggestion, article, err
	case SuggestionStatusDismissed:
		return Suggestion{}, Article{}, fmt.Errorf("%w: suggestion %s was dismissed", ErrInvalidArticle, id)
	}
	article, _, err := s.SaveArticle(ctx, SaveRequest{
		URL: suggestion.URL, Title: suggestion.Title, Byline: suggestion.Byline, SiteName: suggestion.SiteName,
		AddedBy: "suggestion " + suggestion.ID,
	})
	if err != nil {
		return Suggestion{}, Article{}, err
	}
	timestamp := now()
	if _, err := s.db.Exec(`UPDATE suggestions SET status = 'accepted', article_id = ?, decided_at = ?, updated_at = ? WHERE id = ?`,
		article.ID, timestamp, timestamp, id); err != nil {
		return Suggestion{}, Article{}, err
	}
	accepted, err := s.GetSuggestion(id)
	return accepted, article, err
}

// DismissSuggestion marks a proposed suggestion dismissed. Its URL is never
// suggested again. Dismissing an accepted one is refused: the article exists,
// and deleting it is how to be rid of it.
func (s *Store) DismissSuggestion(id string) (Suggestion, error) {
	suggestion, err := s.GetSuggestion(id)
	if err != nil {
		return Suggestion{}, err
	}
	switch suggestion.Status {
	case SuggestionStatusDismissed:
		return suggestion, nil
	case SuggestionStatusAccepted:
		return Suggestion{}, fmt.Errorf("%w: suggestion %s was accepted as %s; delete the article instead", ErrInvalidArticle, id, suggestion.ArticleID)
	}
	timestamp := now()
	if _, err := s.db.Exec(`UPDATE suggestions SET status = 'dismissed', decided_at = ?, updated_at = ? WHERE id = ?`,
		timestamp, timestamp, id); err != nil {
		return Suggestion{}, err
	}
	return s.GetSuggestion(id)
}
