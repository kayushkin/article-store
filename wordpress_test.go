package articlestore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWordPress serves posts the way a WordPress site's REST API does.
type fakeWordPress struct {
	server *httptest.Server
	mutex  sync.Mutex
	// posts is the archive, newest first.
	posts []map[string]any
	// rateLimitNextListRequests answers that many list requests with 429.
	rateLimitNextListRequests int
	listRequests              int
	postRequests              int
}

func newFakeWordPress(t *testing.T) *fakeWordPress {
	t.Helper()
	fake := &fakeWordPress{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wp-json/wp/v2/posts", fake.list)
	mux.HandleFunc("GET /wp-json/wp/v2/posts/{id}", fake.post)
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeWordPress) addPost(slug string, protected bool) {
	id := len(fake.posts) + 1
	body := fmt.Sprintf(`<p>The body of %s, which is long enough to read.</p><script>steal()</script>`, slug)
	if protected {
		body = ""
	}
	fake.posts = append(fake.posts, map[string]any{
		"id":        id,
		"link":      fake.server.URL + "/2015/08/17/" + slug + "/",
		"date_gmt":  "2015-08-17T21:39:40",
		"type":      "post",
		"title":     map[string]any{"rendered": "Scott&#8217;s <em>" + slug + "</em>"},
		"content":   map[string]any{"rendered": body, "protected": protected},
		"excerpt":   map[string]any{"rendered": "<p>About " + slug + " [&hellip;]</p>\n"},
		"author":    57931,
		"_embedded": map[string]any{"author": []map[string]any{{"id": 57931, "name": "Scott Alexander"}}},
	})
}

func (fake *fakeWordPress) list(w http.ResponseWriter, r *http.Request) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.listRequests++
	if fake.rateLimitNextListRequests > 0 {
		fake.rateLimitNextListRequests--
		w.Header().Set("Retry-After", "120")
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	query := r.URL.Query()
	perPage, _ := strconv.Atoi(query.Get("per_page"))
	page, _ := strconv.Atoi(query.Get("page"))
	if query.Get("orderby") != "date" || query.Get("order") != "desc" || query.Get("_embed") != "author" || perPage < 1 || page < 1 {
		http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)
		return
	}
	totalPages := (len(fake.posts) + perPage - 1) / perPage
	w.Header().Set("X-WP-Total", strconv.Itoa(len(fake.posts)))
	w.Header().Set("X-WP-TotalPages", strconv.Itoa(totalPages))
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if page > totalPages {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":"rest_post_invalid_page_number","message":"The page number requested is larger than the number of pages available.","data":{"status":400}}`)
		return
	}
	end := page * perPage
	if end > len(fake.posts) {
		end = len(fake.posts)
	}
	json.NewEncoder(w).Encode(fake.posts[(page-1)*perPage : end])
}

func (fake *fakeWordPress) post(w http.ResponseWriter, r *http.Request) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.postRequests++
	id, _ := strconv.Atoi(r.PathValue("id"))
	for _, post := range fake.posts {
		if post["id"] == id {
			json.NewEncoder(w).Encode(post)
			return
		}
	}
	http.NotFound(w, r)
}

func addWordPressPublication(t *testing.T, store *Store, fake *fakeWordPress) Publication {
	t.Helper()
	publication, created, err := store.AddPublication(PublicationRequest{Platform: "wordpress", BaseURL: fake.server.URL, Name: "Slate Star Codex"})
	if err != nil || !created {
		t.Fatalf("add: %+v created=%v err=%v", publication, created, err)
	}
	if _, err := store.StartBackfill(publication.ID, false); err != nil {
		t.Fatal(err)
	}
	return publication
}

