package articlestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"codeberg.org/readeck/go-readability/v2/render"
	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

// ErrFetchFailed marks a page that could not be fetched or was not HTML. It is
// the remote site's answer, not this service's fault, so the HTTP layer answers
// 502 and passes the reason on.
var ErrFetchFailed = errors.New("fetch failed")

// ErrNothingExtracted marks a page that fetched fine but held no article text
// the extractor could find: a login wall, an app shell drawn by JavaScript, an
// index page. Saving it would store a title and nothing to read.
var ErrNothingExtracted = errors.New("no article text found")

// MaximumPageBytes caps how much of a page is read. A long essay is well under
// 1 MB of HTML; anything past this is not an article.
const MaximumPageBytes = 10 << 20

// FetchTimeout bounds one fetch, redirects included.
const FetchTimeout = 30 * time.Second

// userAgent names this service honestly. Some sites refuse Go's default agent.
const userAgent = "article-store/1 (+https://github.com/kayushkin/article-store)"

// Extraction is what we pulled out of one page.
type Extraction struct {
	FinalURL        string
	Title           string
	Byline          string
	SiteName        string
	Language        string
	PublishedAt     int64
	Excerpt         string
	ContentHTML     string
	ContentMarkdown string
	ContentText     string
	WordCount       int
	// FetchedSource is what was fetched, and SourceKind says what that is.
	FetchedSource string
	SourceKind    string
}

// contentPolicy is what survives in content_html. The page is somebody else's
// markup, rendered on the dashboard's origin, so scripts, styles, forms and
// event handlers go; text, links, images and tables stay.
var contentPolicy = func() *bluemonday.Policy {
	policy := bluemonday.UGCPolicy()
	policy.RequireNoReferrerOnLinks(true)
	policy.AddTargetBlankToFullyQualifiedLinks(true)
	return policy
}()

// FetchPage GETs pageURL and returns the page's HTML and the URL it ended at
// after redirects. Only http and https are fetched.
func FetchPage(ctx context.Context, client *http.Client, pageURL string) (string, string, error) {
	parsed, err := url.Parse(pageURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", "", fmt.Errorf("%w: %q is not an http or https URL", ErrInvalidArticle, pageURL)
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidArticle, err)
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	response, err := client.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", "", fmt.Errorf("%w: %s answered %s", ErrFetchFailed, pageURL, response.Status)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return "", "", fmt.Errorf("%w: %s is %q, not an HTML page", ErrFetchFailed, pageURL, response.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaximumPageBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("%w: reading %s: %v", ErrFetchFailed, pageURL, err)
	}
	if len(body) > MaximumPageBytes {
		return "", "", fmt.Errorf("%w: %s is larger than %d bytes", ErrFetchFailed, pageURL, MaximumPageBytes)
	}
	return string(body), response.Request.URL.String(), nil
}

// Extract pulls the article out of a page's HTML with readability. pageURL
// resolves the page's relative links and images.
func Extract(sourceHTML, pageURL string) (Extraction, error) {
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return Extraction{}, fmt.Errorf("%w: %v", ErrInvalidArticle, err)
	}
	article, err := readability.FromReader(strings.NewReader(sourceHTML), parsed)
	if err != nil {
		return Extraction{}, fmt.Errorf("%w: %v", ErrNothingExtracted, err)
	}
	if article.Node == nil {
		return Extraction{}, fmt.Errorf("%w in %s", ErrNothingExtracted, pageURL)
	}
	var rendered bytes.Buffer
	if err := article.RenderHTML(&rendered); err != nil {
		return Extraction{}, fmt.Errorf("render extracted html: %w", err)
	}
	extraction, err := contentFromArticleHTML(rendered.String(), pageURL)
	if err != nil {
		return Extraction{}, err
	}
	extraction.Title = strings.TrimSpace(article.Title())
	extraction.Byline = strings.TrimSpace(article.Byline())
	extraction.SiteName = strings.TrimSpace(article.SiteName())
	extraction.Language = strings.TrimSpace(article.Language())
	extraction.Excerpt = strings.TrimSpace(article.Excerpt())
	extraction.FetchedSource = sourceHTML
	extraction.SourceKind = SourceKindWebPage
	// A page that states no date leaves published_at at 0, which means unknown.
	if published, err := article.PublishedTime(); err == nil && !published.IsZero() {
		extraction.PublishedAt = published.Unix()
	}
	return extraction, nil
}

// contentFromArticleHTML turns HTML that is already just the article — what
// readability found, or a post body a platform's API served — into the three
// forms stored: sanitized HTML, markdown and plain text.
func contentFromArticleHTML(articleHTML, pageURL string) (Extraction, error) {
	contentHTML := contentPolicy.Sanitize(articleHTML)
	document, err := html.Parse(strings.NewReader(contentHTML))
	if err != nil {
		return Extraction{}, fmt.Errorf("parse sanitized html: %w", err)
	}
	contentText := strings.TrimSpace(render.InnerText(document))
	if contentText == "" {
		return Extraction{}, fmt.Errorf("%w in %s", ErrNothingExtracted, pageURL)
	}
	contentMarkdown, err := htmltomarkdown.ConvertString(contentHTML)
	if err != nil {
		return Extraction{}, fmt.Errorf("convert to markdown: %w", err)
	}
	return Extraction{
		FinalURL:        pageURL,
		ContentHTML:     contentHTML,
		ContentMarkdown: strings.TrimSpace(contentMarkdown),
		ContentText:     contentText,
		WordCount:       len(strings.Fields(contentText)),
	}, nil
}
