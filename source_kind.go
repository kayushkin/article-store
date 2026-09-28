package articlestore

// Where an article's text came from, stored in articles.source_kind. It says
// what fetched_source holds, and a refetch goes back the same way.
const (
	// SourceKindWebPage: fetched_source is the page's HTML and the text was
	// found in it by readability.
	SourceKindWebPage = "web_page"
	// SourceKindSubstackPostAPI: fetched_source is Substack's JSON for the post
	// (GET <publication>/api/v1/posts/<slug>) and the text is its body_html.
	SourceKindSubstackPostAPI = "substack_post_api"
)

// SourceKinds is the vocabulary, served by GET /vocabulary.
var SourceKinds = []string{SourceKindWebPage, SourceKindSubstackPostAPI}
