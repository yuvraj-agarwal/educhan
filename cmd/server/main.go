// Command server runs educhan: a JSON API for an anonymous imageboard plus the
// embedded web UI that consumes it, backed by PostgreSQL.
//
// With -sweep it serves nothing: it deletes the archived threads whose board's
// archive_days have run out, with their media, rewrites the verification
// credentials so that every one issued before the sweep carries the sweep's
// transaction id rather than its own (the README's "Transaction ids, and the
// nightly rewrite" says what that leaves), and exits. That is the mode a
// scheduled Cloud Run Job runs, from the same image.
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
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// run is the whole process: it reads the flags and the environment, logs to
// logs, and either sweeps once or serves until a signal.
func run(args []string, logs io.Writer) error {
	flags := flag.NewFlagSet("educhan", flag.ContinueOnError)
	sweep := flags.Bool("sweep", false,
		"delete archived threads past their board's archive_days, with their media, "+
			"rewrite the verification credentials, then exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// A stray argument is most likely a mistyped flag, and a Job that quietly
	// started a server instead of sweeping would never exit.
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogFormat, logs)
	slog.SetDefault(log)

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStartup()

	application, err := app.New(startupCtx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	if *sweep {
		return runSweep(application)
	}
	return serve(cfg, application, log)
}

// runSweep makes one pass over the archive. SIGTERM, which a Cloud Run Job
// receives when it overruns its task timeout, stops it at the next batch or
// the next file, or has PostgreSQL cancel the credential rewrite or the vacuum
// after it before the sweep returns (Store.RewriteCredentials), so that
// neither runs on, holding its table, after the process has exited. A batch
// commits or rolls back whole, and its files' keys are queued in the same
// transaction, so a file the run did not get to is removed by the next one; a
// cancelled rewrite or vacuum rolls back whole too, and the next run does it
// again. (A vacuum that had finished when the cancel reached it has committed,
// and that table is done.) The deferred Close in run waits for the pool's
// connections to send their own cancel requests.
func runSweep(application *app.App) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	_, err := application.Sweep(ctx)
	return err
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
