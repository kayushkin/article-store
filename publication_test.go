package articlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeSubstack serves an archive and its posts the way Substack's API does.
type fakeSubstack struct {
	t      *testing.T
	server *httptest.Server
	mutex  sync.Mutex
	// entries is the archive, newest first.
	entries []SubstackArchiveEntry
	// rateLimitNextPostRequests answers that many post requests with 429.
	rateLimitNextPostRequests int
	postRequests              int
}

func newFakeSubstack(t *testing.T) *fakeSubstack {
	t.Helper()
	fake := &fakeSubstack{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/archive", fake.archive)
	mux.HandleFunc("GET /api/v1/posts/{slug}", fake.post)
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeSubstack) addPost(slug, postType, audience string) {
	fake.entries = append(fake.entries, SubstackArchiveEntry{
		ID: int64(len(fake.entries) + 1), Slug: slug, Title: "Post " + slug, Subtitle: "About " + slug,
		PostDate: "2026-09-22T17:30:44.948Z", Audience: audience, Type: postType,
		CanonicalURL: fake.server.URL + "/p/" + slug, PublicationID: 7223401,
		PublishedBylines: []SubstackByline{{Name: "Sebastian Jensen"}, {Name: "A. Guest"}},
	})
}

func (fake *fakeSubstack) archive(w http.ResponseWriter, r *http.Request) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page := []SubstackArchiveEntry{}
	for index := offset; index < len(fake.entries) && index < offset+limit; index++ {
		page = append(page, fake.entries[index])
	}
	json.NewEncoder(w).Encode(page)
}

func (fake *fakeSubstack) post(w http.ResponseWriter, r *http.Request) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.postRequests++
	if fake.rateLimitNextPostRequests > 0 {
		fake.rateLimitNextPostRequests--
		w.Header().Set("Retry-After", "120")
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	slug := r.PathValue("slug")
	for _, entry := range fake.entries {
		if entry.Slug == slug {
			if slug == "missing" {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id": entry.ID, "slug": entry.Slug, "title": entry.Title, "subtitle": entry.Subtitle,
				"post_date": entry.PostDate, "audience": entry.Audience, "type": entry.Type,
				"canonical_url": entry.CanonicalURL, "publishedBylines": entry.PublishedBylines,
				"body_html": fmt.Sprintf(`<h3>%s</h3><p>The body of %s, which is long enough to read.</p><script>steal()</script>`, entry.Title, slug),
			})
			return
		}
	}
	http.NotFound(w, r)
}

func newBackfiller(t *testing.T) (*Store, *Backfiller) {
	t.Helper()
	store := newTestStore(t)
	return store, &Backfiller{Store: store}
}

