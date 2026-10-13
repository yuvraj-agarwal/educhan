package app

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// syncBuffer is a bytes.Buffer safe to share between the test and the
// server's handler goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRequestLogCarriesNoClientAddress pins the access log's shape: whatever
// address a request arrives from, directly or through a proxy, none of it is
// written down.
func TestRequestLogCarriesNoClientAddress(t *testing.T) {
	var logs syncBuffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("boom")
		}
		w.WriteHeader(http.StatusNoContent)
	}), requestLogger(log), recoverPanics(log))

	for _, tc := range []struct {
		path, remote, forwarded string
	}{
		{"/api/boards", "203.0.113.7:52000", ""},
		{"/api/verify/request", "[2001:db8::7]:52000", "198.51.100.23"},
		{"/boom", "203.0.113.7:52000", "198.51.100.23, 192.0.2.44"},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		req.RemoteAddr = tc.remote
		if tc.forwarded != "" {
			req.Header.Set("X-Forwarded-For", tc.forwarded)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	out := logs.String()
	if !strings.Contains(out, `"path":"/api/verify/request"`) {
		t.Fatalf("expected a request line for every request, got:\n%s", out)
	}
	for _, ip := range []string{"203.0.113.7", "2001:db8::7", "198.51.100.23", "192.0.2.44"} {
		if strings.Contains(out, ip) {
			t.Errorf("log contains client address %s:\n%s", ip, out)
		}
	}
}

// TestServerLogsCarryNoClientAddress drives the whole application, the way a
// deployment behind a proxy sees it, and checks every line it logs.
func TestServerLogsCarryNoClientAddress(t *testing.T) {
	var logs syncBuffer
	mail := &captureSender{}
	h := testServerLogging(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.VerifySecret = "test-verification-secret"
		cfg.VerifyCodeTTL = defaultVerifyCodeTTL
		cfg.VerifyCredentialTTL = defaultVerifyCredentialTTL
		cfg.Sender = mail
	}, &logs)
	h.addBoard("general")

	const forwarded = "198.51.100.23"
	send := func(method, path, body string) int {
		t.Helper()
		req, err := http.NewRequest(method, h.base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", forwarded)
		return h.send(req, nil)
	}

	var thread struct {
		No int64 `json:"no"`
	}
	h.post("/api/boards/general/threads", map[string]any{"subject": "log", "body": "hello"}, &thread)
	send(http.MethodPost, fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.No), `{"body":"reply"}`)
	send(http.MethodPost, "/api/verify/request", `{"email":"ana@iitb.ac.in","adult":true}`)
	send(http.MethodGet, "/api/boards/nope", "")
	if mail.count() != 1 {
		t.Fatalf("expected one verification code to be sent, got %d", mail.count())
	}

	out := logs.String()
	if !strings.Contains(out, `"msg":"request"`) {
		t.Fatalf("expected request lines in the log, got:\n%s", out)
	}
	for _, ip := range []string{forwarded, "127.0.0.1", "::1"} {
		if strings.Contains(out, ip) {
			t.Errorf("log contains client address %s:\n%s", ip, out)
		}
	}
}

// New builds an App for the sweeper as well as for the web server, and the
// sweeper runs without a salt, a moderation key or verification on purpose. So
// New warns about none of them; the serving path asks WarnServingSettings,
// which does.
func TestServingWarningsAreTheServersAlone(t *testing.T) {
	var logs syncBuffer
	h := testServerLogging(t, func(cfg *Config) {
		cfg.PosterIDSaltEphemeral = true
		cfg.ModKey = ""
		cfg.VerifySecret = ""
	}, &logs)

	if out := logs.String(); strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("New logged a warning:\n%s", out)
	}

	h.app.WarnServingSettings()
	out := logs.String()
	for _, setting := range []string{"EDUCHAN_POSTER_ID_SALT", "EDUCHAN_MOD_KEY", "EDUCHAN_VERIFY_SECRET"} {
		if !strings.Contains(out, setting+" is unset") {
			t.Errorf("WarnServingSettings said nothing about %s:\n%s", setting, out)
		}
	}
	if got := strings.Count(out, `"level":"WARN"`); got != 3 {
		t.Errorf("%d warnings, want 3:\n%s", got, out)
	}
}
