package articlestore

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The article radar's source kinds. A watch is one publication, checked for
// new posts by the store's own Watcher with no model. Research and scout are
// prompts that scripts/article-radar-dispatch.sh hands to a capped agent
// session; the store never branches on those two, and what each one means
// lives in that script's prompt.
const (
	RadarSourceKindWatch    = "watch"
	RadarSourceKindResearch = "research"
	RadarSourceKindScout    = "scout"
)

// RadarSourceKinds is the vocabulary, served by GET /vocabulary.
var RadarSourceKinds = []string{RadarSourceKindWatch, RadarSourceKindResearch, RadarSourceKindScout}

// Radar source statuses. Only a person moves a source to active.
const (
	RadarSourceStatusActive   = "active"
	RadarSourceStatusProposed = "proposed"
	RadarSourceStatusRejected = "rejected"
)

// RadarSourceStatuses is the vocabulary, served by GET /vocabulary.
var RadarSourceStatuses = []string{RadarSourceStatusActive, RadarSourceStatusProposed, RadarSourceStatusRejected}

// DefaultRadarSourceCadenceHours is the cadence a source gets when the caller
// names none. A watch costs a request or two and no model, so it runs daily;
// research and scout each cost an agent session, so they run weekly.
var DefaultRadarSourceCadenceHours = map[string]int{
	RadarSourceKindWatch:    24,
	RadarSourceKindResearch: 168,
	RadarSourceKindScout:    168,
}

