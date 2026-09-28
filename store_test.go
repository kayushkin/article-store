package articlestore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// samplePage is a blog page the way a real one arrives: navigation, a sidebar,
// a script and an inline handler around the post itself.
const samplePage = `<!DOCTYPE html>
<html lang="en-US"><head>
<title>The Lighthouse Keeper | Harbour Notes</title>
<meta property="og:site_name" content="Harbour Notes">
<meta property="og:title" content="The Lighthouse Keeper">
<meta name="author" content="Mara Quill">
<meta property="article:published_time" content="2015-08-17T20:59:40+00:00">
<script>window.tracker = true;</script>
</head><body>
<nav><a href="/">Home</a> <a href="/about">About</a> <a href="/archive">Archive</a></nav>
<article>
<h1>The Lighthouse Keeper</h1>
<p>Every evening the keeper climbed one hundred and twelve steps to light the lamp, and every morning she climbed them again to put it out. The ships never saw her, only the beam, and she liked it that way.</p>
<p onclick="steal()">In the winter of the great storm the lamp failed, and she stood at the top of the tower with a lantern in each hand, swinging them in long slow arcs until the fishing fleet was home. <a href="/2015/07/storms">The storm itself</a> is a story for another night.</p>
<p>When the harbour board asked her what she wanted as thanks, she asked for a second lantern, so that next time she would have a spare. They thought she was joking. She was not.</p>
<p>She kept the light for forty years, and when she retired the board replaced her with a machine that never needed thanks and never asked for lanterns, and the fleet came home just the same.</p>
<script>steal();</script>
</article>
<aside><h3>Recent posts</h3><ul><li><a href="/a">A</a></li><li><a href="/b">B</a></li></ul></aside>
<footer>Copyright Harbour Notes</footer>
</body></html>`

