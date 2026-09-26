package articlestore

// ArticleKinds is the vocabulary an article's kind may take, in display order.
// GET /vocabulary serves it, so a caller builds its picker from here rather
// than restating the list.
var ArticleKinds = []string{"article", "essay", "story", "poem", "post", "paper", "other"}

// DefaultArticleKind is the kind an article gets when the caller names none.
const DefaultArticleKind = "article"

var articleKindSet = func() map[string]bool {
	set := make(map[string]bool, len(ArticleKinds))
	for _, kind := range ArticleKinds {
		set[kind] = true
	}
	return set
}()

// IsArticleKind reports whether kind is in the vocabulary.
func IsArticleKind(kind string) bool { return articleKindSet[kind] }
