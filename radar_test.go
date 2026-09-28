package articlestore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return value
}

func TestASourceWithNoStatusIsProposedAndNeverDue(t *testing.T) {
	server := newTestServer(t)
	status, _, raw := do(t, server, "POST", "/sources", `{"kind":"research","prompt":"housing policy in Europe"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d: %s", status, raw)
	}
	source := decode[RadarSource](t, raw)
	if source.ID != "source_000001" || source.Status != RadarSourceStatusProposed || source.CadenceHours != 168 {
		t.Fatalf("created %+v", source)
	}
	_, _, raw = do(t, server, "GET", "/sources?due=1", "")
	if due := decode[map[string][]RadarSource](t, raw)["sources"]; len(due) != 0 {
		t.Fatalf("a proposed source is due: %+v", due)
	}

	status, _, raw = do(t, server, "PATCH", "/sources/"+source.ID, `{"status":"active"}`)
	if status != http.StatusOK {
		t.Fatalf("approve = %d: %s", status, raw)
	}
	_, _, raw = do(t, server, "GET", "/sources?due=1&kind=research", "")
	if due := decode[map[string][]RadarSource](t, raw)["sources"]; len(due) != 1 || due[0].ID != source.ID {
		t.Fatalf("an approved source is not due at once: %+v", due)
	}

	status, _, raw = do(t, server, "POST", "/sources/"+source.ID+"/ran", `{"result":"4 suggestions"}`)
	if status != http.StatusOK {
		t.Fatalf("ran = %d: %s", status, raw)
	}
	ran := decode[RadarSource](t, raw)
	if ran.LastRunAt == 0 || ran.NextRunAt != ran.LastRunAt+168*3600 || ran.LastResult != "4 suggestions" {
		t.Fatalf("ran did not stamp and advance: %+v", ran)
	}
	_, _, raw = do(t, server, "GET", "/sources?due=1", "")
	if due := decode[map[string][]RadarSource](t, raw)["sources"]; len(due) != 0 {
		t.Fatalf("a source that just ran is still due: %+v", due)
	}

	_, _, raw = do(t, server, "PATCH", "/sources/"+source.ID, `{"cadence_hours":24}`)
	if patched := decode[RadarSource](t, raw); patched.NextRunAt != ran.LastRunAt+24*3600 {
		t.Fatalf("a new cadence did not move next_run_at: %+v", patched)
	}
	do(t, server, "PATCH", "/sources/"+source.ID, `{"enabled":false,"status":"proposed"}`)
	do(t, server, "PATCH", "/sources/"+source.ID, `{"status":"active","enabled":false}`)
	_, _, raw = do(t, server, "GET", "/sources?due=1", "")
	if due := decode[map[string][]RadarSource](t, raw)["sources"]; len(due) != 0 {
		t.Fatalf("a disabled source is due: %+v", due)
	}
	if status, _, _ := do(t, server, "PATCH", "/sources/"+source.ID, `{}`); status != http.StatusBadRequest {
		t.Fatalf("an empty PATCH = %d", status)
	}
}

func TestSourceRequestsAreCheckedByKind(t *testing.T) {
	server := newTestServer(t)
	for _, body := range []string{
		`{"kind":"telepathy","prompt":"x"}`,
		`{"kind":"research"}`,
		`{"kind":"scout","prompt":"  "}`,
		`{"kind":"watch"}`,
		`{"kind":"watch","platform":"substack"}`,
		`{"kind":"watch","platform":"medium","base_url":"https://medium.com"}`,
		`{"kind":"watch","publication_id":"publication_000099"}`,
		`{"kind":"watch","platform":"substack","base_url":"https://a.substack.com/p/post"}`,
		`{"kind":"research","prompt":"x","base_url":"https://a.substack.com"}`,
		`{"kind":"research","prompt":"x","status":"maybe"}`,
		`{"kind":"research","prompt":"x","colour":"red"}`,
	} {
		if status, _, raw := do(t, server, "POST", "/sources", body); status != http.StatusBadRequest {
			t.Errorf("POST %s = %d: %s", body, status, raw)
		}
	}
	if status, _, _ := do(t, server, "GET", "/sources/source_000404", ""); status != http.StatusNotFound {
		t.Errorf("unknown source = %d", status)
	}
	if status, _, _ := do(t, server, "GET", "/sources?kind=nope", ""); status != http.StatusBadRequest {
		t.Errorf("bad kind filter = %d", status)
	}
	_, _, raw := do(t, server, "GET", "/vocabulary", "")
	vocabulary := decode[map[string]any](t, raw)
	for _, key := range []string{"radar_source_kinds", "radar_source_statuses", "suggestion_statuses"} {
		if vocabulary[key] == nil {
			t.Errorf("vocabulary lacks %s: %s", key, raw)
		}
	}
}

func TestApprovingAWatchOfANewBlogMakesItAPublicationWithoutImportingTheArchive(t *testing.T) {
	store := newTestStore(t)
	source, created, err := store.AddRadarSource(RadarSourceRequest{
		Kind: "watch", Platform: "wordpress", BaseURL: "https://Example.org/", Name: "Example", ProposedBy: "source_000002",
	})
	if err != nil || !created {
		t.Fatalf("add = %+v, %v, %v", source, created, err)
	}
	if source.PublicationID != "" || source.BaseURL != "https://example.org" {
		t.Fatalf("proposed watch %+v", source)
	}
	publications, _ := store.ListPublications()
	if len(publications) != 0 {
		t.Fatalf("a proposal made a publication: %+v", publications)
	}
	again, created, err := store.AddRadarSource(RadarSourceRequest{Kind: "watch", Platform: "wordpress", BaseURL: "https://example.org"})
	if err != nil || created || again.ID != source.ID {
		t.Fatalf("a second proposal of the same blog = %+v, %v, %v", again, created, err)
	}

	store.PatchRadarSource(source.ID, RadarSourcePatch{Status: ptr("rejected")})
	again, created, _ = store.AddRadarSource(RadarSourceRequest{Kind: "watch", Platform: "wordpress", BaseURL: "https://example.org"})
	if created || again.Status != RadarSourceStatusRejected {
		t.Fatalf("proposing a rejected watch again changed it: %+v", again)
	}

	approved, err := store.PatchRadarSource(source.ID, RadarSourcePatch{Status: ptr("active")})
	if err != nil {
		t.Fatal(err)
	}
	publication, err := store.GetPublication(approved.PublicationID)
	if err != nil {
		t.Fatalf("approval made no publication: %+v, %v", approved, err)
	}
	if publication.BaseURL != "https://example.org" || publication.Platform != "wordpress" || publication.Name != "Example" {
		t.Fatalf("publication %+v", publication)
	}
	if publication.Backfill.Status != "" {
		t.Fatalf("approval started an archive import: %+v", publication.Backfill)
	}
	if !approved.Enabled || approved.NextRunAt == 0 {
		t.Fatalf("approval did not make the watch due: %+v", approved)
	}
}

func TestAWatchOfAKnownBlogJoinsItsPublicationByID(t *testing.T) {
	store := newTestStore(t)
	publication, _, _ := store.AddPublication(PublicationRequest{Platform: "substack", BaseURL: "https://noahpinion.substack.com", Name: "Noahpinion"})
	byURL, _, err := store.AddRadarSource(RadarSourceRequest{Kind: "watch", Platform: "substack", BaseURL: "https://noahpinion.substack.com/"})
	if err != nil || byURL.PublicationID != publication.ID {
		t.Fatalf("a watch named by base_url did not find the publication: %+v, %v", byURL, err)
	}
	byID, created, err := store.AddRadarSource(RadarSourceRequest{Kind: "watch", PublicationID: publication.ID, Status: "active"})
	if err != nil || created || byID.ID != byURL.ID {
		t.Fatalf("a second watch of one publication = %+v, %v, %v", byID, created, err)
	}
}

func ptr[T any](value T) *T { return &value }

// prependPosts puts new posts at the newest end of the fake archive.
func (fake *fakeSubstack) prependPosts(slugs ...string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	older := fake.entries
	fake.entries = nil
	for _, slug := range slugs {
		fake.entries = append(fake.entries, SubstackArchiveEntry{
			ID: int64(1000 + len(fake.entries)), Slug: slug, Title: "Post " + slug, PostDate: "2026-09-28T10:00:00Z",
			Audience: "everyone", Type: "newsletter", CanonicalURL: fake.server.URL + "/p/" + slug, PublicationID: 7223401,
		})
	}
	fake.entries = append(fake.entries, older...)
}

func (fake *fakeSubstack) requests() (archive, posts int) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.archiveRequests, fake.postRequests
}

func TestAWatchSavesOnlyNewPostsAndStopsAtTheFirstPageWithASavedOne(t *testing.T) {
	fake := newFakeSubstack(t)
	for index := 0; index < ArchivePageSize+10; index++ {
		fake.addPost(fmt.Sprintf("old-%02d", index), "newsletter", "everyone")
	}
	store, backfiller := newBackfiller(t)
	publication, _, _ := store.AddPublication(PublicationRequest{Platform: "substack", BaseURL: fake.server.URL, Name: "Fake"})
	store.StartBackfill(publication.ID, false)
	runUntilIdle(t, backfiller)

	source, _, err := store.AddRadarSource(RadarSourceRequest{Kind: "watch", PublicationID: publication.ID, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	fake.prependPosts("new-a", "new-b")
	archiveBefore, postsBefore := fake.requests()

	watcher := &Watcher{Store: store}
	worked, err := watcher.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("step = %v, %v", worked, err)
	}
	archiveAfter, postsAfter := fake.requests()
	if archiveAfter-archiveBefore != 1 || postsAfter-postsBefore != 2 {
		t.Fatalf("the watch made %d archive and %d post requests; want 1 and 2", archiveAfter-archiveBefore, postsAfter-postsBefore)
	}
	ran, _ := store.GetRadarSource(source.ID)
	if ran.LastRunAt == 0 || !strings.HasPrefix(ran.LastResult, "2 new saved, 48 already saved") {
		t.Fatalf("after the watch: %+v", ran)
	}
	articles, _ := store.ListArticles(ArticleFilter{PublicationID: publication.ID, Order: OrderSaved, Limit: 2})
	for _, article := range articles {
		if article.AddedBy != "watch "+source.ID || !strings.Contains(article.URL, "/p/new-") {
			t.Fatalf("a watched post was saved as %+v", article)
		}
	}
	if worked, _ := watcher.Step(context.Background()); worked {
		t.Fatal("the watch ran again before its cadence")
	}
}

func TestAWatchWaitsWhileTheArchiveImportRunsAndAfterA429(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.addPost("one", "newsletter", "everyone")
	store := newTestStore(t)
	publication, _, _ := store.AddPublication(PublicationRequest{Platform: "substack", BaseURL: fake.server.URL})
	source, _, _ := store.AddRadarSource(RadarSourceRequest{Kind: "watch", PublicationID: publication.ID, Status: "active"})
	watcher := &Watcher{Store: store}

	store.StartBackfill(publication.ID, false)
	if worked, _ := watcher.Step(context.Background()); worked {
		t.Fatal("a watch ran while its publication's archive import was running")
	}
	store.StopBackfill(publication.ID)

	fake.mutex.Lock()
	fake.rateLimitNextPostRequests = 1
	fake.mutex.Unlock()
	if worked, err := watcher.Step(context.Background()); !worked || err != nil {
		t.Fatalf("step = %v, %v", worked, err)
	}
	waiting, _ := store.GetRadarSource(source.ID)
	if waiting.LastRunAt != 0 || !strings.Contains(waiting.LastResult, "429") {
		t.Fatalf("a 429 marked the watch ran: %+v", waiting)
	}
	after, _ := store.GetPublication(publication.ID)
	if after.Backfill.NextAttemptAt == 0 || after.Backfill.Status != BackfillStatusStopped {
		t.Fatalf("the 429 wait was not kept on the publication, or it touched the backfill: %+v", after.Backfill)
	}
	if worked, _ := watcher.Step(context.Background()); worked {
		t.Fatal("the watch asked the site again inside the 429 wait")
	}
}

func TestSuggestionsAreMadeOnceAcceptedIntoArticlesAndDismissedForGood(t *testing.T) {
	server := newTestServer(t)
	pages, fetches := pageServer(t)
	_, _, raw := do(t, server, "POST", "/sources", `{"kind":"research","prompt":"lighthouses","status":"active"}`)
	source := decode[RadarSource](t, raw)

	body := jsonBody(t, map[string]any{"url": pages.URL + "/post#top", "title": "The Lighthouse Keeper", "byline": "Mara Quill",
		"reason": "a short story about work nobody sees", "source_id": source.ID})
	status, _, raw := do(t, server, "POST", "/suggestions", body)
	if status != http.StatusCreated {
		t.Fatalf("suggest = %d: %s", status, raw)
	}
	suggestion := decode[Suggestion](t, raw)
	if suggestion.ID != "suggestion_000001" || suggestion.URL != pages.URL+"/post" || suggestion.Status != "proposed" {
		t.Fatalf("suggestion %+v", suggestion)
	}
	if fetches.Load() != 0 {
		t.Fatal("suggesting fetched the page")
	}
	status, _, raw = do(t, server, "POST", "/suggestions", strings.Replace(body, "/post#top", "/post/", 1))
	if again := decode[Suggestion](t, raw); status != http.StatusOK || again.ID != suggestion.ID {
		t.Fatalf("suggesting the same URL again = %d: %s", status, raw)
	}
	for _, bad := range []string{
		jsonBody(t, map[string]any{"url": pages.URL + "/other"}),
		jsonBody(t, map[string]any{"url": pages.URL + "/other", "reason": "x", "source_id": "source_000404"}),
		jsonBody(t, map[string]any{"url": "ftp://x", "reason": "x"}),
	} {
		if status, _, raw := do(t, server, "POST", "/suggestions", bad); status != http.StatusBadRequest {
			t.Errorf("POST %s = %d: %s", bad, status, raw)
		}
	}

	status, _, raw = do(t, server, "POST", "/suggestions/"+suggestion.ID+"/accept", "")
	if status != http.StatusOK {
		t.Fatalf("accept = %d: %s", status, raw)
	}
	accepted := decode[struct {
		Suggestion Suggestion `json:"suggestion"`
		Article    Article    `json:"article"`
	}](t, raw)
	if accepted.Suggestion.Status != "accepted" || accepted.Suggestion.ArticleID != accepted.Article.ID ||
		accepted.Article.AddedBy != "suggestion "+suggestion.ID || accepted.Article.Byline != "Mara Quill" {
		t.Fatalf("accepted %+v", accepted)
	}
	if status, _, _ := do(t, server, "POST", "/suggestions/"+suggestion.ID+"/dismiss", ""); status != http.StatusBadRequest {
		t.Errorf("dismissing an accepted suggestion = %d", status)
	}

	status, _, raw = do(t, server, "POST", "/suggestions", jsonBody(t, map[string]any{"url": "https" + strings.TrimPrefix(pages.URL, "http") + "/post", "reason": "again"}))
	if status != http.StatusOK {
		t.Fatalf("suggesting an accepted URL under the other scheme = %d: %s", status, raw)
	}

	do(t, server, "POST", "/articles", jsonBody(t, map[string]any{"url": pages.URL + "/moved"}))
	status, _, raw = do(t, server, "POST", "/suggestions", jsonBody(t, map[string]any{"url": pages.URL + "/moved", "reason": "saved already"}))
	if status != http.StatusConflict || !strings.Contains(string(raw), "article_") {
		t.Fatalf("suggesting a saved article = %d: %s", status, raw)
	}

	_, _, raw = do(t, server, "POST", "/suggestions", jsonBody(t, map[string]any{"url": pages.URL + "/dull", "reason": "maybe", "source_id": source.ID}))
	dull := decode[Suggestion](t, raw)
	status, _, raw = do(t, server, "POST", "/suggestions/"+dull.ID+"/dismiss", "")
	if status != http.StatusOK || decode[Suggestion](t, raw).Status != "dismissed" {
		t.Fatalf("dismiss = %d: %s", status, raw)
	}
	status, _, raw = do(t, server, "POST", "/suggestions", jsonBody(t, map[string]any{"url": pages.URL + "/dull", "reason": "really"}))
	if again := decode[Suggestion](t, raw); status != http.StatusOK || again.Status != "dismissed" || again.Reason != "maybe" {
		t.Fatalf("a dismissed URL came back: %d %s", status, raw)
	}
	if status, _, _ := do(t, server, "POST", "/suggestions/"+dull.ID+"/accept", ""); status != http.StatusBadRequest {
		t.Errorf("accepting a dismissed suggestion = %d", status)
	}

	_, _, raw = do(t, server, "GET", "/suggestions?status=proposed", "")
	if proposed := decode[map[string][]Suggestion](t, raw)["suggestions"]; len(proposed) != 0 {
		t.Fatalf("proposed after deciding both: %+v", proposed)
	}
	_, _, raw = do(t, server, "GET", "/suggestions?source_id="+source.ID, "")
	if bySource := decode[map[string][]Suggestion](t, raw)["suggestions"]; len(bySource) != 2 || bySource[0].ID != dull.ID {
		t.Fatalf("by source, newest first: %+v", bySource)
	}
}

func TestAFailedFetchLeavesTheSuggestionProposed(t *testing.T) {
	server := newTestServer(t)
	pages, _ := pageServer(t)
	_, _, raw := do(t, server, "POST", "/suggestions", jsonBody(t, map[string]any{"url": pages.URL + "/gone", "reason": "x"}))
	suggestion := decode[Suggestion](t, raw)
	if status, _, _ := do(t, server, "POST", "/suggestions/"+suggestion.ID+"/accept", ""); status != http.StatusBadGateway {
		t.Fatalf("accepting a page that is gone = %d", status)
	}
	_, _, raw = do(t, server, "GET", "/suggestions/"+suggestion.ID, "")
	if decode[Suggestion](t, raw).Status != "proposed" {
		t.Fatalf("after a failed accept: %s", raw)
	}
}

func TestDeletingASourceKeepsItsSuggestions(t *testing.T) {
	server := newTestServer(t)
	_, _, raw := do(t, server, "POST", "/sources", `{"kind":"scout","prompt":"economics blogs"}`)
	source := decode[RadarSource](t, raw)
	do(t, server, "POST", "/suggestions", jsonBody(t, map[string]any{"url": "https://example.com/a", "reason": "x", "source_id": source.ID}))
	if status, _, raw := do(t, server, "DELETE", "/sources/"+source.ID, ""); status != http.StatusOK {
		t.Fatalf("delete = %d: %s", status, raw)
	}
	if status, _, _ := do(t, server, "DELETE", "/sources/"+source.ID, ""); status != http.StatusNotFound {
		t.Fatalf("second delete = %d", status)
	}
	_, _, raw = do(t, server, "GET", "/suggestions", "")
	if suggestions := decode[map[string][]Suggestion](t, raw)["suggestions"]; len(suggestions) != 1 || suggestions[0].SourceID != source.ID {
		t.Fatalf("suggestions after deleting their source: %+v", suggestions)
	}
}
