package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
	if strings.Contains(out, "Pass") {
		t.Errorf("a server with no Pass warned about one:\n%s", out)
	}
}

// routeReach is a route Routes registers, and whether its handler can run the
// function routesReaching was asked about.
type routeReach struct {
	pattern string
	reaches bool
}

// routesReaching parses this package's sources and returns every route Routes
// registers, in order, each with whether its handler can run fn: whether the
// handler's expression, or a function it names, names fn, followed by name
// through every function in the package. A name is followed whatever it is the
// name of (a method of another type, say), so the answer errs towards reaching.
func routesReaching(t *testing.T, fn string) []routeReach {
	t.Helper()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	namesIn := func(n ast.Node) []string {
		var names []string
		ast.Inspect(n, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok {
				names = append(names, ident.Name)
			}
			return true
		})
		return names
	}
	fset := token.NewFileSet()
	bodies := map[string][]string{}
	var routes *ast.FuncDecl
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			f, ok := decl.(*ast.FuncDecl)
			if !ok || f.Body == nil {
				continue
			}
			bodies[f.Name.Name] = append(bodies[f.Name.Name], namesIn(f.Body)...)
			if f.Name.Name == "Routes" {
				routes = f
			}
		}
	}
	if routes == nil || bodies[fn] == nil {
		t.Fatalf("no Routes or no %s in the package's sources", fn)
	}
	reaching := map[string]bool{fn: true}
	for grew := true; grew; {
		grew = false
		for name, names := range bodies {
			if !reaching[name] && slices.ContainsFunc(names, func(n string) bool { return reaching[n] }) {
				reaching[name], grew = true, true
			}
		}
	}
	var found []routeReach
	ast.Inspect(routes.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Fatalf("a route in Routes has a pattern that is not a string literal: %s", fset.Position(call.Pos()))
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("pattern %s: %v", lit.Value, err)
		}
		reaches := slices.ContainsFunc(namesIn(call.Args[1]), func(n string) bool { return reaching[n] })
		found = append(found, routeReach{pattern, reaches})
		return false
	})
	return found
}

// TestPostingLinesAreUntimed holds the request line of every route whose
// handler can run postPass, which reads the Pass a post presents, to no
// duration, and every other route's line to one. The routes are found in the
// code, so a route added later that takes a post fails here until Routes marks
// it untimed. Each is sent a request, a post where the route takes one, and its
// line read back.
func TestPostingLinesAreUntimed(t *testing.T) {
	routes := routesReaching(t, "postPass")
	var logs syncBuffer
	p := passServerLogging(t, nil, &logs)
	p.addBoard("untimed")
	thread := p.createThread("untimed", "", "", "a thread")
	values := map[string]string{
		"slug": "untimed",
		"no":   strconv.FormatInt(thread.ThreadNo, 10),
		"id":   strconv.FormatInt(p.threadID("untimed", thread.ThreadNo), 10),
		"post": strconv.FormatInt(thread.No, 10),
		"path": "nothing-here.png",
	}
	wildcard := regexp.MustCompile(`\{(\w+)(\.\.\.)?\}`)

	var posting, timed int
	for _, route := range routes {
		method, path, ok := strings.Cut(route.pattern, " ")
		if !ok {
			method, path = http.MethodGet, route.pattern
		}
		path = wildcard.ReplaceAllStringFunc(path, func(w string) string {
			if v, ok := values[wildcard.FindStringSubmatch(w)[1]]; ok {
				return v
			}
			return "1"
		})
		req, err := http.NewRequest(method, p.base+path, strings.NewReader(`{"body":"timed or not"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		status := p.send(req, nil)

		line := requestLine(t, &logs, method, path)
		_, hasDuration := line["duration_ms"]
		switch {
		case route.reaches && hasDuration:
			t.Errorf("%s runs postPass and its line has a duration (status %d): %v", route.pattern, status, line)
		case !route.reaches && !hasDuration:
			t.Errorf("%s does not take a post and its line has no duration (status %d): %v", route.pattern, status, line)
		}
		if route.reaches {
			posting++
		} else {
			timed++
		}
	}
	if posting == 0 || timed == 0 {
		t.Fatalf("found %d routes that take a post and %d that do not, in %v", posting, timed, routes)
	}
}

// requestLine waits for the request line of method and path, which is
// written once the handler returns and can come just after its answer, and
// returns it decoded.
func requestLine(t *testing.T, logs *syncBuffer, method, path string) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		for _, raw := range strings.Split(logs.String(), "\n") {
			var line map[string]any
			if json.Unmarshal([]byte(raw), &line) == nil &&
				line["msg"] == "request" && line["method"] == method && line["path"] == path {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no request line for %s %s in:\n%s", method, path, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