// RadarSource is one place the radar looks for new reading.
type RadarSource struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Enabled      bool   `json:"enabled"`
	CadenceHours int    `json:"cadence_hours"`
	NextRunAt    int64  `json:"next_run_at"`
	LastRunAt    int64  `json:"last_run_at"`
	LastResult   string `json:"last_result"`
	// PublicationID is the publication a watch checks. A proposed watch of a
	// blog with no publication yet has Platform and BaseURL instead, and gets
	// its PublicationID when it is approved.
	PublicationID string `json:"publication_id"`
	Platform      string `json:"platform"`
	BaseURL       string `json:"base_url"`
	Prompt        string `json:"prompt"`
	Notes         string `json:"notes"`
	ProposedBy    string `json:"proposed_by"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// RadarSourceRequest is what a caller sends to add a source.
type RadarSourceRequest struct {
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	CadenceHours  int    `json:"cadence_hours"`
	PublicationID string `json:"publication_id"`
	Platform      string `json:"platform"`
	BaseURL       string `json:"base_url"`
	Prompt        string `json:"prompt"`
	Notes         string `json:"notes"`
	ProposedBy    string `json:"proposed_by"`
}

// RadarSourcePatch is a partial update, and the decision channel: a move to
// active approves the source. A nil field is left alone.
type RadarSourcePatch struct {
	Name         *string `json:"name"`
	Status       *string `json:"status"`
	Enabled      *bool   `json:"enabled"`
	CadenceHours *int    `json:"cadence_hours"`
	Prompt       *string `json:"prompt"`
	Notes        *string `json:"notes"`
}

// RadarSourceFilter narrows a listing of sources.
type RadarSourceFilter struct {
	Kind   string
	Status string
	// Due keeps only the sources that should run now: active, enabled, and
	// next_run_at passed. It is one clause, so no caller can leave out the
	// approval gate.
	Due bool
}

func formatRadarSourceID(seq int64) string { return fmt.Sprintf("source_%06d", seq) }

var radarSourceIDPattern = regexp.MustCompile(`^source_\d{6,}$`)

func checkRadarSourceID(id string) error {
	if !radarSourceIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q is not a source id (source_000001)", ErrInvalidArticle, id)
	}
	return nil
}

const radarSourceColumns = `id, kind, name, status, enabled, cadence_hours, next_run_at, last_run_at, last_result,
	publication_id, platform, base_url, prompt, notes, proposed_by, created_at, updated_at`

func scanRadarSource(row scanner) (RadarSource, error) {
	var source RadarSource
	err := row.Scan(&source.ID, &source.Kind, &source.Name, &source.Status, &source.Enabled, &source.CadenceHours,
		&source.NextRunAt, &source.LastRunAt, &source.LastResult, &source.PublicationID, &source.Platform,
		&source.BaseURL, &source.Prompt, &source.Notes, &source.ProposedBy, &source.CreatedAt, &source.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RadarSource{}, ErrNotFound
	}
	return source, err
}

// AddRadarSource stores a new source. A blank status means proposed, so a
// source nobody approved never runs. A watch of a publication that already has
// a watch answers that watch and false, whatever its status: a rejected watch
// stays rejected.
func (s *Store) AddRadarSource(request RadarSourceRequest) (RadarSource, bool, error) {
	request.Kind = strings.TrimSpace(request.Kind)
	if !contains(RadarSourceKinds, request.Kind) {
		return RadarSource{}, false, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalidArticle, request.Kind, RadarSourceKinds)
	}
	if request.Status == "" {
		request.Status = RadarSourceStatusProposed
	}
	if !contains(RadarSourceStatuses, request.Status) {
		return RadarSource{}, false, fmt.Errorf("%w: status %q is not one of %v", ErrInvalidArticle, request.Status, RadarSourceStatuses)
	}
	if request.CadenceHours < 0 {
		return RadarSource{}, false, fmt.Errorf("%w: cadence_hours %d is negative", ErrInvalidArticle, request.CadenceHours)
	}
	if request.CadenceHours == 0 {
		request.CadenceHours = DefaultRadarSourceCadenceHours[request.Kind]
	}
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.Kind == RadarSourceKindWatch {
		if request.Prompt != "" {
			return RadarSource{}, false, fmt.Errorf("%w: a watch takes no prompt; it checks one publication for new posts", ErrInvalidArticle)
		}
		if err := s.resolveWatchTarget(&request); err != nil {
			return RadarSource{}, false, err
		}
		if existing, found, err := s.findWatch(request.PublicationID, request.BaseURL); err != nil || found {
			return existing, false, err
		}
	} else {
		if request.Prompt == "" {
			return RadarSource{}, false, fmt.Errorf("%w: a %s source needs a prompt", ErrInvalidArticle, request.Kind)
		}
		if request.PublicationID != "" || request.Platform != "" || request.BaseURL != "" {
			return RadarSource{}, false, fmt.Errorf("%w: publication_id, platform and base_url belong to a watch, not a %s source", ErrInvalidArticle, request.Kind)
		}
	}
	if request.Status == RadarSourceStatusActive && request.PublicationID == "" && request.Kind == RadarSourceKindWatch {
		// Adding an active watch of a new blog is approving it in the same
		// breath, so it gets its publication the way an approval would.
		publication, _, err := s.AddPublication(PublicationRequest{Platform: request.Platform, BaseURL: request.BaseURL})
		if err != nil {
			return RadarSource{}, false, err
		}
		request.PublicationID = publication.ID
	}

	transaction, err := s.db.Begin()
	if err != nil {
		return RadarSource{}, false, err
	}
	defer transaction.Rollback()
	var seq int64
	if err := transaction.QueryRow(`UPDATE id_sequences SET last = last + 1 WHERE name = 'source' RETURNING last`).Scan(&seq); err != nil {
		return RadarSource{}, false, err
	}
	id := formatRadarSourceID(seq)
	timestamp := now()
	nextRunAt := int64(0)
	if request.Status == RadarSourceStatusActive {
		nextRunAt = timestamp
	}
	if _, err := transaction.Exec(`INSERT INTO sources (id, seq, kind, name, status, enabled, cadence_hours, next_run_at,
		publication_id, platform, base_url, prompt, notes, proposed_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, seq, request.Kind, strings.TrimSpace(request.Name), request.Status, request.CadenceHours, nextRunAt,
		request.PublicationID, request.Platform, request.BaseURL, request.Prompt, strings.TrimSpace(request.Notes),
		strings.TrimSpace(request.ProposedBy), timestamp, timestamp); err != nil {
		return RadarSource{}, false, err
	}
	if err := transaction.Commit(); err != nil {
		return RadarSource{}, false, err
	}
	created, err := s.GetRadarSource(id)
	return created, true, err
}

