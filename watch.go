package articlestore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// WatchMaximumArchivePages caps how many archive pages one watch run reads.
// A watch looks for posts newer than any already saved; the archive import
// (POST /publications/{id}/backfill) is how to get a whole archive.
const WatchMaximumArchivePages = 2

// Watcher runs due watch sources: it reads each watched publication's archive
// from the newest end and saves the posts not yet saved. It asks no model, so
// it runs in the store like the Backfiller, off each source's next_run_at.
type Watcher struct {
	Store           *Store
	RequestInterval time.Duration
	// IdleInterval is how long to wait before looking again when no watch is
	// due.
	IdleInterval time.Duration
}

// Run works until ctx ends. An error it cannot pin on one source is logged
// and retried after IdleInterval.
func (w *Watcher) Run(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := w.Step(ctx)
		if err != nil {
			log.Printf("article-store: watch: %v", err)
		}
		if !worked || err != nil {
			sleep(ctx, w.IdleInterval)
		}
	}
}

// nextWatchDue returns the due watch that has waited longest among those whose
// publication has no archive backfill running and no 429 wait pending.
func (s *Store) nextWatchDue() (RadarSource, bool, error) {
	timestamp := now()
	source, err := scanRadarSource(s.db.QueryRow(`SELECT `+radarSourceColumns+` FROM sources WHERE id = (
		SELECT s.id FROM sources s JOIN publications p ON p.id = s.publication_id
		WHERE s.kind = 'watch' AND s.status = 'active' AND s.enabled = 1 AND s.next_run_at <= ?
			AND p.backfill_status <> 'running' AND p.backfill_next_attempt_at <= ?
		ORDER BY s.next_run_at, s.seq LIMIT 1)`, timestamp, timestamp))
	if errors.Is(err, ErrNotFound) {
		return RadarSource{}, false, nil
	}
	return source, err == nil, err
}

// setPublicationWait keeps both the watch and the backfill from asking the
// site anything before until. It leaves the backfill's own fields alone.
func (s *Store) setPublicationWait(id string, until time.Time) error {
	_, err := s.db.Exec(`UPDATE publications SET backfill_next_attempt_at = ?, updated_at = ? WHERE id = ?`, until.Unix(), now(), id)
	return err
}

// Step runs the watch most due, and reports whether there was one.
func (w *Watcher) Step(ctx context.Context) (bool, error) {
	source, found, err := w.Store.nextWatchDue()
	if err != nil || !found {
		return false, err
	}
	return true, w.runWatch(ctx, source)
}

// watchTally counts what one watch run did.
type watchTally struct {
	saved, already, skipped, failed, pages int
	lastError                              string
	// stoppedAtCap is set when the run stopped at WatchMaximumArchivePages
	// with every post so far new.
	stoppedAtCap bool
}

func (tally watchTally) String() string {
	result := fmt.Sprintf("%d new saved, %d already saved, %d skipped, %d failed, from %d archive page(s)",
		tally.saved, tally.already, tally.skipped, tally.failed, tally.pages)
	if tally.stoppedAtCap {
		result += "; stopped at the page limit with every post new, so older new posts may remain — the archive import gets them"
	}
	if tally.lastError != "" {
		result += "; last error: " + tally.lastError
	}
	return result
}

// runWatch reads the publication's archive newest first. It reads the next
// page only when every post on this one was new, since posts come newest
// first and the first one already saved means the rest are older; and it
// reads at most WatchMaximumArchivePages. A 429, a backfill starting, or the
// service stopping ends the run without marking it ran, so it runs again.
func (w *Watcher) runWatch(ctx context.Context, source RadarSource) error {
	store := w.Store
	publication, err := store.GetPublication(source.PublicationID)
	if err != nil {
		return err
	}
	archive, known := publicationArchives[publication.Platform]
	if !known {
		_, err := store.MarkRadarSourceRan(source.ID, fmt.Sprintf("failed: platform %q has no archive reader in this build", publication.Platform))
		return err
	}
	addedBy := "watch " + source.ID
	tally := watchTally{}
	waitOut := func(rateLimited *RateLimitedError) error {
		if err := store.setPublicationWait(publication.ID, time.Now().Add(rateLimited.RetryAfter)); err != nil {
			return err
		}
		return store.setRadarSourceLastResult(source.ID, "waiting after a 429, will run again: "+rateLimited.Error()+"; so far "+tally.String())
	}
	offset := 0
	for {
		page, err := archive.readPage(ctx, store.httpClient, publication, offset)
		if rateLimited, is := isRateLimited(err); is {
			return waitOut(rateLimited)
		}
		if err != nil {
			tally.lastError = err.Error()
			_, err := store.MarkRadarSourceRan(source.ID, "failed reading the archive: "+tally.String())
			return err
		}
		tally.pages++
		sleep(ctx, w.RequestInterval)
		if len(page.entries) == 0 {
			break
		}
		savedOnPage, alreadyOnPage := 0, 0
		for _, entry := range page.entries {
			if ctx.Err() != nil {
				return nil
			}
			current, err := store.GetPublication(publication.ID)
			if err != nil {
				return err
			}
			if current.Backfill.Status == BackfillStatusRunning {
				return store.setRadarSourceLastResult(source.ID, "paused while the archive import runs; so far "+tally.String())
			}
			outcome, askedSite, err := store.saveArchiveEntry(ctx, publication, entry, addedBy)
			if rateLimited, is := isRateLimited(err); is {
				return waitOut(rateLimited)
			}
			if err != nil {
				tally.lastError = entry.postURL + ": " + err.Error()
			}
			switch outcome {
			case outcomeSaved:
				tally.saved++
				savedOnPage++
			case outcomeAlready:
				tally.already++
				alreadyOnPage++
			case outcomeSkipped:
				tally.skipped++
			case outcomeFailed:
				tally.failed++
			}
			if askedSite {
				sleep(ctx, w.RequestInterval)
			}
		}
		if alreadyOnPage > 0 || savedOnPage == 0 {
			break
		}
		if tally.pages >= WatchMaximumArchivePages {
			tally.stoppedAtCap = true
			break
		}
		offset += len(page.entries)
	}
	_, err = store.MarkRadarSourceRan(source.ID, tally.String())
	return err
}
