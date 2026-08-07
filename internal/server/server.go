// Package server wires the educhan HTTP API: routing, middleware and the
// server lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/yuvraj-agarwal/educhan/internal/config"
)

// Server owns the HTTP listener and the lifecycle around it.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	http    *http.Server
	stopped chan struct{}
}

// New builds a Server with all routes and middleware registered. It does not
// bind a port; call Run for that.
func New(cfg config.Config, log *slog.Logger) *Server {
	s := &Server{
		cfg:     cfg,
		log:     log,
		stopped: make(chan struct{}),
	}
	s.http = &http.Server{
		Addr:              cfg.Addr(),
		Handler:           s.handler(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// Handler exposes the fully decorated handler so it can be exercised in tests
// without binding a port.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run binds the configured address and serves until ctx is cancelled, at which
// point in-flight requests are drained within the configured shutdown timeout.
// Binding happens before Run blocks, so a port conflict is reported immediately.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.http.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		defer close(s.stopped)
		s.log.Info("server listening", "addr", listener.Addr().String())
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err, ok := <-serveErr:
		if ok {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	// Durations are logged as strings; slog would otherwise render them as
	// raw nanosecond counts.
	s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout.String())

	// Context is deliberately detached from ctx, which is already cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// Requests still running past the deadline are cut off rather than
		// leaking; Close makes that explicit.
		_ = s.http.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	<-s.stopped
	s.log.Info("shutdown complete")
	return nil
}
