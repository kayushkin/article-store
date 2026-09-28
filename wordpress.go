package articlestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	htmlstd "html"
	"net/http"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
)

// WordPressPost is one post as a WordPress site's REST API serves it, from
// GET <site>/wp-json/wp/v2/posts with _embed=author. Only the fields used are
// named.
type WordPressPost struct {
	ID      int64  `json:"id"`
	Link    string `json:"link"`
	DateGMT string `json:"date_gmt"`
	Title   struct {
		Rendered string `json:"rendered"`
	} `json:"title"`
	Content struct {
		Rendered  string `json:"rendered"`
		Protected bool   `json:"protected"`
	} `json:"content"`
	Excerpt struct {
		Rendered string `json:"rendered"`
	} `json:"excerpt"`
	Embedded struct {
		// Author holds the post's author, or an error object with no name when
		// the site hides its users.
		Author []struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"_embedded"`
	// Raw is the post's JSON as served, kept as the article's fetched source.
	Raw string `json:"-"`
}

// wordpressPostsPath is where a WordPress site serves its posts.
const wordpressPostsPath = "/wp-json/wp/v2/posts"

// wordpressInvalidPageCode is the error code WordPress answers, with a 400,
// for a page past the last one.
const wordpressInvalidPageCode = "rest_post_invalid_page_number"

// plainTextPolicy strips every tag, for a title or excerpt WordPress serves as
// HTML.
var plainTextPolicy = bluemonday.StrictPolicy()

// plainTextFromHTML turns rendered HTML into one line of text: tags dropped,
// entities such as &#8217; decoded, runs of whitespace made one space.
func plainTextFromHTML(rendered string) string {
	return strings.Join(strings.Fields(htmlstd.UnescapeString(plainTextPolicy.Sanitize(rendered))), " ")
}

// Byline is the embedded author's name, or empty when the site did not say.
func (post WordPressPost) Byline() string {
	if len(post.Embedded.Author) == 0 {
		return ""
	}
	return strings.TrimSpace(post.Embedded.Author[0].Name)
}

// PublishedAt is date_gmt as unix seconds, or 0 when it does not parse.
// WordPress writes date_gmt in UTC with no zone.
func (post WordPressPost) PublishedAt() int64 {
	published, err := time.Parse("2006-01-02T15:04:05", post.DateGMT)
	if err != nil {
		return 0
	}
	return published.Unix()
}

// Extraction turns the post into what the store keeps. postURL is the URL it
// is saved under.
func (post WordPressPost) Extraction(postURL string) (Extraction, error) {
	extraction, err := contentFromArticleHTML(post.Content.Rendered, postURL)
	if err != nil {
		return Extraction{}, err
	}
	extraction.Title = plainTextFromHTML(post.Title.Rendered)
	extraction.Byline = post.Byline()
	extraction.Excerpt = plainTextFromHTML(post.Excerpt.Rendered)
	extraction.PublishedAt = post.PublishedAt()
	extraction.FetchedSource = post.Raw
	extraction.SourceKind = SourceKindWordPressPostAPI
	return extraction, nil
}

// wordpressArchive reads a WordPress archive through the REST API. The list
// carries each post's content, so a page of posts costs one request.
type wordpressArchive struct{}

// readPage turns the offset into a page number and a number of posts to skip
// on that page, since WordPress pages by number, not offset.
func (wordpressArchive) readPage(ctx context.Context, client *http.Client, publication Publication, offset int) (archivePage, error) {
	pageNumber := offset/ArchivePageSize + 1
	posts, err := FetchWordPressPostsPage(ctx, client, publication.BaseURL, pageNumber, ArchivePageSize)
	if err != nil {
		return archivePage{}, err
	}
	skip := offset % ArchivePageSize
	if skip >= len(posts) {
		return archivePage{}, nil
	}
	page := archivePage{entries: make([]archiveEntry, 0, len(posts)-skip)}
	for _, post := range posts[skip:] {
		post := post
		page.entries = append(page.entries, archiveEntry{
			postURL:   post.Link,
			isArticle: !post.Content.Protected,
			extract: func(_ context.Context, _ *http.Client, postURL string) (Extraction, error) {
				return post.Extraction(postURL)
			},
		})
	}
	return page, nil
}

// FetchWordPressPostsPage fetches page pageNumber (from 1) of the posts of the
// WordPress site at baseURL, newest first, perPage to a page. A page past the
// last one is no posts.
func FetchWordPressPostsPage(ctx context.Context, client *http.Client, baseURL string, pageNumber, perPage int) ([]WordPressPost, error) {
	pageURL := fmt.Sprintf("%s%s?per_page=%d&page=%d&orderby=date&order=desc&_embed=author",
		baseURL, wordpressPostsPath, perPage, pageNumber)
	raw, err := fetchJSON(ctx, client, pageURL)
	var unexpectedStatus *UnexpectedStatusError
	if errors.As(err, &unexpectedStatus) && unexpectedStatus.StatusCode == http.StatusBadRequest &&
		bytes.Contains(unexpectedStatus.Body, []byte(wordpressInvalidPageCode)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rawPosts []json.RawMessage
	if err := json.Unmarshal(raw, &rawPosts); err != nil {
		return nil, fmt.Errorf("%w: %s did not answer a list of posts: %v", ErrFetchFailed, pageURL, err)
	}
	posts := make([]WordPressPost, 0, len(rawPosts))
	for _, rawPost := range rawPosts {
		post, err := decodeWordPressPost(rawPost, pageURL)
		if err != nil {
			return nil, err
		}
		posts = append(posts, post)
	}
	return posts, nil
}

// FetchWordPressPost fetches one post by its WordPress id from the site at
// baseURL.
func FetchWordPressPost(ctx context.Context, client *http.Client, baseURL string, postID int64) (WordPressPost, error) {
	postURL := fmt.Sprintf("%s%s/%d?_embed=author", baseURL, wordpressPostsPath, postID)
	raw, err := fetchJSON(ctx, client, postURL)
	if err != nil {
		return WordPressPost{}, err
	}
	return decodeWordPressPost(raw, postURL)
}

func decodeWordPressPost(raw []byte, requestURL string) (WordPressPost, error) {
	var post WordPressPost
	if err := json.Unmarshal(raw, &post); err != nil {
		return WordPressPost{}, fmt.Errorf("%w: %s did not answer a post: %v", ErrFetchFailed, requestURL, err)
	}
	post.Raw = string(raw)
	return post, nil
}

// refetchWordPressPost fetches a backfilled WordPress article's post again. The
// post's id is in the JSON it was saved from, and the site is its publication.
func (s *Store) refetchWordPressPost(ctx context.Context, article Article) (Extraction, error) {
	if article.PublicationID == "" {
		return Extraction{}, fmt.Errorf("article %s has source_kind %q and no publication to fetch it from", article.ID, article.SourceKind)
	}
	publication, err := s.GetPublication(article.PublicationID)
	if err != nil {
		return Extraction{}, err
	}
	source, err := s.GetFetchedSource(article.ID)
	if err != nil {
		return Extraction{}, err
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(source), &saved); err != nil || saved.ID == 0 {
		return Extraction{}, fmt.Errorf("article %s: its fetched source names no WordPress post id (%v)", article.ID, err)
	}
	post, err := FetchWordPressPost(ctx, s.httpClient, publication.BaseURL, saved.ID)
	if err != nil {
		return Extraction{}, err
	}
	return post.Extraction(article.URL)
}