// pageServer serves samplePage at /post and counts how often it was fetched.
func pageServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var fetches atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, samplePage)
	})
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/post", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[]}`)
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>App</title></head><body><div id="root"></div><script src="/app.js"></script></body></html>`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &fetches
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir(), http.DefaultClient)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestSaveFetchesExtractsAndSanitizes(t *testing.T) {
	store := newTestStore(t)
	pages, _ := pageServer(t)

	article, created, err := store.SaveArticle(context.Background(), SaveRequest{
		URL: pages.URL + "/post#comments", Tags: []string{"Fiction", "fiction", " sea "}, Kind: "story", AddedBy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || article.ID != "article_000001" {
		t.Fatalf("created=%v id=%s, want a new article_000001", created, article.ID)
	}
	if article.URL != pages.URL+"/post" {
		t.Errorf("url = %q: the fragment should be dropped", article.URL)
	}
	if article.Title != "The Lighthouse Keeper" || article.SiteName != "Harbour Notes" || article.Language != "en-US" {
		t.Errorf("title=%q site=%q language=%q", article.Title, article.SiteName, article.Language)
	}
	if article.PublishedAt != 1439845180 {
		t.Errorf("published_at = %d, want 1439845180 (2015-08-17T20:59:40Z)", article.PublishedAt)
	}
	if strings.Join(article.Tags, ",") != "fiction,sea" || article.Kind != "story" {
		t.Errorf("tags=%v kind=%q", article.Tags, article.Kind)
	}
	if !strings.Contains(article.ContentText, "one hundred and twelve steps") || strings.Contains(article.ContentText, "Recent posts") {
		t.Errorf("content_text should hold the post and not the sidebar:\n%s", article.ContentText)
	}
	for _, unsafe := range []string{"<script", "onclick", "steal()"} {
		if strings.Contains(article.ContentHTML, unsafe) {
			t.Errorf("content_html still holds %q:\n%s", unsafe, article.ContentHTML)
		}
	}
	if !strings.Contains(article.ContentHTML, `href="`+pages.URL+`/2015/07/storms"`) {
		t.Errorf("relative links should be made absolute:\n%s", article.ContentHTML)
	}
	if !strings.Contains(article.ContentMarkdown, "[The storm itself]("+pages.URL+"/2015/07/storms)") {
		t.Errorf("content_markdown:\n%s", article.ContentMarkdown)
	}
	if article.WordCount < 120 || article.WordCount > 220 {
		t.Errorf("word_count = %d", article.WordCount)
	}
	source, err := store.GetFetchedSource(article.ID)
	if err != nil || !strings.Contains(source, "window.tracker") {
		t.Errorf("source_html should keep the page as fetched (err=%v)", err)
	}
}

func TestSavingTheSameURLAgainReturnsTheStoredArticleWithoutFetching(t *testing.T) {
	store := newTestStore(t)
	pages, fetches := pageServer(t)
	first, _, err := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post"})
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post#top"})
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ID != first.ID || fetches.Load() != 1 {
		t.Fatalf("created=%v id=%s fetches=%d, want the stored %s and one fetch", created, second.ID, fetches.Load(), first.ID)
	}
}

func TestCallerFieldsReplaceWhatExtractionFound(t *testing.T) {
	store := newTestStore(t)
	pages, _ := pageServer(t)
	article, _, err := store.SaveArticle(context.Background(), SaveRequest{
		URL: pages.URL + "/post", Byline: "M. Quill", Title: "Keeper", PublishedAt: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if article.Byline != "M. Quill" || article.Title != "Keeper" || article.PublishedAt != 100 {
		t.Errorf("byline=%q title=%q published_at=%d", article.Byline, article.Title, article.PublishedAt)
	}
}

func TestSourceHTMLIsExtractedWithoutFetching(t *testing.T) {
	store := newTestStore(t)
	// Nothing listens here: a fetch would fail.
	article, created, err := store.SaveArticle(context.Background(), SaveRequest{
		URL: "http://127.0.0.1:1/behind-a-login", SourceHTML: samplePage,
	})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if article.Title != "The Lighthouse Keeper" || article.FinalURL != "http://127.0.0.1:1/behind-a-login" {
		t.Errorf("title=%q final_url=%q", article.Title, article.FinalURL)
	}
}

func TestRedirectIsFollowedAndRecorded(t *testing.T) {
	store := newTestStore(t)
	pages, _ := pageServer(t)
	article, _, err := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/moved"})
	if err != nil {
		t.Fatal(err)
	}
	if article.URL != pages.URL+"/moved" || article.FinalURL != pages.URL+"/post" {
		t.Errorf("url=%q final_url=%q", article.URL, article.FinalURL)
	}
}

func TestSaveRefusesWhatIsNotAnArticle(t *testing.T) {
	store := newTestStore(t)
	pages, _ := pageServer(t)
	cases := []struct {
		url  string
		kind string
		want error
	}{
		{"ftp://example.com/a", "", ErrInvalidArticle},
		{"not a url", "", ErrInvalidArticle},
		{pages.URL + "/post", "novel", ErrInvalidArticle},
		{pages.URL + "/feed.json", "", ErrFetchFailed},
		{pages.URL + "/gone", "", ErrFetchFailed},
		{pages.URL + "/empty", "", ErrNothingExtracted},
	}
	for _, c := range cases {
		_, _, err := store.SaveArticle(context.Background(), SaveRequest{URL: c.url, Kind: c.kind})
		if !errors.Is(err, c.want) {
			t.Errorf("save %q kind %q: err = %v, want %v", c.url, c.kind, err, c.want)
		}
	}
	if total, _ := store.CountArticles(ArticleFilter{IncludeDeleted: true}); total != 0 {
		t.Errorf("%d rows stored from refused saves", total)
	}
}

func TestSearchFindsTextTagsAndNoteAndFollowsEdits(t *testing.T) {
	store := newTestStore(t)
	pages, _ := pageServer(t)
	article, _, err := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post", Tags: []string{"maritime"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"lanterns", "maritime", "Harbour"} {
		found, err := store.ListArticles(ArticleFilter{Query: query})
		if err != nil || len(found) != 1 {
			t.Fatalf("search %q: %d results, err=%v", query, len(found), err)
		}
		if query == "lanterns" && !strings.Contains(found[0].Snippet, "<mark>lanterns</mark>") {
			t.Errorf("snippet = %q", found[0].Snippet)
		}
	}
	note := "reread when tired"
	if _, err := store.PatchArticle(article.ID, ArticlePatch{Note: &note, Tags: &[]string{"comfort"}}); err != nil {
		t.Fatal(err)
	}
	if found, _ := store.ListArticles(ArticleFilter{Query: "tired"}); len(found) != 1 {
		t.Errorf("a new note is not searchable")
	}
	if found, _ := store.ListArticles(ArticleFilter{Query: "maritime"}); len(found) != 0 {
		t.Errorf("a removed tag is still searchable")
	}
	if _, err := store.ListArticles(ArticleFilter{Query: `"unclosed`}); !errors.Is(err, ErrInvalidArticle) {
		t.Errorf("a malformed query: err = %v, want ErrInvalidArticle", err)
	}
}

func TestFiltersByTagKindAndReadState(t *testing.T) {
	store := newTestStore(t)
	pages, _ := pageServer(t)
	story, _, _ := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post", Kind: "story", Tags: []string{"sea"}})
	if _, _, err := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post?page=2", Tags: []string{"sea", "later"}}); err != nil {
		t.Fatal(err)
	}
	read := true
	if _, err := store.PatchArticle(story.ID, ArticlePatch{Read: &read}); err != nil {
		t.Fatal(err)
	}
	count := func(filter ArticleFilter) int {
		found, err := store.ListArticles(filter)
		if err != nil {
			t.Fatal(err)
		}
		return len(found)
	}
	unread := false
	if count(ArticleFilter{Tag: "sea"}) != 2 || count(ArticleFilter{Tag: "later"}) != 1 || count(ArticleFilter{Kind: "story"}) != 1 ||
		count(ArticleFilter{Read: &read}) != 1 || count(ArticleFilter{Read: &unread}) != 1 {
		t.Errorf("filters returned the wrong rows")
	}
	tags, err := store.ListTags()
	if err != nil || len(tags) != 2 || tags[0] != (TagCount{Tag: "sea", Count: 2}) {
		t.Errorf("tags = %v, err=%v", tags, err)
	}
}

