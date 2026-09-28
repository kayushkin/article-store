package articlestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RateLimitedError is a 429 from a site. The backfill waits RetryAfter before
// asking that site again.
type RateLimitedError struct {
	URL        string
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("%s answered 429 Too Many Requests; retry after %s", e.URL, e.RetryAfter)
}

// UnexpectedStatusError is an answer outside 2xx other than a 429. It is
// ErrFetchFailed to errors.Is, and keeps the start of the body so a caller can
// read an error code the site put there.
type UnexpectedStatusError struct {
	URL        string
	Status     string
	StatusCode int
	Body       []byte
}

func (e *UnexpectedStatusError) Error() string {
	return fmt.Sprintf("%v: %s answered %s", ErrFetchFailed, e.URL, e.Status)
}

func (e *UnexpectedStatusError) Unwrap() error { return ErrFetchFailed }

// MaximumErrorBodyBytes caps how much of an error answer is kept.
const MaximumErrorBodyBytes = 64 << 10

// DefaultRateLimitWait is how long to wait after a 429 that names no
// Retry-After.
const DefaultRateLimitWait = 10 * time.Minute

// SubstackArchiveEntry is one post in a publication's archive list, as
// GET <publication>/api/v1/archive serves it. Only the fields used are named.
type SubstackArchiveEntry struct {
	ID               int64            `json:"id"`
	Slug             string           `json:"slug"`
	Title            string           `json:"title"`
	Subtitle         string           `json:"subtitle"`
	PostDate         string           `json:"post_date"`
	Audience         string           `json:"audience"` // everyone | only_paid | founding
	Type             string           `json:"type"`     // newsletter | podcast | thread | …
	CanonicalURL     string           `json:"canonical_url"`
	PublicationID    int64            `json:"publication_id"`
	PublishedBylines []SubstackByline `json:"publishedBylines"`
}

// SubstackByline is one author of a post.
type SubstackByline struct {
	Name string `json:"name"`
}

// SubstackPost is one post as GET <publication>/api/v1/posts/<slug> serves it.
// For a paid post read without a subscription, BodyHTML is only the preview.
type SubstackPost struct {
	SubstackArchiveEntry
	BodyHTML string `json:"body_html"`
	// Raw is the whole response, kept as the article's fetched source.
	Raw string `json:"-"`
}

// SubstackPostTypesSaved are the archive entry types that are articles. A
// thread is a chat, and has no body to read.
var SubstackPostTypesSaved = map[string]bool{"newsletter": true, "podcast": true}

// SubstackPaidOnlyTag marks an article whose publication showed only a preview
// to a reader without a subscription, so its text stops short.
const SubstackPaidOnlyTag = "paid-only"

// IsPaidOnly reports whether a reader without a subscription sees only a
// preview.
func (entry SubstackArchiveEntry) IsPaidOnly() bool {
	return entry.Audience != "" && entry.Audience != "everyone"
}

