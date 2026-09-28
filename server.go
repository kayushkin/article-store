package articlestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RegisterHandlers wires all routes onto the mux. Routes are rooted at /, like
// quote-store and prediction-store; dash is what puts them under a prefix.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /vocabulary", h.vocabulary)
	mux.HandleFunc("GET /tags", h.listTags)

	mux.HandleFunc("GET /articles", h.listArticles)
	mux.HandleFunc("POST /articles", h.saveArticle)
	mux.HandleFunc("GET /articles/{id}", h.getArticle)
	mux.HandleFunc("GET /articles/{id}/source", h.getSource)
	mux.HandleFunc("PATCH /articles/{id}", h.patchArticle)
	mux.HandleFunc("DELETE /articles/{id}", h.deleteArticle)
	mux.HandleFunc("POST /articles/{id}/restore", h.restoreArticle)
	mux.HandleFunc("POST /articles/{id}/refetch", h.refetchArticle)

	mux.HandleFunc("GET /publications", h.listPublications)
	mux.HandleFunc("POST /publications", h.addPublication)
	mux.HandleFunc("GET /publications/{id}", h.getPublication)
	mux.HandleFunc("PATCH /publications/{id}", h.patchPublication)
	mux.HandleFunc("POST /publications/{id}/backfill", h.startBackfill)
	mux.HandleFunc("POST /publications/{id}/backfill/stop", h.stopBackfill)
}

type handler struct{ s *Store }

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) vocabulary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"kinds": ArticleKinds, "default_kind": DefaultArticleKind,
		"source_kinds": SourceKinds, "publication_platforms": PublicationPlatforms,
		"orders": ArticleOrders,
	})
}

func (h *handler) listTags(w http.ResponseWriter, r *http.Request) {
	tags, err := h.s.ListTags()
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

func (h *handler) listArticles(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := ArticleFilter{
		Query:          query.Get("q"),
		Tag:            query.Get("tag"),
		Kind:           query.Get("kind"),
		PublicationID:  query.Get("publication_id"),
		Order:          ArticleOrder(query.Get("order")),
		IncludeDeleted: isTrue(query.Get("include_deleted")),
	}
	var err error
	if filter.Limit, err = optionalInt(query.Get("limit")); err != nil {
		writeErr(w, http.StatusBadRequest, "limit: "+err.Error())
		return
	}
	if filter.Offset, err = optionalInt(query.Get("offset")); err != nil {
		writeErr(w, http.StatusBadRequest, "offset: "+err.Error())
		return
	}
	if raw := query.Get("read"); raw != "" {
		read, err := strconv.ParseBool(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "read must be true or false")
			return
		}
		filter.Read = &read
	}
	if raw := query.Get("favorite"); raw != "" {
		favorite, err := strconv.ParseBool(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "favorite must be true or false")
			return
		}
		filter.Favorite = &favorite
	}
	articles, err := h.s.ListArticles(filter)
	if respondStoreError(w, err) {
		return
	}
	// total ignores limit and offset, so a pager knows how far it can page.
	total, err := h.s.CountArticles(filter)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"articles": articles, "total": total})
}

// saveArticle answers 201 with the new article, or 200 with the stored one when
// the URL was already saved.
func (h *handler) saveArticle(w http.ResponseWriter, r *http.Request) {
	var request SaveRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	article, created, err := h.s.SaveArticle(r.Context(), request)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, article)
}

// getArticle answers JSON, or with ?format=markdown the article as one markdown
// document headed by its title, byline, source and id — what an agent reads.
func (h *handler) getArticle(w http.ResponseWriter, r *http.Request) {
	article, err := h.s.GetArticle(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	switch format := r.URL.Query().Get("format"); format {
	case "", "json":
		writeJSON(w, http.StatusOK, article)
	case "markdown":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		fmt.Fprint(w, RenderMarkdown(article))
	default:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("format %q is not json or markdown", format))
	}
}

