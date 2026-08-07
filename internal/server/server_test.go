package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/config"
)

func TestRoutes(t *testing.T) {
	s := New(config.Load())

	tests := []struct {
		name       string
		method     string
		target     string
		wantStatus int
	}{
		{"home", http.MethodGet, "/", http.StatusOK},
		{"health", http.MethodGet, "/health", http.StatusOK},
		{"wrong method on health", http.MethodPost, "/health", http.StatusMethodNotAllowed},
		// "GET /" is a prefix pattern, so unrouted paths fall through to Home.
		{"unrouted path falls through to home", http.MethodGet, "/boards", http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestStartReportsUnavailableAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()

	cfg := config.Load()
	cfg.Addr = listener.Addr().String()

	if err := New(cfg).start(context.Background()); err == nil {
		t.Error("start() on an address already in use returned no error")
	}
}

func TestStartDrainsOnContextCancel(t *testing.T) {
	cfg := config.Load()
	cfg.Addr = "127.0.0.1:0" // let the kernel choose a free port

	s := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.start(ctx) }()

	// Give start a moment to bind before asking it to stop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("start() returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start() did not return after context cancellation")
	}
}
