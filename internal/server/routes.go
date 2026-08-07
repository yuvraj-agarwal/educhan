package server

import "net/http"

// handler returns the router wrapped in the middleware every request passes
// through.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	// "/{$}" matches only the exact root path, unlike "/" which would match
	// every unrouted request.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)

	// Order matters: requestID runs first so that both the recovery and
	// logging layers below it can attribute their output to a request, and
	// recoverPanic wraps logRequests so a panic still produces a log line.
	return chain(mux,
		requestID,
		recoverPanic(s.log),
		logRequests(s.log),
	)
}

// middleware decorates an http.Handler.
type middleware func(http.Handler) http.Handler

// chain applies middleware so that the first argument is the outermost layer.
func chain(h http.Handler, mw ...middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
