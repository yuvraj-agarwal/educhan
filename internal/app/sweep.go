package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// The sweeper deletes archived threads once their board's archive_days have
// passed, media included. It runs as `educhan -sweep`, a one-shot mode Cloud
// Scheduler starts as a Cloud Run Job: Cloud Run gives a service CPU only
// while it answers a request, so an in-process timer would fire whenever it
// happened to, and a Job leaves no sweep endpoint to protect.
//
// Rows go first, and the storage keys of their files are queued in
// media_deletions in the same transaction; the files go after, each key
// forgotten only once its file is. So a file that cannot be removed now is
// tried again by the next sweep rather than left public with nothing pointing
// at it, and a sweeper that is not looking at the server's media store — a Job
// with a different backend, bucket, prefix or upload directory — finds the
// files missing and fails, rather than deleting nothing and reporting success.
//
// Last, it deletes the codes that expired unconfirmed and rewrites the
// verification tables (Store.RewriteCredentials), so that when it ends every
// credential row carries the sweep's transaction id rather than its own, which
// would date its issuance against the posts written around it, the tables'
// files hold no other version of any row, and pg_statistic's files hold none
// of the samples ANALYZE took of the tables before 0015. A code still in
// flight as it ran keeps its row, with the time it was asked for, in the
// table's file until the next sweep, whether a confirmation or a delete has
// removed it by then or not; rows written after it — a confirmation, a code
// requested — keep their own transaction ids until the next sweep too; and a
// backup taken meanwhile holds both (the README's "Transaction ids, and the
// nightly rewrite").

// sweepBatchSize bounds one sweep transaction: how many threads it deletes and
// how long it holds their row locks.
const sweepBatchSize = 100

// SweepReport is what a sweep did, in counts and nothing else: which boards or
// threads went is not worth keeping in a log.
type SweepReport struct {
	Threads     int
	Posts       int
	Attachments int
	// The files the sweep went through: this run's, and any an earlier run
	// left queued. A file it could not remove (BlobFailures) or that the
	// media store does not hold (BlobsMissing) keeps its place in the queue,
	// and either makes the sweep report failure, so the job's exit status
	// says so.
	BlobsRemoved int
	BlobFailures int
	BlobsMissing int
	// The rows of the credentials table the sweep rewrote, which is all of
	// them (Store.RewriteCredentials), expired ones that no confirmation has
	// pruned yet included: it shows the rewrite ran, and over how much. Kept
	// night after night it is a history of the table's size, which the
	// database does not keep, and it says how many credentials there were
	// and nothing about any one of them; the log already counts issuances
	// one by one ("verification issued"). Nothing finer — no label, no
	// expiry — belongs here.
	Credentials int
}

// Sweep deletes every archived thread that has outlived its board's
// archive_days, in batches of sweepBatchSize, removes the files those threads
// had, along with any an earlier sweep could not, and then rewrites the
// verification tables.
func (a *App) Sweep(ctx context.Context) (SweepReport, error) {
	return a.sweep(ctx, time.Now(), sweepBatchSize)
}

