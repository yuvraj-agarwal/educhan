// Command server runs educhan: a JSON API for an anonymous imageboard plus the
// embedded web UI that consumes it, backed by PostgreSQL.
//
// With -sweep it serves nothing: it deletes the archived threads whose board's
// archive_days have run out, with their media, and the spent captcha answers
// past their day, and exits. That is the mode a scheduled Cloud Run Job runs,
// from the same image.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/app"
)

const (
	startupTimeout  = 30 * time.Second
	shutdownTimeout = 10 * time.Second
	// sweepCloseTimeout is how long a sweep that is done waits for the
	// database pool and the media store to close. The process exits either
	// way, so a sweep has nothing to lose by not waiting longer, and pgx gives
	// a connection that has stopped answering fifteen seconds to close, longer
	// than Cloud Run waits after SIGTERM before it kills the Job.
	sweepCloseTimeout = 2 * time.Second
)

func main() {
	// run has logged what went wrong (fatal).
	if err := run(os.Args[1:], os.Stderr); err != nil {
		os.Exit(1)
	}
}

// run is the whole process: it reads the flags and the environment, logs to
// logs, and either sweeps once or serves until a signal. The error that ends
// it is logged here, once (fatal), before the application is closed, and
// returned for main's exit status.
func run(args []string, logs io.Writer) error {
	// Made before the flags and the configuration are read, so a mistake in
	// either is written to logs too, in the format the environment names (or
	// as text, if the configuration then refuses it).
	log := newLogger(os.Getenv("EDUCHAN_LOG_FORMAT"), logs)
	slog.SetDefault(log)

	// The flag set writes nothing of its own: a flag it does not know is
	// written once, as the fatal line, not also as text with the usage after
	// it, and the usage goes to logs only when asked for, with -h.
	flags := flag.NewFlagSet("educhan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sweep := flags.Bool("sweep", false,
		"delete archived threads past their board's archive_days, with their media, and spent captcha answers past their day, then exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(logs)
			flags.Usage()
			return nil
		}
		return fatal(err)
	}
	// A stray argument is most likely a mistyped flag, and a Job that quietly
	// started a server instead of sweeping would never exit.
	if flags.NArg() > 0 {
		return fatal(fmt.Errorf("unexpected arguments %q", flags.Args()))
	}

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		return fatal(err)
	}
	// The same format, now as the configuration has read it.
	log = newLogger(cfg.LogFormat, logs)
	slog.SetDefault(log)

	// A sweep listens for SIGTERM from before it connects, so a stop while it
	// is still starting up (connecting, waiting for or running migrations, or
	// checking the media store's bucket, all of which end with startupCtx)
	// ends New with the stop as its error, which is written, and the process
	// exits 1 with nothing deleted, the spent answers included. Without this,
	// the signal would kill it there at once, without a word. The error is
	// written at once, unless the database stops answering while the sweep is
	// connected to it: New gives up the connection before it returns, which
	// pgx allows fifteen seconds, longer than Cloud Run waits. The server
	// listens for signals only once it serves (serve).
	base := context.Background()
	if *sweep {
		var stop context.CancelFunc
		base, stop = signal.NotifyContext(base, syscall.SIGINT, syscall.SIGTERM)
		defer stop()
	}
	startupCtx, cancelStartup := context.WithTimeout(base, startupTimeout)
	defer cancelStartup()

	application, err := app.New(startupCtx, cfg, log)
	if err != nil {
		return fatal(err)
	}

	if *sweep {
		return runSweep(base, application)
	}
	defer application.Close()
	return fatal(serve(cfg, application, log))
}

// fatal logs err, when there is one, before the application is closed, and
// returns it.
func fatal(err error) error {
	if err != nil {
		slog.Error("fatal", "error", err)
	}
	return err
}

// runSweep makes one pass over the archive, writes its error, and closes the
// application. ctx is run's: SIGTERM, which a Cloud Run Job receives when it
// overruns its task timeout, stops the pass at the next batch or the next
// file, or in looking up the next file. A batch commits or rolls back whole,
// and its files' keys are queued in the same transaction, so a file the run
// did not get to is removed by the next one. A stopped run still deletes the
// spent captcha answers, giving that five seconds at most, then writes its
// error and closes, waiting on the close for sweepCloseTimeout at most. So
// even with the database not answering, a stopped run writes its error and
// exits within those five seconds and the close's two, inside the ten Cloud
// Run gives it, as long as no file deletion is in hand when the media store or
// the database stops answering: the file in hand is finished, and its key
// forgotten, whatever the stop, and that waits on both. A stop that came while
// the sweep was still starting up never gets here: it ended New, and run wrote
// that, with nothing deleted.
func runSweep(ctx context.Context, application *app.App) error {
	_, err := application.Sweep(ctx)
	// Written before the close, so a process killed while it closes has
	// already said why it failed.
	fatal(err)
	closeWithin(application, sweepCloseTimeout)
	return err
}

// closeWithin closes application, waiting for that no longer than bound.
func closeWithin(application *app.App, bound time.Duration) {
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		application.Close()
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-closed:
	case <-timer.C:
	}
}

func serve(cfg app.Config, application *app.App, log *slog.Logger) error {
	// Only a server has a use for these; the sweeper runs without them.
	application.WarnServingSettings()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           application.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}

func newLogger(format string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
