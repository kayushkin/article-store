package articlestore

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// platformArchive reads one platform's archive of posts. Every platform in
// PublicationPlatforms has one in publicationArchives.
type platformArchive interface {
	// readPage returns the archive entries starting offset entries in, newest
	// first. A page with no entries is the end of the archive.
	readPage(ctx context.Context, client *http.Client, publication Publication, offset int) (archivePage, error)
}

// publicationArchives is the archive reader of each publication platform.
var publicationArchives = map[string]platformArchive{
	"substack":  substackArchive{},
	"wordpress": wordpressArchive{},
}

// archivePage is one page of a publication's archive.
type archivePage struct {
	entries []archiveEntry
	// platformPublicationRef is the platform's own id for the publication, or
	// empty when the page does not name one.
	platformPublicationRef string
}

// archiveEntry is one post in an archive page.
type archiveEntry struct {
	// postURL is the post's canonical URL as the platform gives it.
	postURL string
	// isArticle is false for an entry with nothing to read, such as a
	// Substack chat thread or a password-protected WordPress post.
	isArticle bool
	tags      []string
	// extract returns the post's content saved under normalizedURL.
	extract func(ctx context.Context, client *http.Client, normalizedURL string) (Extraction, error)
	// extractAsksSite says whether extract makes a request, so the backfill
	// knows to pause after it.
	extractAsksSite bool
}

// findSavedPost looks a post up under its URL and under the forms of it a
// person may have saved by hand: the other of http and https, and the path
// with or without its trailing slash. A post saved under any of them counts as
// already saved. A deleted one is ErrDeleted, as in findSaved.
func (s *Store) findSavedPost(normalizedURL string) (Article, bool, error) {
	for _, candidate := range equivalentPostURLs(normalizedURL) {
		existing, found, err := s.findSaved(candidate)
		if found || err != nil {
			return existing, found, err
		}
	}
	return Article{}, false, nil
}

// equivalentPostURLs returns normalizedURL first, then the same URL with the
// other scheme, and each of those with the trailing slash of the path toggled.
func equivalentPostURLs(normalizedURL string) []string {
	parsed, err := url.Parse(normalizedURL)
	if err != nil {
		return []string{normalizedURL}
	}
	otherScheme := map[string]string{"http": "https", "https": "http"}[parsed.Scheme]
	schemes := []string{parsed.Scheme}
	if otherScheme != "" {
		schemes = append(schemes, otherScheme)
	}
	paths := []string{parsed.Path}
	if trimmed := strings.TrimSuffix(parsed.Path, "/"); trimmed != "" {
		if trimmed == parsed.Path {
			paths = append(paths, parsed.Path+"/")
		} else {
			paths = append(paths, trimmed)
		}
	}
	candidates := make([]string, 0, len(schemes)*len(paths))
	for _, path := range paths {
		for _, scheme := range schemes {
			variant := *parsed
			variant.Scheme = scheme
			variant.Path = path
			variant.RawPath = ""
			candidates = append(candidates, variant.String())
		}
	}
	return candidates
}

// stepArchive reads one archive page of the publication and saves each post on
// it, moving the offset past every entry as it goes.
func (b *Backfiller) stepArchive(ctx context.Context, publication Publication, archive platformArchive) error {
	store := b.Store
	page, err := archive.readPage(ctx, store.httpClient, publication, publication.Backfill.Offset)
	if rateLimited, is := isRateLimited(err); is {
		return store.setBackfillWait(publication.ID, time.Now().Add(rateLimited.RetryAfter), rateLimited.Error())
	}
	if err != nil {
		return store.finishBackfill(publication.ID, BackfillStatusFailed, err.Error())
	}
	sleep(ctx, b.RequestInterval)
	if len(page.entries) == 0 {
		return store.finishBackfill(publication.ID, BackfillStatusDone, publication.Backfill.LastError)
	}
	if publication.PlatformPublicationRef == "" && page.platformPublicationRef != "" {
		if _, err := store.db.Exec(`UPDATE publications SET platform_publication_ref = ? WHERE id = ?`,
			page.platformPublicationRef, publication.ID); err != nil {
			return err
		}
	}
	for _, entry := range page.entries {
		if ctx.Err() != nil {
			return nil
		}
		// A stop request between entries ends the page here.
		current, err := store.GetPublication(publication.ID)
		if err != nil {
			return err
		}
		if current.Backfill.Status != BackfillStatusRunning {
			return nil
		}
		outcome, askedSite, err := store.saveArchiveEntry(ctx, publication, entry, "backfill "+publication.ID)
		if rateLimited, is := isRateLimited(err); is {
			return store.setBackfillWait(publication.ID, time.Now().Add(rateLimited.RetryAfter), rateLimited.Error())
		}
		lastError := ""
		if err != nil {
			lastError = entry.postURL + ": " + err.Error()
		}
		if err := store.recordEntry(publication.ID, outcome, lastError); err != nil {
			return err
		}
		if askedSite {
			sleep(ctx, b.RequestInterval)
		}
	}
	return nil
}

// saveArchiveEntry saves one archive entry of the publication, with addedBy
// naming what saved it, and reports what happened and whether it asked the
// site for anything. The backfill and the watch both save posts through here.
func (s *Store) saveArchiveEntry(ctx context.Context, publication Publication, entry archiveEntry, addedBy string) (backfillOutcome, bool, error) {
	if !entry.isArticle {
		return outcomeSkipped, false, nil
	}
	normalized, err := NormalizeURL(entry.postURL)
	if err != nil {
		return outcomeFailed, false, err
	}
	_, found, err := s.findSavedPost(normalized)
	if found || errors.Is(err, ErrDeleted) {
		// A deleted article was removed by someone; the backfill leaves it so.
		return outcomeAlready, false, nil
	}
	if err != nil {
		return outcomeFailed, false, err
	}
	extraction, err := entry.extract(ctx, s.httpClient, normalized)
	if err != nil {
		return outcomeFailed, entry.extractAsksSite, err
	}
	extraction.SiteName = publication.Name
	tags := entry.tags
	if tags == nil {
		tags = []string{}
	}
	_, _, err = s.insertArticle(ctx, normalized, extraction, savedFields{
		Kind: "post", Tags: tags, AddedBy: addedBy, PublicationID: publication.ID,
	})
	if err != nil {
		return outcomeFailed, entry.extractAsksSite, err
	}
	return outcomeSaved, entry.extractAsksSite, nil
}
