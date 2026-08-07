package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// requestIDHeader is both read from the incoming request (so a request can be
// traced across services behind a proxy) and echoed on the response.
const requestIDHeader = "X-Request-Id"

type contextKey struct{ name string }

var requestIDKey = contextKey{"request-id"}

// RequestIDFrom returns the request ID attached to ctx, or "" if there is none.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requestID ensures every request carries an ID, reusing the inbound header
// when present so traces stay stitched together across hops.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func newRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand cannot realistically fail; a time-based ID keeps the
		// request traceable rather than failing it outright.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}

// sanitizeRequestID drops client-supplied values that are unreasonably long or
// contain characters that would let a caller forge log or header structure.
func sanitizeRequestID(id string) string {
	if len(id) == 0 || len(id) > 64 {
		return ""
	}
	for _, r := range id {
		if r > unicode.MaxASCII || !(r == '-' || r == '_' || r == '.' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return ""
		}
	}
	return id
}

// logRequests emits one structured line per request. Health probes are logged
// at debug level so they do not drown out real traffic.
func logRequests(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			log.Log(r.Context(), levelFor(r, rec.status), "request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.written,
				"duration_ms", float64(time.Since(start).Microseconds())/1000,
				"remote_addr", clientIP(r),
				"user_agent", r.UserAgent(),
				"request_id", RequestIDFrom(r.Context()),
			)
		})
	}
}

func levelFor(r *http.Request, status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	case isProbe(r.URL.Path):
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

// clientIP prefers the left-most address in X-Forwarded-For, which is what the
// managed load balancers in front of this service set.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, found := strings.Cut(fwd, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	return r.RemoteAddr
}

// recoverPanic keeps a panic in one handler from taking down the process.
func recoverPanic(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// ErrAbortHandler is the documented way to abort a response;
				// the net/http server expects to handle it itself.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				log.ErrorContext(r.Context(), "panic recovered",
					"panic", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", RequestIDFrom(r.Context()),
					"stack", string(debug.Stack()),
				)

				// Signal to the client that the connection is unusable; the
				// response may already be partially written.
				w.Header().Set("Connection", "close")
				writeError(w, http.StatusInternalServerError)
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder captures the response status and size for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer for
// flushing, hijacking and deadline control.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