func TestWordPressBackfillSavesTheWholeArchiveOnePageARequest(t *testing.T) {
	fake := newFakeWordPress(t)
	for index := 0; index < ArchivePageSize+3; index++ {
		fake.addPost(fmt.Sprintf("post-%02d", index), false)
	}
	fake.addPost("locked", true)
	store, backfiller := newBackfiller(t)
	publication := addWordPressPublication(t, store, fake)
	runUntilIdle(t, backfiller)

	publication, err := store.GetPublication(publication.ID)
	if err != nil {
		t.Fatal(err)
	}
	backfill := publication.Backfill
	if backfill.Status != BackfillStatusDone || backfill.PostsSaved != ArchivePageSize+3 || backfill.PostsSkipped != 1 ||
		backfill.PostsFailed != 0 || backfill.Offset != ArchivePageSize+4 || publication.ArticleCount != ArchivePageSize+3 {
		t.Fatalf("backfill = %+v, article_count = %d", backfill, publication.ArticleCount)
	}
	// Two pages of posts and the page past the end; no request per post.
	if fake.listRequests != 3 || fake.postRequests != 0 {
		t.Errorf("list requests %d, post requests %d", fake.listRequests, fake.postRequests)
	}

	article, err := store.getArticleByURL(fake.server.URL + "/2015/08/17/post-00/")
	if err != nil {
		t.Fatal(err)
	}
	if article.Title != "Scott’s post-00" || article.Byline != "Scott Alexander" || article.PublishedAt != 1439847580 ||
		article.Kind != "post" || article.SourceKind != SourceKindWordPressPostAPI || article.SiteName != "Slate Star Codex" ||
		article.Excerpt != "About post-00 […]" || article.PublicationID != publication.ID {
		t.Errorf("article = %+v", article.ArticleSummary)
	}
	if !strings.Contains(article.ContentText, "The body of post-00") || strings.Contains(article.ContentHTML, "<script>") ||
		!strings.Contains(article.ContentMarkdown, "The body of post-00") {
		t.Errorf("content: html=%q markdown=%q", article.ContentHTML, article.ContentMarkdown)
	}
	source, err := store.GetFetchedSource(article.ID)
	var saved map[string]any
	if err != nil || json.Unmarshal([]byte(source), &saved) != nil || saved["id"] != float64(1) || saved["link"] != article.URL {
		t.Errorf("fetched_source = %.200q err=%v", source, err)
	}
}