func TestDeleteRestoreAndPurge(t *testing.T) {
	store := newTestStore(t)
	pages, _ := pageServer(t)
	article, _, _ := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post"})
	if err := store.SoftDeleteArticle(article.ID); err != nil {
		t.Fatal(err)
	}
	if found, _ := store.ListArticles(ArticleFilter{Query: "lanterns"}); len(found) != 0 {
		t.Errorf("a deleted article is still found by search")
	}
	if _, _, err := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post"}); !errors.Is(err, ErrDeleted) {
		t.Errorf("saving a deleted URL: err = %v, want ErrDeleted", err)
	}
	if restored, err := store.RestoreArticle(article.ID); err != nil || restored.DeletedAt != 0 {
		t.Fatalf("restore: %v", err)
	}
	if err := store.PurgeArticle(article.ID); err != nil {
		t.Fatal(err)
	}
	again, created, err := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post"})
	if err != nil || !created || again.ID != "article_000002" {
		// A purged id is never handed out again: something may still point at it.
		t.Errorf("save after purge: created=%v id=%s err=%v", created, again.ID, err)
	}
	if _, err := store.GetArticle("article_999999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: err = %v", err)
	}
	if _, err := store.GetArticle("1"); !errors.Is(err, ErrInvalidArticle) {
		t.Errorf("malformed id: err = %v", err)
	}
}

func TestRefetchReplacesContentAndKeepsCorrections(t *testing.T) {
	store := newTestStore(t)
	pages, fetches := pageServer(t)
	article, _, _ := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post"})
	byline := "Mara Quill"
	if _, err := store.PatchArticle(article.ID, ArticlePatch{Byline: &byline}); err != nil {
		t.Fatal(err)
	}
	refetched, err := store.RefetchArticle(context.Background(), article.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 2 || refetched.Byline != "Mara Quill" || !strings.Contains(refetched.ContentText, "lanterns") {
		t.Errorf("fetches=%d byline=%q", fetches.Load(), refetched.Byline)
	}
}

func TestFavoriteStampsOnceClearsAndFilters(t *testing.T) {
	store := newTestStore(t)
	pages, _ := pageServer(t)
	first, _, _ := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post"})
	if _, _, err := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post?page=2"}); err != nil {
		t.Fatal(err)
	}
	favorite, notFavorite := true, false
	marked, err := store.PatchArticle(first.ID, ArticlePatch{Favorite: &favorite})
	if err != nil || marked.FavoritedAt == 0 {
		t.Fatalf("favorite: at=%d err=%v", marked.FavoritedAt, err)
	}
	again, err := store.PatchArticle(first.ID, ArticlePatch{Favorite: &favorite})
	if err != nil || again.FavoritedAt != marked.FavoritedAt {
		t.Errorf("favoriting twice moved favorited_at from %d to %d (err=%v)", marked.FavoritedAt, again.FavoritedAt, err)
	}
	favorites, err := store.ListArticles(ArticleFilter{Favorite: &favorite})
	if err != nil || len(favorites) != 1 || favorites[0].ID != first.ID {
		t.Errorf("favorite=true listed %v, err=%v", favorites, err)
	}
	if others, _ := store.ListArticles(ArticleFilter{Favorite: &notFavorite}); len(others) != 1 || others[0].ID == first.ID {
		t.Errorf("favorite=false listed %v", others)
	}
	cleared, err := store.PatchArticle(first.ID, ArticlePatch{Favorite: &notFavorite})
	if err != nil || cleared.FavoritedAt != 0 {
		t.Errorf("unfavorite: at=%d err=%v", cleared.FavoritedAt, err)
	}
}

func TestListingPutsTheMostRecentlyPublishedFirstUnlessAskedBySaveOrder(t *testing.T) {
	store := newTestStore(t)
	pages, _ := pageServer(t)
	// Saved oldest-published last, the way an archive backfill saves them.
	newer, _, _ := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post", PublishedAt: 1_700_000_000})
	older, _, err := store.SaveArticle(context.Background(), SaveRequest{URL: pages.URL + "/post?page=2", PublishedAt: 1_600_000_000})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(filter ArticleFilter) []string {
		found, err := store.ListArticles(filter)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, a := range found {
			out = append(out, a.ID)
		}
		return out
	}
	if got := ids(ArticleFilter{}); len(got) != 2 || got[0] != newer.ID {
		t.Errorf("default order = %v, want %s first", got, newer.ID)
	}
	if got := ids(ArticleFilter{Order: OrderSaved}); len(got) != 2 || got[0] != older.ID {
		t.Errorf("saved order = %v, want %s first", got, older.ID)
	}
	if _, err := store.ListArticles(ArticleFilter{Order: OrderRelevance}); !errors.Is(err, ErrInvalidArticle) {
		t.Errorf("relevance without a query: err = %v, want ErrInvalidArticle", err)
	}
	if _, err := store.ListArticles(ArticleFilter{Order: "alphabetical"}); !errors.Is(err, ErrInvalidArticle) {
		t.Errorf("an unknown order: err = %v, want ErrInvalidArticle", err)
	}
}
