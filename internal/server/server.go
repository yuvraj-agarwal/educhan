// Package server wires the educhan HTTP routes and owns the server lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/config"
	"github.com/yuvraj-agarwal/educhan/internal/handler"
)

// shutdownTimeout bounds how long in-flight requests may take to finish once a
// termination signal has been received. Keep it below the deployment
// platform's termination grace period.
const shutdownTimeout = 15 * time.Second

// Server holds the routed handler and the configuration it serves under.
type Server struct {
	cfg    config.Config
	router *http.ServeMux
}

// New builds a Server with all routes registered. It does not bind a port;
// call Start for that.
func New(cfg config.Config) *Server {
	s := &Server{
		cfg:    cfg,
		router: http.NewServeMux(),
	}

	s.routes()
	return s
}

// routes registers every route the service serves. The method prefix is a Go
// 1.22 ServeMux pattern, so a wrong method yields 405 without a third-party
// router.
func (s *Server) routes() {
	s.router.HandleFunc("GET /health", handler.Health)
	s.router.HandleFunc("GET /", handler.Home)
}

// Start serves until the process is interrupted or receives SIGTERM, then
// drains in-flight requests before returning.
func (s *Server) Start() error {
	// SIGTERM is what container runtimes send before killing a container.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return s.start(ctx)
}

// start is Start without the signal handling, so shutdown can be exercised in
// tests by cancelling ctx.
func (s *Server) start(ctx context.Context) error {
	srv := &http.Server{
		Addr:         s.cfg.Addr,
		Handler:      s.router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Bind before serving so that an address already in use is reported to the
	// caller instead of surfacing later as a process that never accepts traffic.
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", srv.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

	log.Printf("educhan shutting down, draining for up to %s", shutdownTimeout)

	// Deliberately detached from ctx, which is already cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Requests still running past the deadline are cut off rather than
		// leaked; Close makes that explicit.
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	return nil
}
