package app

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// contentSecurityPolicy keeps the front end to same-origin assets. The UI ships
// no inline scripts or styles, so no exceptions are needed.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// middleware is a decorator over an http.Handler.
type middleware func(http.Handler) http.Handler

// chain applies middleware so that the first entry is the outermost handler.
func chain(h http.Handler, mw ...middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// statusRecorder captures the response status for access logs.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	return r.ResponseWriter.Write(b)
}

// requestLogger emits one structured line per request.
//
// The line deliberately carries no client address. In production these lines
// land in Cloud Logging next to the method, path, and time, and an address
// beside "POST /api/verify/request" at the moment a code was issued is exactly
// the join verification is built to avoid. Flood control still keys on the
// address in memory; it is only kept out of the log.
func requestLogger(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			if rec.status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			log.Log(r.Context(), level, "request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// recoverPanics turns a handler panic into a 500 instead of a dropped
// connection, and logs the offending request.
func recoverPanics(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v)
					writeError(w, http.StatusInternalServerError, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// securityHeaders sets the defensive headers appropriate for a service that
// serves both an HTML front end and a JSON API.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the address flood control is keyed on, and that a poster ID
// is derived from when the request carries no poster key.
//
// X-Forwarded-For is read only when the deployment declares how many reverse
// proxies stand in front of the server (EDUCHAN_TRUST_PROXY). Each of those
// proxies appends the address it saw, so with N trusted hops the real peer is
// the Nth entry counted from the right; every entry to its left was supplied by
// the client and is ignored. Taking the leftmost entry instead — the usual
// mistake — would let any client pick its own rate-limit bucket and poster ID
// by sending a header of its own.
//
// Anything that does not match the declared shape falls back to the peer
// address: a chain shorter than the configured depth, or an entry that is not
// an IP address. Both mean the request did not arrive the way the operator
// described, and the peer is the only thing left that a client cannot forge.
func clientIP(r *http.Request, trustedHops int) string {
	peer := peerIP(r)
	if trustedHops <= 0 {
		return peer
	}
	forwarded := forwardedFor(r)
	if len(forwarded) < trustedHops {
		return peer
	}
	if ip, ok := parseClientIP(forwarded[len(forwarded)-trustedHops]); ok {
		return ip
	}
	return peer
}

// forwardedFor flattens every X-Forwarded-For header into one list of entries,
// in the order the proxies appended them. The header may legally be repeated,
// and a chain that splits across lines must read the same as a single line.
func forwardedFor(r *http.Request) []string {
	var entries []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for _, entry := range strings.Split(header, ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

// parseClientIP normalises one forwarded entry. Entries may carry a port or
// brackets, and IPv4-mapped IPv6 is folded down so one client cannot occupy two
// rate-limit buckets by switching notation.
func parseClientIP(entry string) (string, bool) {
	if host, _, err := net.SplitHostPort(entry); err == nil {
		entry = host
	}
	addr, err := netip.ParseAddr(strings.Trim(entry, "[]"))
	if err != nil {
		return "", false
	}
	return addr.Unmap().String(), true
}

// peerIP is the address of whoever opened the connection.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
