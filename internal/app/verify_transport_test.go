package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The credential is a header rather than a cookie so that it travels only on
// the posts that ask for a badge. A cookie is attached by the browser to
// everything — anonymous posts, page loads, images — which would make the badge
// a stable identifier across a poster's whole session whether or not they ever
// used it. That property is only worth anything if the server reads the
// credential in exactly one place and nothing else can pull it in by accident,
// so these two tests hold the shape from both sides: what the code is allowed
// to reference, and what the running server actually does with it.

// funcsReferencing walks the package's non-test sources and returns the names
// of the functions whose bodies match want.
func funcsReferencing(t *testing.T, want func(ast.Node) bool) []string {
	t.Helper()

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}

	fset := token.NewFileSet()
	var found []string
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if want(n) {
					found = append(found, fn.Name.Name)
					return false
				}
				return true
			})
		}
	}
	sort.Strings(found)
	return found
}

// calls reports whether n is a method call with the given name, which is how
// `a.credential(…)` and `a.postBadge(…)` appear in the tree.
func calls(name string) func(ast.Node) bool {
	return func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == name
	}
}

// TestCredentialIsReadOnlyOnTheBadgePath pins the call graph that carries the
// guarantee: the header is read in one function, that function has one caller,
// and that caller runs only when a submission asked for a badge.
//
// A new handler that reaches for the credential — to log it, to personalise a
// response, to check a status — fails here, because the whole point is that
// there is nothing else for it to be used for.
func TestCredentialIsReadOnlyOnTheBadgePath(t *testing.T) {
	cases := []struct {
		what  string
		match func(ast.Node) bool
		want  []string
		why   string
	}{
		{
			what: "read " + credentialHeader,
			match: func(n ast.Node) bool {
				ident, ok := n.(*ast.Ident)
				return ok && ident.Name == "credentialHeader"
			},
			want: []string{"credential"},
			why:  "the header is parsed in one place so there is one thing to reason about",
		},
		{
			what:  "call a.credential",
			match: calls("credential"),
			want:  []string{"postBadge"},
			why:   "a credential is examined when it is being used, and at no other time",
		},
		{
			what:  "call a.postBadge",
			match: calls("postBadge"),
			want:  []string{"createPost", "handleCreateThread"},
			why:   "only the two handlers that create posts can want a badge",
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			got := funcsReferencing(t, tc.match)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("functions that %s = %v, want exactly %v\n%s", tc.what, got, tc.want, tc.why)
			}
		})
	}

	// And no cookie fallback crept back in: a credential arriving in a cookie
	// would arrive on every request, which is the thing this design gives up.
	inCookies := funcsReferencing(t, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		return ok && strings.Contains(strings.ToLower(ident.Name), "credential") &&
			strings.Contains(strings.ToLower(ident.Name), "cookie")
	})
	if len(inCookies) != 0 {
		t.Errorf("something names a credential cookie in %v; the credential is not a cookie", inCookies)
	}
}

// TestCredentialHasNoEffectOffTheBadgePath is the same claim from the outside:
// present a real, valid credential to everything else the server answers and
// nothing changes. Whatever the source says, this is what a client can observe.
func TestCredentialHasNoEffectOffTheBadgePath(t *testing.T) {
	h := verifyServer(t, nil)
	credential := h.verify("ana@iitb.ac.in").Credential

	// A post that does not ask for a badge does not get one, even carrying a
	// credential the server would honour if it had been asked to.
	var created createdPost
	if status := h.postWithCredential(credential, "/api/boards/general/threads",
		map[string]any{"subject": "quiet", "body": "no badge requested"}, &created); status != http.StatusCreated {
		t.Fatalf("unbadged thread: status %d (%s)", status, created.Error)
	}
	detail := fetchThread(t, h.harness, "general", created.ThreadNo)
	if got := detail.Posts[0].Badge; got != "" {
		t.Errorf("post badge = %q, want none: the credential was present but never requested", got)
	}

	// Explicitly declining a badge is the same, credential or not.
	if status := h.postWithCredential(credential,
		fmt.Sprintf("/api/boards/general/threads/%d/posts", created.ThreadNo),
		map[string]any{"body": "still anonymous", "badge": false}, &createdPost{}); status != http.StatusCreated {
		t.Fatalf("declined badge: status %d", status)
	}
	if got := fetchThread(t, h.harness, "general", created.ThreadNo).Posts[1].Badge; got != "" {
		t.Errorf("post badge = %q, want none", got)
	}

	// Every read-only endpoint answers a request carrying the credential
	// exactly as it answers one without it.
	for _, path := range []string{
		"/api/verify",
		"/api/boards",
		"/api/boards/general",
		"/api/boards/general/threads",
		"/api/boards/general/catalog",
		"/api/boards/general/archive",
		fmt.Sprintf("/api/boards/general/threads/%d", created.ThreadNo),
		"/healthz",
	} {
		t.Run(path, func(t *testing.T) {
			var bare, presented json.RawMessage
			if code := h.do(http.MethodGet, path, nil, &bare); code != http.StatusOK {
				t.Fatalf("GET %s: status %d", path, code)
			}
			if code := h.getWithCredential(credential, path, &presented); code != http.StatusOK {
				t.Fatalf("GET %s with a credential: status %d", path, code)
			}
			if !bytes.Equal(bare, presented) {
				t.Errorf("GET %s answered differently with a credential:\n without: %s\n with:    %s",
					path, bare, presented)
			}
		})
	}
}