// resolveWatchTarget checks a watch's publication fields and, when the blog
// named by platform and base_url is already a publication, fills in its id.
func (s *Store) resolveWatchTarget(request *RadarSourceRequest) error {
	if request.PublicationID != "" {
		if request.Platform != "" || request.BaseURL != "" {
			return fmt.Errorf("%w: a watch names its publication by publication_id, or a blog not yet a publication by platform and base_url, not both", ErrInvalidArticle)
		}
		publication, err := s.GetPublication(request.PublicationID)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: publication_id %s is not a publication", ErrInvalidArticle, request.PublicationID)
		}
		if err != nil {
			return err
		}
		request.Platform = publication.Platform
		request.BaseURL = publication.BaseURL
		return nil
	}
	if request.Platform == "" || request.BaseURL == "" {
		return fmt.Errorf("%w: a watch needs publication_id, or platform and base_url for a blog not yet a publication", ErrInvalidArticle)
	}
	if !contains(PublicationPlatforms, request.Platform) {
		return fmt.Errorf("%w: platform %q is not one of %v", ErrInvalidArticle, request.Platform, PublicationPlatforms)
	}
	baseURL, err := NormalizePublicationURL(request.BaseURL)
	if err != nil {
		return err
	}
	request.BaseURL = baseURL
	publication, err := s.getPublicationWhere(`p.base_url = ?`, baseURL)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	request.PublicationID = publication.ID
	return nil
}

// findWatch returns the watch of this publication or base URL, if there is one.
func (s *Store) findWatch(publicationID, baseURL string) (RadarSource, bool, error) {
	source, err := scanRadarSource(s.db.QueryRow(`SELECT `+radarSourceColumns+` FROM sources
		WHERE kind = 'watch' AND ((publication_id <> '' AND publication_id = ?) OR base_url = ?) ORDER BY seq LIMIT 1`,
		publicationID, baseURL))
	if errors.Is(err, ErrNotFound) {
		return RadarSource{}, false, nil
	}
	return source, err == nil, err
}

// GetRadarSource returns one source.
func (s *Store) GetRadarSource(id string) (RadarSource, error) {
	if err := checkRadarSourceID(id); err != nil {
		return RadarSource{}, err
	}
	source, err := scanRadarSource(s.db.QueryRow(`SELECT `+radarSourceColumns+` FROM sources WHERE id = ?`, id))
	if errors.Is(err, ErrNotFound) {
		return RadarSource{}, fmt.Errorf("%w: source %s", ErrNotFound, id)
	}
	return source, err
}