// runUntilIdle steps the backfiller until no backfill is due.
func runUntilIdle(t *testing.T, backfiller *Backfiller) {
	t.Helper()
	for step := 0; step < 100; step++ {
		worked, err := backfiller.Step(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("the backfill never went idle")
}

func TestBackfillSavesTheWholeArchiveAndSkipsWhatIsNotAnArticle(t *testing.T) {
	fake := newFakeSubstack(t)
	for index := 0; index < ArchivePageSize+3; index++ {
		fake.addPost(fmt.Sprintf("post-%02d", index), "newsletter", "everyone")
	}
	fake.addPost("chat", "thread", "everyone")
	fake.addPost("paid", "newsletter", "only_paid")
	fake.addPost("missing", "newsletter", "everyone")
	store, backfiller := newBackfiller(t)

	publication, created, err := store.AddPublication(PublicationRequest{Platform: "substack", BaseURL: fake.server.URL + "/", Name: "Selective Contrarianism"})
	if err != nil || !created || publication.ID != "publication_000001" || publication.BaseURL != fake.server.URL {
		t.Fatalf("add: %+v created=%v err=%v", publication, created, err)
	}
	if _, err := store.StartBackfill(publication.ID, false); err != nil {
		t.Fatal(err)
	}
	runUntilIdle(t, backfiller)

	publication, err = store.GetPublication(publication.ID)
	if err != nil {
		t.Fatal(err)
	}
	backfill := publication.Backfill
	if backfill.Status != BackfillStatusDone || backfill.PostsSaved != ArchivePageSize+4 || backfill.PostsSkipped != 1 ||
		backfill.PostsFailed != 1 || backfill.Offset != ArchivePageSize+6 || publication.ArticleCount != ArchivePageSize+4 {
		t.Fatalf("backfill = %+v, article_count = %d", backfill, publication.ArticleCount)
	}
	if publication.PlatformPublicationRef != "7223401" {
		t.Errorf("platform_publication_ref = %q", publication.PlatformPublicationRef)
	}

	articles, err := store.ListArticles(ArticleFilter{PublicationID: publication.ID, Tag: SubstackPaidOnlyTag})
	if err != nil || len(articles) != 1 {
		t.Fatalf("paid-only articles: %v, err=%v", articles, err)
	}
	paid, err := store.GetArticle(articles[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if paid.Byline != "Sebastian Jensen, A. Guest" || paid.Kind != "post" || paid.SourceKind != SourceKindSubstackPostAPI ||
		paid.SiteName != "Selective Contrarianism" || paid.Excerpt != "About paid" || paid.PublishedAt != 1790098244 {
		t.Errorf("article = %+v", paid.ArticleSummary)
	}
	if paid.ContentText == "" || contains([]string{paid.ContentHTML}, "<script>steal()</script>") {
		t.Errorf("content_html = %q", paid.ContentHTML)
	}
}

func TestBackfillWaitsOutA429AndCarriesOn(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.addPost("first", "newsletter", "everyone")
	fake.addPost("second", "newsletter", "everyone")
	store, backfiller := newBackfiller(t)
	publication, _, _ := store.AddPublication(PublicationRequest{Platform: "substack", BaseURL: fake.server.URL})
	store.StartBackfill(publication.ID, false)
	fake.rateLimitNextPostRequests = 1

	runUntilIdle(t, backfiller)
	publication, _ = store.GetPublication(publication.ID)
	if publication.Backfill.Status != BackfillStatusRunning || publication.Backfill.Offset != 0 || publication.Backfill.PostsSaved != 0 {
		t.Fatalf("after a 429: %+v", publication.Backfill)
	}
	if wait := publication.Backfill.NextAttemptAt - time.Now().Unix(); wait < 110 || wait > 125 {
		t.Errorf("next attempt in %ds, want the 120 Retry-After named", wait)
	}

	// The wait passes.
	if _, err := store.db.Exec(`UPDATE publications SET backfill_next_attempt_at = 0`); err != nil {
		t.Fatal(err)
	}
	runUntilIdle(t, backfiller)
	publication, _ = store.GetPublication(publication.ID)
	if publication.Backfill.Status != BackfillStatusDone || publication.Backfill.PostsSaved != 2 || publication.Backfill.PostsFailed != 0 {
		t.Fatalf("after the wait: %+v", publication.Backfill)
	}
}

func TestStopThenStartCarriesOnAndRestartCountsWhatIsSaved(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.addPost("one", "newsletter", "everyone")
	fake.addPost("two", "newsletter", "everyone")
	store, backfiller := newBackfiller(t)
	publication, _, _ := store.AddPublication(PublicationRequest{Platform: "substack", BaseURL: fake.server.URL})
	store.StartBackfill(publication.ID, false)
	if stopped, err := store.StopBackfill(publication.ID); err != nil || stopped.Backfill.Status != BackfillStatusStopped {
		t.Fatalf("stop: %+v %v", stopped.Backfill, err)
	}
	if worked, _ := backfiller.Step(context.Background()); worked {
		t.Fatal("a stopped backfill still ran")
	}
	store.StartBackfill(publication.ID, false)
	runUntilIdle(t, backfiller)
	requestsAfterFirstRun := fake.postRequests

	restarted, err := store.StartBackfill(publication.ID, true)
	if err != nil || restarted.Backfill.Offset != 0 || restarted.Backfill.PostsSaved != 0 {
		t.Fatalf("restart: %+v %v", restarted.Backfill, err)
	}
	runUntilIdle(t, backfiller)
	publication, _ = store.GetPublication(publication.ID)
	if publication.Backfill.PostsAlready != 2 || publication.Backfill.PostsSaved != 0 || fake.postRequests != requestsAfterFirstRun {
		t.Errorf("rerun: %+v, post requests %d then %d", publication.Backfill, requestsAfterFirstRun, fake.postRequests)
	}
}

func TestRefetchOfASubstackArticleGoesBackToThePostAPI(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.addPost("only", "newsletter", "everyone")
	store, backfiller := newBackfiller(t)
	publication, _, _ := store.AddPublication(PublicationRequest{Platform: "substack", BaseURL: fake.server.URL})
	store.StartBackfill(publication.ID, false)
	runUntilIdle(t, backfiller)
	before := fake.postRequests
	article, err := store.RefetchArticle(context.Background(), "article_000001", "")
	if err != nil || article.SourceKind != SourceKindSubstackPostAPI || fake.postRequests != before+1 {
		t.Fatalf("refetch: kind=%q requests %d→%d err=%v", article.SourceKind, before, fake.postRequests, err)
	}
}

func TestAddPublicationRefusesWhatIsNotAPublicationRoot(t *testing.T) {
	store := newTestStore(t)
	for _, request := range []PublicationRequest{
		{Platform: "medium", BaseURL: "https://example.medium.com"},
		{Platform: "substack", BaseURL: "https://noahpinion.substack.com/p/a-post"},
		{Platform: "substack", BaseURL: "ftp://noahpinion.substack.com"},
	} {
		if _, _, err := store.AddPublication(request); !errors.Is(err, ErrInvalidArticle) {
			t.Errorf("add %+v: err = %v", request, err)
		}
	}
	first, _, _ := store.AddPublication(PublicationRequest{Platform: "substack", BaseURL: "https://Noahpinion.substack.com/"})
	again, created, err := store.AddPublication(PublicationRequest{Platform: "substack", BaseURL: "https://noahpinion.substack.com"})
	if err != nil || created || again.ID != first.ID {
		t.Errorf("the same site twice: created=%v %s vs %s err=%v", created, again.ID, first.ID, err)
	}
}

// A database made before publications existed opens, keeps its rows and its
// page source, and gains the new columns.
func TestOpenMigratesADatabaseFromTheFirstSchema(t *testing.T) {
	directory := t.TempDir()
	database, err := sql.Open("sqlite3", filepath.Join(directory, "article-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		CREATE TABLE articles (
			id TEXT PRIMARY KEY, seq INTEGER NOT NULL UNIQUE, url TEXT NOT NULL UNIQUE,
			final_url TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', byline TEXT NOT NULL DEFAULT '',
			site_name TEXT NOT NULL DEFAULT '', language TEXT NOT NULL DEFAULT '', published_at INTEGER NOT NULL DEFAULT 0,
			excerpt TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT 'article', content_html TEXT NOT NULL DEFAULT '',
			content_markdown TEXT NOT NULL DEFAULT '', content_text TEXT NOT NULL DEFAULT '', word_count INTEGER NOT NULL DEFAULT 0,
			source_html TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', tags TEXT NOT NULL DEFAULT '[]',
			added_by TEXT NOT NULL DEFAULT '', read_at INTEGER NOT NULL DEFAULT 0, fetched_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER NOT NULL DEFAULT 0);
		INSERT INTO articles (id, seq, url, title, content_text, source_html, created_at, updated_at)
		VALUES ('article_000001', 1, 'https://example.com/a', 'Old', 'old text', '<html>old page</html>', 1, 1);`); err != nil {
		t.Fatal(err)
	}
	database.Close()

	store, err := Open(directory, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	article, err := store.GetArticle("article_000001")
	if err != nil || article.SourceKind != SourceKindWebPage || article.PublicationID != "" {
		t.Fatalf("migrated article: %+v err=%v", article.ArticleSummary, err)
	}
	if source, err := store.GetFetchedSource("article_000001"); err != nil || source != "<html>old page</html>" {
		t.Errorf("fetched_source = %q err=%v", source, err)
	}
	// Opening again finds nothing left to do.
	store.Close()
	if reopened, err := Open(directory, http.DefaultClient); err != nil {
		t.Fatalf("second open: %v", err)
	} else {
		reopened.Close()
	}
}