// Byline joins the post's authors' names.
func (entry SubstackArchiveEntry) Byline() string {
	names := make([]string, 0, len(entry.PublishedBylines))
	for _, byline := range entry.PublishedBylines {
		if name := strings.TrimSpace(byline.Name); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

// PublishedAt is post_date as unix seconds, or 0 when it does not parse.
func (entry SubstackArchiveEntry) PublishedAt() int64 {
	published, err := time.Parse(time.RFC3339, entry.PostDate)
	if err != nil {
		return 0
	}
	return published.Unix()
}

// Extraction turns the post into what the store keeps. postURL is the URL it
// is saved under.
func (post SubstackPost) Extraction(postURL string) (Extraction, error) {
	extraction, err := contentFromArticleHTML(post.BodyHTML, postURL)
	if err != nil {
		return Extraction{}, err
	}
	extraction.Title = strings.TrimSpace(post.Title)
	extraction.Byline = post.Byline()
	extraction.Excerpt = strings.TrimSpace(post.Subtitle)
	extraction.PublishedAt = post.PublishedAt()
	extraction.FetchedSource = post.Raw
	extraction.SourceKind = SourceKindSubstackPostAPI
	return extraction, nil
}

// substackArchive reads a Substack archive through /api/v1/archive. The list
// leaves body_html empty, so each post costs a request of its own.
type substackArchive struct{}

func (substackArchive) readPage(ctx context.Context, client *http.Client, publication Publication, offset int) (archivePage, error) {
	substackEntries, err := FetchSubstackArchivePage(ctx, client, publication.BaseURL, offset, ArchivePageSize)
	if err != nil {
		return archivePage{}, err
	}
	page := archivePage{entries: make([]archiveEntry, 0, len(substackEntries))}
	if len(substackEntries) > 0 && substackEntries[0].PublicationID != 0 {
		page.platformPublicationRef = fmt.Sprint(substackEntries[0].PublicationID)
	}
	for _, substackEntry := range substackEntries {
		entry := archiveEntry{
			postURL:         substackEntry.CanonicalURL,
			isArticle:       SubstackPostTypesSaved[substackEntry.Type],
			extract:         extractSubstackPost,
			extractAsksSite: true,
		}
		if substackEntry.IsPaidOnly() {
			entry.tags = []string{SubstackPaidOnlyTag}
		}
		page.entries = append(page.entries, entry)
	}
	return page, nil
}

// extractSubstackPost fetches the post at postURL through the API and returns
// what the store keeps of it.
func extractSubstackPost(ctx context.Context, client *http.Client, postURL string) (Extraction, error) {
	post, err := FetchSubstackPost(ctx, client, postURL)
	if err != nil {
		return Extraction{}, err
	}
	return post.Extraction(postURL)
}

// FetchSubstackArchivePage fetches up to limit archive entries of the
// publication at baseURL, newest first, starting offset entries in.
func FetchSubstackArchivePage(ctx context.Context, client *http.Client, baseURL string, offset, limit int) ([]SubstackArchiveEntry, error) {
	pageURL := fmt.Sprintf("%s/api/v1/archive?sort=new&offset=%d&limit=%d", baseURL, offset, limit)
	raw, err := fetchJSON(ctx, client, pageURL)
	if err != nil {
		return nil, err
	}
	var entries []SubstackArchiveEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%w: %s did not answer an archive list: %v", ErrFetchFailed, pageURL, err)
	}
	return entries, nil
}

// FetchSubstackPost fetches the post at postURL (https://<host>/p/<slug>)
// through the API of the host it is on, which works for a custom domain too.
func FetchSubstackPost(ctx context.Context, client *http.Client, postURL string) (SubstackPost, error) {
	parsed, err := url.Parse(postURL)
	if err != nil {
		return SubstackPost{}, fmt.Errorf("%w: %v", ErrInvalidArticle, err)
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) != 2 || segments[0] != "p" || segments[1] == "" {
		return SubstackPost{}, fmt.Errorf("%w: %s is not a Substack post URL (/p/<slug>)", ErrInvalidArticle, postURL)
	}
	apiURL := fmt.Sprintf("%s://%s/api/v1/posts/%s", parsed.Scheme, parsed.Host, url.PathEscape(segments[1]))
	raw, err := fetchJSON(ctx, client, apiURL)
	if err != nil {
		return SubstackPost{}, err
	}
	var post SubstackPost
	if err := json.Unmarshal(raw, &post); err != nil {
		return SubstackPost{}, fmt.Errorf("%w: %s did not answer a post: %v", ErrFetchFailed, apiURL, err)
	}
	post.Raw = string(raw)
	return post, nil
}

func fetchJSON(ctx context.Context, client *http.Client, requestURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArticle, err)
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitedError{URL: requestURL, RetryAfter: retryAfter(response.Header.Get("Retry-After"))}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, MaximumErrorBodyBytes))
		return nil, &UnexpectedStatusError{URL: requestURL, Status: response.Status, StatusCode: response.StatusCode, Body: body}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaximumPageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrFetchFailed, requestURL, err)
	}
	if len(body) > MaximumPageBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrFetchFailed, requestURL, MaximumPageBytes)
	}
	return body, nil
}

// retryAfter reads a Retry-After header given in seconds. An HTTP date or a
// missing header gets DefaultRateLimitWait.
func retryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds <= 0 {
		return DefaultRateLimitWait
	}
	return time.Duration(seconds) * time.Second
}

// isRateLimited reports whether err is a 429, and returns it.
func isRateLimited(err error) (*RateLimitedError, bool) {
	var rateLimited *RateLimitedError
	return rateLimited, errors.As(err, &rateLimited)
}