// ListRadarSources lists sources oldest first.
func (s *Store) ListRadarSources(filter RadarSourceFilter) ([]RadarSource, error) {
	clauses := []string{}
	arguments := []any{}
	if filter.Kind != "" {
		if !contains(RadarSourceKinds, filter.Kind) {
			return nil, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalidArticle, filter.Kind, RadarSourceKinds)
		}
		clauses = append(clauses, `kind = ?`)
		arguments = append(arguments, filter.Kind)
	}
	if filter.Status != "" {
		if !contains(RadarSourceStatuses, filter.Status) {
			return nil, fmt.Errorf("%w: status %q is not one of %v", ErrInvalidArticle, filter.Status, RadarSourceStatuses)
		}
		clauses = append(clauses, `status = ?`)
		arguments = append(arguments, filter.Status)
	}
	if filter.Due {
		clauses = append(clauses, `(status = 'active' AND enabled = 1 AND next_run_at <= ?)`)
		arguments = append(arguments, now())
	}
	where := ""
	if len(clauses) > 0 {
		where = ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	rows, err := s.db.Query(`SELECT `+radarSourceColumns+` FROM sources`+where+` ORDER BY seq`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := []RadarSource{}
	for rows.Next() {
		source, err := scanRadarSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// PatchRadarSource applies a partial update. A move to active from any other
// status is the approval: it enables the source and makes it due now, and a
// watch of a blog with no publication yet gets one — found by base_url, or
// created, with no archive backfill started.
func (s *Store) PatchRadarSource(id string, patch RadarSourcePatch) (RadarSource, error) {
	existing, err := s.GetRadarSource(id)
	if err != nil {
		return RadarSource{}, err
	}
	sets := []string{}
	arguments := []any{}
	set := func(column string, value any) {
		sets = append(sets, column+" = ?")
		arguments = append(arguments, value)
	}
	if patch.Name != nil {
		set("name", strings.TrimSpace(*patch.Name))
	}
	if patch.Notes != nil {
		set("notes", strings.TrimSpace(*patch.Notes))
	}
	if patch.Prompt != nil {
		prompt := strings.TrimSpace(*patch.Prompt)
		if existing.Kind == RadarSourceKindWatch {
			return RadarSource{}, fmt.Errorf("%w: a watch takes no prompt", ErrInvalidArticle)
		}
		if prompt == "" {
			return RadarSource{}, fmt.Errorf("%w: a %s source needs a prompt", ErrInvalidArticle, existing.Kind)
		}
		set("prompt", prompt)
	}
	if patch.CadenceHours != nil {
		if *patch.CadenceHours <= 0 {
			return RadarSource{}, fmt.Errorf("%w: cadence_hours must be more than 0", ErrInvalidArticle)
		}
		set("cadence_hours", *patch.CadenceHours)
		// A source that has run comes due again a cadence after that run,
		// counted with the new cadence.
		if existing.LastRunAt != 0 && existing.Status == RadarSourceStatusActive {
			set("next_run_at", existing.LastRunAt+int64(*patch.CadenceHours)*3600)
		}
	}
	if patch.Enabled != nil {
		set("enabled", *patch.Enabled)
	}
	if patch.Status != nil {
		if !contains(RadarSourceStatuses, *patch.Status) {
			return RadarSource{}, fmt.Errorf("%w: status %q is not one of %v", ErrInvalidArticle, *patch.Status, RadarSourceStatuses)
		}
		set("status", *patch.Status)
		if *patch.Status == RadarSourceStatusActive && existing.Status != RadarSourceStatusActive {
			if patch.Enabled == nil {
				set("enabled", true)
			}
			set("next_run_at", now())
			if existing.Kind == RadarSourceKindWatch && existing.PublicationID == "" {
				publication, _, err := s.AddPublication(PublicationRequest{Platform: existing.Platform, BaseURL: existing.BaseURL, Name: existing.Name})
				if err != nil {
					return RadarSource{}, err
				}
				set("publication_id", publication.ID)
			}
		}
	}
	if len(sets) == 0 {
		return RadarSource{}, fmt.Errorf("%w: nothing to change; PATCH takes name, status, enabled, cadence_hours, prompt and notes", ErrInvalidArticle)
	}
	set("updated_at", now())
	if _, err := s.db.Exec(`UPDATE sources SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(arguments, id)...); err != nil {
		return RadarSource{}, err
	}
	return s.GetRadarSource(id)
}

// MarkRadarSourceRan stamps last_run_at and last_result, and sets next_run_at a
// cadence from now, not from the old next_run_at, so a late run never brings
// on a string of catch-up runs.
func (s *Store) MarkRadarSourceRan(id, result string) (RadarSource, error) {
	if err := checkRadarSourceID(id); err != nil {
		return RadarSource{}, err
	}
	timestamp := now()
	outcome, err := s.db.Exec(`UPDATE sources SET last_run_at = ?, last_result = ?, next_run_at = ? + cadence_hours * 3600,
		updated_at = ? WHERE id = ?`, timestamp, strings.TrimSpace(result), timestamp, timestamp, id)
	if err != nil {
		return RadarSource{}, err
	}
	if affected, err := outcome.RowsAffected(); err != nil || affected == 0 {
		if err != nil {
			return RadarSource{}, err
		}
		return RadarSource{}, fmt.Errorf("%w: source %s", ErrNotFound, id)
	}
	return s.GetRadarSource(id)
}

// setRadarSourceLastResult records what happened on a run that did not finish,
// such as a 429, and leaves the source due.
func (s *Store) setRadarSourceLastResult(id, result string) error {
	_, err := s.db.Exec(`UPDATE sources SET last_result = ?, updated_at = ? WHERE id = ?`, result, now(), id)
	return err
}

// DeleteRadarSource removes a source. Suggestions it made keep its id.
func (s *Store) DeleteRadarSource(id string) error {
	if err := checkRadarSourceID(id); err != nil {
		return err
	}
	result, err := s.db.Exec(`DELETE FROM sources WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: source %s", ErrNotFound, id)
	}
	return nil
}
