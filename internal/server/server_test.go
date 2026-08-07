package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/config"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Port = 0 // let the kernel choose a free port
	cfg.ShutdownTimeout = time.Second

	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRoutes(t *testing.T) {
	handler := newTestServer(t).Handler()

	tests := []struct {
		name       string
		method     string
		target     string
		wantStatus int
		wantType   string
	}{
		{"index", http.MethodGet, "/", http.StatusOK, "text/html; charset=utf-8"},
		{"liveness", http.MethodGet, "/healthz", http.StatusOK, "application/json; charset=utf-8"},
		{"readiness", http.MethodGet, "/readyz", http.StatusOK, "application/json; charset=utf-8"},
		{"unknown path", http.MethodGet, "/does-not-exist", http.StatusNotFound, ""},
		{"wrong method", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantType != "" {
				if got := rec.Header().Get("Content-Type"); got != tc.wantType {
					t.Errorf("Content-Type = %q, want %q", got, tc.wantType)
				}
			}
			if rec.Header().Get(requestIDHeader) == "" {
				t.Error("response is missing a request ID header")
			}
		})
	}
}

func TestLivenessReportsVersion(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
	if body["version"] != Version {
		t.Errorf("version = %q, want %q", body["version"], Version)
	}
}

func TestRequestIDIsReusedFromInboundHeader(t *testing.T) {
	t.Run("valid header is preserved", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set(requestIDHeader, "trace-abc123")

		rec := httptest.NewRecorder()
		newTestServer(t).Handler().ServeHTTP(rec, req)

		if got := rec.Header().Get(requestIDHeader); got != "trace-abc123" {
			t.Errorf("request ID = %q, want %q", got, "trace-abc123")
		}
	})

	t.Run("malformed header is replaced", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set(requestIDHeader, "not a valid id")

		rec := httptest.NewRecorder()
		newTestServer(t).Handler().ServeHTTP(rec, req)

		got := rec.Header().Get(requestIDHeader)
		if got == "" || got == "not a valid id" {
			t.Errorf("request ID = %q, want a generated value", got)
		}
	})
}

func TestPanicIsRecovered(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		requestID,
		recoverPanic(log),
		logRequests(log),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(logs.String(), "panic recovered") {
		t.Errorf("panic was not logged; got %q", logs.String())
	}
}

// The request ID is only present in log output if the middleware that assigns
// it wraps the middleware that logs, which is easy to break by reordering.
func TestRequestIDIsLogged(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	var logs bytes.Buffer
	s := New(cfg, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(requestIDHeader, "trace-abc123")
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("decode log entry %q: %v", logs.String(), err)
	}
	if entry["request_id"] != "trace-abc123" {
		t.Errorf("logged request_id = %v, want %q", entry["request_id"], "trace-abc123")
	}
}

func TestRunShutsDownOnContextCancel(t *testing.T) {
	s := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give Run a moment to bind before asking it to stop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
