package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
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

// routeReach is a route Routes registers, whether its handler can run the
// function routesReaching was asked about, and whether the handler's expression
// names untimed.
type routeReach struct {
	pattern string
	reaches bool
	untimed bool
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
		names := namesIn(call.Args[1])
		reaches := slices.ContainsFunc(names, func(n string) bool { return reaching[n] })
		found = append(found, routeReach{pattern, reaches, slices.Contains(names, "untimed")})
		return false
	})
	return found
}

// TestPostingLinesAreUntimed holds the request line of every route whose
// handler can run postPass, which reads the Pass a post presents, to no
// duration, and every other route's line to one. The routes are found in the
// code, so a route added later that takes a post fails here until Routes marks
// it untimed. Each is sent a request, a post that must be saved where the
// route takes one, and its own line read back, the one logged after it was
// sent, which must carry the status it was answered with: postPass takes the
// duration off too, so a route whose handler is not seen to run it and whose
// post leaves no duration fails here as well. A posting handler served without
// untimed leaves none once it reads the post's Pass.
//
// Each route that takes a post is then sent three posts that fail after their
// Pass is read, a trigger refusing their insert: one with a good Pass, one
// with a Pass that is no good and one with none. Each is answered 500 and
// leaves no duration, and the three leave the same log, byte for byte but for
// its time. Last, each such route is sent posts it turns away before it reads
// their Pass, so that untimed's own mark alone takes the duration off their
// lines: a malformed poster key, no text and a body that is not JSON on every
// one; a board that is not there on each whose path names a board; a thread
// number that is not one on each that names a thread by number; and a thread
// id that is not one, and one that names no thread, on each that names a
// thread by id. Each carries a code that parses while the passes table cannot
// be read, so a post that got as far as postPass would be answered 500, as a
// post that does is on each route.
func TestPostingLinesAreUntimed(t *testing.T) {
	routes := routesReaching(t, "postPass")
	var logs syncBuffer
	p := passServerLogging(t, nil, &logs)
	p.addBoard("untimed")
	thread := p.createThread("untimed", "", "", "a thread")
	// Its line is in before any request below notes where the log stands.
	requestLine(t, &logs, 0, http.MethodPost, "/api/boards/untimed/threads")
	values := map[string]string{
		"slug": "untimed",
		"no":   strconv.FormatInt(thread.ThreadNo, 10),
		"id":   strconv.FormatInt(p.threadID("untimed", thread.ThreadNo), 10),
		"post": strconv.FormatInt(thread.No, 10),
		"path": "nothing-here.png",
	}
	wildcard := regexp.MustCompile(`\{(\w+)(\.\.\.)?\}`)
	// fill gives the method and the path a route's pattern stands for, each
	// wildcard filled from with, else from values, else with 1.
	fill := func(pattern string, with map[string]string) (string, string) {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			method, path = http.MethodGet, pattern
		}
		return method, wildcard.ReplaceAllStringFunc(path, func(w string) string {
			name := wildcard.FindStringSubmatch(w)[1]
			if v, ok := with[name]; ok {
				return v
			}
			if v, ok := values[name]; ok {
				return v
			}
			return "1"
		})
	}
	// A redirect is not followed, so the status a request is answered with is
	// the one its own line holds.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	stamp := regexp.MustCompile(`"time":"[^"]*"`)
	// send sends a request and returns its status, its own request line and
	// everything logged from its sending up to that line, times blanked.
	send := func(method, path string, header http.Header, body string) (int, map[string]any, string) {
		t.Helper()
		req, err := http.NewRequest(method, p.base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if header != nil {
			req.Header = header.Clone()
		}
		req.Header.Set("Content-Type", "application/json")
		from := len(logs.String())
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		line := requestLine(t, &logs, from, method, path)
		if line["status"] != float64(res.StatusCode) {
			t.Errorf("%s %s was answered %d, and its line is %v", method, path, res.StatusCode, line)
		}
		return res.StatusCode, line, stamp.ReplaceAllString(logs.String()[from:], `"time":""`)
	}

	var posting []string
	var timed int
	for _, route := range routes {
		method, path := fill(route.pattern, nil)
		status, line, _ := send(method, path, nil, `{"body":"timed or not"}`)
		_, hasDuration := line["duration_ms"]
		switch {
		case route.reaches && !route.untimed:
			t.Errorf("%s runs postPass and Routes does not mark it untimed", route.pattern)
		case route.reaches && hasDuration:
			t.Errorf("%s runs postPass and its line has a duration (status %d): %v", route.pattern, status, line)
		case route.reaches && status != http.StatusCreated:
			t.Errorf("%s runs postPass and its post was not saved (status %d): %v", route.pattern, status, line)
		case !route.reaches && !hasDuration:
			t.Errorf("%s is not seen to run postPass and its line has no duration (status %d): %v", route.pattern, status, line)
		}
		if route.reaches {
			posting = append(posting, route.pattern)
		} else {
			timed++
		}
	}
	if len(posting) == 0 || timed == 0 {
		t.Fatalf("found %d routes that take a post and %d that do not, in %v", len(posting), timed, routes)
	}

	bare := http.NewServeMux()
	bare.HandleFunc("POST /bare/{slug}/threads", p.app.handleCreateThread)
	req := httptest.NewRequest(http.MethodPost, "/bare/untimed/threads", strings.NewReader(`{"body":"bare"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	from := len(logs.String())
	requestLogger(p.app.log)(bare).ServeHTTP(rec, req)
	if line := requestLine(t, &logs, from, http.MethodPost, "/bare/untimed/threads"); rec.Code != http.StatusCreated || line["duration_ms"] != nil {
		t.Errorf("a post served without untimed: status %d, line %v", rec.Code, line)
	}

	// Posts that fail after their Pass is read, at their insert, which a
	// trigger refuses.
	from = len(logs.String())
	good := p.buy("pi_untimed")
	// The purchase's last line is in before the posts below note where the log
	// stands.
	requestLine(t, &logs, from, http.MethodPost, "/api/pass/claim")
	p.exec(`CREATE FUNCTION refuse_post() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'no post is saved'; END$$`)
	p.exec(`CREATE TRIGGER refuse_post BEFORE INSERT ON posts FOR EACH ROW EXECUTE FUNCTION refuse_post()`)
	presented := []struct{ what, code string }{
		{"a good Pass", good.Pass},
		{"a Pass that is no good", "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ"},
		{"no Pass", ""},
	}
	for _, pattern := range posting {
		method, path := fill(pattern, nil)
		var logged []string
		for _, sent := range presented {
			header := http.Header{}
			if sent.code != "" {
				header.Set(passHeader, sent.code)
			}
			status, line, all := send(method, path, header, `{"body":"never saved"}`)
			if _, timed := line["duration_ms"]; status != http.StatusInternalServerError || timed || !strings.Contains(all, "no post is saved") {
				t.Errorf("%s with %s, its insert refused: status %d, logged:\n%s", pattern, sent.what, status, all)
			}
			logged = append(logged, all)
		}
		if slices.ContainsFunc(logged, func(l string) bool { return l != logged[0] }) {
			t.Errorf("%s, its insert refused, logged differently with %d kinds of Pass and none:\n%s", pattern, len(presented)-1, strings.Join(logged, ""))
		}
	}

	// Posts turned away before their Pass is read.
	p.exec(`ALTER TABLE passes RENAME TO passes_unreadable`)
	unreadable := http.Header{passHeader: {"ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ"}}
	early := []struct {
		what, wildcard, value, key, body string
		status                           int
	}{
		{what: "a malformed poster key", key: "not a key", status: http.StatusBadRequest},
		{what: "no text", body: `{"body":""}`, status: http.StatusBadRequest},
		{what: "a body that is not JSON", body: `{"body":`, status: http.StatusBadRequest},
		{what: "a board that is not there", wildcard: "slug", value: "nowhere", status: http.StatusNotFound},
		{what: "a thread number that is not one", wildcard: "no", value: "none", status: http.StatusBadRequest},
		{what: "a thread id that is not one", wildcard: "id", value: "none", status: http.StatusBadRequest},
		{what: "a thread id that names no thread", wildcard: "id", value: "999999", status: http.StatusNotFound},
	}
	sentTo := map[string]int{}
	for _, pattern := range posting {
		method, path := fill(pattern, nil)
		if status, line, _ := send(method, path, unreadable, `{"body":"as far as postPass"}`); status != http.StatusInternalServerError || line["duration_ms"] != nil {
			t.Errorf("%s with a Pass that cannot be read: status %d, line %v", pattern, status, line)
		}
		for _, post := range early {
			if post.wildcard != "" && !strings.Contains(pattern, "{"+post.wildcard+"}") {
				continue
			}
			sentTo[post.what]++
			method, path := fill(pattern, map[string]string{post.wildcard: post.value})
			header := unreadable.Clone()
			if post.key != "" {
				header.Set(posterKeyHeader, post.key)
			}
			body := post.body
			if body == "" {
				body = `{"body":"turned away"}`
			}
			if status, line, _ := send(method, path, header, body); status != post.status || line["duration_ms"] != nil {
				t.Errorf("%s with %s: status %d, want %d, line %v", pattern, post.what, status, post.status, line)
			}
		}
	}
	for _, post := range early {
		if sentTo[post.what] == 0 {
			t.Errorf("no route that takes a post has a {%s}, so none was sent %s", post.wildcard, post.what)
		}
	}
}

// requestLine waits for the request line of method and path among what was
// logged after the log's first from bytes, which the caller notes before it
// sends the request, once the lines of the requests it sent before are in: so
// the line read is that request's own, never one an earlier request to the
// same path left. The line is written once the handler returns and can come
// just after its answer. It returns the line decoded.
func requestLine(t *testing.T, logs *syncBuffer, from int, method, path string) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		for _, raw := range strings.Split(logs.String()[from:], "\n") {
			var line map[string]any
			if json.Unmarshal([]byte(raw), &line) == nil &&
				line["msg"] == "request" && line["method"] == method && line["path"] == path {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no request line for %s %s in what was logged after it was sent:\n%s", method, path, logs.String()[from:])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