func (a *App) sweep(ctx context.Context, asOf time.Time, batch int) (SweepReport, error) {
	var report SweepReport
	// A sweeper older than the schema decides what to delete without the
	// newer rules — a hold on a thread, say — and deletion cannot be undone.
	if len(a.schemaAhead) > 0 {
		return report, fmt.Errorf("sweep: the database has migrations this release does not know (%v); "+
			"run the sweeper from the same release as the server", a.schemaAhead)
	}
	for {
		swept, err := a.store.SweepArchived(ctx, asOf, batch)
		if err != nil {
			a.logSweep(report)
			return report, fmt.Errorf("sweep archived threads: %w", err)
		}
		report.Threads += swept.Threads
		report.Posts += swept.Posts
		report.Attachments += swept.Attachments
		// A short batch means nothing expired is left, or what is left is
		// locked by somebody else and will keep until the next sweep.
		if swept.Threads < batch {
			break
		}
	}

	firstFailure, err := a.removeQueuedMedia(ctx, &report)
	if err == nil {
		// Whatever became of the files: the rewrite has nothing to do with
		// them, and a storage outage should not keep credentials dated.
		report.Credentials, err = a.store.RewriteCredentials(ctx)
		if err != nil {
			err = fmt.Errorf("sweep verification tables: %w", err)
		}
	}
	a.logSweep(report)
	switch {
	case err != nil:
		return report, err
	case report.BlobsMissing > 0:
		return report, fmt.Errorf("sweep: %d of the files queued for removal are not in the media store; "+
			"check that the sweeper has the server's media settings (EDUCHAN_MEDIA_BACKEND, "+
			"EDUCHAN_GCS_BUCKET, EDUCHAN_GCS_PREFIX, EDUCHAN_UPLOAD_DIR); they stay queued in media_deletions",
			report.BlobsMissing)
	case report.BlobFailures > 0:
		return report, fmt.Errorf("sweep: %d files could not be removed from the media store (the first: %s); "+
			"they stay queued in media_deletions for the next sweep", report.BlobFailures, firstFailure)
	}
	return report, nil
}

// removeQueuedMedia works through media_deletions once, removing each file and
// forgetting its key, and counting into report what it could not do. It
// returns the first removal error, with the key taken out of it, for the
// sweep's own error to carry.
//
// A cancelled sweep stops between files. The file in hand is finished, and its
// key forgotten, whatever the context says: stopping half way would leave a
// key whose file is gone, which every later sweep would report as missing.
func (a *App) removeQueuedMedia(ctx context.Context, report *SweepReport) (string, error) {
	work := context.WithoutCancel(ctx)
	var firstFailure string
	var after int64
	for ctx.Err() == nil {
		id, found, err := a.store.NextMediaDeletion(work, after, func(key string) bool {
			switch err := removeExisting(work, a.media, key); {
			case err == nil:
				report.BlobsRemoved++
				return true
			case errors.Is(err, media.ErrNotFound):
				report.BlobsMissing++
			default:
				report.BlobFailures++
				if firstFailure == "" {
					firstFailure = withoutKey(err, key)
				}
			}
			return false
		})
		if err != nil {
			return firstFailure, fmt.Errorf("sweep media: %w", err)
		}
		if !found {
			return firstFailure, nil
		}
		after = id
	}
	return firstFailure, fmt.Errorf("sweep media: %w", ctx.Err())
}

// removeExisting deletes key, and reports media.ErrNotFound when the store did
// not hold it. Store.Delete counts a missing key as deleted, which is right
// for a moderator removing a post twice and wrong here: every key in the queue
// was a file the server stored, so a store without it is not the server's.
func removeExisting(ctx context.Context, store media.Store, key string) error {
	r, _, err := store.Get(ctx, key)
	if err != nil {
		return err
	}
	r.Close()
	return store.Delete(ctx, key)
}

// withoutKey is err's text with the storage key taken out, in each form a
// backend's error might carry it — a file path ends with it, and a Cloud
// Storage URL escapes it — because a key names its board, and the sweep logs
// counts and nothing about which boards or threads went.
func withoutKey(err error, key string) string {
	text := err.Error()
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		text = strings.ReplaceAll(text, pathErr.Path, "<file>")
	}
	for _, form := range []string{url.PathEscape(key), url.QueryEscape(key), key} {
		text = strings.ReplaceAll(text, form, "<file>")
	}
	return text
}

func (a *App) logSweep(report SweepReport) {
	a.log.Info("sweep",
		"threads", report.Threads,
		"posts", report.Posts,
		"attachments", report.Attachments,
		"blobs_removed", report.BlobsRemoved,
		"blob_failures", report.BlobFailures,
		"blobs_missing", report.BlobsMissing,
		"credentials_rewritten", report.Credentials)
}