func TestWordPressBackfillCountsAPostSavedByHandUnderAnotherFormOfItsURL(t *testing.T) {
	fake := newFakeWordPress(t)
	fake.addPost("the-goddess-of-everything-else-2", false)
	fake.addPost("meditations-on-moloch", false)
	store, backfiller := newBackfiller(t)
	// Saved by hand without the trailing slash; the API's link has one.
	handSaved, _, err := store.SaveArticle(context.Background(), SaveRequest{
		URL:        fake.server.URL + "/2015/08/17/the-goddess-of-everything-else-2",
		SourceHTML: `<html><body><article><p>` + strings.Repeat("Everything else. ", 40) + `</p></article></body></html>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	publication := addWordPressPublication(t, store, fake)
	runUntilIdle(t, backfiller)

	publication, _ = store.GetPublication(publication.ID)
	if publication.Backfill.PostsSaved != 1 || publication.Backfill.PostsAlready != 1 {
		t.Fatalf("backfill = %+v", publication.Backfill)
	}
	total, err := store.CountArticles(ArticleFilter{})
	if err != nil || total != 2 {
		t.Errorf("%d articles, want the hand-saved one and one new (err=%v)", total, err)
	}
	if kept, _ := store.GetArticle(handSaved.ID); kept.PublicationID != "" || kept.SourceKind != SourceKindWebPage {
		t.Errorf("the hand-saved article changed: %+v", kept.ArticleSummary)
	}
}

func TestEquivalentPostURLsCoverSchemeAndTrailingSlash(t *testing.T) {
	got := equivalentPostURLs("https://slatestarcodex.com/2015/08/17/goddess/")
	want := []string{
		"https://slatestarcodex.com/2015/08/17/goddess/", "http://slatestarcodex.com/2015/08/17/goddess/",
		"https://slatestarcodex.com/2015/08/17/goddess", "http://slatestarcodex.com/2015/08/17/goddess",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	if got := equivalentPostURLs("http://example.com/"); !reflect.DeepEqual(got, []string{"http://example.com/", "https://example.com/"}) {
		t.Errorf("root: got %q", got)
	}
}

func TestWordPressBackfillCarriesOnPartWayThroughAPage(t *testing.T) {
	fake := newFakeWordPress(t)
	for index := 0; index < ArchivePageSize+5; index++ {
		fake.addPost(fmt.Sprintf("post-%02d", index), false)
	}
	store, backfiller := newBackfiller(t)
	publication := addWordPressPublication(t, store, fake)
	// As if a deploy stopped it after the first 52 entries.
	if _, err := store.db.Exec(`UPDATE publications SET backfill_offset = ? WHERE id = ?`, ArchivePageSize+2, publication.ID); err != nil {
		t.Fatal(err)
	}
	runUntilIdle(t, backfiller)
	publication, _ = store.GetPublication(publication.ID)
	if publication.Backfill.Status != BackfillStatusDone || publication.Backfill.PostsSaved != 3 || publication.Backfill.Offset != ArchivePageSize+5 {
		t.Fatalf("backfill = %+v", publication.Backfill)
	}
	for _, slug := range []string{"post-52", "post-53", "post-54"} {
		if _, err := store.getArticleByURL(fake.server.URL + "/2015/08/17/" + slug + "/"); err != nil {
			t.Errorf("%s: %v", slug, err)
		}
	}
}

func TestWordPressBackfillWaitsOutA429OnTheList(t *testing.T) {
	fake := newFakeWordPress(t)
	fake.addPost("first", false)
	store, backfiller := newBackfiller(t)
	publication := addWordPressPublication(t, store, fake)
	fake.rateLimitNextListRequests = 1

	runUntilIdle(t, backfiller)
	publication, _ = store.GetPublication(publication.ID)
	if publication.Backfill.Status != BackfillStatusRunning || publication.Backfill.Offset != 0 {
		t.Fatalf("after a 429: %+v", publication.Backfill)
	}
	if wait := publication.Backfill.NextAttemptAt - time.Now().Unix(); wait < 110 || wait > 125 {
		t.Errorf("next attempt in %ds, want the 120 Retry-After named", wait)
	}
	if _, err := store.db.Exec(`UPDATE publications SET backfill_next_attempt_at = 0`); err != nil {
		t.Fatal(err)
	}
	runUntilIdle(t, backfiller)
	publication, _ = store.GetPublication(publication.ID)
	if publication.Backfill.Status != BackfillStatusDone || publication.Backfill.PostsSaved != 1 {
		t.Fatalf("after the wait: %+v", publication.Backfill)
	}
}

func TestRefetchOfAWordPressArticleGoesBackToThePostAPI(t *testing.T) {
	fake := newFakeWordPress(t)
	fake.addPost("only", false)
	store, backfiller := newBackfiller(t)
	addWordPressPublication(t, store, fake)
	runUntilIdle(t, backfiller)
	article, err := store.RefetchArticle(context.Background(), "article_000001", "")
	if err != nil || article.SourceKind != SourceKindWordPressPostAPI || fake.postRequests != 1 ||
		!strings.Contains(article.ContentText, "The body of only") {
		t.Fatalf("refetch: kind=%q post requests %d err=%v", article.SourceKind, fake.postRequests, err)
	}
}

func TestEveryPublicationPlatformHasAnArchiveReader(t *testing.T) {
	readers := make([]string, 0, len(publicationArchives))
	for platform := range publicationArchives {
		readers = append(readers, platform)
	}
	sort.Strings(readers)
	platforms := append([]string(nil), PublicationPlatforms...)
	sort.Strings(platforms)
	if !reflect.DeepEqual(readers, platforms) {
		t.Errorf("PublicationPlatforms %v, archive readers %v", platforms, readers)
	}
}
