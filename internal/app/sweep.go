package app

import (
	"context"
	"fmt"
	"time"
)

// The sweeper deletes archived threads once their board's archive_days have
// passed, media included. It runs as `educhan -sweep`, a one-shot mode Cloud
// Scheduler starts as a Cloud Run Job: Cloud Run gives a service CPU only
// while it answers a request, so an in-process timer would fire whenever it
// happened to, and a Job leaves no sweep endpoint to protect.

// sweepBatchSize bounds one sweep transaction: how many threads it deletes,
// how long it holds their row locks, and how many blob paths it carries out
// to remove afterwards.
const sweepBatchSize = 100

// SweepReport is what a sweep did, in counts and nothing else: which boards or
// threads went is not worth keeping in a log.
type SweepReport struct {
	Threads     int
	Posts       int
	Attachments int
	// BlobFailures counts attachments whose rows were deleted but whose files
	// could not be. Nothing will come back for them, so a sweep with any
	// reports failure, and the job's exit status says so.
	BlobFailures int
}

// Sweep deletes every archived thread that has outlived its board's
// archive_days, in batches of sweepBatchSize, removing each batch's media once
// its transaction has committed.
func (a *App) Sweep(ctx context.Context) (SweepReport, error) {
	return a.sweep(ctx, time.Now(), sweepBatchSize)
}

func (a *App) sweep(ctx context.Context, asOf time.Time, batch int) (SweepReport, error) {
	var report SweepReport
	for {
		swept, err := a.store.SweepArchived(ctx, asOf, batch)
		if err != nil {
			a.logSweep(report)
			return report, fmt.Errorf("sweep archived threads: %w", err)
		}
		report.Threads += swept.Threads
		report.Posts += swept.Posts
		report.Attachments += len(swept.Images)
		report.BlobFailures += a.removeBlobs(ctx, swept.Images)
		// A short batch means nothing expired is left, or what is left is
		// locked by somebody else and will keep until the next sweep.
		if swept.Threads < batch {
			break
		}
	}
	a.logSweep(report)
	if report.BlobFailures > 0 {
		return report, fmt.Errorf("sweep: %d of %d attachments could not be removed from the media store",
			report.BlobFailures, report.Attachments)
	}
	return report, nil
}

func (a *App) logSweep(report SweepReport) {
	a.log.Info("sweep",
		"threads", report.Threads,
		"posts", report.Posts,
		"attachments", report.Attachments,
		"blob_failures", report.BlobFailures)
}
