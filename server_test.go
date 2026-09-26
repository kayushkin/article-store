package articlestore

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	RegisterHandlers(mux, newTestStore(t))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func do(t *testing.T, server *httptest.Server, method, path, body string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, raw
}

func jsonBody(t *testing.T, value any) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}

func TestPostCreatesThenReturnsTheStoredArticle(t *testing.T) {
	server := newTestServer(t)
	pages, _ := pageServer(t)
	body := jsonBody(t, map[string]any{"url": pages.URL + "/post", "byline": "Mara Quill", "tags": []string{"sea"}})

	status, _, raw := do(t, server, "POST", "/articles", body)
	if status != http.StatusCreated {
		t.Fatalf("first POST = %d: %s", status, raw)
	}
	var article Article
	if err := json.Unmarshal(raw, &article); err != nil {
		t.Fatal(err)
	}
	if article.ID != "article_000001" || article.Byline != "Mara Quill" || article.ContentMarkdown == "" {
		t.Errorf("id=%s byline=%q markdown empty=%v", article.ID, article.Byline, article.ContentMarkdown == "")
	}
	if strings.Contains(string(raw), "source_html") {
		t.Errorf("the article JSON carries source_html, which is large and served on its own route")
	}
	if status, _, raw := do(t, server, "POST", "/articles", body); status != http.StatusOK {
		t.Errorf("second POST = %d: %s", status, raw)
	}
}

func TestPostRefusesAMisspelledField(t *testing.T) {
	server := newTestServer(t)
	status, _, raw := do(t, server, "POST", "/articles", `{"url":"https://example.com/a","tag":["x"]}`)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), `unknown field \"tag\"`) {
		t.Errorf("POST with a misspelled field = %d: %s", status, raw)
	}
}

func TestStatusCodesNameWhoseFaultItIs(t *testing.T) {
	server := newTestServer(t)
	pages, _ := pageServer(t)
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/articles", `{"url":"` + pages.URL + `/gone"}`, http.StatusBadGateway},
		{"POST", "/articles", `{"url":"mailto:a@b.c"}`, http.StatusBadRequest},
		{"GET", "/articles/article_000042", "", http.StatusNotFound},
		{"GET", "/articles/42", "", http.StatusBadRequest},
		{"GET", "/articles?q=%22open", "", http.StatusBadRequest},
		{"GET", "/articles?read=maybe", "", http.StatusBadRequest},
		{"GET", "/articles?kind=novel", "", http.StatusBadRequest},
		{"PATCH", "/articles/article_000042", `{"note":"x"}`, http.StatusNotFound},
	}
	for _, c := range cases {
		if status, _, raw := do(t, server, c.method, c.path, c.body); status != c.want {
			t.Errorf("%s %s = %d, want %d: %s", c.method, c.path, status, c.want, raw)
		}
	}
}

func TestMarkdownFormatIsOneDocumentAnAgentCanRead(t *testing.T) {
	server := newTestServer(t)
	pages, _ := pageServer(t)
	do(t, server, "POST", "/articles", jsonBody(t, map[string]any{"url": pages.URL + "/post", "byline": "Mara Quill", "note": "the ending"}))

	status, header, raw := do(t, server, "GET", "/articles/article_000001?format=markdown", "")
	if status != http.StatusOK || !strings.HasPrefix(header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("GET markdown = %d %s", status, header.Get("Content-Type"))
	}
	document := string(raw)
	for _, want := range []string{"# The Lighthouse Keeper\n", "By Mara Quill · Harbour Notes · 2015-08-17", "Saved as article_000001", "> Note: the ending", "second lantern"} {
		if !strings.Contains(document, want) {
			t.Errorf("markdown lacks %q:\n%s", want, document)
		}
	}
}

func TestSourceIsServedAsTextThatCannotRun(t *testing.T) {
	server := newTestServer(t)
	pages, _ := pageServer(t)
	do(t, server, "POST", "/articles", jsonBody(t, map[string]any{"url": pages.URL + "/post"}))
	status, header, raw := do(t, server, "GET", "/articles/article_000001/source", "")
	if status != http.StatusOK || !strings.HasPrefix(header.Get("Content-Type"), "text/plain") ||
		header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(string(raw), "<script>") {
		t.Errorf("source = %d %q %q", status, header.Get("Content-Type"), header.Get("X-Content-Type-Options"))
	}
}

func TestPatchMarksReadAndListFiltersOnIt(t *testing.T) {
	server := newTestServer(t)
	pages, _ := pageServer(t)
	do(t, server, "POST", "/articles", jsonBody(t, map[string]any{"url": pages.URL + "/post"}))
	do(t, server, "POST", "/articles", jsonBody(t, map[string]any{"url": pages.URL + "/post?page=2"}))

	status, _, raw := do(t, server, "PATCH", "/articles/article_000001", `{"read":true}`)
	if status != http.StatusOK || !strings.Contains(string(raw), `"read_at":`) || strings.Contains(string(raw), `"read_at":0`) {
		t.Fatalf("PATCH read = %d: %s", status, raw)
	}
	_, _, raw = do(t, server, "GET", "/articles?read=false&limit=1", "")
	var listing struct {
		Articles []ArticleSummary `json:"articles"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Total != 1 || len(listing.Articles) != 1 || listing.Articles[0].ID != "article_000002" {
		t.Errorf("unread listing = %s", raw)
	}
	if strings.Contains(string(raw), "content_html") {
		t.Errorf("a listing carries article content")
	}
}

func TestDeleteAndRestoreOverHTTP(t *testing.T) {
	server := newTestServer(t)
	pages, _ := pageServer(t)
	do(t, server, "POST", "/articles", jsonBody(t, map[string]any{"url": pages.URL + "/post"}))
	if status, _, _ := do(t, server, "DELETE", "/articles/article_000001", ""); status != http.StatusOK {
		t.Fatalf("DELETE = %d", status)
	}
	if status, _, raw := do(t, server, "POST", "/articles", jsonBody(t, map[string]any{"url": pages.URL + "/post"})); status != http.StatusConflict {
		t.Errorf("POST of a deleted URL = %d: %s", status, raw)
	}
	if status, _, _ := do(t, server, "POST", "/articles/article_000001/restore", ""); status != http.StatusOK {
		t.Errorf("restore = %d", status)
	}
	if status, _, raw := do(t, server, "POST", "/articles/article_000001/refetch", ""); status != http.StatusOK {
		t.Errorf("refetch with no body = %d: %s", status, raw)
	}
}