// RenderMarkdown renders an article as one markdown document.
func RenderMarkdown(article Article) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", article.Title)
	details := []string{}
	if article.Byline != "" {
		details = append(details, "By "+article.Byline)
	}
	if article.SiteName != "" {
		details = append(details, article.SiteName)
	}
	if article.PublishedAt != 0 {
		details = append(details, time.Unix(article.PublishedAt, 0).UTC().Format("2006-01-02"))
	}
	if len(details) > 0 {
		fmt.Fprintf(&b, "%s\n\n", strings.Join(details, " · "))
	}
	fmt.Fprintf(&b, "Source: <%s>  \nSaved as %s\n\n", article.URL, article.ID)
	if article.Note != "" {
		fmt.Fprintf(&b, "> Note: %s\n\n", strings.ReplaceAll(article.Note, "\n", "\n> "))
	}
	b.WriteString("---\n\n")
	b.WriteString(article.ContentMarkdown)
	b.WriteString("\n")
	return b.String()
}

// getSource serves what was fetched, as plain text: it is somebody else's
// unsanitized markup, and served as HTML it would run on whatever origin
// proxies this store.
func (h *handler) getSource(w http.ResponseWriter, r *http.Request) {
	source, err := h.s.GetFetchedSource(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	fmt.Fprint(w, source)
}

func (h *handler) patchArticle(w http.ResponseWriter, r *http.Request) {
	var patch ArticlePatch
	if !decodeStrict(w, r, &patch) {
		return
	}
	article, err := h.s.PatchArticle(r.PathValue("id"), patch)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, article)
}

// deleteArticle soft-deletes by default, so the article can be restored.
// ?hard=true destroys it and frees its URL.
func (h *handler) deleteArticle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	if isTrue(r.URL.Query().Get("hard")) {
		err = h.s.PurgeArticle(id)
	} else {
		err = h.s.SoftDeleteArticle(id)
	}
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (h *handler) restoreArticle(w http.ResponseWriter, r *http.Request) {
	article, err := h.s.RestoreArticle(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, article)
}

// refetchArticle takes an optional {"source_html": …}; with none it fetches the
// URL again.
func (h *handler) refetchArticle(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SourceHTML string `json:"source_html"`
	}
	if r.ContentLength != 0 && !decodeStrict(w, r, &request) {
		return
	}
	article, err := h.s.RefetchArticle(r.Context(), r.PathValue("id"), request.SourceHTML)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, article)
}

func (h *handler) listPublications(w http.ResponseWriter, r *http.Request) {
	publications, err := h.s.ListPublications()
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"publications": publications})
}

// addPublication answers 201 with the new publication, or 200 with the one
// already stored at that base URL.
func (h *handler) addPublication(w http.ResponseWriter, r *http.Request) {
	var request PublicationRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	publication, created, err := h.s.AddPublication(request)
	if respondStoreError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, publication)
}

func (h *handler) getPublication(w http.ResponseWriter, r *http.Request) {
	publication, err := h.s.GetPublication(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, publication)
}

func (h *handler) patchPublication(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		Name *string `json:"name"`
	}
	if !decodeStrict(w, r, &patch) {
		return
	}
	if patch.Name == nil {
		writeErr(w, http.StatusBadRequest, "nothing to change: name is the only field PATCH takes")
		return
	}
	publication, err := h.s.PatchPublicationName(r.PathValue("id"), *patch.Name)
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, publication)
}

// startBackfill sets the archive backfill running and answers 202: the work
// happens in the background, and GET /publications/{id} shows its progress.
func (h *handler) startBackfill(w http.ResponseWriter, r *http.Request) {
	publication, err := h.s.StartBackfill(r.PathValue("id"), isTrue(r.URL.Query().Get("restart")))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, publication)
}

func (h *handler) stopBackfill(w http.ResponseWriter, r *http.Request) {
	publication, err := h.s.StopBackfill(r.PathValue("id"))
	if respondStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, publication)
}

// decodeStrict decodes a JSON body and refuses a field the type does not have:
// a misspelled field would otherwise be dropped and the caller told it worked.
func decodeStrict(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaximumPageBytes+(1<<20)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already sent, so this cannot become an error
		// response. Say it out loud rather than leave a truncated body.
		fmt.Printf("article-store: encode response: %v\n", err)
	}
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// respondStoreError maps a store error onto a status code and reports whether
// it handled the request.
func respondStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrDeleted):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalidArticle):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrFetchFailed), errors.Is(err, ErrNothingExtracted):
		writeErr(w, http.StatusBadGateway, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

func optionalInt(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	return strconv.Atoi(strings.TrimSpace(raw))
}

func isTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
