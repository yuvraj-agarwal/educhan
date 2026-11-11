package app

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The client keeps promises the server cannot check: that the poster key and
// the credential leave the browser only on a post, that a birth date is never
// kept or sent, that the badge's 18+ attestation is never ticked in advance or
// kept, and that TeX in a post is typeset with its dangerous parts switched
// off. CI runs no browser, so these tests read app.js and pin the
// code that keeps each one, the way the CSP tests pin the markup. They are a
// floor, not a proof: an edit that trips one has changed a privacy or safety
// property and needs a browser check before the test is changed to match.

func appScript(t *testing.T) string {
	t.Helper()
	script, err := fs.ReadFile(webFS(), "app.js")
	if err != nil {
		t.Fatalf("reading app.js: %v", err)
	}
	return string(script)
}

// jsBlock returns the source from the line that starts with opener to the
// first line after it that is exactly closer — a top-level function, or a
// top-level listener — or fails the test. app.js is formatted with top-level
// blocks closing at column 0, which TestAppDeclaresEachTopLevelNameOnce relies
// on too.
func jsBlock(t *testing.T, script, opener, closer string) string {
	t.Helper()
	start := strings.Index(script, "\n"+opener)
	if start < 0 {
		t.Fatalf("app.js has no line starting %q; this test reads it", opener)
	}
	start++
	end := strings.Index(script[start:], "\n"+closer+"\n")
	if end < 0 {
		t.Fatalf("no %q closes the block starting %q", closer, opener)
	}
	return script[start : start+end+1+len(closer)]
}

func jsFunction(t *testing.T, script, name string) string {
	t.Helper()
	if strings.Contains(script, "\nasync function "+name+"(") {
		return jsBlock(t, script, "async function "+name+"(", "}")
	}
	return jsBlock(t, script, "function "+name+"(", "}")
}

// withoutComments drops // comments, so a test about what code does is not
// tripped by a comment explaining what it must not do.
func withoutComments(src string) string {
	return regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(src, "")
}

// jsLines is src without its comments, line by line, each trimmed and the
// blank ones dropped: what a test holds a stretch of code to when every
// statement in it matters, so that one added anywhere in it fails the test.
func jsLines(src string) []string {
	var lines []string
	for _, line := range strings.Split(withoutComments(src), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// holdLines fails the test unless the lines of got that follow the one line
// equal to after, up to the first line equal to until, are exactly want.
func holdLines(t *testing.T, what string, got []string, after, until string, want []string) {
	t.Helper()
	start := slices.Index(got, after)
	if start < 0 || slices.Index(got[start+1:], after) >= 0 {
		t.Errorf("%s: want exactly one line %q to start from", what, after)
		return
	}
	rest := got[start+1:]
	end := slices.Index(rest, until)
	if end < 0 {
		t.Errorf("%s: no line %q after %q", what, until, after)
		return
	}
	if stretch := rest[:end]; !slices.Equal(stretch, want) {
		t.Errorf("%s runs\n\t%s\nwant exactly\n\t%s", what, strings.Join(stretch, "\n\t"), strings.Join(want, "\n\t"))
	}
}

// jsCallees lists the names code calls, comments left out: each name followed
// by an opening parenthesis. A method call (x.name()) is not a call of a
// top-level function and is left out. A call in another's arguments, f(g()),
// is found as well as the call around it.
func jsCallees(code string) map[string]bool {
	code = withoutComments(code)
	named := map[string]bool{}
	for _, m := range regexp.MustCompile(`[A-Za-z_$][\w$]*\(`).FindAllStringIndex(code, -1) {
		if m[0] > 0 && code[m[0]-1] == '.' {
			continue
		}
		named[code[m[0]:m[1]-1]] = true
	}
	return named
}

// jsCallGraph maps each top-level function of app.js to the names it calls,
// itself left out.
func jsCallGraph(t *testing.T, script string) map[string]map[string]bool {
	t.Helper()
	callees := map[string]map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^(?:async )?function ([A-Za-z_$][\w$]*)\(`).FindAllStringSubmatch(script, -1) {
		callees[m[1]] = jsCallees(jsFunction(t, script, m[1]))
		delete(callees[m[1]], m[1])
	}
	if len(callees) < 100 {
		t.Fatalf("read %d top-level functions from app.js; this test reads them", len(callees))
	}
	return callees
}

// jsReached lists, sorted, the top-level functions in graph that roots name,
// and every one they call, however far down.
func jsReached(t *testing.T, graph map[string]map[string]bool, roots ...string) []string {
	t.Helper()
	reached := map[string]bool{}
	for next := roots; len(next) > 0; next = next[1:] {
		name := next[0]
		if graph[name] == nil {
			t.Fatalf("app.js has no top-level function %s; this test follows its calls", name)
		}
		if reached[name] {
			continue
		}
		reached[name] = true
		for callee := range graph[name] {
			if graph[callee] != nil && !reached[callee] {
				next = append(next, callee)
			}
		}
	}
	var got []string
	for name := range reached {
		got = append(got, name)
	}
	slices.Sort(got)
	return got
}

// noticeCallees lists, sorted, the top-level functions of app.js that src
// calls and that say something in the notice: they call showNotice, or call a
// function that does, however far down. A method call (x.name()) is not a call
// of a top-level function and is left out.
func noticeCallees(t *testing.T, script, src string) []string {
	t.Helper()
	callees := jsCallGraph(t, script)
	if _, ok := callees["showNotice"]; !ok {
		t.Fatal("app.js has no top-level function showNotice; this test reads it")
	}
	says := map[string]bool{"showNotice": true}
	for grew := true; grew; {
		grew = false
		for name, named := range callees {
			if says[name] {
				continue
			}
			for callee := range named {
				if says[callee] {
					says[name], grew = true, true
					break
				}
			}
		}
	}
	var got []string
	for callee := range jsCallees(src) {
		// A function's own declaration is not a call of it.
		if strings.Contains(src, "function "+callee+"(") {
			continue
		}
		if says[callee] && callees[callee] != nil {
			got = append(got, callee)
		}
	}
	slices.Sort(got)
	return got
}

// submitHandler is the listener that sends posts.
func submitHandler(t *testing.T, script string) string {
	t.Helper()
	return jsBlock(t, script, `el.main.addEventListener("submit", async (event) => {`, "});")
}

// squash collapses every run of white space to one space, so a test can name
// a statement that spans lines, or has a comment line taken out of it, on one
// line of its own.
func squash(src string) string {
	return strings.Join(strings.Fields(src), " ")
}

// jsBraced returns src from the one place opener occurs to the brace that
// closes the brace opener ends with, or fails the test. Braces inside string
// and template literals are skipped; app.js has none in a regular expression
// in the code these tests read this way.
func jsBraced(t *testing.T, src, opener string) string {
	t.Helper()
	if n := strings.Count(src, opener); n != 1 || !strings.HasSuffix(opener, "{") {
		t.Fatalf("found %q %d times; this test reads exactly one, ending in {", opener, n)
	}
	start := strings.Index(src, opener)
	depth := 0
	for i := start + len(opener) - 1; i < len(src); i++ {
		switch c := src[i]; c {
		case '"', '\'', '`':
			for i++; i < len(src) && src[i] != c; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("nothing closes the brace %q opens", opener)
	return ""
}

// panelRedrawn is a change of the page's badge drawing /verify again where it
// shows the badge panel, unless the reader has drawn another step there by the
// time the page has asked the server (renderVerify).
const panelRedrawn = `const panel = el.main.querySelector('[data-action="forget-verification"]'); ` +
	`if (onVerifyPage() && ageStatus() === "ok" && panel) { renderVerify(); }`

// awayFromScreen is how the submit handler's catch ends for a post refused, or
// dropped unsent, while its form is off the screen, once it has released the
// form and dealt with a refused badge, noting the form, and with uploads
// switched off: it keeps with the form how to word what the form would have
// been told on screen, for the form a redraw then draws or keeps and the text
// and image it holds then, with whether it had the focus at the press, whether
// that refuses its badge and whether it refuses the image itself, for the
// thread's image limit or not, and the wording of a refusal but for one of the
// image itself or a post the forget stopped (refusalNotices), with the image
// release took off and whether it is the notice of a replaced badge as things
// stand when it is worded, its words yet to be told, for the redraw that tells
// them to word them again for the form it draws and a picker that takes the
// image off, and for them to be worded again in place while they are still the
// notice on screen; it takes the image off after a refusal of the image itself,
// as on screen, where a page kept for the form, or the form's next press if
// that comes first, unless an image has been attached since, which is left for
// the server to answer, then switches the picker off (blockRefusedPicker); and
// it returns.
const awayFromScreen = "if (!shown) { const imageRefused = limitReached || uploadsOff; " +
	`const fileField = form.querySelector("[data-file-field]"); if (imageRefused && fileField) { clearFileField(fileField); } ` +
	"const word = (on, text, image) => imageRefused ? errorMessage(err, { hasText: text, imageOff }) : " +
	"notice(imageOff, on, text, image); " +
	"if (!imageRefused && !stoppedByForget) { " +
	`refusalNotices.set(form, { notice, replaced: replacedNow, off: imageOff, told: "", after: "" }); } ` +
	"unsaidNotices.set(form, { " +
	"word, focused, badgeRefused, imageRefused, limitReached, }); return; }"

// inOrder fails the test unless every one of steps occurs in src, each after
// the one before it.
func inOrder(t *testing.T, what, src string, steps ...string) {
	t.Helper()
	at := 0
	for i, step := range steps {
		n := strings.Index(src[at:], step)
		if n < 0 {
			if strings.Contains(src, step) {
				t.Errorf("%s: %q comes before %q, not after it", what, step, steps[i-1])
			} else {
				t.Errorf("%s: no %q", what, step)
			}
			return
		}
		at += n + len(step)
	}
}

// The poster key ties every unbadged post from a browser together, and the
// credential is the badge; Verified Anonymity rests on each leaving the
// browser only on a post, and never the two together. A refactor that moved
// either header into api() would attach it to every request the page makes.
func TestOnlyThePostingRequestCarriesThePosterKeyOrCredential(t *testing.T) {
	script := appScript(t)
	submit := submitHandler(t, script)

	for _, name := range []string{"POSTER_KEY_HEADER", "CREDENTIAL_HEADER"} {
		uses := strings.Count(script, name)
		inSubmit := strings.Count(submit, name)
		// The declaration, and the one use.
		if uses != 2 || inSubmit != 1 {
			t.Errorf("%s appears %d times in app.js, %d of them in the submit handler; "+
				"want the declaration and one use, in the submit handler", name, uses, inSubmit)
		}
	}
	// One request carries one or the other: the credential on a badged post,
	// the key on any other.
	exclusive := regexp.MustCompile(`const headers = held \? \{ \[CREDENTIAL_HEADER\]: held\.token \} : \{ \[POSTER_KEY_HEADER\]: posterKey\(\) \};`)
	if !exclusive.MatchString(submit) {
		t.Error("the submit handler no longer chooses between the credential and the poster key; " +
			"a post must never carry both")
	}
	// The key is made and read only for a post: besides its own definition,
	// posterKey is called once, there.
	calls := regexp.MustCompile(`[^\w.]posterKey\(\)`).FindAllString(script, -1)
	if got := len(calls) - strings.Count(script, "function posterKey()"); got != 1 || strings.Count(submit, "posterKey()") != 1 {
		t.Errorf("posterKey() is called %d times in app.js; want once, in the submit handler", got)
	}
	// And the credential is sent only when the badge box asked for it.
	if !strings.Contains(submit, `const held = wantsBadge ? state.verification.badge : null;`) {
		t.Error("the submit handler no longer ties the credential to the badge box")
	}
	api := withoutComments(jsFunction(t, script, "api"))
	for _, banned := range []string{"posterKey", "storedCredential", "verification.badge", "localStorage"} {
		if strings.Contains(api, banned) {
			t.Errorf("api() reads %s; every request would carry it", banned)
		}
	}
}

// The age screen asks for a month and a year and promises they are never
// saved or sent. Its form submits to the dialog and its selects have no name,
// so there is nothing for a browser to send; what is stored is the outcome
// alone, under its two keys, and nothing in the screen makes a request.
func TestAgeScreenNeverKeepsOrSendsTheAnswer(t *testing.T) {
	script := appScript(t)

	question := jsFunction(t, script, "ageQuestionHTML")
	if !strings.Contains(question, `method="dialog"`) {
		t.Error(`the age question's form does not submit to its dialog (method="dialog")`)
	}
	if strings.Contains(question, "action=") {
		t.Error("the age question's form has an action, which would send the answer somewhere")
	}
	selects := regexp.MustCompile(`<select[^>]*>`).FindAllString(question, -1)
	if len(selects) != 2 {
		t.Fatalf("found %d selects in the age question, want the month and the year", len(selects))
	}
	for _, sel := range selects {
		if regexp.MustCompile(`\sname=`).MatchString(sel) {
			t.Errorf("an age select has a name, so a submitted form would carry it: %s", sel)
		}
		if !strings.Contains(sel, `autocomplete="off"`) {
			t.Errorf("an age select lets the browser remember the birth date: %s", sel)
		}
	}

	for _, name := range []string{
		"ageStatus", "ageFrom", "ageQuestionHTML", "openAgeScreen", "answerAgeQuestion",
		"passAgeScreen", "refuseAgeScreen", "reopenAgeScreen", "clearAgeError",
	} {
		body := withoutComments(jsFunction(t, script, name))
		for _, banned := range []string{"api(", "fetch(", "XMLHttpRequest", "sendBeacon", "localStorage", "sessionStorage", "document.cookie", "history.", "location"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s uses %s; the age screen makes no request and stores nothing but its outcome", name, banned)
			}
		}
	}

	// What the screen stores: "1" under the pass key or the refusal key, and
	// nothing else anywhere under an age key.
	stores := regexp.MustCompile(`storageSet\((AGE_[A-Z_]+), ([^)]*)\)`).FindAllStringSubmatch(script, -1)
	if len(stores) != 2 {
		t.Errorf("found %d storageSet calls with an age key, want the pass and the refusal", len(stores))
	}
	for _, store := range stores {
		if store[2] != `"1"` {
			t.Errorf("storageSet(%s, %s) stores more than the outcome", store[1], store[2])
		}
	}
	answer := jsFunction(t, script, "answerAgeQuestion")
	if strings.Count(script, "month.value") != strings.Count(answer, "month.value") ||
		strings.Count(script, "year.value") != strings.Count(answer, "year.value") {
		t.Error("the age answer is read outside answerAgeQuestion")
	}
}

// The badge is for students aged 18 and over, on their own word (build plan
// 2.14), and that word is asked for, sent once and kept nowhere. The box
// starts unticked on every render, the attestation lives only in the address
// step and the request it submits, neither of which touches storage, and the
// server's refusal is recognised in the server's own words.
func TestBadgeAttestationIsNeverPreselectedOrKept(t *testing.T) {
	script := appScript(t)
	view := withoutComments(jsFunction(t, script, "verifyViewHTML"))
	submit := withoutComments(jsFunction(t, script, "submitVerification"))

	box := regexp.MustCompile(`<input[^>]*\bid="f-adult"[^>]*>`).FindString(view)
	if box == "" {
		t.Fatal(`verifyViewHTML has no input with id "f-adult"; the address step asks for the attestation there`)
	}
	if !strings.Contains(box, `type="checkbox"`) {
		t.Errorf("the attestation is not a checkbox: %s", box)
	}
	// Ticked in advance it would be the page's word, not the visitor's; and
	// required, the browser's message would replace the page's refusal.
	for _, banned := range []string{"checked", "required"} {
		if regexp.MustCompile(`\s` + banned + `\b`).MatchString(box) {
			t.Errorf("the attestation box is %s: %s", banned, box)
		}
	}

	if all, here := strings.Count(withoutComments(script), "adult"),
		strings.Count(view, "adult")+strings.Count(submit, "adult"); all != here {
		t.Errorf("app.js mentions the attestation %d times outside verifyViewHTML and submitVerification", all-here)
	}
	for name, body := range map[string]string{"verifyViewHTML": view, "submitVerification": submit} {
		for _, banned := range []string{"localStorage", "sessionStorage", "storageSet", "document.cookie", "indexedDB"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s uses %s; the attestation is never stored", name, banned)
			}
		}
	}

	if !strings.Contains(script, `const BADGE_AGE_REFUSED = "`+adultRequired+`";`) {
		t.Errorf("BADGE_AGE_REFUSED in app.js is not the server's refusal %q, so the page would show it raw", adultRequired)
	}
}

// TeX in a post is untrusted input typeset in every reader's tab. trust:false
// keeps \href, \url and \htmlData from making links or attributes; maxExpand,
// maxSize and the per-expression macro table that switches \def and its
// family off bound the work and the output. Loosening any of them needs the
// browser probes AGENTS.md describes.
func TestKaTeXIsCalledWithItsSafetyOptions(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	if strings.Count(code, "katex.render(") != 1 {
		t.Errorf("katex.render is called %d times; want once, in typeset", strings.Count(code, "katex.render("))
	}
	for _, banned := range []string{"renderToString", "trust: true", "__renderToDomTree", "__parse"} {
		if strings.Contains(code, banned) {
			t.Errorf("app.js uses %s", banned)
		}
	}

	typeset := withoutComments(jsFunction(t, script, "typeset"))
	for _, want := range []string{
		"trust: false,",
		"throwOnError: false,",
		"macros: disabledCommands(katex),",
	} {
		if !strings.Contains(typeset, want) {
			t.Errorf("typeset no longer passes %q to katex.render", want)
		}
	}
	bounded := func(option string, max int) {
		m := regexp.MustCompile(option + `: (\d+),`).FindStringSubmatch(typeset)
		if m == nil {
			t.Errorf("typeset no longer passes %s to katex.render", option)
			return
		}
		if n, _ := strconv.Atoi(m[1]); n > max || n <= 0 {
			t.Errorf("typeset passes %s: %s, want 1–%d", option, m[1], max)
		}
	}
	bounded("maxExpand", 1000)
	bounded("maxSize", 20)
	if !strings.Contains(typeset, "withStyleAttributesThroughCSSOM(") {
		t.Error("typeset no longer builds through withStyleAttributesThroughCSSOM; the CSP refuses KaTeX's style attributes")
	}

	list := regexp.MustCompile(`(?s)const KATEX_DISABLED = \[(.*?)\];`).FindStringSubmatch(script)
	if list == nil {
		t.Fatal("app.js has no KATEX_DISABLED list; this test reads it")
	}
	for _, name := range []string{`\\def`, `\\gdef`, `\\edef`, `\\xdef`, `\\newcommand`, `\\renewcommand`, `\\providecommand`} {
		if !strings.Contains(list[1], `"`+name+`"`) {
			t.Errorf("KATEX_DISABLED no longer switches off %s", name)
		}
	}
	// The table is fresh per expression, and the stand-ins for \tag and
	// \nonumber must not reach \gdef by another name.
	table := withoutComments(jsFunction(t, script, "disabledCommands"))
	if !strings.Contains(table, "const macros = {};") {
		t.Error("disabledCommands no longer builds a fresh table per expression")
	}
	if strings.Contains(table, "gdef") {
		t.Error("disabledCommands expands to \\gdef, which it switches off")
	}
}

// One highlight.js call cannot be interrupted, and some grammars take time
// that grows with the square of a long identifier run; a cap on block length
// and highlighting only near the viewport keep a crafted thread from freezing
// its readers' tabs.
func TestCodeHighlightingIsBoundedAndLazy(t *testing.T) {
	script := appScript(t)
	m := regexp.MustCompile(`const HIGHLIGHT_MAX_CHARS = (\d+);`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal("app.js has no HIGHLIGHT_MAX_CHARS; this test reads it")
	}
	if n, _ := strconv.Atoi(m[1]); n <= 0 || n > 3000 {
		t.Errorf("HIGHLIGHT_MAX_CHARS = %s, want at most 3000", m[1])
	}
	highlight := withoutComments(jsFunction(t, script, "highlightCode"))
	for _, want := range []string{"HIGHLIGHT_MAX_CHARS", "IntersectionObserver"} {
		if !strings.Contains(highlight, want) {
			t.Errorf("highlightCode no longer uses %s", want)
		}
	}
	if strings.Contains(highlight, "loadHighlighter") {
		t.Error("highlightCode loads highlight.js before any block is near the viewport")
	}
}

// GET /api/site says what this server takes, and like the board list it is
// asked for only once the age screen has its answer: from loadSite, which
// render calls once, past the screen. It goes out with nothing of the
// reader's on it, no header or body, because it is the same answer for
// everyone.
func TestSiteSettingsAreFetchedOnlyPastTheAgeScreen(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	load := withoutComments(jsFunction(t, script, "loadSite"))

	if n := strings.Count(code, `"/api/site"`); n != 1 || !strings.Contains(load, `api("/api/site")`) {
		t.Errorf(`"/api/site" appears %d times in app.js; want once, as api("/api/site") in loadSite`, n)
	}
	// The definition, and one call.
	if n := strings.Count(code, "loadSite()"); n != 2 {
		t.Errorf("loadSite() appears %d times in app.js; want its definition and one call, in render", n)
	}
	render := withoutComments(jsFunction(t, script, "render"))
	gate := strings.Index(render, "renderAgeGate(age);")
	passed := strings.Index(render, `if (age === "ok") {`)
	call := strings.Index(render, "await loadSite();")
	if gate < 0 || passed < 0 || call < 0 || !(gate < passed && passed < call) {
		t.Error(`render no longer calls loadSite only past the age screen, inside if (age === "ok")`)
	}
}

// jsString returns the value of a top-level string constant in app.js.
func jsString(t *testing.T, script, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^const ` + name + ` = "([^"\\]*)";$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("app.js has no string constant %s; this test reads it", name)
	}
	return m[1]
}

// The posting form recovers from two refusals of an image by taking the image
// off and keeping the text, and it knows them by the server's own words. If
// those words drifted apart, a page would show the raw refusal and leave the
// image on the form to be refused again, so they are checked against what the
// server actually answers.
func TestClientKnowsTheServersImageRefusals(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	limitReached := jsString(t, script, "IMAGE_LIMIT_REACHED")
	switchedOff := jsString(t, script, "UPLOADS_SWITCHED_OFF")
	for name, words := range map[string]string{"IMAGE_LIMIT_REACHED": limitReached, "UPLOADS_SWITCHED_OFF": switchedOff} {
		if n := strings.Count(code, `"`+words+`"`); n != 1 {
			t.Errorf("app.js spells out %q %d times; want once, as %s", words, n, name)
		}
		if n := strings.Count(code, "err.message === "+name); n != 2 {
			t.Errorf("app.js compares a refusal with %s %d times; want twice, in errorMessage and in the submit handler", name, n)
		}
	}

	on := testServer(t, nil)
	on.addBoard("words")
	on.exec(`UPDATE boards SET image_limit = 0 WHERE slug = 'words'`)
	thread := on.createThread("words", "", "", "a text thread")
	if status, msg := on.imageReply("words", thread.ThreadNo, "an image"); status != http.StatusForbidden || msg != limitReached {
		t.Errorf("image reply past the cap: %d %q, want 403 %q", status, msg, limitReached)
	}

	off := testServer(t, func(cfg *Config) { cfg.UploadsEnabled = false })
	off.addBoard("words")
	var refused createdPost
	if status := off.upload("/api/boards/words/threads", map[string]string{"body": "pic"},
		"pic.png", pngBytes(t, 24, 24), &refused); status != http.StatusForbidden || refused.Error != switchedOff {
		t.Errorf("image post with uploads off: %d %q, want 403 %q", status, refused.Error, switchedOff)
	}
}

// Forget this browser is the FAQ's advice for a shared computer, and its
// links land on /verify#forget-browser whatever state the page is in, so
// every state the page can draw carries the control.
func TestEveryVerifyStateOffersToForgetTheBrowser(t *testing.T) {
	view := withoutComments(jsFunction(t, appScript(t), "verifyViewHTML"))
	returns := strings.Count(view, "return `")
	offers := strings.Count(view, "${forgetBrowserHTML()}`;")
	if returns < 4 || offers != returns {
		t.Errorf("verifyViewHTML draws %d states and ends %d of them with forgetBrowserHTML(); want every one", returns, offers)
	}
}

// captchaFunctions are the client's captcha, from fetching a challenge to
// sending an answer.
var captchaFunctions = []string{
	"captchaFormNeedsIt", "prepareCaptcha", "captchaAnswer", "stopCaptcha", "captchaBadgeToggled",
	"takeCaptcha", "keepCaptcha", "solveCaptcha", "runCaptchaWorkers", "sendWithCaptcha",
}

// A captcha answer is good for one request and kept in memory only: never in
// storage, a cookie or a draft, where it would outlive the page or be found by
// the next person at a shared computer. It is set where it is solved, taken out
// where a request takes it, put back where a request left it unspent, or may
// have spent it without saying so, while it is good and into an empty place,
// and read nowhere else.
func TestCaptchaAnswerStaysInMemory(t *testing.T) {
	script := appScript(t)
	banned := []string{"storageSet", "storageGet", "localStorage", "sessionStorage", "indexedDB", "document.cookie"}
	for _, name := range captchaFunctions {
		fn := withoutComments(jsFunction(t, script, name))
		for _, b := range banned {
			if strings.Contains(fn, b) {
				t.Errorf("%s uses %s; the captcha answer stays in memory", name, b)
			}
		}
	}
	for _, name := range []string{"captureDraft", "restoreDraft", "keepUnsentDraft"} {
		if fn := withoutComments(jsFunction(t, script, name)); strings.Contains(strings.ToLower(fn), "captcha") {
			t.Errorf("%s handles the captcha; a draft carries what the poster wrote and nothing else", name)
		}
	}
	// The two requests that carry an answer put it in their bodies in the
	// closure they hand sendWithCaptcha, and that closure keeps nothing at
	// all: no storage, no cookie, no draft. Around it, where the body that
	// held the answer is still in reach, each keeps only what it is for: the
	// submit handler the name of an unbadged post, and submitVerification the
	// credential the server issued.
	keepers := append(banned, "cookieStore", "caches", "storeCredential", "captureDraft", "restoreDraft", "keepUnsentDraft")
	keeps := regexp.MustCompile(`\b(` + strings.Join(keepers, "|") + `)\b`)
	for name, allowed := range map[string][]string{
		"the submit handler": {"storageSet(NAME_STORAGE_KEY, name)", "storageGet(NAME_STORAGE_KEY)"},
		"submitVerification": {"storeCredential(issued.credential)"},
	} {
		code := withoutComments(submitHandler(t, script))
		if name == "submitVerification" {
			code = withoutComments(jsFunction(t, script, "submitVerification"))
		}
		send := jsBraced(t, code, "(answer) => {")
		if !strings.Contains(send, "return api(") {
			t.Errorf("%s's send closure no longer makes the request; this test reads it", name)
		}
		for _, call := range keeps.FindAllString(send, -1) {
			t.Errorf("%s's send closure, which holds the answer, uses %s", name, call)
		}
		for _, at := range keeps.FindAllStringIndex(code, -1) {
			ok := false
			for _, call := range allowed {
				ok = ok || strings.HasPrefix(code[at[0]:], call)
			}
			if !ok {
				t.Errorf("%s keeps something besides %v: %s", name, allowed, strings.SplitN(code[at[0]:], "\n", 2)[0])
			}
		}
	}
	code := withoutComments(script)
	if n := strings.Count(code, "captcha.answer = "); n != 3 ||
		!strings.Contains(jsFunction(t, script, "solveCaptcha"), "captcha.answer = btoa(") ||
		!strings.Contains(jsFunction(t, script, "takeCaptcha"), `captcha.answer = "";`) ||
		!strings.Contains(jsFunction(t, script, "keepCaptcha"), "captcha.answer = taken.answer;") {
		t.Errorf("captcha.answer is assigned %d times; want once in solveCaptcha, once, clearing it, in takeCaptcha, "+
			"and once, putting it back, in keepCaptcha", n)
	}
	// Read anywhere else, the held answer could go out on a request that
	// should not carry one, a badged post's.
	reads := strings.Count(code, "captcha.answer")
	for _, name := range []string{"captchaAnswer", "takeCaptcha", "keepCaptcha", "solveCaptcha"} {
		reads -= strings.Count(withoutComments(jsFunction(t, script, name)), "captcha.answer")
	}
	if reads != 0 {
		t.Errorf("captcha.answer is used %d times outside captchaAnswer, takeCaptcha, keepCaptcha and solveCaptcha; "+
			"a request gets its answer from sendWithCaptcha", reads)
	}
}

// A page that is drawn and read asks for no captcha. The challenge is fetched
// in one place, and the only things that start a fetch before a request needs
// it are focus moving into a box to write in, in a form that needs an answer
// (however it gets there: a click or tap, Tab, a quote, "Use a different
// address"), an image picked or dropped on it and attached (not one a re-render
// puts back), unticking the badge on a form whose tick stopped a puzzle or that
// has something written or attached, the page taking a ticked badge out of a
// form still on screen with something written or attached (its badge changed in
// another tab or in this one, or refused as expired or revoked, never by Forget
// this browser, nor as /verify is drawn over the form; a form off the screen
// then starts none, nor does the form a redraw draws in its place, noted or
// told of the refusal), the press refused once for that, of a form with
// something written or attached, and a post or code request refused with the
// header saying the server spent its answer, which the reader, still on its
// form, is about to send again.
func TestCaptchaIsAskedForOnlyWhenSomeoneWrites(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	if n := strings.Count(code, `"/api/captcha"`); n != 1 || !strings.Contains(jsFunction(t, script, "solveCaptcha"), `api("/api/captcha", { signal: verifyDeadline(signal) })`) {
		t.Errorf(`"/api/captcha" appears %d times in app.js; want once, in solveCaptcha`, n)
	}
	// An answer held and still good is sent rather than a fresh one solved,
	// and one past CAPTCHA_LIFETIME_MS is replaced.
	// A puzzle started in the background says nothing when it fails: the
	// request that needs the answer tries again and says what went wrong. Its
	// rejection is caught, so it never surfaces as an unhandled one.
	if prepare := squash(withoutComments(jsFunction(t, script, "prepareCaptcha"))); prepare != "function prepareCaptcha() { captchaAnswer().catch(() => {}); }" {
		t.Errorf("prepareCaptcha is %q; want it to start the puzzle and catch its failure, and do nothing else", prepare)
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "captchaAnswer")), "if (captcha.answer && Date.now() < captcha.goodUntil) {") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "solveCaptcha")), "captcha.goodUntil = fetched + CAPTCHA_LIFETIME_MS;") {
		t.Error("captchaAnswer no longer reuses a held answer only while it is good")
	}
	calls := map[string]struct {
		count int
		in    []string
	}{
		"solveCaptcha(":  {1, []string{"captchaAnswer"}},
		"captchaAnswer(": {2, []string{"prepareCaptcha", "sendWithCaptcha"}},
	}
	for call, want := range calls {
		// Besides the definition.
		if n := strings.Count(code, call) - 1; n != want.count {
			t.Errorf("%s is called %d times in app.js; want %d, in %v", call, n, want.count, want.in)
		}
		for _, caller := range want.in {
			if !strings.Contains(jsFunction(t, script, caller), call) {
				t.Errorf("%s no longer calls %s", caller, call)
			}
		}
	}
	focus := squash(withoutComments(jsBlock(t, script, `el.main.addEventListener("focusin", (event) => {`, "});")))
	send := squash(withoutComments(jsFunction(t, script, "sendWithCaptcha")))
	toggled := squash(withoutComments(jsFunction(t, script, "captchaBadgeToggled")))
	attached := squash(withoutComments(jsFunction(t, script, "captchaImageAttached")))
	withdrawn := squash(withoutComments(jsFunction(t, script, "withdrawBadgeOption")))
	submitted := squash(withoutComments(submitHandler(t, script)))
	if n := strings.Count(code, "prepareCaptcha()") - 1; n != 7 ||
		!strings.Contains(focus, `if (box.matches("textarea, input[type=text], input[type=email]") && captchaFormNeedsIt(box.form)) { prepareCaptcha(); }`) ||
		!strings.Contains(send, `=== "spent")) { keepCaptcha(taken); } else if (answer && needsIt() && onScreen()) { prepareCaptcha(); }`) ||
		!strings.Contains(toggled, `if (captchaFormNeedsIt(form) && (stopped || captchaFormHasWriting(form))) { prepareCaptcha(); }`) ||
		!strings.Contains(attached, `if (captchaFormNeedsIt(form) && image && image.files && image.files.length) { prepareCaptcha(); }`) ||
		!strings.Contains(withdrawn, `if (ticked && !startNothing && captchaFormNeedsIt(form) && captchaFormHasWriting(form)) { `+
			`prepareCaptcha(); } }`) ||
		!strings.Contains(submitted, `if (shown) { drawBadgeOption(form); if (replaced) { badgeWithdrawn.delete(form); } `+
			`if (captchaFormNeedsIt(form) && captchaFormHasWriting(form)) { prepareCaptcha(); } } else { withdrawBadgeOption(form, true); } }`) ||
		!strings.Contains(submitted, "refuseUnsent(form, badgeNoteWords(withdrawn), Boolean(body.trim()), Boolean(file)); "+
			`const box = field(form, "badge"); if (box) box.focus(); `+
			`if (captchaFormNeedsIt(form) && captchaFormHasWriting(form)) { prepareCaptcha(); } return; }`) {
		t.Errorf("prepareCaptcha() is called %d times in app.js; want seven: from the focusin listener in a box to write "+
			"in a form that needs an answer, on unticking the badge where the tick stopped a puzzle or something is "+
			"written, on an image attached to a form that needs an answer, after a refusal saying it spent the answer "+
			"of a form still on screen, once a ticked badge is taken out of a form with something written, "+
			"by the page seeing its badge change other than by a forget or by a refusal of it as expired or revoked "+
			"of a form still on screen, and on the press that note refuses, of a form with something written", n)
	}
	// Forget this browser takes the badge out of every form too, but starts
	// no request: a forget sends nothing. Not in the tab it was pressed in
	// or in a tab that hears it (forgetOnThisPage), and a tab hears it from
	// storage as the credential emptied, which the pressing tab does before
	// it removes it: there the tab forgets, and returns before it reads the
	// badge again and brings the forms into line with it, which is what
	// starts the puzzle for a badge that went any other way.
	if forget := withoutComments(jsFunction(t, script, "forgetOnThisPage") + jsFunction(t, script, "forgetForm")); strings.Contains(forget, "prepareCaptcha") ||
		strings.Contains(forget, "withdrawBadgeOption") {
		t.Error("forgetOnThisPage starts a puzzle for the forms it takes the badge out of")
	}
	follow := squash(withoutComments(jsFunction(t, script, "followCredentialStorage")))
	offer := squash(withoutComments(jsFunction(t, script, "offerHeldBadge")))
	inOrder(t, "forgetBrowser", squash(withoutComments(jsFunction(t, script, "forgetBrowser"))),
		"forgetOnThisPage();", "const emptied = emptyCredential();", "forgetCredential();")
	inOrder(t, "followCredentialStorage", follow,
		`if (event.key === CREDENTIAL_STORAGE_KEY && event.newValue === "") { forgottenElsewhere(true); return; }`,
		"if (!readStoredBadge()) { return; }", "offerHeldBadge();")
	if offer != `function offerHeldBadge(startNothing = false, except = null) { for (const form of el.main.querySelectorAll("form[data-kind]")) { `+
		`if (form !== except) withdrawBadgeOption(form, startNothing); } }` ||
		!strings.Contains(withoutComments(jsFunction(t, script, "emptyCredential")), `localStorage.setItem(CREDENTIAL_STORAGE_KEY, "");`) ||
		strings.Count(code, `localStorage.setItem(CREDENTIAL_STORAGE_KEY, "")`) != 1 || strings.Count(code, "emptyCredential(") != 2 ||
		strings.Count(code, `event.newValue === ""`) != 1 {
		t.Error("a tab that hears the browser forgotten in another tab, as the credential emptied, may start a " +
			"puzzle for it, or the credential is emptied somewhere besides forgetBrowser")
	}
	// Nor does /verify, drawn over a posting form whose badge went as it
	// loads (switched off, or run out): that is drawing a page. The form,
	// noted, starts it at the press the note refuses, if a later render draws
	// it again.
	if load := squash(withoutComments(jsFunction(t, script, "loadVerification"))); strings.Count(load, "offerHeldBadge(") != 1 ||
		strings.Count(load, "withdrawBadgeOption(") != 0 || !strings.Contains(load, "offerHeldBadge(true);") {
		t.Error("loadVerification starts a puzzle for the forms /verify is being drawn over")
	}
	// An image counts once it is attached, picked or dropped; one a re-render
	// puts back (restoreDraft, through setFile) is part of drawing a page, and
	// one dropped on a held form, which setFile does not take, is not attached.
	change := squash(withoutComments(jsBlock(t, script, `el.main.addEventListener("change", (event) => {`, "});")))
	drop := squash(withoutComments(jsBlock(t, script, `document.addEventListener("drop", (event) => {`, "});")))
	if strings.Count(code, "captchaImageAttached(") != 3 ||
		!strings.Contains(change, "clearNotice(); updateFileField(field); captchaImageAttached(event.target.form); }") ||
		!strings.Contains(drop, `if (setFile(field, event.dataTransfer.files[0])) { clearNotice(); updateFileField(field); `+
			`captchaImageAttached(field.closest("form")); }`) {
		t.Error("an image picked or dropped no longer starts the puzzle, once it is attached, or something else calls captchaImageAttached")
	}
	if set := squash(withoutComments(jsFunction(t, script, "setFile"))); strings.Count(set, "return true;") != 1 ||
		!strings.HasSuffix(set, "input.files = transfer.files; } catch { return false; } return true; }") {
		t.Error("setFile no longer says whether it took the file, which is what a drop starts the puzzle on")
	}
	// Unticking starts the puzzle again only where the tick stopped one, which
	// stopCaptcha says, or where the form holds some of the post already.
	if !strings.Contains(toggled, "if (box.checked) { if (stopCaptcha() && form) { captchaStoppedByBadge.add(form); } return; }") ||
		!strings.Contains(toggled, "const stopped = Boolean(form) && captchaStoppedByBadge.delete(form);") {
		t.Error("captchaBadgeToggled no longer remembers which forms' ticks stopped a puzzle")
	}
	stop := squash(withoutComments(jsFunction(t, script, "stopCaptcha")))
	if !strings.Contains(stop, "captcha.stop = null; return true; } return false; }") {
		t.Error("stopCaptcha no longer says whether it stopped a puzzle")
	}
	writing := squash(withoutComments(jsFunction(t, script, "captchaFormHasWriting")))
	for _, want := range []string{`return Boolean(box && box.value.trim());`,
		`return written("subject") || written("body") || Boolean(image && image.files && image.files.length);`} {
		if !strings.Contains(writing, want) {
			t.Errorf("captchaFormHasWriting no longer reads the subject, the message and the image: missing %s", want)
		}
	}
	if strings.Contains(writing, `"name"`) {
		t.Error("captchaFormHasWriting reads the name, which the form is drawn with")
	}
	for _, name := range []string{"render", "formHTML", "verifyViewHTML", "loadSite", "restoreDraft", "setFile"} {
		if fn := withoutComments(jsFunction(t, script, name)); strings.Contains(fn, "Captcha(") {
			t.Errorf("%s starts the captcha; drawing a page must not", name)
		}
	}
}

// A badged post skips the captcha, so it never waits for or carries an answer,
// and ticking the badge stops a puzzle the reader started by writing first; a
// code request always needs one when the captcha is on.
func TestCaptchaIsSkippedOnlyByABadgedPost(t *testing.T) {
	script := appScript(t)
	toggled := squash(withoutComments(jsFunction(t, script, "captchaBadgeToggled")))
	stop := squash(withoutComments(jsFunction(t, script, "stopCaptcha")))
	waits := squash(withoutComments(jsFunction(t, script, "sendWithCaptcha")))
	change := withoutComments(jsBlock(t, script, `el.main.addEventListener("change", (event) => {`, "});"))
	if !strings.Contains(change, "captchaBadgeToggled(event.target);") ||
		!strings.Contains(toggled, "if (box.checked) { if (stopCaptcha() && form) {") ||
		!strings.Contains(stop, "if (captcha.stop && captcha.waiting === 0) {") {
		t.Error("ticking the badge no longer stops a puzzle nobody is waiting for")
	}
	// A request counts itself as waiting for the puzzle, and stops counting
	// however the wait ends: an answer, a failed solve, a timeout. A count left
	// up after a failure would stop stopCaptcha for good, and the badge with it.
	if strings.Count(waits, "captcha.waiting") != 2 || !strings.Contains(waits, "solving(true); captcha.waiting += 1; try {") ||
		!strings.Contains(waits, "} finally { captcha.waiting -= 1; solving(false); }") {
		t.Error("sendWithCaptcha no longer counts a request waiting for the puzzle from just before its try to its finally")
	}
	// Stopping it aborts the solve, which terminates every copy of the worker
	// (TestCaptchaWorkersAreAlwaysStopped), and forgets it, so the next start
	// solves afresh rather than waiting on the one stopped.
	if !strings.Contains(stop, "{ captcha.stop.abort(); captcha.solving = null; captcha.stop = null; return true; }") {
		t.Error("stopCaptcha no longer aborts the puzzle and forgets it")
	}
	submit := withoutComments(submitHandler(t, script))
	if !strings.Contains(submit, "() => state.site.captcha && !held,") {
		t.Error("the submit handler no longer skips the captcha for a badged post, and only for one")
	}
	verify := withoutComments(jsFunction(t, script, "submitVerification"))
	if !strings.Contains(verify, `sendWithCaptcha(
        () => state.site.captcha,`) {
		t.Error("a code request no longer always carries an answer when the captcha is on")
	}
	needs := squash(withoutComments(jsFunction(t, script, "captchaFormNeedsIt")))
	if !strings.Contains(needs, "!(badge && badge.checked)") {
		t.Error("captchaFormNeedsIt no longer leaves a badged post's puzzle unsolved")
	}
	// Of the two forms on /verify only the code request needs an answer; the
	// confirmation sends none, so a puzzle solved for it would be work, and a
	// challenge counted against the address, for nothing.
	if !strings.Contains(needs, `if (form.dataset.verify) { return form.dataset.verify === "request"; }`) {
		t.Error("captchaFormNeedsIt no longer says that only the code request, of /verify's forms, needs an answer")
	}
	// And no form needs one while the captcha is off: on a site without it, a
	// box to write in would otherwise fetch a challenge on every visit.
	if !strings.HasPrefix(needs, "function captchaFormNeedsIt(form) { if (!state.site.captcha || !(form instanceof HTMLFormElement)) { return false; }") {
		t.Error("captchaFormNeedsIt no longer starts by saying no form needs an answer while the captcha is off")
	}
}

// The client and the server have to agree on the answer's format, its
// lifetime and the refusals' names, and nothing but a browser would notice if
// they drifted apart.
func TestClientAndServerAgreeOnTheCaptcha(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	m := regexp.MustCompile(`(?m)^const CAPTCHA_LIFETIME_MS = (\d+) \* 60 \* 1000;$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal("app.js has no CAPTCHA_LIFETIME_MS in minutes; this test reads it")
	}
	minutes, _ := strconv.Atoi(m[1])
	if lifetime := time.Duration(minutes) * time.Minute; lifetime > captchaTTL-time.Minute {
		t.Errorf("the client uses an answer for %s, but the server takes one for only %s; leave a margin", lifetime, captchaTTL)
	}

	solve := withoutComments(jsFunction(t, script, "solveCaptcha"))
	for _, want := range []string{
		"challenge: { parameters: challenge.parameters, signature: challenge.signature },",
		"solution: { counter: solution.counter, derivedKey: solution.derivedKey },",
	} {
		if !strings.Contains(solve, want) {
			t.Errorf("solveCaptcha no longer builds the answer as the server decodes it: missing %s", want)
		}
	}
	// captchaKey appends the counter as four big-endian bytes, which is the
	// worker's uint32 mode.
	if !strings.Contains(withoutComments(jsFunction(t, script, "runCaptchaWorkers")), `counterMode: "uint32",`) {
		t.Error(`runCaptchaWorkers no longer asks the worker for counterMode "uint32", the one captchaKey matches`)
	}
	// The copies' counters interleave: copy i counts from i in steps of their
	// number, so between them they try each counter once. Copies counting
	// from one start, or in steps of one, would all try the same counters.
	if !strings.Contains(squash(withoutComments(jsFunction(t, script, "runCaptchaWorkers"))),
		`counterMode: "uint32", counterStart: i, counterStep: count,`) {
		t.Error("runCaptchaWorkers no longer has copy i count from i in steps of the number of copies")
	}

	// A worker gives up at CAPTCHA_TIMEOUT_MS, which has to leave a slow phone
	// time for the hardest puzzle the server gives at the base work. The work
	// is at most maxCaptchaWork/captchaMaxScale (EDUCHAN_CAPTCHA_WORK's
	// ceiling) and the counter under twice the work (captchaCounter), so that
	// puzzle takes at most 2*maxCaptchaWork/captchaMaxScale attempts, and at
	// slowAttempt it takes at most half the timeout, as app.js and captcha.go
	// say, which leaves the other half for a phone slower still.
	// slowAttempt is what one attempt at captchaCost is allowed to cost a slow
	// phone. It is an allowance, not a measurement: in headless Chromium 141 on
	// a four-core 2.1 GHz Xeon, one copy of the worker took about 0.56 ms an
	// attempt and four copies together 0.38 ms.
	const slowAttempt = 4 * time.Millisecond
	m = regexp.MustCompile(`(?m)^const CAPTCHA_TIMEOUT_MS = ([0-9 *]+);$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal("app.js has no CAPTCHA_TIMEOUT_MS as a product of numbers; this test reads it")
	}
	timeout := time.Millisecond
	for _, factor := range strings.Split(m[1], "*") {
		n, err := strconv.Atoi(strings.TrimSpace(factor))
		if err != nil {
			t.Fatalf("CAPTCHA_TIMEOUT_MS = %s: %v", m[1], err)
		}
		timeout *= time.Duration(n)
	}
	if hardest := time.Duration(2*maxCaptchaWork/captchaMaxScale) * slowAttempt; 2*hardest > timeout {
		t.Errorf("a worker gives up after %s, but the hardest base puzzle can take a slow phone %s, more than half of that",
			timeout, hardest)
	}
	// The README and captcha.go give the timeout as a figure, two minutes, and
	// app.js and captcha.go give the hardest base puzzle as half of it, so it
	// is held to that figure as well as to the margin: a longer timeout passes
	// the check above and leaves all three wrong.
	if timeout != 2*time.Minute {
		t.Errorf("CAPTCHA_TIMEOUT_MS is %s, but the README, captcha.go and app.js say two minutes", timeout)
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "runCaptchaWorkers")), "timeout: CAPTCHA_TIMEOUT_MS,") {
		t.Error("runCaptchaWorkers no longer gives the worker CAPTCHA_TIMEOUT_MS")
	}
	// The README and the vendored worker's README say the page runs the worker
	// in up to four copies, as a figure.
	if !regexp.MustCompile(`(?m)^const CAPTCHA_MAX_WORKERS = 4;$`).MatchString(script) ||
		!strings.Contains(withoutComments(jsFunction(t, script, "runCaptchaWorkers")),
			"const count = Math.min(CAPTCHA_MAX_WORKERS, Math.max(1, navigator.hardwareConcurrency || 1));") {
		t.Error("app.js no longer runs the worker in up to four copies (CAPTCHA_MAX_WORKERS), as the READMEs say")
	}

	send := withoutComments(jsFunction(t, script, "sendWithCaptcha"))
	if !strings.Contains(send, "err.payload && err.payload.captcha") || !strings.Contains(send, `reason === "`+captchaMissing+`"`) {
		t.Errorf("sendWithCaptcha no longer reads the refusal's captcha field, or no longer knows %q", captchaMissing)
	}
	// "required" means the server wants an answer this page did not know to
	// send, so the page knows from then on, and the resend carries one.
	if !strings.Contains(squash(send), `if (reason === "`+captchaMissing+`") { state.site.captcha = true; }`) {
		t.Errorf("a %q refusal no longer turns the captcha on for a page drawn without knowing", captchaMissing)
	}
	// And the other way: with the captcha off, GET /api/captcha answers 404
	// (TestCaptchaIsOffWithoutASecret), so a page drawn while it was on learns
	// there that it is off, turns it off, and sends the request without an
	// answer. Any other failure to get one fails the request.
	if !strings.Contains(squash(send), "} catch (err) { if (err.status !== 404) { throw err; } state.site.captcha = false; } finally {") ||
		strings.Count(code, "state.site.captcha = false;") != 1 {
		t.Error("a 404 from /api/captcha no longer turns the captcha off and sends the request without an answer")
	}
	// A refusal of the answer drops it and sends once more, once: a loop here
	// would hammer /api/captcha. Which outcomes put the answer back is
	// TestEachCaptchaAnswerGoesOnOneRequest's.
	if !strings.Contains(send, "for (let retried = false; ; retried = true) {") ||
		!strings.Contains(send, "if (retried || !needsIt()) {") {
		t.Error("sendWithCaptcha no longer drops a refused answer and resends exactly once")
	}
	// Any other refusal puts the answer back, on keepCaptcha's terms
	// (TestEachCaptchaAnswerGoesOnOneRequest), unless the server says it spent
	// it, and then, while its form is on screen and the browser not forgotten
	// since the press, the next puzzle starts at once.
	if !strings.Contains(code, `const CAPTCHA_SPENT_HEADER = "`+captchaSpentHeader+`";`) ||
		!strings.Contains(send, `err.headers.get(CAPTCHA_SPENT_HEADER) === "spent"`) ||
		!strings.Contains(withoutComments(jsFunction(t, script, "api")), "err.headers = res.headers;") {
		t.Errorf("sendWithCaptcha no longer reads the %s header that says a refusal spent the answer", captchaSpentHeader)
	}
	if !strings.Contains(code, `const CAPTCHA_WORKER_SRC = "/lib/altcha/pbkdf2.js";`) || strings.Count(code, "new Worker(") != 1 ||
		!strings.Contains(code, "new Worker(CAPTCHA_WORKER_SRC)") {
		t.Error("app.js starts a worker other than the vendored ALTCHA one")
	}
}

// Every copy of the worker started for a puzzle is stopped when the puzzle
// ends, however it ends: at the first answer, when the last copy gives up,
// when the puzzle is stopped (the badge ticked), and when starting the copies
// throws part way. A copy left running would hold a reader's processor for up
// to CAPTCHA_TIMEOUT_MS for nothing. And a puzzle the copies gave up on is said
// to have taken too long only when every one of them ran out of time.
func TestCaptchaWorkersAreAlwaysStopped(t *testing.T) {
	run := squash(withoutComments(jsFunction(t, appScript(t), "runCaptchaWorkers")))
	for _, want := range []string{
		// finish settles the puzzle once, and terminates every copy started.
		"const finish = (settle, value) => { if (settled) return; settled = true; for (const worker of workers) worker.terminate(); settle(value); };",
		// Stopping the puzzle ends it there.
		`signal.addEventListener("abort", () => finish(reject, signal.reason), { once: true });`,
		// A copy is in workers as soon as it exists, so finish reaches it.
		"const worker = new Worker(CAPTCHA_WORKER_SRC); workers.push(worker);",
		// The answer, the last copy giving up, and a throw all end in finish.
		"finish(resolve, solution);",
		"if (left === 0) finish(reject, ",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("runCaptchaWorkers no longer stops every copy of the worker: missing %s", want)
		}
	}
	// How it ended is told apart: a copy answers null when it ran out of
	// time, so the puzzle took too long (CAPTCHA_TOO_SLOW) when every copy
	// did, and failed (CAPTCHA_FAILED) when any copy could not work at all.
	for _, want := range []string{
		"const giveUp = (outOfTime) => { left -= 1; if (outOfTime) timedOut += 1; " +
			"if (left === 0) finish(reject, timedOut === count ? new Error(CAPTCHA_TOO_SLOW) : failed()); };",
		"} else { giveUp(solution === null); } };",
		"worker.onerror = (event) => { event.preventDefault(); giveUp(false); };",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("runCaptchaWorkers no longer tells a puzzle that took too long from one that failed: missing %s", want)
		}
	}
	// Nothing settles the puzzle but finish, except a stop that came before
	// any copy was started.
	if strings.Count(run, "resolve(") != 0 || strings.Count(run, "reject(") != 1 ||
		strings.Index(run, "reject(signal.reason); return;") > strings.Index(run, "new Worker(") {
		t.Error("runCaptchaWorkers settles the puzzle outside finish, which terminates the copies")
	}
	// The whole loop that starts the copies is inside one try, so a throw at
	// any point in it, after some copies are running, reaches finish.
	try := jsBraced(t, run, "try {")
	after := run[strings.Index(run, try)+len(try):]
	if !strings.HasPrefix(try, "try { for (let i = 0; i < count; i++) {") || !strings.HasSuffix(try, "}); } }") ||
		!strings.HasPrefix(after, " catch { finish(reject, failed()); }") {
		t.Error("runCaptchaWorkers starts its copies outside a try whose catch stops them all")
	}
}

// The puzzle is never worked on for nothing. A page that cannot run the
// worker is told so before a challenge is fetched: one that is not a secure
// context, which no browser gives Web Crypto, that the check needs https, and
// one without Worker or Web Crypto that its browser could not finish it. And
// a response that is not a challenge, a proxy's page say, is taken for a
// server that could not make one before any worker is started on it. A 429
// from /api/captcha, the server handing out no more puzzles to the reader's
// network for now, says so, with its wait, ahead of anything that would word
// it as a post's or a code request's cooldown: that would tell a reader behind
// a busy campus address to wait out a cooldown they never started.
func TestCaptchaIsNotWorkedOnForNothing(t *testing.T) {
	script := appScript(t)
	inOrder(t, "solveCaptcha", squash(withoutComments(jsFunction(t, script, "solveCaptcha"))),
		"if (!window.isSecureContext) { throw new Error(CAPTCHA_INSECURE); }",
		`if (typeof Worker !== "function" || !(window.crypto && crypto.subtle)) { throw new Error(CAPTCHA_FAILED); }`,
		`challenge = await api("/api/captcha", { signal: verifyDeadline(signal) });`,
		"} catch (err) { if (err.status === 429) { throw new Error(captchaBusy(err)); } "+
			"throw err.status >= 500 ? new Error(CAPTCHA_UNAVAILABLE) : err; }",
		`if ( !challenge || !challenge.parameters || typeof challenge.parameters !== "object" || `+
			`typeof challenge.signature !== "string" ) { throw new Error(CAPTCHA_UNAVAILABLE); }`,
		"await runCaptchaWorkers(challenge, signal);",
	)
	busy := squash(withoutComments(jsFunction(t, script, "captchaBusy")))
	if !strings.Contains(busy, "const seconds = err.payload && err.payload.retry_after;") ||
		!strings.Contains(busy, "return `Too many anti-spam checks have been started from your network, "+
			"so nothing was sent. ${wait} and try again.`;") {
		t.Errorf("captchaBusy no longer says too many checks were started from the network, that nothing was sent, and the wait: %s", busy)
	}
	for _, banned := range []string{"Flood control", "post again", "request a code", "Codes can't"} {
		if strings.Contains(busy, banned) {
			t.Errorf("captchaBusy words a busy captcha as a cooldown: %q", banned)
		}
	}
	// No browser passes the check on such a page, so the reader is not sent
	// to try another.
	insecure := jsString(t, script, "CAPTCHA_INSECURE")
	if !strings.Contains(insecure, "needs a secure (https) connection") || !strings.Contains(insecure, "nothing was sent") ||
		strings.Contains(insecure, "browser") {
		t.Errorf("CAPTCHA_INSECURE should say the check needs https and nothing was sent, and nothing about browsers: %q", insecure)
	}
}

// A post or a code request may wait seconds on its puzzle, and the reader can
// leave meanwhile, for another page. A request that waits for its answer, and
// whose form has gone by the time it is ready, is not sent, and its answer is
// put back for the next press, while it is good, into an empty place; one with
// an answer held already, or needing none, goes with the press. A post goes to
// the board of the page its form is on, which the render that drew it noted
// (drawnFor), as drawVerifyStep notes every /verify drawn, after a forget say,
// and a post or a code request is answered on that page, never a render's begun
// since that has not drawn yet: pressed just after a link to a page with
// another route key, another board or thread say, it goes where its form was
// drawn for and is answered as off the screen, and any press made while a
// redraw that brings its form back loads, a post link's lookup into its own
// thread or another page of the same board's index say, has the form itself
// carried held, a post as if pressed just before the redraw began
// (TestAHeldFormSurvivesARedrawOfItsPage). So does a press refused then before
// anything is sent, an empty one say, so a redraw by its route key keeps the
// page for it, with no skeleton, and a failed load keeps it, while the same
// press made just before the redraw began is read for its values, and the page
// is not kept for it. A posting form stays on screen across a redraw of its own
// page, by route key, while it is held: the redraw puts it in place of the form
// the new page draws, or keeps the page around it when the new one will not
// load (TestAHeldFormSurvivesARedrawOfItsPage). No redraw carries a /verify
// form, so one leaves the screen at the next render, of /verify too, one begun
// just before its press included, if not before, when /verify is drawn again
// over it after a forget say, and a code request still waiting on its puzzle
// then is not sent: the step drawn again, waiting, is let go for a press of its
// own (submitVerification). Sent or not, a request whose form has left the
// screen is not navigated from and says nothing on another page the reader went
// to (a Verify request's answer is kept for the step drawn again when they come
// back, TestVerifyAnswersDrawOnlyOnTheVerifyPage), while a post that did go
// still empties and releases its form, wherever it is. A post's form keeps
// instead the way to word what it would have been told, and is told it once,
// worded for the form and the text and image it holds then, with the focus it
// would have had, by a redraw that carries it, held or still owed that notice,
// begun before the answer or after it, as it puts its values back into its own
// page, after a post link's lookup into its thread say, or keeps that page when
// it will not load: a refusal as on screen, say, or for a post not sent, which
// on screen would have gone, that nothing was posted. Every press of the form
// takes out what an earlier one left unsaid, switching the picker off first
// after a refusal of the image itself unless an image has been attached since.
func TestARequestGoesOnlyFromAFormOnScreen(t *testing.T) {
	script := appScript(t)
	if on := squash(withoutComments(jsFunction(t, script, "formOnScreen"))); !strings.Contains(on,
		"return form.isConnected && (form.dataset.kind ? routeKey(state.route) === routeKey(route) : state.route === route);") {
		t.Error("formOnScreen no longer asks that the form is in the document and its page still the one asked for, " +
			"the same render of it for a form no redraw carries")
	}
	// The forms a redraw carries are the posting forms, which alone have a
	// kind; the /verify forms have none.
	if verify := jsFunction(t, script, "verifyViewHTML"); strings.Contains(verify, "data-kind") || strings.Count(verify, "<form ") != 2 ||
		strings.Count(verify, "data-verify=") != 2 {
		t.Error("verifyViewHTML's two forms no longer go without a kind, which is what tells formOnScreen no redraw carries them")
	}

	send := squash(withoutComments(jsFunction(t, script, "sendWithCaptcha")))
	inOrder(t, "sendWithCaptcha", send,
		"async function sendWithCaptcha(needsIt, send, solving, onScreen) {",
		"let taken = null; let waited = false; if (needsIt()) {",
		// It waited if its answer was the puzzle's, not one held already.
		"const ready = captchaAnswer(); waited = waited || ready === captcha.solving; taken = takeCaptcha(await ready);",
		// A request not sent has not spent its answer, which goes back, and
		// the error says it was not sent.
		"if (waited && !onScreen()) { keepCaptcha(taken); throw notSent(); }",
		"return await send(answer);",
	)
	if n := strings.Count(send, "waited"); n != 4 {
		t.Errorf("waited appears %d times in sendWithCaptcha; want it declared, noted from the puzzle, and read before the send", n)
	}
	for _, want := range []string{
		// No second send, and no next puzzle, for a form that has gone. A
		// refusal of the answer that arrives once it has is answered as a
		// request not sent, since the second send it calls for is not.
		"if (retried || !needsIt()) { throw err; } if (!onScreen()) { throw notSent(); }",
		"else if (answer && needsIt() && onScreen()) { prepareCaptcha(); }",
	} {
		if !strings.Contains(send, want) {
			t.Errorf("sendWithCaptcha no longer stops for a form that has gone: missing %s", want)
		}
	}

	// The page is the one the form is on at the press, which each request reads
	// as it was noted when drawn (drawnFor), and sendWithCaptcha checks against
	// it; neither request is sent once the browser is forgotten either
	// (TestForgettingTheBrowserCancelsWhatIsOnItsWay).
	submit := squash(withoutComments(submitHandler(t, script)))
	// The post's route and board are those of the page its form was drawn on,
	// not state.route, which a link pressed just before has moved on; the
	// submit handler asks only whether the reader is on /verify, to draw it
	// again there (panelRedrawn).
	inOrder(t, "the submit handler", submit, "const route = drawnFor.route;",
		"path = `/api/boards/${encodeURIComponent(route.slug)}/threads`;",
		"path = `/api/boards/${encodeURIComponent(route.slug)}/threads/${form.dataset.thread}/posts`;",
		"const onScreen = () => formOnScreen(form, route);")
	if n := strings.Count(submit, "state.route"); n != 0 || strings.Count(submit, panelRedrawn) != 1 {
		t.Errorf("the submit handler reads state.route %d times; want none, and onVerifyPage only to draw /verify the reader went to again", n)
	}
	// A post's release says what it took off (TestCaptchaRefusalSaysWhatTheFormStillHolds).
	inOrder(t, "the submit handler", submit,
		"const onScreen = () => formOnScreen(form, route);",
		"() => onScreen() && !forgotten(), );",
		"} catch (err) { const shown = onScreen(); const imageOff = release(shown); button.textContent = label;",
		"if (!shown) {",
	)
	catch := submit[strings.Index(submit, "} catch (err) {"):]
	// What a form off the screen does there and then is to return, keeping
	// first what it would have been told (below).
	away := jsBraced(t, catch, "if (!shown) {")
	if away != awayFromScreen {
		t.Errorf("the submit handler does something besides return for a form off the screen, or keeping what it would have "+
			"been told: %s", away)
	}
	// Before it or in it, the other posting forms on the page the reader went
	// to are brought into line with the badge held now, their box drawn again
	// for it or taken out when there is none (offerHeldBadge), and /verify
	// there showing the refused badge's panel is drawn again, as for any change
	// of the page's badge (TestAFormSendsOnlyTheBadgeItWasDrawnFor).
	before := catch[:max(strings.Index(catch, "if (!shown) {"), 0)] + away
	before = strings.Replace(before, panelRedrawn, "", 1)
	for _, banned := range []string{"showNotice(", "navigate(", "reset(", "el.main", "redrawThreadCounts(", "state.thread", "blockFileField("} {
		if strings.Contains(before, banned) {
			t.Errorf("the submit handler's refusal uses %s before it knows the form is still on screen", banned)
		}
	}

	// A Verify request reads its page from drawnFor too, not state.route: a
	// code request answered before the page a link went to drew would draw its
	// code step over the page being left. drawnFor names /verify however it was
	// drawn, by its render or by a side path (drawVerifyStep, every Verify
	// step's one drawing, which notes it only while /verify is the page on
	// screen, where every caller draws it), one Forget this browser draws over
	// the error of a /verify that would not load say, which noted nothing:
	// naming the page before, it would have every press of the form judged off
	// the screen. Its form is on screen only while the page is on the step it
	// was sent from.
	if drawn := squash(withoutComments(jsFunction(t, script, "drawVerifyStep"))); !strings.Contains(drawn,
		"el.main.innerHTML = verifyViewHTML(); verifyShown = { step: verifyStep, node: el.main.firstElementChild, at: performance.now() }; "+
			"state.thread = null; expandedLink = null; if (onVerifyPage()) { drawnFor = { path: renderedPath, route: state.route, thread: null }; }") ||
		strings.Count(withoutComments(script), "verifyViewHTML()") != 2 {
		t.Error("drawVerifyStep no longer notes what the /verify it draws was drawn for, or /verify is drawn elsewhere, so a " +
			"press of its form, drawn after a forget over another page, would be judged on that page")
	}
	verify := squash(withoutComments(jsFunction(t, script, "submitVerification")))
	inOrder(t, "submitVerification", verify,
		"const route = drawnFor.route; const onScreen = () => formOnScreen(form, route) && verifyStep === sentFrom;",
		"onScreen, );",
		"} catch (err) { unhold(false); if (verifyStep !== sentFrom) return;",
		"if (!onVerifyPage()) return;",
	)
	if n := strings.Count(verify, "state.route"); n != 0 {
		t.Errorf("submitVerification reads state.route %d times; want none: its page is drawnFor's, and the page on screen onVerifyPage's", n)
	}
	// Its answer, refused or not, says nothing on a page the reader went to,
	// and is kept for the step drawn again when they come back
	// (TestVerifyAnswersDrawOnlyOnTheVerifyPage).
	vcatch := verify[strings.Index(verify, "} catch (err) {"):]
	vbefore := vcatch[:max(strings.Index(vcatch, "if (!onVerifyPage()) return;"), 0)]
	for _, banned := range []string{"showNotice(", "navigate(", "el.main", "drawVerifyStep(", "renderVerify(", "sayVerifyRefusal(", "refuseVerification("} {
		if strings.Contains(vbefore, banned) {
			t.Errorf("submitVerification's refusal uses %s before it knows the reader is still on /verify", banned)
		}
	}
	// What a post's form off the screen would have been told is said once, by a
	// redraw that carries the form held, begun before the answer or after it
	// while the form has something unsaid, Update or Back say (captureDraft):
	// by restoreDraft, once it has put the form's values back into its own
	// page, its badge box included, after a post link's lookup into its thread
	// say, and before it words that notice again for a picker the new page drew
	// switched off, so that ends as it would have on screen; or by render,
	// ahead of why, when the redraw will not load and keeps the page, form and
	// all, the picker switched off after a refusal of the image itself, as on
	// screen (TestAHeldFormSurvivesARedrawOfItsPage). Either words it as it
	// says it, for the form drawn or kept and the text and image it holds then,
	// so a forget, another badge, Forget this browser or the reader's own edits
	// since the answer are read as they stand (badgeReplacedNotice). The focus
	// goes to the form's button as release gives it on screen: if the form had
	// it at the press, noted as holdForm notes it, and it is lost now. So it
	// does for a form refused on screen while the redraw loaded, which release
	// gave it then on a button gone with the old page since (refocusedForms),
	// every press taking that out first. A refusal of the badge is said in
	// place of the note the catch left on the form, which would refuse the next
	// press for the badge again. Every press of a posting form takes out what
	// an earlier one left unsaid before anything else, one refused before it is
	// sent included, an empty one or one the badge's note refuses say, so a
	// form is told only of the press it is answering, and a form that does not
	// come back takes what it was not told with it. A refusal of the image
	// itself is acted on first, the picker switched off as on screen, so the
	// press is judged as it would have been there, unless an image has been
	// attached since, which is left for the server to answer rather than taken
	// off unsaid.
	inOrder(t, "the submit handler", submit,
		"if (form.dataset.verify) { await submitVerification(form); return; } const owed = takeUnsaidNotice(form); "+
			`const image = field(form, "image"); if (owed && owed.imageRefused && !(image && image.files.length)) { `+
			"blockRefusedPicker(form, owed.limitReached); } presses.set(form, (presses.get(form) ?? 0) + 1); "+
			"refocusedForms.delete(form); refusalNotices.delete(form); const button = ",
		"const focused = form.contains(document.activeElement); const release = holdForm(form);",
		"} catch (err) {", `const box = field(form, "body");`, "const said = (off, on, text, image) =>",
		"const notice = (off, on = form, text = hasText, image = hasImage) =>",
		awayFromScreen)
	if take := squash(withoutComments(jsFunction(t, script, "takeUnsaidNotice"))); take != "function takeUnsaidNotice(form) { "+
		"const unsaid = unsaidNotices.get(form); unsaidNotices.delete(form); return unsaid; }" {
		t.Errorf("takeUnsaidNotice no longer takes what a form was not told out as it reads it: %s", take)
	}
	restored := squash(withoutComments(jsFunction(t, script, "restoreDraft")))
	inOrder(t, "restoreDraft", restored,
		"answeredForm = heldForms.has(held) ? null : held;",
		"if (!form) { if (unsentOnScreen(answeredForm)) { clearNotice(); } keepUnsentDraft(draft); return; } "+
			"const unsaid = answeredForm ? takeUnsaidNotice(answeredForm) : undefined; const forgotten = draft.forgets !== browserForgets;",
		"node.value = draft[name];",
		"if (badge) badge.checked = same; if (ticked && !same && !(unsaid && unsaid.badgeRefused)) {",
		"const refused = answeredForm ? refusalNotices.get(answeredForm) : undefined; const text = Boolean(draft.body.trim()); "+
			`const words = unsaid ? unsaid.word(form, text, Boolean(draft.file)) : ""; if (unsaid) { showNotice(words); }`,
		`const refusal = answeredForm && draft.file ? pickerRefusal(form) : ""; if (refusal && unsaid && unsaid.imageRefused) { } `+
			"else if (refusal && !unchanged) {",
		"if (details && (draft.open || draft.body || draft.file)) { details.open = true; } "+
			"const focused = unsaid ? unsaid.focused : refused && refused.unsent ? refusalNotices.has(form) : "+
			"refocusedForms.has(answeredForm); "+
			"if (focused && (!document.activeElement || document.activeElement === document.body)) { "+
			`form.querySelector('button[type="submit"]')?.focus({ preventScroll: true }); } }`,
	)
	if code := withoutComments(script); !strings.Contains(code, "\nconst unsaidNotices = new WeakMap();\n") ||
		strings.Count(code, "unsaidNotices") != 5 || strings.Count(code, "takeUnsaidNotice(") != 4 ||
		strings.Count(restored, "unsaid") != 11 || strings.Count(code, "focused,") != 1 {
		t.Error("unsaidNotices is no longer only declared, kept by the submit handler for a form off the screen, read " +
			"by captureDraft and taken out at its next press, by restoreDraft and by a kept page's failed redraw, or " +
			"restoreDraft reads it for something besides the notice, the badge, the image, the notice's match and the focus")
	}
	if code := withoutComments(script); !strings.Contains(code, "\nconst refocusedForms = new WeakSet();\n") ||
		strings.Count(code, "refocusedForms") != 4 {
		t.Error("refocusedForms is no longer only declared, added to by release, taken out at every press and read by restoreDraft")
	}
	// A post not sent because its form had left the screen would have gone on
	// screen, so it is told that nothing was posted and, unless it is left with
	// nothing to post, neither text nor image, to post again, worded for what
	// the form still holds; one the forget stopped is told that.
	notSent := jsBraced(t, squash(withoutComments(jsFunction(t, script, "errorMessage"))), "if (err.notSent) {")
	m := regexp.MustCompile(`^if \(err\.notSent\) \{ if \(nothingLeft\) \{ return "([^"]*)"; \} ` +
		`return hasText \? "([^"]*)" : "([^"]*)"; \}$`).FindStringSubmatch(notSent)
	if m == nil || !strings.Contains(m[1], "nothing was posted") || strings.Contains(m[1], "again") ||
		m[2] != m[1]+" Your text is still in the box; post again." || m[3] != m[1]+" Your image is still attached; post again." {
		t.Errorf("errorMessage no longer tells a post not sent from a form off the screen that nothing was posted and to "+
			"post again, by what its form holds: %s", notSent)
	}
	if !strings.Contains(submit, "const stoppedByForget = err.notSent && forgotten(); const replacedNow = () => ") ||
		!strings.Contains(submit, "const said = (off, on, text, image) => stoppedByForget ? FORGOTTEN_BEFORE_SENDING : ") {
		t.Error("a post the forget stopped while its form was off the screen is no longer told it was the forget")
	}
	// A post that went from a form since gone leaves the page the reader went
	// to as it is, and a code request does not draw /verify over it.
	// The form a post went from is emptied wherever it is, so a redraw that
	// carried it while it was held does not show the post again. A badge
	// confirmed is offered from then on, /verify on screen or not, so a page
	// the reader goes to while /verify is being drawn again, which that
	// drawing then leaves alone, has it too, and so does a posting form on
	// screen (offerHeldBadge). Its form gone, the confirmation draws /verify
	// again where the reader is on /verify, unless they have drawn another
	// step there by the time the page has asked the server (renderVerify).
	inOrder(t, "the submit handler", submit, "() => onScreen() && !forgotten(), );", "form.reset();",
		"release(false); if (!onScreen()) { return; } await navigate(")
	inOrder(t, "submitVerification", verify,
		"onScreen, );", `verifyStep = { step: "code", label: sent.label || "" }; if (!onVerifyPage()) return; drawVerifyStep();`,
		`await api("/api/verify/confirm"`,
		`storeCredential(issued.credential); state.verification.badge = readBadge(); offerHeldBadge(); `+
			`verifyStep = { step: "email", label: "" }; if (!onVerifyPage()) return; if (await renderVerify()) showHeldBadge(); } catch (err) {`,
	)
	// And that drawing's GET /api/verify failing withdraws neither the badge
	// nor the feature: a failed request keeps the feature as the last answer
	// had it and reads the badge from storage as an answer would, so /verify
	// shows the badge and every posting form drawn after it offers it. Only a
	// page that has had no answer yet, which starts with the feature off,
	// takes a failure to mean off.
	load := squash(withoutComments(jsFunction(t, script, "loadVerification")))
	if !strings.Contains(load, "const answered = !!answer || state.verification.answered; "+
		"const enabled = answer ? !!answer.enabled : state.verification.enabled; "+
		"state.verification = { answered, enabled, badge: enabled ? readBadge() : null };") ||
		!strings.Contains(withoutComments(script), "\n  verification: { answered: false, enabled: false, badge: null },\n") {
		t.Errorf("a GET /api/verify that fails no longer keeps what the page knew, or a first one no longer means off: %s", load)
	}
}

// No answer is on two requests at once. The server takes an answer from only
// one of two requests that carry it, and refuses the other as used, so a page
// that handed its held answer to a second press while the first was on its way,
// a slow upload say, would have one of its own posts refused, and that one, its
// form gone, dropped without a word. So a request takes the answer out of the
// page as it is handed over, in the same step, and a press meanwhile solves a
// fresh puzzle. The answer goes back only when the request left it unspent, or
// may have spent it without saying so (as when it brought no response, or the
// 500 for a spend whose reply from the database was lost), and only into an
// empty place while it is still good: if it was spent, the first request whose
// captcha check on the server can look it up is refused as used and solves a
// fresh one, while one refused before that check, a reply to a thread locked
// meanwhile say, or by a lookup that fails, a 500 with no header, puts it back
// again, and one checked after the server's secret was rotated is refused as
// invalid and solves a fresh one too.
func TestEachCaptchaAnswerGoesOnOneRequest(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	take := squash(withoutComments(jsFunction(t, script, "takeCaptcha")))
	if !strings.Contains(take, `{ if (!answer || captcha.answer !== answer) { return null; } `+
		`const taken = { answer, goodUntil: captcha.goodUntil }; captcha.answer = ""; captcha.goodUntil = 0; return taken; }`) {
		t.Error("takeCaptcha no longer takes the held answer out of the page, or hands out one another request took")
	}
	keep := squash(withoutComments(jsFunction(t, script, "keepCaptcha")))
	if !strings.Contains(keep, "{ if (taken && !captcha.answer && Date.now() < taken.goodUntil) { "+
		"captcha.answer = taken.answer; captcha.goodUntil = taken.goodUntil; } }") {
		t.Error("keepCaptcha puts an answer back over another one, or past its time")
	}
	send := squash(withoutComments(jsFunction(t, script, "sendWithCaptcha")))
	for call, want := range map[string]int{"takeCaptcha(": 1, "keepCaptcha(": 2} {
		if n, in := strings.Count(code, call)-1, strings.Count(send, call); n != want || in != want {
			t.Errorf("%s is called %d times in app.js, %d of them in sendWithCaptcha; want %d, all there", call, n, in, want)
		}
	}
	for _, want := range []string{
		// Taken in the step that hands it over, and another solved if a
		// request waiting on the same puzzle took it first.
		"let taken = null;",
		"do { const ready = captchaAnswer(); waited = waited || ready === captcha.solving; taken = takeCaptcha(await ready); } while (!taken);",
		`const answer = taken ? taken.answer : "";`,
		// Not sent: put back.
		"if (waited && !onScreen()) { keepCaptcha(taken); throw notSent(); }",
		// Sent and taken: kept by nothing.
		"try { return await send(answer); } catch (err) {",
		// Refused without the spent header or a captcha reason, or never
		// answered: put back. Refused with the header: the next puzzle starts,
		// for a form still on screen, in a browser not forgotten since the
		// press.
		`const reason = err.payload && err.payload.captcha; if (!reason) { ` +
			`if (!(err.headers && err.headers.get(CAPTCHA_SPENT_HEADER) === "spent")) { keepCaptcha(taken); } ` +
			`else if (answer && needsIt() && onScreen()) { prepareCaptcha(); } throw err; }`,
	} {
		if !strings.Contains(send, want) {
			t.Errorf("sendWithCaptcha no longer hands each answer to one request: missing %s", want)
		}
	}
}

// "Forget this browser" covers what is already on its way. A code request, a
// confirmation or a post sent before the press and answered after it keeps
// nothing: not the badge the server issued, not the code step, which would
// greet the next person with the last one's institution, not the name, and not
// a note that a badge was lost, which would tell them there had been one. A
// code request or a post not sent yet, still waiting on its puzzle, is not
// sent: the post would go under the Name and the poster key the forget took
// away, while its form showed neither. That holds for the second send after a
// refused answer too, whether the forget is heard while the second puzzle is
// solving or while the first send is on its way, before its refusal arrives. A
// post's form is released with its text and says why, whichever it was:
// sendWithCaptcha throws the error notSent makes for a request it did not send,
// which on a form still on screen only the forget causes, and the submit
// handler answers that mark before it words any other error. A redraw on its
// way puts back no Name its draft read before the press, and a form left on
// screen loses its Name and the option to post with the badge and says what a
// form without it says about the anti-spam check, rather than offering a badge
// that has gone, and without a word at its next press, which would tell whoever
// presses it that the browser held one: a note that another tab took a ticked
// badge away goes too (badgeWithdrawn). So does a held form a redraw has taken
// off the screen, as it is put back, for a forget it missed there but not again
// for one it had on screen, and one answered while away has its Name read as of
// the newest forget it had. Each post and each draft notes browserForgets, and
// each Verify request the step it was sent from, and forgetOnThisPage alone
// moves browserForgets, as it puts a new step in place, for a press in this tab
// or one heard from another (TestForgettingTheBrowserReachesEveryTab).
func TestForgettingTheBrowserCancelsWhatIsOnItsWay(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	// Two assignments: the declaration, and forgetOnThisPage's.
	moves := regexp.MustCompile(`(\+\+|--)\s*browserForgets|browserForgets\s*([-+*/]?=[^=]|\+\+|--)`)
	if !strings.Contains(code, "\nlet browserForgets = 0;\n") || len(moves.FindAllString(code, -1)) != 2 ||
		!strings.Contains(withoutComments(jsFunction(t, script, "forgetOnThisPage")), "browserForgets += 1;") {
		t.Error("browserForgets is no longer moved by forgetOnThisPage alone")
	}
	// What the page holds goes with it: the badge it read, the note about a
	// lost one, the code step, its own poster key, what it noted it stored,
	// and from every posting form on screen the name it was drawn with and the
	// option to post with the badge, which names the institution.
	page := squash(withoutComments(jsFunction(t, script, "forgetOnThisPage")))
	if !strings.Contains(page, `{ browserForgets += 1; state.verification.badge = null; badgeLost = ""; `+
		`verifyStep = { step: "email", label: "" }; pagePosterKey = ""; storedHere.clear(); `+
		`for (const form of el.main.querySelectorAll("form[data-kind]")) { forgetForm(form); } }`) {
		t.Error("forgetOnThisPage no longer drops everything the page holds that the forget covers")
	}
	if forget := squash(withoutComments(jsFunction(t, script, "forgetForm"))); forget != `function forgetForm(form) { `+
		`const name = field(form, "name"); if (name) { name.defaultValue = ""; name.value = ""; } `+
		`badgeWithdrawn.delete(form); drawBadgeOption(form); forgottenAt.set(form, browserForgets); rewordRefusal(form); }` {
		t.Errorf("forgetForm no longer empties a form's Name and takes out its badge option and any note on it, "+
			"noting the forget it did and wording again a refusal the form was told: %s", forget)
	}
	// A held form a redraw has taken off the screen, while a post link's
	// lookup draws its skeleton say, is out of the forget's reach, so the
	// draft carries browserForgets as it was, and the form is forgotten as it
	// is put back if a forget has come since that did not reach it on screen
	// (forgottenAt). One answered while away is read as of the newest forget
	// it had, so its Name, read after a forget it missed, is not restored.
	// Without this the thread the lookup resolves to draws the forgotten Name
	// again, tripcode secret and all, and the next press posts under it and
	// stores it. Without forgottenAt, a form the redraw left on screen, its
	// page kept while Update loads say, would lose a Name typed after a forget
	// it had there, and a post pressed since would go out under a Name the
	// form no longer shows.
	if strings.Count(code, "forgetForm(") != 3 {
		t.Error("forgetForm is called from somewhere besides forgetOnThisPage and restoreDraft")
	}
	if !strings.Contains(code, "\nconst forgottenAt = new WeakMap();\n") || strings.Count(code, "forgottenAt") != 3 {
		t.Error("forgottenAt is no longer a WeakMap noted only by forgetForm and read only by restoreDraft")
	}
	if capture := squash(withoutComments(jsFunction(t, script, "captureDraft"))); !strings.Contains(capture,
		"if (heldForms.has(form) || unsaidNotices.has(form)) { return { key, held: form, forgets: browserForgets }; }") {
		t.Error("captureDraft no longer notes browserForgets beside a held form it carries")
	}
	inOrder(t, "restoreDraft", squash(withoutComments(jsFunction(t, script, "restoreDraft"))),
		"if (draft.held) { const held = draft.held; const forgets = Math.max(draft.forgets, forgottenAt.get(held) ?? 0);",
		"if (forgets !== browserForgets) { forgetForm(held); }",
		"form.replaceWith(held);",
		"answeredForm = heldForms.has(held) ? null : held; draft = { key: draft.key, ...formDraft(held), forgets }; }",
	)
	// The option is drawn again for the badge the page holds, none once it is
	// forgotten, with its hint, and the form's note on the anti-spam check,
	// drawn in its own element, says what a form drawn with that option, or
	// without one, says. The submit handler draws it again the same way when
	// the server refuses the badge as expired or revoked. Focus on the box goes
	// to the box drawn in its place, or to the form's button when there is
	// none, rather than to the page. The note is drawn only while the captcha
	// is on: a site without it has no check for a press to wait on.
	drop := squash(withoutComments(jsFunction(t, script, "drawBadgeOption")))
	inOrder(t, "drawBadgeOption", drop,
		`const held = state.verification.badge; const number = held ? held.number : ""; `+
			`const slot = form.querySelector("[data-badge-option]"); if (!slot || slot.dataset.badgeOption === number) return false;`,
		"const focused = slot.contains(document.activeElement); slot.dataset.badgeOption = number; slot.innerHTML = badgeOptionHTML(held);",
		`const check = form.querySelector("[data-check-hint]"); if (check) check.textContent = held ? CHECK_HINT_WITH_BADGE : CHECK_HINT;`,
		`const next = drawn || form.querySelector('button[type="submit"]'); `+
			"if (focused && next) next.focus({ preventScroll: true }); return ticked; }",
	)
	if option := squash(withoutComments(jsFunction(t, script, "badgeOptionHTML"))); !strings.HasPrefix(option, `function badgeOptionHTML(held) { if (!held) return "";`) ||
		strings.Count(code, "f-badge-hint") != 2 || strings.Count(option, "f-badge-hint") != 2 {
		t.Error("badgeOptionHTML no longer draws nothing for no badge, or the option's hint is drawn or removed outside it")
	}
	form := squash(withoutComments(jsFunction(t, script, "formHTML")))
	if !strings.Contains(form, `let check = ""; if (state.site.captcha) { `+
		"check = ` <span data-check-hint>${held ? CHECK_HINT_WITH_BADGE : CHECK_HINT}</span>`; }") ||
		strings.Count(form, "check = ") != 2 || strings.Contains(form, "anti-spam") {
		t.Error("formHTML no longer draws the note on the anti-spam check, only while the captcha is on, in the element " +
			"drawBadgeOption rewords, from the two constants")
	}
	// And the markup draws it, once, in the hint of the actions row beside the
	// button whose press may wait on the check. Left out, no form would say so,
	// and drawBadgeOption would find no note to reword, without a word.
	actions := form[max(strings.Index(form, `<div class="actions">`), 0):]
	actions = actions[:max(strings.Index(actions, "</div>"), 0)]
	if !strings.Contains(actions, `<button class="btn primary" type="submit">`) ||
		!strings.Contains(actions, `<span class="hint">Posting is anonymous.${check}</span>`) || strings.Count(form, "${check}") != 1 {
		t.Error("formHTML no longer draws the note on the anti-spam check, once, in the hint of the actions row beside its button")
	}
	hint, badgeHint := jsString(t, script, "CHECK_HINT"), jsString(t, script, "CHECK_HINT_WITH_BADGE")
	if strings.Contains(hint, "badge") || !strings.Contains(badgeHint, "badge") || !strings.HasSuffix(badgeHint, hint[1:]) {
		t.Errorf("CHECK_HINT should say nothing of a badge, and CHECK_HINT_WITH_BADGE add only that: %q, %q", hint, badgeHint)
	}
	if n := strings.Count(code, "drawBadgeOption(form)"); n != 4 || !strings.Contains(withoutComments(submitHandler(t, script)), "drawBadgeOption(form);") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "withdrawBadgeOption")), "const ticked = drawBadgeOption(form);") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "forgetForm")), "drawBadgeOption(form);") ||
		strings.Count(code, "slot.innerHTML") != 1 || strings.Count(code, "offerHeldBadge(") != 5 ||
		!strings.Contains(withoutComments(jsFunction(t, script, "followCredentialStorage")), "offerHeldBadge(") {
		t.Errorf("the badge option is drawn again in a form %d times besides in drawBadgeOption; want three, the submit "+
			"handler's, a forget's and withdrawBadgeOption's, which offerHeldBadge calls for followCredentialStorage among others", n-1)
	}
	// A redraw's draft is read before the page is fetched, so a forget can
	// come between the two: the draft notes browserForgets as it is read, and
	// its Name goes back into the new form only if nothing has moved it since.
	if draft := squash(withoutComments(jsFunction(t, script, "formDraft"))); !strings.Contains(draft, "return { forgets: browserForgets,") {
		t.Error("formDraft no longer notes browserForgets beside the Name it reads")
	}
	inOrder(t, "restoreDraft", squash(withoutComments(jsFunction(t, script, "restoreDraft"))),
		"const forgotten = draft.forgets !== browserForgets;",
		`for (const name of ["subject", "name", "body"]) { const node = field(form, name); `+
			`if (node && name in draft && !(name === "name" && forgotten)) { node.value = draft[name]; } }`,
	)
	// A Verify request is cancelled by the new step the forget moves the page
	// to (forgetOnThisPage, above): one waiting on its turn or its puzzle is
	// not sent, and what one sent brings back is kept only while the page is
	// on the step it was sent from.
	verify := squash(withoutComments(jsFunction(t, script, "submitVerification")))
	inOrder(t, "submitVerification", verify,
		"const sentFrom = verifyStep;",
		"const onScreen = () => formOnScreen(form, route) && verifyStep === sentFrom;",
		"const unhold = holdForm(form);",
		"await earlier; if (verifyStep !== sentFrom) return;",
		`onScreen, ); if (verifyStep !== sentFrom) return; verifyStep = { step: "code", label: sent.label || "" };`,
		`await api("/api/verify/confirm", { method: "POST", signal: verifyDeadline(), body: JSON.stringify({ code: String(data.get("code") || "") }), }); `+
			"} finally { sentFrom.confirming -= 1; } if (verifyStep !== sentFrom) return; storeCredential(issued.credential);",
		"} catch (err) { unhold(false); if (verifyStep !== sentFrom) return;",
	)
	if strings.Count(verify, "verifyStep = {") != 3 || strings.Count(verify, "storeCredential(") != 1 || strings.Count(verify, "readBadge(") != 1 ||
		strings.Count(verify, "badgeLost") != 0 {
		t.Error("submitVerification keeps what a request brings back somewhere this test does not read")
	}
	// A post is held to the same: not sent once forgotten (sendWithCaptcha asks
	// before each send, and again when a refusal of the answer calls for the
	// second), and what it brings back kept only if nothing has moved
	// browserForgets since the press. A post the forget stopped says so on its
	// form, or keeps it for a redraw to say if its form was off the screen, and
	// the error sendWithCaptcha throws for it, whose message is for nobody,
	// never reaches errorMessage, which words only one not sent because its
	// form had left the screen (TestARequestGoesOnlyFromAFormOnScreen).
	submit := squash(withoutComments(submitHandler(t, script)))
	inOrder(t, "the submit handler", submit,
		"const forgets = browserForgets; const forgotten = () => forgets !== browserForgets;",
		"const release = holdForm(form);",
		"() => onScreen() && !forgotten(), );",
		"if (!held && forgets === browserForgets) { storageSet(NAME_STORAGE_KEY, name); storedHere.set(NAME_STORAGE_KEY, name); }",
		`nameField.value = held || forgets !== browserForgets ? storageGet(NAME_STORAGE_KEY) || "" : name;`,
		`if (held && (reason === "expired" || reason === "revoked")) { `+
			"if (forgets === browserForgets && storedCredential() === held.token) { forgetCredential(); badgeLost =",
		`const box = field(form, "body"); const hasText = Boolean(box && box.value.trim());`,
		"const stoppedByForget = err.notSent && forgotten(); "+
			"const replacedNow = () => (replaced || (badgeRefused && state.verification.badge !== null)) && !forgotten(); "+
			"const said = (off, on, text, image) => stoppedByForget ? FORGOTTEN_BEFORE_SENDING : replacedNow() ? "+
			"badgeReplacedNotice(on, { hasText: text, hasImage: image, imageOff: off }) : "+
			"errorMessage(err, { hasText: text, hasImage: image, imageOff: off }); "+
			"const notice = (off, on = form, text = hasText, image = hasImage) => "+
			"imageTakenOff(said(off, on, text, image), off, { hasText: text, isThread, closed });",
		"if (!shown) {",
		"return; } if (err.notSent) { showNotice(imageTakenOff(FORGOTTEN_BEFORE_SENDING, imageOff, { hasText, isThread })); return; }",
		"showNotice(errorMessage(err, { hasText, imageOff }));",
	)
	if strings.Count(submit, "storageSet(") != 1 || strings.Count(submit, "badgeLost") != 1 || strings.Count(submit, "forgetCredential(") != 1 {
		t.Error("the submit handler keeps something, or drops a badge, without checking it against browserForgets")
	}
	send := squash(withoutComments(jsFunction(t, script, "sendWithCaptcha")))
	refused := submit[max(strings.Index(submit, "} catch (err) {"), 0):]
	mark := squash(withoutComments(jsFunction(t, script, "notSent")))
	if mark != `function notSent() { const err = new Error("not sent"); err.notSent = true; return err; }` ||
		strings.Count(code, "notSent") != 8 || strings.Count(verify, "if (err.notSent) {") != 1 ||
		strings.Count(send, "throw notSent();") != 2 || strings.Count(code, "notSent()") != 3 ||
		strings.Count(refused, "err.notSent") != 2 || strings.Count(send, "new Error(") != 0 || strings.Count(refused, "showNotice(") != 3 ||
		strings.Count(squash(withoutComments(jsFunction(t, script, "errorMessage"))), "err.notSent") != 1 {
		t.Error("an error sendWithCaptcha throws for a request it did not send, before a send or in place of the " +
			"second, is no longer marked, or no longer answered by the submit handler, errorMessage for a form " +
			"off the screen and submitVerification, before any other error is worded")
	}
	// Including the second: a refusal of the answer that finds the browser
	// forgotten, the forget heard while the first send was on its way, is
	// answered as a request not sent, not with the refusal, which errorMessage
	// would word as the anti-spam check failing.
	inOrder(t, "sendWithCaptcha", send,
		"try { return await send(answer); } catch (err) {",
		"if (retried || !needsIt()) { throw err; } if (!onScreen()) { throw notSent(); } } } }",
	)
	stopped := jsString(t, script, "FORGOTTEN_BEFORE_SENDING")
	if !strings.Contains(stopped, "forgotten") || !strings.Contains(stopped, "nothing was posted") {
		t.Errorf("FORGOTTEN_BEFORE_SENDING should say the browser was forgotten and nothing was posted: %q", stopped)
	}
}

// "Forget this browser" pressed in one tab reaches every other educhan tab
// open in the browser: each cancels what it has on its way and drops what it
// holds itself, as the tab it was pressed in does, and draws /verify again if
// it shows it. A tab hears each forget once. It hears it from storage, as the
// credential emptied, which the pressing tab does last of what it clears and
// before it removes the credential: a tab's view of storage has, by the time
// it is told of a change, every change made before it, so at the emptying it
// has the forget whole. That matters because a tab that handled a
// confirmation or a post before it heard may have written the badge, the
// poster key or the name back. So each tab notes what it stores of those
// three, and on hearing removes each that its view of storage still holds as
// it stored it, and touches nothing else: not what it stored before the
// forget, and not a badge confirmed or a key made in another tab since,
// however late it hears. The message on the forget's channel can reach a tab
// before its view of storage has the forget, when what the view shows as the
// tab's own may have gone from storage, a badge verified or a key made since in
// its place: a tab that compared at the message removed it. So the message is
// the hearing only for a tab that storage tells nothing, and nothing is
// compared at it: a tab that cannot read storage acts on FORGET_MESSAGE, and
// every tab on FORGET_UNSTORED, which the pressing tab sends in its place when
// storage would not take the emptying. Both are constants that say nothing
// about anyone, only forgetBrowser sends them, one for each forget, and a tab
// acts on nothing else that arrives. No test here runs two tabs: what a
// browser's storage tells a tab, and when, is measured in the browser
// (the pull request's test plan).
func TestForgettingTheBrowserReachesEveryTab(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if !regexp.MustCompile(`\nconst FORGET_CHANNEL = "educhan:[a-z-]+";\n`).MatchString(code) ||
		!regexp.MustCompile(`\nconst FORGET_MESSAGE = "[a-z-]+";\n`).MatchString(code) ||
		!regexp.MustCompile(`\nconst FORGET_UNSTORED = "[a-z-]+";\n`).MatchString(code) ||
		jsString(t, script, "FORGET_MESSAGE") == jsString(t, script, "FORGET_UNSTORED") {
		t.Error("the forget channel's name or one of its two messages is no longer a constant, or the two are the same")
	}
	// One channel, opened once. Its listener acts on the two messages and
	// nothing else, and on FORGET_MESSAGE only where the tab cannot read
	// storage: a tab that can hears the forget from storage, and acting on the
	// message too would forget twice, the second time over what was done
	// since the first. It compares nothing at either.
	open := squash(withoutComments(jsFunction(t, script, "openForgetChannel")))
	if strings.Count(code, "new BroadcastChannel(") != 1 || !strings.Contains(code, "\nconst forgetChannel = openForgetChannel();\n") ||
		!strings.Contains(open, `if (typeof BroadcastChannel !== "function") { return null; } try { `+
			`const channel = new BroadcastChannel(FORGET_CHANNEL); channel.addEventListener("message", (event) => { `+
			`if (event.data === FORGET_UNSTORED || (event.data === FORGET_MESSAGE && !storageAnswers())) { `+
			`forgottenElsewhere(false); } }); return channel; } catch { return null; }`) {
		t.Error("openForgetChannel no longer opens the one channel, where there is one, or a tab acts on a message " +
			"where storage tells it of the forget, on something else that arrives, or compares what it stored at one")
	}
	answers := squash(withoutComments(jsFunction(t, script, "storageAnswers")))
	if answers != "function storageAnswers() { try { localStorage.getItem(CREDENTIAL_STORAGE_KEY); return true; } catch { return false; } }" ||
		strings.Count(code, "storageAnswers(") != 2 {
		t.Errorf("storageAnswers no longer says whether this tab can read storage, or is asked somewhere besides "+
			"the channel's listener: %s", answers)
	}
	// forgetBrowser, whole: what the page holds, then the key and the name out
	// of storage, then the credential emptied, the last of the three and what
	// the other tabs hear, before it is removed, then the message, which says
	// whether storage took the emptying. Emptied first, a tab would hear with
	// the key and the name still in its view of storage, find them as it
	// stored them, and remove a key made or a name stored in another tab
	// since. The only other postMessage in app.js gives the captcha's worker
	// its puzzle.
	forget := squash(withoutComments(jsFunction(t, script, "forgetBrowser")))
	if forget != "function forgetBrowser() { forgetOnThisPage(); "+
		"for (const key of [POSTER_KEY_STORAGE_KEY, NAME_STORAGE_KEY]) { try { localStorage.removeItem(key); } catch { } } "+
		"const emptied = emptyCredential(); forgetCredential(); "+
		"if (forgetChannel) { forgetChannel.postMessage(emptied ? FORGET_MESSAGE : FORGET_UNSTORED); } }" {
		t.Errorf("forgetBrowser no longer removes the key and the name, then empties the credential before it removes "+
			"it, then posts the message that says whether storage took the emptying: %s", forget)
	}
	// The emptying is a change, so that storage tells the other tabs of it: a
	// credential emptied already is removed first.
	empty := squash(withoutComments(jsFunction(t, script, "emptyCredential")))
	if empty != `function emptyCredential() { try { if (localStorage.getItem(CREDENTIAL_STORAGE_KEY) === "") { `+
		`localStorage.removeItem(CREDENTIAL_STORAGE_KEY); } localStorage.setItem(CREDENTIAL_STORAGE_KEY, ""); return true; } `+
		`catch { return false; } }` {
		t.Errorf("emptyCredential no longer empties the credential, removing one emptied already first, "+
			"and says whether storage took it: %s", empty)
	}
	if strings.Count(code, ".postMessage(") != 2 || strings.Count(code, "forgetChannel.postMessage(") != 1 ||
		strings.Count(withoutComments(jsFunction(t, script, "runCaptchaWorkers")), "worker.postMessage({") != 1 ||
		strings.Count(code, "forgetChannel") != 3 {
		t.Error("something besides forgetBrowser posts on the forget channel, or uses it")
	}
	// A tab that hears it from storage removes what it stored itself, only
	// where its view of storage still holds it as stored; one that hears it
	// from the message compares nothing. Either then forgets what it holds.
	elsewhere := squash(withoutComments(jsFunction(t, script, "forgottenElsewhere")))
	if elsewhere != `function forgottenElsewhere(byStorage) { if (byStorage) { for (const [key, value] of storedHere) { `+
		`if (storageGet(key) === value) { storageRemove(key); } } } forgetOnThisPage(); `+
		`if (onVerifyPage() && ageStatus() === "ok") { renderVerify().then((drawn) => { `+
		`if (drawn) { showNotice(BROWSER_FORGOTTEN_ELSEWHERE); } }); } }` {
		t.Errorf("a tab that hears the forget no longer removes only what it stored and its view of storage still "+
			"holds, and only when it hears from storage, forgets what it holds, or draws /verify again: %s", elsewhere)
	}
	// It is heard from storage in one place, the credential emptied, where the
	// tab stops before it treats the emptying as a change of badge, and from
	// the message in one, above.
	if !strings.Contains(squash(withoutComments(jsFunction(t, script, "followCredentialStorage"))),
		`{ return; } if (event.key === CREDENTIAL_STORAGE_KEY && event.newValue === "") { forgottenElsewhere(true); return; } `+
			`if (!readStoredBadge()) {`) ||
		strings.Count(code, "forgottenElsewhere(true)") != 1 || strings.Count(code, "forgottenElsewhere(false)") != 1 ||
		strings.Count(code, "storageRemove(") != 2 {
		t.Error("a tab no longer hears the forget from storage at the credential emptied, and only there, " +
			"or removes from storage somewhere besides what it stored itself")
	}
	// What it says on /verify is what happened, and that what the browser held
	// then is gone, not what the browser holds: a badge or a key stored since
	// the forget stays, whenever the tab hears, and the page drawn may show
	// that badge's panel under the notice. The words that say what the browser
	// holds are said only under the button, in the tab it was pressed in.
	said := regexp.MustCompile(`\nconst BROWSER_FORGOTTEN_ELSEWHERE =\s+"([^"\\]*)";\n`).FindStringSubmatch(code)
	if said == nil || !strings.Contains(said[1], "forgotten in another tab") || !strings.Contains(said[1], "held then") ||
		strings.Contains(said[1], "no longer holds") || strings.Contains(said[1], "holds no") {
		t.Errorf("BROWSER_FORGOTTEN_ELSEWHERE is %q; want it to say the browser was forgotten in another tab and that "+
			"what it held then is gone, and not what it holds: a badge stored since the forget stays", said)
	}
	if n := len(regexp.MustCompile(`\bBROWSER_FORGOTTEN\b`).FindAllString(code, -1)); n != 2 ||
		!strings.Contains(code, "done.textContent = BROWSER_FORGOTTEN;") {
		t.Errorf("app.js names BROWSER_FORGOTTEN %d times; want twice, the words and confirmBrowserForgotten, which "+
			"says them under the button: said in a tab that hears the forget, they can be untrue of a badge stored since", n)
	}
	if fn := withoutComments(jsFunction(t, script, "forgetOnThisPage")); strings.Contains(strings.ToLower(fn), "storage") ||
		strings.Contains(fn, "Credential(") {
		t.Error("forgetOnThisPage touches storage, which only forgetBrowser clears and only forgottenElsewhere compares")
	}
	remove := squash(withoutComments(jsFunction(t, script, "storageRemove")))
	if remove != "function storageRemove(key) { try { localStorage.removeItem(key); } catch { } }" {
		t.Errorf("storageRemove does more than remove the one key: %s", remove)
	}
	// What a tab notes it stored is exactly the credential, the poster key
	// and the name, each as it is written, in the one place each is written,
	// and nothing reads the note but forgottenElsewhere.
	if !strings.Contains(squash(withoutComments(jsFunction(t, script, "storeCredential"))),
		"localStorage.setItem(CREDENTIAL_STORAGE_KEY, token); storedHere.set(CREDENTIAL_STORAGE_KEY, token);") {
		t.Error("storeCredential no longer notes the credential it stores as it stores it")
	}
	// posterKey, whole: a key found in storage is used as it is, neither stored
	// again nor noted: another tab may have made it after a forget this tab has
	// not heard yet, and a key noted here is removed when the tab hears. Only a
	// key this page made is stored and noted.
	poster := squash(withoutComments(jsFunction(t, script, "posterKey")))
	if poster != "function posterKey() { try { const stored = localStorage.getItem(POSTER_KEY_STORAGE_KEY); "+
		"if (stored && POSTER_KEY_PATTERN.test(stored)) { return stored; } } catch { } "+
		"if (!pagePosterKey) { pagePosterKey = newPosterKey(); } "+
		"try { localStorage.setItem(POSTER_KEY_STORAGE_KEY, pagePosterKey); storedHere.set(POSTER_KEY_STORAGE_KEY, pagePosterKey); } "+
		"catch { } return pagePosterKey; }" {
		t.Errorf("posterKey no longer uses a key found in storage as it is, or no longer notes the key it makes "+
			"as it stores it: %s", poster)
	}
	submit := squash(withoutComments(submitHandler(t, script)))
	if !strings.Contains(submit, "storageSet(NAME_STORAGE_KEY, name); storedHere.set(NAME_STORAGE_KEY, name);") {
		t.Error("the submit handler no longer notes the name it stores as it stores it")
	}
	if !strings.Contains(code, "\nconst storedHere = new Map();\n") || strings.Count(code, "storedHere.set(") != 3 ||
		strings.Count(code, "storedHere") != 6 || strings.Count(code, "of storedHere)") != 1 || strings.Count(code, "storedHere.clear();") != 1 {
		t.Error("storedHere is written, read or cleared somewhere besides where the three are stored, forgottenElsewhere and forgetOnThisPage")
	}
	if strings.Count(code, "forgottenElsewhere(") != 3 || strings.Count(code, "forgetOnThisPage(") != 3 {
		t.Error("forgetOnThisPage or forgottenElsewhere is called from somewhere besides a forget")
	}
}

// "Forget this credential" pressed in one tab takes the badge out of every
// other educhan tab open in the browser too, and so does the server refusing
// the badge there as run out or replaced, or another address verified there:
// the tab it happened in changes the credential in storage, and every other
// tab, told by the storage event, reads it again from storage
// (readStoredBadge), so a badged post from any of them sends only what the
// browser still holds. A tab whose badge has changed draws the option again in
// every posting form on screen, for the badge it holds now, unticked, or for
// none, where the form would otherwise offer a badge the browser no longer
// holds, noting a form that had it ticked (withdrawBadgeOption), and in a held
// form a redraw has taken off the screen as it puts it back, and draws /verify
// again if it shows the badge panel, unless the reader has drawn another step
// there by the time the page has asked the server (renderVerify): its address
// step, which a reader may be filling in, is left as it is, as is one the
// reader moves to while the page asks, and so is a code step unless the badge
// read in is one newly verified there. The browser has one verification in
// progress, so that badge spent or replaced the code a code step with no
// confirmation on its way asks for: the step goes, and is drawn again, saying
// so, if it is on screen (codeSpentElsewhere). A confirmation on its way,
// counted on the step whether its form is still on screen or /verify has been
// drawn again over it, may have reached the server first and verified this
// tab's address: it is left to its answer, the step only marked, and a refusal
// of it, whatever it says, puts the step away then, drawing /verify again
// saying so if its code step is on screen (submitVerification). Verify requests
// go one at a time, so there is at most one such confirmation. A success draws
// the badge it earned as any success does. The notice says another tab
// verified an address and the code here no longer works, and not that the one
// is why. A code request still on its way is not a code step yet, and keeps its
// own. It acts on the credential's key, and on storage cleared, and on nothing
// else; the credential emptied is not a change of badge but the browser
// forgotten in another tab, heard there
// (TestForgettingTheBrowserReachesEveryTab).
func TestForgettingTheBadgeReachesEveryTab(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	forget := squash(jsBraced(t, code, `if (event.target.closest('[data-action="forget-verification"]')) {`))
	if !strings.Contains(forget, "event.preventDefault(); forgetCredential();") ||
		!strings.Contains(squash(withoutComments(jsFunction(t, script, "forgetCredential"))), "localStorage.removeItem(CREDENTIAL_STORAGE_KEY);") {
		t.Error("Forget this credential no longer takes the credential out of storage, which is what the other tabs hear")
	}
	follow := squash(withoutComments(jsFunction(t, script, "followCredentialStorage")))
	if follow != `function followCredentialStorage(event) { if (event.key !== CREDENTIAL_STORAGE_KEY && event.key !== null) { return; } `+
		`if (event.key === CREDENTIAL_STORAGE_KEY && event.newValue === "") { forgottenElsewhere(true); return; } `+
		`if (!readStoredBadge()) { return; } offerHeldBadge(); `+
		panelRedrawn+" }" {
		t.Errorf("a tab no longer reads the badge again when another changes it, or no longer draws the option to post "+
			"with it again, or draws /verify again in some other case: %s", follow)
	}
	read := squash(withoutComments(jsFunction(t, script, "readStoredBadge")))
	if read != `function readStoredBadge() { const offered = state.verification.badge; `+
		"state.verification.badge = state.verification.enabled ? readBadge() : null; const held = state.verification.badge; "+
		"const changed = held ? !(offered && held.token === offered.token) : offered !== null; "+
		"if (held && changed) { codeSpentElsewhere(); } return changed; }" ||
		strings.Count(code, "readStoredBadge(") != 3 {
		t.Errorf("the badge read again from storage no longer says whether it changed, or puts away a code step for one "+
			"newly verified elsewhere, or is read somewhere besides followCredentialStorage and a badged post's refusal: %s", read)
	}
	// A held form a redraw has taken off the screen, while a post link's
	// lookup draws its skeleton say, misses the change, so as it is put back
	// its option is drawn again for the badge the page holds, noted as another
	// tab's change notes it, but starting no puzzle, since drawing a page never
	// does. Without this the thread the lookup resolves to offers the badge
	// that has gone.
	inOrder(t, "restoreDraft", squash(withoutComments(jsFunction(t, script, "restoreDraft"))),
		"const forgets = Math.max(draft.forgets, forgottenAt.get(held) ?? 0);",
		"if (forgets !== browserForgets) { forgetForm(held); } else { withdrawBadgeOption(held, true); } form.replaceWith(held);",
	)
	if strings.Count(code, `addEventListener("storage",`) != 1 ||
		!strings.Contains(code, "\nwindow.addEventListener(\"storage\", followCredentialStorage);\n") ||
		strings.Count(code, "followCredentialStorage") != 2 {
		t.Error("followCredentialStorage is no longer the one thing a tab does when another changes storage, or is called from elsewhere")
	}
	spent := squash(withoutComments(jsFunction(t, script, "codeSpentElsewhere")))
	if spent != `function codeSpentElsewhere() { if (verifyStep.step !== "code") { return; } `+
		`if (verifyStep.confirming > 0) { verifyStep.spentElsewhere = true; return; } `+
		`verifyStep = { step: "email", label: "" }; const step = el.main.querySelector('form[data-verify="confirm"]'); `+
		`if (onVerifyPage() && ageStatus() === "ok" && step) { `+
		`renderVerify().then((drawn) => { if (drawn) { showNotice(CODE_SPENT_ELSEWHERE); } }); } }` ||
		strings.Count(code, "codeSpentElsewhere(") != 3 || strings.Count(code, `data-verify="confirm"`) != 2 {
		t.Errorf("a code step drawn before another tab verified a badge is no longer put away, or drawn again saying so "+
			"only while on screen, or a confirmation of it on its way is no longer left to its answer, or the step is "+
			"put away from somewhere besides readStoredBadge and that confirmation's refusal: %s", spent)
	}
	// The confirmation left to its answer settles the step: a success is drawn
	// as any success is (TestARequestGoesOnlyFromAFormOnScreen), and a refusal of
	// any kind, the form released, puts the step away as it would have been,
	// before the refusal is worded, and whether or not the reader is on
	// /verify. What tells codeSpentElsewhere a confirmation is on its way is a
	// count on the step it confirms, not its form, which /verify drawn again
	// since, the reader back from another page say, has taken off the screen:
	// it goes up before the request is sent and down when it is answered,
	// however that ends.
	verify := squash(withoutComments(jsFunction(t, script, "submitVerification")))
	inOrder(t, "submitVerification", verify,
		`sentFrom.confirming = (sentFrom.confirming || 0) + 1; let issued; try { issued = await api("/api/verify/confirm",`,
		"} finally { sentFrom.confirming -= 1; } if (verifyStep !== sentFrom) return; storeCredential(issued.credential);",
		"} catch (err) { unhold(false); if (verifyStep !== sentFrom) return; if (sentFrom.spentElsewhere) { codeSpentElsewhere(); return; } "+
			"const ended =",
	)
	// Each of those redraws, and Forget this credential's own, draws only while
	// the reader is on /verify, and leaves a step the reader has drawn there
	// meanwhile as it is, by Verify another address or Use a different address
	// say, or by a forget with its notice (renderVerify, held whole by
	// TestVerifyStepIsDrawnForAReaderArriving).
	if !strings.Contains(forget, `verifyStep = { step: "email", label: "" }; renderVerify().then((drawn) => {`) ||
		strings.Count(squash(code), panelRedrawn) != 2 {
		t.Error("a redraw of /verify for a change of the badge no longer draws the page again, or one draws it that should not")
	}
	if store := squash(withoutComments(jsFunction(t, script, "storeCredential"))); !strings.HasSuffix(store,
		"storedHere.set(CREDENTIAL_STORAGE_KEY, token); return true; } catch { showNotice(BADGE_NOT_STORED); return false; } }") {
		t.Errorf("storeCredential no longer says whether it stored the badge, and says so on the page when it could not: %s", store)
	}
	if strings.Count(code, ".confirming") != 4 {
		t.Error("a confirmation on its way is counted somewhere besides submitVerification, or the count is read " +
			"somewhere besides codeSpentElsewhere")
	}
	if strings.Count(code, "spentElsewhere") != 2 || strings.Count(code, "verifyStep.spentElsewhere = true;") != 1 ||
		strings.Count(code, "if (sentFrom.spentElsewhere) {") != 1 {
		t.Error("a code step is marked as left to its confirmation somewhere besides codeSpentElsewhere, or the mark " +
			"is read somewhere besides that confirmation's refusal")
	}
	if got := jsString(t, script, "CODE_SPENT_ELSEWHERE"); !strings.Contains(got, "verified in another tab") ||
		!strings.Contains(got, "no longer works") || strings.Contains(got, "so the code") || strings.Contains(got, "because") {
		t.Errorf("CODE_SPENT_ELSEWHERE = %s, want it to say an address was verified in another tab and the code no "+
			"longer works, and not that the one is why: a confirmation from the tab may have spent the code itself", got)
	}
	// What followCredentialStorage and a badged post's refusal look for is the
	// badge panel's own button, and only the panel draws it; a pending code
	// step is drawn whatever badge is held.
	view := squash(withoutComments(jsFunction(t, script, "verifyViewHTML")))
	panel := `if (v.badge && !verifyStep.replacing) {`
	codeAt := strings.Index(view, `if (verifyStep.step === "code") {`)
	if !strings.Contains(view, panel) || strings.Count(view, `data-action="forget-verification"`) != 1 ||
		strings.Index(view, `data-action="forget-verification"`) < strings.Index(view, panel) ||
		codeAt < 0 || codeAt > strings.Index(view, panel) ||
		strings.Count(code, `data-action="forget-verification"`) != 5 {
		t.Error("the badge panel is no longer the one part of /verify that draws Forget this credential, or a pending code " +
			"step is put away for the panel when a badge is held")
	}
}

// A ticked badge taken away, by another tab forgetting, refusing or replacing
// it, by this tab replacing it or refusing it for a post from another page, or
// for the form's own post while the form is off the screen, or by a press
// finding the page holds another badge or none
// (TestAFormSendsOnlyTheBadgeItWasDrawnFor), is not quietly turned into a post
// without it: the option is drawn again, unticked, for the badge the page holds
// now or none (drawBadgeOption), the form is noted (badgeWithdrawn) with the
// badge that went, whether it was idle or its badged post was on its way, and
// its next press is refused once, saying so, naming the badge the form now
// offers, if any, with the focus on its box, and saying what the press after it
// does, and starting the puzzle that press needs on a form with something
// written. A reply then goes out under the same ID as the reader's posts in the
// thread without a badge, and the notice says that only while storage holds the
// poster key it held when the badge went. A post that goes through with its
// badge takes the note off, and so does the reader ticking or unticking the
// badge the form offers; Forget this browser takes it off too
// (TestForgettingTheBrowserCancelsWhatIsOnItsWay). A redraw carries a ticked
// badge in the draft as the badge it was ticked for, and ticks the new form's
// box only if that is the badge it offers: a new form offering none, or
// another, is noted instead, so a badge verified in another tab meanwhile never
// goes out unticked by the reader. And a badged post refused as expired or
// revoked forgets that credential only if storage still holds it: one stored
// meanwhile, in another tab or by a confirmation in this one, is not the one
// refused, and, unless the browser was forgotten since the press, a form on
// screen is told that its badge was replaced by one verified since in this
// browser, not the server's "verify again", which would cost another code and
// replace the badge the browser holds. That notice is the form's one refusal
// for the badge, so a note left on it while the post was on its way goes, as
// does one left on the form before that notice is worded again, in place or by
// a redraw for the form it draws
// (TestARefusalOnScreenIsWordedAgainForWhatChanges,
// TestCaptchaRefusalSaysWhatTheFormStillHolds), and, unless the post is left
// with nothing in it, neither text nor image, it says what the next press does
// and how to post with the new badge.
func TestABadgeTakenFromUnderAFormIsSaidAtItsNextPress(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if !strings.Contains(code, "\nconst badgeWithdrawn = new WeakMap();\n") ||
		strings.Count(code, "badgeWithdrawn.set(") != 2 || strings.Count(code, "badgeWithdrawn.delete(") != 7 {
		t.Error("badgeWithdrawn is no longer a WeakMap noted only by withdrawBadgeOption and restoreDraft, and " +
			"forgotten only by the press it refuses, a post that goes, a refusal that says its badge was replaced, " +
			"that notice worded again, the badge box changed, a forget and a kept page's failed " +
			"redraw that says its form's badge was refused")
	}
	// The puzzle it then starts is TestCaptchaIsAskedForOnlyWhenSomeoneWrites'.
	withdraw := squash(withoutComments(jsFunction(t, script, "withdrawBadgeOption")))
	if withdraw != `function withdrawBadgeOption(form, startNothing) { const offered = badgeOffered(form); `+
		`const key = storageGet(POSTER_KEY_STORAGE_KEY) || ""; const ticked = drawBadgeOption(form); `+
		`if (ticked) { badgeWithdrawn.set(form, { number: offered, key }); } `+
		`if (badgeOffered(form) !== offered) { rewordRefusal(form); } `+
		`if (ticked && !startNothing && captchaFormNeedsIt(form) && captchaFormHasWriting(form)) { prepareCaptcha(); } }` {
		t.Errorf("withdrawBadgeOption no longer notes a ticked box, with the badge it was drawn for and the poster key "+
			"storage holds, as its option is drawn again, or starts the puzzle the form then needs: %s", withdraw)
	}
	// drawBadgeOption says whether the box it drew over was ticked, and only
	// then is the form noted.
	if draw := squash(withoutComments(jsFunction(t, script, "drawBadgeOption"))); !strings.Contains(draw,
		`const box = slot.querySelector('input[name="badge"]'); const ticked = Boolean(box && box.checked);`) ||
		!strings.HasSuffix(draw, "return ticked; }") || strings.Count(draw, "return") != 2 {
		t.Errorf("drawBadgeOption no longer says whether the box it drew over was ticked: %s", draw)
	}
	// Another tab's change notes it, through offerHeldBadge; a forget takes
	// the note off and notes nothing.
	if !strings.Contains(withoutComments(jsFunction(t, script, "followCredentialStorage")), "offerHeldBadge(") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "offerHeldBadge")), "withdrawBadgeOption(form, startNothing);") {
		t.Error("followCredentialStorage no longer notes a form whose ticked badge it takes away")
	}
	if forget := withoutComments(jsFunction(t, script, "forgetOnThisPage") + jsFunction(t, script, "forgetForm")); strings.Contains(forget, "withdrawBadgeOption") ||
		!strings.Contains(forget, "badgeWithdrawn.delete(form);") {
		t.Error("forgetOnThisPage notes a form whose badge it takes away, or leaves a note another tab's change made")
	}
	// The reader's own tick or untick of the badge on offer supersedes the
	// note, which is about a badge ticked before it.
	if !strings.Contains(squash(code), `if (event.target.matches('input[type="checkbox"][name="badge"]')) { `+
		"badgeWithdrawn.delete(event.target.form); badgeToggled(event.target);") {
		t.Error("ticking or unticking the badge no longer takes a form's note off")
	}

	submit := squash(withoutComments(submitHandler(t, script)))
	inOrder(t, "the submit handler", submit,
		"if (!body.trim() && !file) {",
		"if (badgeWithdrawn.has(form)) { const withdrawn = badgeWithdrawn.get(form); badgeWithdrawn.delete(form); "+
			"refuseUnsent(form, badgeNoteWords(withdrawn), Boolean(body.trim()), Boolean(file)); "+
			`const box = field(form, "badge"); if (box) box.focus(); `+
			"if (captchaFormNeedsIt(form) && captchaFormHasWriting(form)) { prepareCaptcha(); } return; }",
		`const wantsBadge = data.get("badge") === "on"; const held = wantsBadge ? state.verification.badge : null;`,
		"const release = holdForm(form);",
		"form.reset(); badgeWithdrawn.delete(form);",
		"} catch (err) {",
	)
	if strings.Count(submit, "badgeWithdrawn.") != 5 {
		t.Error("the submit handler reads or forgets badgeWithdrawn somewhere besides the press it refuses, a post that goes " +
			"and a refusal that says its badge was replaced")
	}
	if got := jsString(t, script, "BADGE_GONE"); got != "Your badge is no longer available in this browser — verify again to post with it." {
		t.Errorf("BADGE_GONE = %s, want the wording a press with the badge ticked and none held has always had", got)
	}
	notice := squash(withoutComments(jsFunction(t, script, "badgeWithdrawnNotice")))
	if notice != `function badgeWithdrawnNotice(form, { key }) { const held = state.verification.badge; `+
		"const gone = held ? `The badge you ticked for this post has gone from this browser, and the one this form now offers, "+
		"Verified: ${held.label}, is not ticked.` : BADGE_GONE; return `${gone} ${pressedWithoutBadge(form, key)}`; }" {
		t.Errorf("badgeWithdrawnNotice no longer says the badge has gone, as the page now stands, and what the next press does: %s", notice)
	}
	if press := squash(withoutComments(jsFunction(t, script, "pressedWithoutBadge"))); press != "function pressedWithoutBadge(form, key) { "+
		`const sameID = form.dataset.kind === "reply" && key !== "" && key === storageGet(POSTER_KEY_STORAGE_KEY); `+
		`return sameID ? "Pressing again posts this without a badge, under the same ID as the posts you make in this thread without one." `+
		`: "Pressing again posts this without a badge."; }` || strings.Count(code, "pressedWithoutBadge(") != 3 {
		t.Errorf("pressedWithoutBadge no longer says what the next press does, naming the ID only for a reply while the key is "+
			"the one the reader's posts in the thread had, or is called from somewhere besides the two badge notices: %s", press)
	}

	draft := squash(withoutComments(jsFunction(t, script, "formDraft")))
	if !strings.Contains(draft, "const ticked = badgeWithdrawn.get(form) || "+
		"(badge && badge.checked ? { number: badgeOffered(form) } : null);") ||
		!strings.Contains(draft, "badge: ticked,") {
		t.Error("formDraft no longer reads a ticked badge as the badge it was ticked for, or a form noted in " +
			"badgeWithdrawn as ticked for the badge that went, with its key")
	}
	restore := squash(withoutComments(jsFunction(t, script, "restoreDraft")))
	if !strings.Contains(restore, `const badge = field(form, "badge"); `+
		"const ticked = forgotten ? null : draft.badge; "+
		"const same = Boolean(ticked && badge && ticked.number === badgeOffered(form)); "+
		"if (badge) badge.checked = same; if (ticked && !same && !(unsaid && unsaid.badgeRefused)) { badgeWithdrawn.set(form, "+
		`{ number: ticked.number, key: ticked.key ?? (storageGet(POSTER_KEY_STORAGE_KEY) || "") }); }`) {
		t.Error("restoreDraft no longer ticks the badge only when the new form offers the one its draft was ticked for, " +
			"noting the new form otherwise, unless the draft was read before a forget, or notes it when the form's notice, " +
			"said as it goes back, refuses its badge")
	}

	// A badged post refused as expired or revoked forgets the credential it
	// sent only while storage holds it. A form on screen is told by the
	// refusal's notice. One off the screen, which the refusal tells nothing
	// then (TestARequestGoesOnlyFromAFormOnScreen), keeps how to word that
	// notice, which a redraw that carries it, held or still owed the notice,
	// words and says as it puts its values back into its page or keeps that
	// page, and is noted, starting nothing, so that a press before then is
	// refused once, as is the next press of the form a redraw draws in its
	// place after a press refused as empty has dropped the notice: without
	// either, that press posts the reply without its badge, unsaid, under the
	// ID the reader's other posts there without one share. A form whose badge
	// storage has replaced since the press, the browser not forgotten
	// meanwhile, is told so in place of the server's wording, which says to
	// verify again, on screen at once or, off it, by the redraw that tells it,
	// if the browser has not been forgotten by then either, as a form off the
	// screen is too when storage holds no badge at the answer but one by the
	// time the redraw tells it; and a note left on it while the post was on its
	// way goes, whatever left it (a change of badge in another tab, a
	// confirmation answered in this one, another badged post's refusal in this
	// one, /verify loading over the form, or a redraw that puts it back), so
	// that notice is its one refusal for the badge.
	refused := submit[max(strings.Index(submit, "} catch (err) {"), 0):]
	if !strings.Contains(refused, `if (held && (reason === "expired" || reason === "revoked")) { `+
		"if (forgets === browserForgets && storedCredential() === held.token) { forgetCredential(); badgeLost =") ||
		!strings.Contains(refused, `: "That badge was replaced by a newer verification."; } `+
			"else if (forgets === browserForgets) { readStoredBadge(); } "+
			"replaced = forgets === browserForgets && state.verification.badge !== null; offerHeldBadge(false, form); ") ||
		!strings.Contains(refused, panelRedrawn+` if (shown) { drawBadgeOption(form); if (replaced) { badgeWithdrawn.delete(form); } `+
			`if (captchaFormNeedsIt(form) && captchaFormHasWriting(form)) { prepareCaptcha(); } } `+
			`else { withdrawBadgeOption(form, true); } }`) {
		t.Error("a badged post refused as expired or revoked forgets a credential other than the one it sent, " +
			"or no longer takes the option out of its form, or out of a form on the page the reader went to, " +
			"or no longer starts the puzzle the post it goes on to needs on a form on screen, or no longer notes a form " +
			"off the screen, starting nothing, or no longer drops a note from a form on screen told its badge was replaced")
	}
	inOrder(t, "the submit handler", refused,
		"const imageOff = release(shown);",
		`const reason = err.payload && err.payload.credential; let replaced = false; if (held && (reason === "expired" || reason === "revoked")) {`,
		"replaced = forgets === browserForgets && state.verification.badge !== null;",
		`const badgeRefused = Boolean(held) && (reason === "expired" || reason === "revoked");`,
		"const replacedNow = () => (replaced || (badgeRefused && state.verification.badge !== null)) && !forgotten(); "+
			"const said = (off, on, text, image) => stoppedByForget ? FORGOTTEN_BEFORE_SENDING : replacedNow() ? "+
			"badgeReplacedNotice(on, { hasText: text, hasImage: image, imageOff: off }) : "+
			"errorMessage(err, { hasText: text, hasImage: image, imageOff: off });",
		"if (!shown) {",
	)
	if strings.Count(submit, "replaced = ") != 2 || strings.Count(code, "badgeReplacedNotice(") != 2 {
		t.Error("the submit handler decides a badge was replaced somewhere this test does not read, or badgeReplacedNotice is " +
			"called from somewhere besides it")
	}
	replacedNotice := squash(withoutComments(jsFunction(t, script, "badgeReplacedNotice")))
	// It reads the badge held, the box of the form it is given, the poster key
	// and the text and image when it is worded, which is when it is said: on
	// screen at the answer, by a redraw for a form answered off the screen, and
	// again by a redraw that takes the image off, for the form it draws
	// (restoreDraft): so with none held by then, forgotten since, it ends with
	// what pressing again does, and offers no box to tick, and with the new
	// badge's box ticked since, it says that pressing again posts with that
	// badge, not to tick it.
	if replacedNotice != "function badgeReplacedNotice(form, { hasText, hasImage, imageOff }) { "+
		`if (!hasText && (imageOff !== "" || !hasImage)) { return BADGE_REPLACED; } const held = state.verification.badge; `+
		`const box = field(form, "badge"); if (held && box && box.checked && badgeOffered(form) === held.number) { `+
		"return `${BADGE_REPLACED} The box for the new badge, Verified: ${held.label}, is ticked, so pressing again "+
		"posts this with that badge.`; } "+
		`const press = pressedWithoutBadge(form, storageGet(POSTER_KEY_STORAGE_KEY) || ""); `+
		"if (!held) { return `${BADGE_REPLACED} ${press}`; } "+
		"return `${BADGE_REPLACED} ${press} To post it with the new badge, Verified: ${held.label}, tick its box.`; }" {
		t.Errorf("badgeReplacedNotice no longer says what the next press does, with the poster key storage holds now, "+
			"and how to post with the new badge the form now offers, or says it of a post left with nothing, or reads "+
			"a badge forgotten since, or a box ticked since: %s", replacedNotice)
	}
	if got := jsString(t, script, "BADGE_REPLACED"); !strings.Contains(got, "replaced by one verified since in this browser") ||
		!strings.Contains(got, "nothing was posted") || strings.Contains(got, "verify again") {
		t.Errorf("BADGE_REPLACED = %s, want it to say the badge was replaced by one verified since in this browser and "+
			"nothing was posted, and not to verify again", got)
	}
	// The form refused offers the new badge by then, its box drawn again
	// unticked (drawBadgeOption), so ticking it there is all the notice asks.
	if !strings.Contains(refused, "if (shown) { drawBadgeOption(form);") {
		t.Error("a form told its badge was replaced no longer offers the new one, which badgeReplacedNotice tells the reader to tick")
	}
}

// A posting form's badge box is drawn for the badge the page holds as it is
// drawn: its slot names that badge (badgeOffered), written by formHTML as
// renderIndex or renderThread draws the page, and by drawBadgeOption as it
// draws the box again. A press sends the credential only while the page still
// holds that badge: a box ticked for one badge never sends another, stored
// since in this tab or another. A press that finds the page holding another
// badge, or none, draws the option again as for a badge taken from under the
// form (withdrawBadgeOption), and is refused once with the note
// (TestABadgeTakenFromUnderAFormIsSaidAtItsNextPress). And wherever the page's
// badge changes while a posting form may be on screen, the forms there are
// brought into line with it there and then (offerHeldBadge), as for another
// tab's change (followCredentialStorage): a confirmation answered in this tab
// after the reader left /verify, the server switching the feature off or the
// badge running out as /verify loads (loadVerification), and a badged post
// refused as expired or revoked, for the forms on the page the reader went to
// meanwhile, and for /verify there, drawn again where it shows that badge's
// panel. Forget this browser takes the option out itself (forgetOnThisPage),
// and Forget this credential and Keep my badge are pressed on /verify, which
// draws no posting form.
func TestAFormSendsOnlyTheBadgeItWasDrawnFor(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if strings.Count(code, "badgeOffered") != 7 || strings.Count(code, `form.querySelector("[data-badge-option]")`) != 2 ||
		strings.Count(code, "[data-badge-option]") != 2 {
		t.Error("the badge a form's box was drawn for is read somewhere besides badgeOffered's callers (formDraft, " +
			"withdrawBadgeOption, a press, a redraw putting a held form back and badgeReplacedNotice) and drawBadgeOption")
	}
	// Written in the same template as the form, so the badge the slot names is
	// the one formHTML drew the box for.
	for _, view := range []string{"renderIndex", "renderThread"} {
		fn := squash(withoutComments(jsFunction(t, script, view)))
		inOrder(t, view, fn, "el.main.innerHTML = `", "${formHTML({")
	}
	if strings.Count(code, "formHTML(") != 3 {
		t.Error("a posting form is drawn somewhere besides renderIndex and renderThread")
	}

	// The press: the box's badge, or none, before anything reads the credential,
	// with nothing between that lets another task change the badge.
	submit := squash(withoutComments(submitHandler(t, script)))
	check := `const badgeBox = field(form, "badge"); const offered = state.verification.badge; ` +
		`if (badgeBox && badgeBox.checked && !(offered && badgeOffered(form) === offered.number)) { ` +
		`withdrawBadgeOption(form, false); }`
	read := `const wantsBadge = data.get("badge") === "on"; const held = wantsBadge ? state.verification.badge : null;`
	inOrder(t, "the submit handler", submit, check, "if (badgeWithdrawn.has(form)) {", read,
		"const headers = held ? { [CREDENTIAL_HEADER]: held.token } : { [POSTER_KEY_HEADER]: posterKey() };")
	// Past those, it reads whether storage holds a badge only to word the
	// refusal of the one sent: at the answer (replaced), and for a form off the
	// screen by the time a redraw tells it (said).
	wording := []string{"replaced = forgets === browserForgets && state.verification.badge !== null;",
		"(replaced || (badgeRefused && state.verification.badge !== null))"}
	if from, to := strings.Index(submit, check), strings.Index(submit, read); from < 0 || to < from ||
		strings.Contains(submit[from:to], "await") || strings.Count(submit, "state.verification.badge") != 2+len(wording) ||
		strings.Count(submit, wording[0]) != 1 || strings.Count(submit, wording[1]) != 1 {
		t.Error("the submit handler reads the badge to send somewhere besides the press's check, or lets another task " +
			"in between the check and the read, or reads the badge held for something besides a refusal's wording")
	}

	// Every change of the page's badge while a form may be on screen.
	verify := squash(withoutComments(jsFunction(t, script, "submitVerification")))
	if !strings.Contains(verify, "state.verification.badge = readBadge(); offerHeldBadge(); "+
		`verifyStep = { step: "email", label: "" };`) {
		t.Error("a confirmation answered in this tab no longer brings the forms on screen into line with the badge it stored")
	}
	load := squash(withoutComments(jsFunction(t, script, "loadVerification")))
	inOrder(t, "loadVerification", load,
		"state.verification = { answered, enabled, badge: enabled ? readBadge() : null };", "offerHeldBadge(true);")
	// A badged post refused as expired or revoked brings the forms on the page
	// the reader went to into line with the badge held now
	// (TestABadgeTakenFromUnderAFormIsSaidAtItsNextPress), and draws /verify
	// there again where it shows the panel for that badge, as another tab's
	// change does, so the page that says why a badge went (badgeLost) says it,
	// and no tab goes on showing a badge the browser no longer holds.
	refused := submit[max(strings.Index(submit, "} catch (err) {"), 0):]
	if !strings.Contains(refused, "offerHeldBadge(false, form); "+panelRedrawn+" if (shown) {") {
		t.Error("a badged post refused as expired or revoked no longer brings the other forms on screen into line, " +
			"or no longer draws /verify again where it shows that badge's panel")
	}
	if strings.Count(code, "state.verification.badge = ") != 5 || strings.Count(code, "state.verification = ") != 1 ||
		strings.Count(code, "readBadge()") != 5 || strings.Count(code, "forgetCredential();") != 7 ||
		strings.Count(code, "readStoredBadge(") != 3 || strings.Count(code, "offerHeldBadge(") != 5 ||
		strings.Count(code, "withdrawBadgeOption(") != 5 {
		t.Error("the page's badge changes somewhere this test does not read, or a form's option is drawn again from " +
			"somewhere it does not, which may leave a form on screen offering a badge the page no longer holds")
	}
}

// From the press until the answer, a post's, a code request's or a
// confirmation's form is held: its controls switched off, once it has been read
// and checked, so nothing the reader changes meanwhile (a badge ticked, an
// address corrected, a line added) is left out of what is sent, or wiped by the
// reset after it. A refusal switches back on what was switched off, and nothing
// else, before the recovery that may switch the picker off for good. A post's
// refusal gives the focus that switching off took back to the form's button,
// unless the reader has put it somewhere since, without scrolling a reader who
// has scrolled away from the form back to it; a Verify refusal puts it in the
// field it is about instead (sayVerifyRefusal). A held form takes nothing added
// from outside it, such as a quote, or a dropped image, whose drop on a picker
// only the hold switched off leaves the notice on screen as it was; a badge box
// drawn into it meanwhile is drawn switched off, and switched on with the rest;
// and app.css shows the held controls switched off.
func TestAPendingFormIsHeld(t *testing.T) {
	script := appScript(t)
	hold := squash(withoutComments(jsFunction(t, script, "holdForm")))
	for _, want := range []string{
		"const controls = Array.from(form.elements).filter((control) => !control.disabled);",
		"for (const control of controls) { control.disabled = true; } heldForms.set(form, controls);",
		"return (refocus) => { heldForms.delete(form); for (const control of controls) { control.disabled = false; }",
	} {
		if !strings.Contains(hold, want) {
			t.Errorf("holdForm no longer switches off exactly the controls that were on, and back: missing %s", want)
		}
	}
	if strings.Count(hold, "form.elements") != 1 || strings.Count(hold, ".disabled = false") != 1 {
		t.Error("holdForm switches on controls it did not switch off")
	}
	// A badge box drawn into a held form, for a badge stored meanwhile in
	// another tab or by a confirmation in this one, say (drawBadgeOption), is
	// drawn switched off and kept with the controls holdForm switched off, so
	// release switches it on with them: what is sent was read at the press, and
	// a box ticked meanwhile would show a badge the post does not carry. A box
	// taken out, the badge forgotten say, stays out. Nothing else adds to what
	// release switches on, and no box drawn since the hold began is left on
	// while it lasts.
	code := withoutComments(script)
	if draw := squash(withoutComments(jsFunction(t, script, "drawBadgeOption"))); !strings.Contains(draw,
		`slot.innerHTML = badgeOptionHTML(held); const drawn = slot.querySelector('input[name="badge"]'); `+
			"if (drawn && heldForms.has(form)) { drawn.disabled = true; heldForms.get(form).push(drawn); }") ||
		strings.Count(code, "heldForms.get(") != 1 || strings.Count(code, "heldForms.set(") != 1 ||
		strings.Count(code, "slot.innerHTML") != 1 || strings.Count(code, "badgeOptionHTML(") != 3 {
		t.Errorf("a badge box drawn into a held form is no longer drawn switched off and switched on by release, "+
			"or something else adds to what release switches on: %s", draw)
	}
	// Switching off the focused control leaves the focus nowhere; a posting
	// form that had it at the press, refused while on screen, has it back on
	// its button, unless the reader moved it during the wait: then it stays
	// where they put it. A form given it is noted (refocusedForms), so that a
	// redraw loading meanwhile gives it to the button of the form it draws in
	// this one's place; a /verify form, which no redraw carries, is released
	// without the refocus (below). The page does not scroll to the button: a
	// reader who scrolled away meanwhile, which leaves the focus lost, stays
	// where they are, whether or not that is the top of the page, where the
	// notice is.
	inOrder(t, "holdForm", hold,
		"const focused = form.contains(document.activeElement);",
		"control.disabled = true;",
		"const lost = !document.activeElement || document.activeElement === document.body; "+
			`if (refocus && focused && lost) { form.querySelector('button[type="submit"]')?.focus({ preventScroll: true }); `+
			"if (form.dataset.kind) { refocusedForms.add(form); } }",
	)
	if strings.Count(hold, ".focus(") != 1 {
		t.Error("holdForm moves the focus somewhere besides the one refocus, which is for a focus that was lost")
	}
	if n := strings.Count(withoutComments(script), "= holdForm(form);"); n != 2 {
		t.Errorf("holdForm(form) is called %d times; want twice, in the submit handler and submitVerification", n)
	}

	submit := squash(withoutComments(submitHandler(t, script)))
	verify := squash(withoutComments(jsFunction(t, script, "submitVerification")))
	// The hold starts once the form has been read and checked, and goes
	// straight into the try whose catch releases it: a check that refused after
	// the hold, or any return between the hold and the try, would leave the
	// form switched off for good, and a hold before the file is read would read
	// the picker as switched off and post without the image. A Verify form's
	// buttons are the Verify page's to switch off, before the hold, and on
	// again where its refusal is said
	// (TestVerifyAnswersBelongToTheStepTheyWereSentFrom); its catch releases
	// the rest first thing, without the refocus.
	held := "const release = holdForm(form); button.textContent = sending; clearNotice(); try {"
	inOrder(t, "the submit handler", submit, "const data = new FormData(form);",
		"const file = image && !image.disabled && image.files[0];", "if (!body.trim() && !file) {", "if (badgeWithdrawn.has(form)) {",
		held, "sendWithCaptcha(", "} catch (err) {", "release(shown);")
	verifyHeld := "for (const b of buttons) b.disabled = true; const unhold = holdForm(form);"
	inOrder(t, "submitVerification", verify, "const data = new FormData(form);",
		"if (requesting && !(adult && adult.checked)) {",
		verifyHeld, "try {", "sendWithCaptcha(", `await api("/api/verify/confirm"`, "} catch (err) { unhold(false);")
	if from, to := strings.Index(verify, verifyHeld), strings.Index(verify, "try {"); from < 0 || to < from ||
		strings.Contains(verify[from:to], "return") {
		t.Error("submitVerification can return between holding its form and the try whose catch releases it")
	}
	// The button is held with the rest, and released on a refusal, and for a
	// post also after a success, once the form is emptied, so that a redraw
	// does not carry it as still held (TestAHeldFormSurvivesARedrawOfItsPage);
	// after a code request's or a confirmation's success the page is replaced.
	for name, fn := range map[string]string{"the submit handler": submit, "submitVerification": verify} {
		want, release := 2, "release("
		if name == "submitVerification" {
			want, release = 1, "unhold("
		}
		if strings.Count(fn, release) != want || strings.Contains(fn, "button.disabled") {
			t.Errorf("%s switches its controls on somewhere besides its refusal and, for a post, its success, or its button apart from them", name)
		}
	}
	inOrder(t, "the submit handler", submit, "sendWithCaptcha(", "form.reset();", "clearFileField(fileField); } release(false);", "} catch (err) {")
	// The picker switched off at a press for a refusal answered off the screen
	// is off before holdForm reads which controls to switch off, so release
	// leaves it off too.
	holding, catching := strings.Index(submit, "const release = holdForm(form);"), strings.Index(submit, "} catch (err) {")
	caught := submit[max(catching, 0):]
	if blocked, released := strings.Index(caught, "blockRefusedPicker("), strings.Index(caught, "release(shown);"); blocked < released ||
		holding < 0 || catching < holding || strings.Contains(submit[holding:catching], "blockRefusedPicker(") ||
		strings.Contains(submit, "blockFileField(") {
		t.Error("the submit handler switches its controls back on after switching the picker off, which turns the picker back on")
	}

	if quote := withoutComments(jsFunction(t, script, "quoteTarget")); !strings.Contains(quote, `textarea[name="body"]:not(:disabled)`) {
		t.Error("quoteTarget writes a quote into a reply box held while its post is pending")
	}
	if set := squash(withoutComments(jsFunction(t, script, "setFile"))); !strings.Contains(set,
		`if (input.disabled) { if (field.classList.contains("blocked")) { showNotice(field.querySelector(".file-blocked").textContent); } return false; }`) {
		t.Error("setFile takes a file into a picker held while its post is pending, or says nothing for a blocked one")
	}
	// A drop clears what was said before it only once the form has taken the
	// file. setFile takes it and says whether it did, and says why itself when
	// it refuses one because the picker is blocked or the file is not an
	// accepted image; any other refusal, such as a picker switched off only
	// while its post is pending, no file, or a browser that cannot build a
	// DataTransfer or set input.files, says nothing and leaves the notice on
	// screen. setFile neither shows the field nor clears the notice: the field
	// is shown by the drop handler, as by the picker's own change, and by
	// restoreDraft putting a draft's image back, neither of which clears a
	// notice before the take.
	dropped := squash(withoutComments(jsBlock(t, script, `document.addEventListener("drop", (event) => {`, "});")))
	inOrder(t, "the drop handler", dropped, `field.classList.remove("dragging");`,
		`if (setFile(field, event.dataTransfer.files[0])) { clearNotice(); updateFileField(field); `)
	if set := squash(withoutComments(jsFunction(t, script, "setFile"))); strings.Count(dropped, "clearNotice(") != 1 ||
		strings.Contains(set, "clearNotice(") || strings.Contains(set, "updateFileField(") ||
		strings.Count(withoutComments(script), "updateFileField(") != 5 ||
		!strings.Contains(squash(withoutComments(jsFunction(t, script, "restoreDraft"))),
			"else if (!refusal && fileField && draft.file && setFile(fileField, draft.file)) { updateFileField(fileField);") {
		t.Error("a drop clears the notice before the form has taken its file, or setFile shows the field itself, " +
			"or the field is shown somewhere besides clearFileField, the picker's change, the drop and restoreDraft")
	}
	drag := squash(withoutComments(jsBlock(t, script, `document.addEventListener("dragover", (event) => {`, "});")))
	if !strings.Contains(drag, `if (field && !field.querySelector(".file-input").disabled) { field.classList.add("dragging"); }`) {
		t.Error("a picker held while its post is pending is still shown as a place to drop an image")
	}

	// A held form looks held: a box, a button, a tick box or a picker that
	// will not take a tap does not look ready to.
	css, err := fs.ReadFile(webFS(), "app.css")
	if err != nil {
		t.Fatalf("reading app.css: %v", err)
	}
	styles := squash(string(css))
	for _, rule := range []string{
		`.fields input[type="text"]:disabled, .fields input[type="email"]:disabled, .fields select:disabled, ` +
			`.fields textarea:disabled { color: var(--muted); background: var(--panel); cursor: progress; }`,
		`.fields .btn:disabled { opacity: 0.6; cursor: progress; }`,
		`.fields label:has(> input:disabled) { opacity: 0.6; cursor: progress; }`,
		`.file-input:disabled + .file-button { opacity: 0.6; cursor: progress; }`,
	} {
		if !strings.Contains(styles, rule) {
			t.Errorf("app.css no longer shows a held form's controls switched off: missing %s", rule)
		}
	}
}

// Pressing Update while a post is on its way redraws the thread, and a redraw
// replaces the form. The post belongs to the page, so the form it went from is
// carried across as itself, still held, and put in the new page in place of the
// one drawn there; the post goes ahead and is answered on the form the reader
// sees. A copy of its text in a live form would show a post that went as one
// not sent, and post it twice on the next press. So that the held form stays in
// the document meanwhile, a redraw of its own page keeps the page, dimmed,
// rather than drawing a skeleton over it, and one that then fails to load
// leaves the page there, as it was drawn, with the error in a notice above it
// rather than in its place, unless a newer render has begun, which has the
// screen. A held form that has been answered by the time the page is drawn is
// read like any draft, so one whose post went is drawn empty. A form answered
// off the screen before the redraw began, and not told yet what it missed, is
// carried held as well, so that the redraw tells it
// (TestARequestGoesOnlyFromAFormOnScreen). So is a form pressed while the
// redraw loads, its page still on screen, though the redraw read its values
// before the press: read as the draft stands when it is used, by the skeleton's
// timer, a failed load and restoreDraft (carriedDraft), it is carried held
// whatever became of the press, so a post is answered on it, as if pressed just
// before the redraw began, and a copy of what it sent is not left in the new
// form. A press refused then before anything is sent, an empty one say, is
// carried held too, so a redraw of its page by route key keeps the page for it,
// with no skeleton, and a failed load keeps it, while the same press made just
// before the redraw began has the form read for its values, and the page is not
// kept for it. Its page is told by routeKey, which leaves out the index page,
// so another page of the same board's index carries a held new-thread form as
// Update does. And when the hold ends, success or refusal, release leaves its
// picker as the new page drew one: a redraw that drew it switched off, the
// thread having reached its image limit meanwhile, has the held form's switched
// off with the same reason, rather than offering an image to a thread that
// takes no more, and the image it held taken off, which release reports for the
// notice to say (TestCaptchaRefusalSaysWhatTheFormStillHolds); one that drew it
// live, the limit raised meanwhile, has it switched on, though it was switched
// off at the press; and one that drew none, on a site that takes no images, has
// it switched off with the site's reason. A refusal of the image itself, the
// image limit reached or uploads switched off, then switches it off
// (blockRefusedPicker): the submit handler does that for a form on screen, and
// for one answered off the screen a page that a failed redraw keeps for it
// does, while a page drawn afresh draws its picker as the server says.
func TestAHeldFormSurvivesARedrawOfItsPage(t *testing.T) {
	script := appScript(t)
	if !strings.Contains(withoutComments(script), "\nconst heldForms = new WeakMap();\n") {
		t.Fatal("app.js no longer keeps the held forms in heldForms")
	}
	if key := squash(withoutComments(jsFunction(t, script, "routeKey"))); key !=
		"function routeKey(route) { return `${route.view}:${route.slug || \"\"}:${route.no || \"\"}`; }" {
		t.Errorf("routeKey is no longer the view, the board and the thread, without the index page: %s", key)
	}
	// A draft is keyed by the page on screen, which the render that drew it
	// noted, never by a render begun since that has not drawn: the second of a
	// double click would carry this page's form into the other page's. A form
	// is carried as itself while it is held, or has something it was not told
	// off the screen: read for its values instead, it would be redrawn with
	// that left unsaid, the focus lost, and an image a refusal took off gone
	// without a word.
	capture := squash(withoutComments(jsFunction(t, script, "captureDraft")))
	inOrder(t, "captureDraft", capture,
		"const key = routeKey(drawnFor.route || state.route);",
		`const form = el.main.querySelector("form[data-kind]");`,
		"return unsent ? { key, open: true, body: unsent.value } : null;",
		"if (heldForms.has(form) || unsaidNotices.has(form)) { return { key, held: form, forgets: browserForgets }; }",
		"return { key, ...formDraft(form), from: form, pressed: presses.get(form) ?? 0 };",
	)
	if strings.Count(capture, "routeKey(") != 1 || strings.Count(capture, "key") != 4 {
		t.Error("captureDraft keys a draft by something besides the page on screen")
	}
	// A draft read for its values notes the form and how often it had been
	// pressed, and every press of a posting form counts, so a press made while
	// a redraw loads has the redraw carry the form held after all.
	if carried := squash(withoutComments(jsFunction(t, script, "carriedDraft"))); carried != "function carriedDraft(draft) { "+
		"if (!draft || !draft.from || (presses.get(draft.from) ?? 0) === draft.pressed) { return draft; } "+
		"return { key: draft.key, held: draft.from, forgets: draft.forgets }; }" {
		t.Errorf("carriedDraft no longer carries held a form pressed since its values were read: %s", carried)
	}
	if code := withoutComments(script); !strings.Contains(code, "\nconst presses = new WeakMap();\n") ||
		strings.Count(code, "presses") != 5 || strings.Count(code, "carriedDraft(") != 6 ||
		!strings.Contains(squash(withoutComments(submitHandler(t, script))), "presses.set(form, (presses.get(form) ?? 0) + 1);") {
		t.Error("presses is no longer counted at every press and read only by captureDraft and carriedDraft, or carriedDraft " +
			"is called somewhere besides render's keepPage, the notice it keeps as it begins, its failed load, its " +
			"Verify page and its restoreDraft")
	}
	// The held form goes into the new page's <details>, which another page of
	// the index draws closed, so that is opened: a form on its way is never
	// folded away out of sight.
	restore := squash(withoutComments(jsFunction(t, script, "restoreDraft")))
	inOrder(t, "restoreDraft", restore,
		"if (!draft || draft.key !== routeKey(state.route)) { if (draft && unsentOnScreen(draft.held)) { clearNotice(); } return; }",
		`const form = el.main.querySelector("form[data-kind]");`,
		"if (draft.held) { const held = draft.held; const forgets = Math.max(draft.forgets, forgottenAt.get(held) ?? 0); "+
			"if ( heldForms.has(held) && form && "+
			"form.dataset.kind === held.dataset.kind && form.dataset.thread === held.dataset.thread ) { "+
			`if (held.querySelector("[data-file-field]")) { redrawnPickers.set(held, pickerRefusal(form)); } `,
		`form.replaceWith(held); const details = held.closest("details"); if (details) details.open = true;`,
		"return; } answeredForm = heldForms.has(held) ? null : held; draft = { key: draft.key, ...formDraft(held), forgets }; }",
		"if (!form) { if (unsentOnScreen(answeredForm)) { clearNotice(); } keepUnsentDraft(draft); return; }",
	)
	if refusal := squash(withoutComments(jsFunction(t, script, "pickerRefusal"))); refusal != "function pickerRefusal(form) { "+
		`const drawn = form.querySelector("[data-file-field]"); if (!drawn) { return UPLOADS_OFF_REASON; } `+
		`return drawn.classList.contains("blocked") ? drawn.querySelector(".file-blocked").textContent : ""; }` {
		t.Errorf("pickerRefusal no longer says why a form just drawn takes no image, by its picker or the site's: %s", refusal)
	}
	// What the redraw drew is applied when the hold ends, after the controls
	// the hold switched off are switched back on, and by release, which both
	// the success and the refusal call (TestAPendingFormIsHeld): a picker
	// switched off at the press is not among those controls, so a picker drawn
	// live is switched on here, and left as a live one is drawn.
	inOrder(t, "holdForm", squash(withoutComments(jsFunction(t, script, "holdForm"))),
		"return (refocus) => { heldForms.delete(form); for (const control of controls) { control.disabled = false; }",
		`let imageOff = ""; if (redrawnPickers.has(form)) { const reason = redrawnPickers.get(form); `+
			`const picker = form.querySelector("[data-file-field]"); redrawnPickers.delete(form); `+
			`if (reason) { if (picker.querySelector(".file-input").files.length > 0) { imageOff = reason; } `+
			"blockFileField(picker, reason); } else { unblockFileField(picker); } }",
		"return imageOff; }; }",
	)
	if n := strings.Count(withoutComments(script), "redrawnPickers"); !strings.Contains(withoutComments(script),
		"\nconst redrawnPickers = new WeakMap();\n") || n != 5 {
		t.Errorf("redrawnPickers appears %d times in app.js; want its declaration, restoreDraft's set, and release's has, get and delete", n)
	}
	if unblock := squash(withoutComments(jsFunction(t, script, "unblockFileField"))); unblock != "function unblockFileField(field) { "+
		`const input = field.querySelector(".file-input"); const note = field.querySelector(".file-blocked"); `+
		`input.disabled = false; note.textContent = ""; note.hidden = true; field.classList.remove("blocked"); }` {
		t.Errorf("unblockFileField no longer leaves a picker as a live one is drawn, holding what it held: %s", unblock)
	}
	if n := strings.Count(withoutComments(script), "unblockFileField("); n != 2 {
		t.Errorf("unblockFileField is called %d times in app.js; want once, in release", n-1)
	}
	// A page keeps its thread and its open image until something is drawn in
	// its place, the skeleton, the age screen, an error or a new page, whatever
	// loads meanwhile, so a refusal answered while a redraw loads updates the
	// page on screen, after a post link's lookup too, and Escape closes the
	// image open there. A render a newer one has overtaken draws nothing,
	// loaded or not, and one that draws notes what the page was drawn for
	// (drawnFor), which a kept page whose redraw will not load gets back, with
	// the error as a notice, after what its form, answered off the screen, was
	// not told (TestARequestGoesOnlyFromAFormOnScreen), never in place of the
	// page: after two redraws that fail, the page is still the one drawn, not
	// the first redraw's half-begun one. A refusal of that form's image
	// switches its picker off there, and puts the thread kept with the page in
	// step, as on screen (blockRefusedPicker); the page's thread is that
	// form's, the draft being keyed by the page the form is on. The address bar
	// goes back to that page's address too, the history entry the reader is on
	// replaced, or a jump within the page afterwards would find an address the
	// page was not drawn for and draw the page that failed. Every render begun,
	// the age screen's and the rules' included, ends the loading state of any
	// before it (below). A post link's lookup goes on to the thread unless the
	// reader went somewhere else meanwhile, which a jump within the page,
	// moving only the fragment, is not: the address is compared less its
	// fragment, or a skip link pressed during the lookup would leave the
	// skeleton on screen for good.
	render := squash(withoutComments(jsFunction(t, script, "render")))
	inOrder(t, "render", render,
		"async function render({ draft = captureDraft() } = {}) {",
		"const requested = window.location.pathname + window.location.search; renderedPath = requested; "+
			"const route = parseRoute(); state.route = route; "+
			"rendersBegun += 1; endLoading(); "+
			"const keepPage = () => { const carried = carriedDraft(draft); "+
			"return Boolean(carried && carried.held && carried.key === routeKey(route)); }; renderNav(route);",
		"leavePage(); renderAgeGate(age); return; }",
		"const finish = beginLoading({ skeleton: () => !keepPage() });",
		"await loadSite();",
		"if (overtaken(route)) { return; } renderNav(route);",
		"} catch (err) { if (overtaken(route)) { return; }",
		"draft = carriedDraft(draft); if (keepPage() && draft.held.isConnected) { renderedPath = drawnFor.path; "+
			`window.history.replaceState(window.history.state, "", drawnFor.path + window.location.hash); `+
			"state.route = drawnFor.route; "+
			"state.thread = drawnFor.thread; const unsaid = takeUnsaidNotice(draft.held); "+
			"if (unsaid && unsaid.badgeRefused) { badgeWithdrawn.delete(draft.held); } "+
			"if (unsaid && unsaid.imageRefused) { blockRefusedPicker(draft.held, unsaid.limitReached); } "+
			"const { text, image } = formHolds(draft.held); "+
			`const words = unsaid ? unsaid.word(draft.held, text, image) : ""; const said = noticeSaid(); `+
			`const told = [words, said, message].filter(Boolean).join(" "); showNotice(told); `+
			"const refused = refusalNotices.get(draft.held); "+
			`if (refused && (unsaid || (said !== "" && said === refused.told))) { `+
			`const after = [unsaid ? said : refused.after, message].filter(Boolean).join(" "); `+
			"refusalNotices.set(draft.held, { ...refused, told, after }); } "+
			"if (unsaid && unsaid.focused && (!document.activeElement || document.activeElement === document.body)) { "+
			`draft.held.querySelector('button[type="submit"]')?.focus({ preventScroll: true }); } return; }`,
		"leavePage(); el.main.innerHTML = emptyStateHTML(message, board);",
		"} finally {",
		"if (!overtaken(route) && window.location.pathname + window.location.search === requested) { "+
			`window.history.replaceState(null, "", resolved); await render({ draft }); } return; }`,
		"if (overtaken(route)) { return; } "+
			`if (route.view !== "thread") { state.thread = null; } drawnFor = { path: renderedPath, route, thread: state.thread }; `+
			"expandedLink = null; enhancePosts(el.main); restoreDraft(carriedDraft(draft));",
	)
	// Nothing else forgets the page's thread or its open image: leavePage, at
	// the skeleton, the age screen and an error, and the page drawn in its
	// place, which sets both for itself, renderThread noting its thread, and
	// drawVerifyStep forgetting both for every /verify it draws, after a forget
	// say.
	if leave := squash(withoutComments(jsFunction(t, script, "leavePage"))); leave !=
		"function leavePage() { state.thread = null; expandedLink = null; }" {
		t.Errorf("leavePage no longer forgets the page's thread and its open image: %s", leave)
	}
	if code := withoutComments(script); strings.Count(code, "leavePage()") != 4 || strings.Count(render, "leavePage();") != 2 ||
		strings.Count(code, "state.thread = ") != 5 || strings.Count(render, "state.thread = ") != 2 ||
		strings.Count(code, "expandedLink = ") != 6 || strings.Count(render, "expandedLink = ") != 1 {
		t.Error("a page's thread or open image is forgotten, or set, somewhere besides leavePage, the page render " +
			"draws, a kept page's failed redraw, renderThread, the /verify drawVerifyStep draws and the image's own " +
			"expand and collapse")
	}
	// The address bar put back is half of what keeps the page. The other half
	// is popstate, which Back and a jump within the page fire: it draws nothing
	// when the address, less its fragment, is the one the page was drawn for
	// (renderedPath), and only moves to the fragment. The first Back after a
	// kept link, and every jump within the kept page, rely on it: a popstate
	// that always rendered would load the kept page's own address again (after
	// Update, that of the page that failed too) and, while that will not load,
	// draw its error over the kept page, its draft and its notice.
	pop := squash(withoutComments(jsBlock(t, script, `window.addEventListener("popstate", () => {`, "});")))
	if pop != `window.addEventListener("popstate", () => { if (window.location.pathname + window.location.search === renderedPath) { `+
		"focusHash(); return; } render(); });" || strings.Count(withoutComments(script), `"popstate"`) != 1 {
		t.Errorf("popstate no longer only moves to the fragment when the address is the one the page was drawn for: %s", pop)
	}
	if n := strings.Count(render, "requested"); n != 3 || strings.Contains(render, "location.href") {
		t.Errorf("requested appears %d times in render; want it taken, less the fragment, noted as renderedPath "+
			"and compared once", n)
	}
	if n := strings.Count(render, "keepPage"); n != 4 {
		t.Errorf("keepPage appears %d times in render; want its declaration, the notice it keeps as it begins, the "+
			"skeleton and the failed load", n)
	}
	if over := squash(withoutComments(jsFunction(t, script, "overtaken"))); over !=
		"function overtaken(route) { return state.route !== route; }" {
		t.Errorf("overtaken no longer says whether a newer render has begun: %s", over)
	}
	if code := withoutComments(script); !strings.Contains(code, "\nlet drawnFor = { path: null, route: null, thread: null };\n") ||
		strings.Count(code, "drawnFor") != 10 {
		t.Error("drawnFor is no longer only declared, noted by a render that draws and by drawVerifyStep, read by captureDraft, " +
			"the submit handler and submitVerification and put back, with the address bar, by a kept page's failed redraw")
	}
	for _, view := range []string{"renderIndex", "renderCatalog", "renderArchive", "renderThread", "renderVerify"} {
		fn := squash(withoutComments(jsFunction(t, script, view)))
		steps := []string{" = await api(", "if (overtaken(route)) { return; }", "el.main.innerHTML = "}
		switch view {
		case "renderThread":
			steps = []string{" = await api(", "if (overtaken(route)) { return; }", "state.thread = detail;", "el.main.innerHTML = "}
		case "renderVerify":
			steps = []string{"async function renderVerify() { const step = verifyStep; const route = state.route; " +
				"await loadVerification(); if (!onVerifyPage() || overtaken(route)) return false;", "drawVerifyStep();"}
		}
		inOrder(t, view, fn, steps...)
	}
	if draw := squash(withoutComments(jsFunction(t, script, "drawVerifyStep"))); draw != "function drawVerifyStep() { "+
		"el.main.innerHTML = verifyViewHTML(); verifyShown = { step: verifyStep, node: el.main.firstElementChild, at: performance.now() }; "+
		"state.thread = null; expandedLink = null; "+
		`if (onVerifyPage()) { drawnFor = { path: renderedPath, route: state.route, thread: null }; } badgeLost = ""; }` {
		t.Errorf("drawVerifyStep no longer forgets the page's thread and its open image, or notes what it drew for, "+
			"and only on the Verify page: %s", draw)
	}
	// /verify is drawn for the page on screen as renderVerify is called, once
	// the server has answered, only while /verify is still that page and no
	// newer render has begun since (overtaken): by render, by the confirmation,
	// and after a forget, pressed here or heard from another tab, a badge
	// another tab took away or a post here had refused, or a code step another
	// tab's verification spent. A notice is said only where it drew.
	code := withoutComments(script)
	squashed := squash(code)
	for _, call := range []string{
		`forgetOnThisPage(); if (onVerifyPage() && ageStatus() === "ok") { ` +
			"renderVerify().then((drawn) => { if (drawn) { showNotice(BROWSER_FORGOTTEN_ELSEWHERE); } }); }",
		`const step = el.main.querySelector('form[data-verify="confirm"]'); if (onVerifyPage() && ageStatus() === "ok" && step) { ` +
			"renderVerify().then((drawn) => { if (drawn) { showNotice(CODE_SPENT_ELSEWHERE); } }); }",
		`verifyStep = { step: "email", label: "" }; renderVerify().then((drawn) => { const lost = drawn && document.getElementById("badge-lost");`,
		"forgetBrowser(); renderVerify().then((drawn) => { if (drawn) confirmBrowserForgotten(); });",
		"if (!onVerifyPage()) return; if (await renderVerify()) showHeldBadge();",
	} {
		if strings.Count(squashed, call) != 1 {
			t.Errorf("app.js no longer draws /verify, saying what it says only where it drew, with %s", call)
		}
	}
	if strings.Count(code, "renderVerify(") != 9 || strings.Count(squashed, panelRedrawn) != 2 ||
		!strings.Contains(render, `case "verify": await renderVerify();`) {
		t.Error("renderVerify is called somewhere this test does not read")
	}
	// The loading state is the latest render's: beginning one ends any before
	// it, and a render's skeleton and its finish act only while it is the
	// latest, so an overtaken render neither blanks the newer page nor takes
	// the dimming off one still loading. overtaken would not do: a kept page's
	// failed redraw overtakes itself. The skeleton is drawn only over the page
	// dimmed as the load began, too: a page drawn over it since, /verify drawn
	// again for a forget heard from another tab say, is not blanked.
	loading := squash(withoutComments(jsFunction(t, script, "beginLoading")))
	inOrder(t, "beginLoading", loading,
		`function beginLoading({ skeleton = () => true } = {}) { const begun = rendersBegun; el.main.setAttribute("aria-busy", "true"); `+
			"const dimmed = el.main.firstChild; loadingTimer = setTimeout(() => { "+
			"if (begun === rendersBegun && el.main.firstChild === dimmed && skeleton()) { el.main.innerHTML =",
		"</div>'; leavePage(); } }, 250); return () => { if (begun === rendersBegun) { endLoading(); } }; }",
	)
	if strings.Count(loading, "el.main.innerHTML") != 1 {
		t.Error("beginLoading draws something besides its skeleton")
	}
	if end := squash(withoutComments(jsFunction(t, script, "endLoading"))); end !=
		`function endLoading() { clearTimeout(loadingTimer); el.main.removeAttribute("aria-busy"); }` {
		t.Errorf("endLoading no longer takes the dimming and the skeleton to come off the page: %s", end)
	}
	if !strings.Contains(code, "\nlet rendersBegun = 0;\n") || !strings.Contains(code, "\nlet loadingTimer = 0;\n") ||
		strings.Count(code, "rendersBegun") != 5 || strings.Count(code, "rendersBegun += 1;") != 1 ||
		strings.Count(code, "loadingTimer") != 3 || strings.Count(code, "endLoading(") != 3 || strings.Count(code, `"aria-busy"`) != 2 {
		t.Error("the loading state is started, ended or counted somewhere besides beginLoading, endLoading and render's start")
	}
	if n := strings.Count(code, "beginLoading("); n != 2 {
		t.Errorf("beginLoading is called %d times in app.js; want once, in render", n-1)
	}
}

// The FAQ's account of the check is the page's: the puzzle starts when focus
// moves into a box to write in, which quoting a post does too, and a press
// before it is solved waits with the button saying what the FAQ says it says,
// on /verify in the address step drawn again meanwhile too, which reads it from
// the mark submitVerification sets on the step while the puzzle is solved and
// takes off as the request goes.
func TestTheCaptchaFAQSaysWhatThePageDoes(t *testing.T) {
	script := appScript(t)
	at := strings.Index(script, `id: "faq-check",`)
	if at < 0 {
		t.Fatal(`app.js has no FAQ entry "faq-check"; this test reads it`)
	}
	answer := script[at:]
	answer = answer[strings.Index(answer, "answer: `")+len("answer: `"):]
	answer = squash(answer[:strings.Index(answer, "`")])
	for _, want := range []string{
		"the puzzle starts when you move into a box to write in, quote a post or attach an image,",
		"If the puzzle hasn't finished when you press the button, the button says <strong>Checking…</strong> until it has.",
	} {
		if !strings.Contains(answer, want) {
			t.Errorf("the FAQ's answer on the anti-spam check no longer says %q", want)
		}
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "insertQuote")), "textarea.focus();") {
		t.Error("quoting a post no longer moves the focus into the reply box, which is what starts the puzzle")
	}
	if !strings.Contains(submitHandler(t, script), `button.textContent = solving ? "Checking…" : sending;`) {
		t.Error("the submit handler no longer says Checking… on its button while it waits for the puzzle")
	}
	code := withoutComments(script)
	if !strings.Contains(code, "\nconst VERIFY_SOLVING = \"Checking…\";\n") ||
		!strings.Contains(squash(withoutComments(jsFunction(t, script, "submitVerification"))),
			"(solving) => { sentFrom.solving = solving; button.textContent = solving ? VERIFY_SOLVING : VERIFY_SENDING; },") ||
		!strings.Contains(code, `${busy ? (verifyStep.solving ? VERIFY_SOLVING : VERIFY_SENDING) : "Send code"}`) {
		t.Error("submitVerification, or the address step drawn again while it waits, no longer says Checking… on its " +
			"button while it waits for the puzzle, and Sending… once the request goes")
	}
}

// A refused post keeps what its form still holds, and the notice says so in the
// terms of what is there once the hold has ended, never of what was pressed:
// text in the box, or only an image, or nothing, when a redraw while the post
// was held drew its picker switched off and release took the image off, or, for
// a notice a redraw words for a form answered off the screen, when the reader
// has since left it neither, its text erased, say. Taking the image off,
// release returns the picker's reason, and every notice after it but the two
// refusals of the image itself, which say so in their own words, adds that the
// image has been taken off and why, and tells a post left with nothing to write
// something instead; a refusal of the thread itself (archived, locked, gone)
// adds only that the image was taken off, since no post can go there. A refusal
// answered before the redraw has drawn ends the same: restoreDraft words its
// notice again with the picker's reason, as it would have read had the redraw
// come first, or, after a refusal of the image itself answered off the screen,
// whose notice says already why the image went, takes off an image attached
// since with nothing added, so the refusal is said once. So a captcha refusal
// or a connection lost says "Your image is still attached" only of an image
// that is, a code request's connection lost, said in its step drawn again, says
// nothing of a box the reader did not write in (kept), and an archived thread
// is not answered with "Text replies still work".
func TestCaptchaRefusalSaysWhatTheFormStillHolds(t *testing.T) {
	script := appScript(t)
	message := squash(withoutComments(jsFunction(t, script, "errorMessage")))
	if !strings.HasPrefix(message, "function errorMessage(err, "+
		`{ hasText = true, hasImage = true, imageOff = "", action = "post", kept = true } = {}) { `+
		`const nothingLeft = !hasText && (imageOff !== "" || !hasImage);`) {
		t.Errorf("errorMessage no longer tells a post left with nothing from one that kept its text or its image: %.200s", message)
	}
	branch := jsBraced(t, message, "if (err.status === 403 && err.payload && err.payload.captcha) {")
	// A code request refused so says no code was sent, never that nothing
	// was posted: a reader asking for a code posted nothing.
	forCode := regexp.MustCompile(`^if \(err\.status === 403 && err\.payload && err\.payload\.captcha\) \{ ` +
		`if \(action === "code"\) \{ return "([^"]*)"; \}`).FindStringSubmatch(branch)
	if forCode == nil || !strings.Contains(forCode[1], "no code was sent") || strings.Contains(forCode[1], "post") {
		t.Errorf("errorMessage no longer tells a code request the captcha refused that no code was sent: %s", branch)
	}
	m := regexp.MustCompile(`if \(nothingLeft\) \{ return "([^"]*)"; \} return hasText \? "([^"]*)" : "([^"]*)";`).FindStringSubmatch(branch)
	if m == nil {
		t.Fatalf("errorMessage no longer words a post the captcha refused by what its form still holds: %s", branch)
	}
	if !strings.Contains(m[1], "nothing was posted") || strings.Contains(m[1], "text") || strings.Contains(m[1], "image") ||
		strings.Contains(m[1], "again") {
		t.Errorf("a refused post left with nothing should hear only that nothing was posted: %q", m[1])
	}
	if !strings.Contains(m[2], "Your text is still in the box") || strings.Contains(m[3], "text") || !strings.Contains(m[3], "image") {
		t.Errorf("a refused post with text should hear its text is in the box, and one without, that its image is: %q, %q", m[2], m[3])
	}
	// The other wordings that say what is still there say it only of what is.
	for _, want := range []string{
		"if (err.unreachable) { return nothingLeft || !kept ? UNREACHABLE : hasText ? `${UNREACHABLE} What you wrote is still in the box.` : " +
			"`${UNREACHABLE} Your image is still attached.`; }",
		`if (err.status === 413) { return imageOff ? "That image was too large." : imageTooLargeMessage(); }`,
		`const kept = hasText ? " Your text is still in the box." : "";`,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("errorMessage no longer words this for what the form still holds: missing %s", want)
		}
	}
	taken := squash(withoutComments(jsFunction(t, script, "imageTakenOff")))
	if taken != "function imageTakenOff(message, reason, { hasText, isThread, closed = false }) { if (!reason) { return message; } "+
		"const said = /[.!?]$/.test(message) ? message : `${message}.`; "+
		"if (closed) { return `${said} The image has been taken off your post.`; } "+
		`const next = hasText ? "" : isThread ? " Write something to post text instead." : " Write a reply to post text instead."; `+
		"return `${said} The image has been taken off your post. ${reason}${next}`; }" {
		t.Errorf("imageTakenOff no longer says the image was taken off and, unless the thread takes no posts, why and what a post "+
			"left with nothing can do: %s", taken)
	}
	// The submit handler reads the text from the box after release, for a
	// notice it says then, and adds what release took off to every notice but
	// the image's own refusals; a notice worded later, by a redraw, is given
	// the text and image the form holds then
	// (TestARequestGoesOnlyFromAFormOnScreen).
	submit := squash(withoutComments(submitHandler(t, script)))
	refused := submit[max(strings.Index(submit, "} catch (err) {"), 0):]
	inOrder(t, "the submit handler", refused,
		"const imageOff = release(shown);",
		`const box = field(form, "body"); const hasText = Boolean(box && box.value.trim()); const hasImage = Boolean(file); `+
			"const limitReached = err.status === 403 && err.message === IMAGE_LIMIT_REACHED; "+
			`const closed = err.status === 404 || (err.status === 403 && (err.message === "thread is archived" || `+
			`err.message === "thread is locked")); `+
			`const badgeRefused = Boolean(held) && (reason === "expired" || reason === "revoked"); `+
			"const stoppedByForget = err.notSent && forgotten(); "+
			"const replacedNow = () => (replaced || (badgeRefused && state.verification.badge !== null)) && !forgotten(); "+
			"const said = (off, on, text, image) => stoppedByForget ? FORGOTTEN_BEFORE_SENDING : replacedNow() ? "+
			"badgeReplacedNotice(on, { hasText: text, hasImage: image, imageOff: off }) : "+
			"errorMessage(err, { hasText: text, hasImage: image, imageOff: off }); "+
			"const notice = (off, on = form, text = hasText, image = hasImage) => "+
			"imageTakenOff(said(off, on, text, image), off, { hasText: text, isThread, closed });",
		awayFromScreen,
		"showNotice(imageTakenOff(FORGOTTEN_BEFORE_SENDING, imageOff, { hasText, isThread }));",
		"if (limitReached || uploadsOff) { blockRefusedPicker(form, limitReached); "+
			"showNotice(errorMessage(err, { hasText, imageOff })); return; } "+
			"const told = notice(imageOff); "+
			`refusalNotices.set(form, { notice, replaced: replacedNow, off: imageOff, told, after: "" }); showNotice(told); } });`,
	)
	// A refusal of the image itself switches the picker off with its reason,
	// and puts the thread on screen in step with what it taught: on screen
	// here, and for a form answered off the screen on the page a failed redraw
	// keeps for it (TestAHeldFormSurvivesARedrawOfItsPage) or at the form's
	// next press, if that comes first, unless an image has been attached since,
	// which is left for the server to answer
	// (TestARequestGoesOnlyFromAFormOnScreen), and nowhere else, since a page
	// drawn afresh draws both from what the server says by then.
	code := withoutComments(script)
	if block := squash(withoutComments(jsFunction(t, script, "blockRefusedPicker"))); block !=
		"function blockRefusedPicker(form, limitReached) { "+
			`const fileField = form.querySelector("[data-file-field]"); const thread = state.thread; `+
			"if (limitReached) { if (fileField) { blockFileField(fileField, imageLimitReason(thread && thread.board)); } "+
			"if (thread) { thread.at_image_limit = true; if (thread.board && thread.board.image_limit > 0) { "+
			"thread.images = Math.max(Number(thread.images) || 0, thread.board.image_limit); } } } "+
			"else if (fileField) { blockFileField(fileField, UPLOADS_OFF_REASON); } redrawThreadCounts(); }" ||
		strings.Count(code, "blockRefusedPicker(") != 4 {
		t.Errorf("blockRefusedPicker no longer switches the picker off with the refusal's reason and redraws the thread's "+
			"counts, or is called besides on screen, on a kept page and at the next press: %s", block)
	}
	if strings.Count(refused, "showNotice(") != 3 || strings.Count(refused, "body.trim()") != 0 ||
		strings.Count(code, "imageTakenOff(") != 5 {
		t.Error("the submit handler words a refusal somewhere besides the three notices this test reads, or from the text pressed, " +
			"or something besides it and restoreDraft says an image was taken off")
	}
	// A held form answered while a redraw of its page loaded, refused before
	// the redraw drew, is read like any draft, and an image the new page takes
	// none of is taken off, its notice saying so as it would have, had the
	// redraw come first, rather than giving way to the picker's reason: the
	// refusal's own notice, still on screen in the words it was told, or for a
	// form answered off the screen in those the redraw has just told it, worded
	// again with that reason by the wording the submit handler kept, and any
	// other with the image's going added, but for the notice of a refusal of
	// the image itself answered off the screen, which says so already: an image
	// attached since is taken off with nothing added, as a page kept for the
	// form takes it off, so the refusal is said once. The notice worded again
	// reads the text put back into the new form, so a post emptied since, its
	// image taken off, is told only what went and to write something; and the
	// notice of a replaced badge reads the new form's box too, and as that
	// form's one refusal for the badge takes off a note the draft gave it, for
	// a box ticked on the kept page and taken out since with its badge say, so
	// the next press does what the notice says.
	restore := squash(withoutComments(jsFunction(t, script, "restoreDraft")))
	inOrder(t, "restoreDraft", restore,
		"let answeredForm = null;",
		"answeredForm = heldForms.has(held) ? null : held; draft = { key: draft.key, ...formDraft(held), forgets }; }",
		"const said = noticeSaid(); "+
			"const unchanged = Boolean(refused && said) && said === (unsaid ? words : refused.told); "+
			`const fileField = form.querySelector("[data-file-field]"); const refusal = answeredForm && draft.file ? pickerRefusal(form) : ""; `+
			"if (refusal && unsaid && unsaid.imageRefused) { } "+
			"else if (refusal && !unchanged) { showNotice(said ? imageTakenOff(said, refusal, "+
			`{ hasText: text, isThread: form.dataset.kind === "thread" }) : refusal); } `+
			"else if (!refusal && fileField && draft.file && setFile(fileField, draft.file)) {",
		"if (unchanged) { "+
			`refusalNotices.set(form, { ...refused, off: refusal || refused.off, told: said, after: "" }); rewordRefusal(form); }`,
	)
	if !strings.Contains(code, "\nconst refusalNotices = new WeakMap();\n") || strings.Count(code, "refusalNotices") != 13 {
		t.Error("refusalNotices is no longer only declared, read and kept by rewordRefusal, read by unsentOnScreen, kept by " +
			"refuseUnsent, taken out at every press and kept by the submit handler on screen or off it, and read and moved to " +
			"the form drawn or kept by restoreDraft, which reads it again for the focus, and by a kept page's failed redraw")
	}
}

// A refusal a posting form was told, while it is still the notice on screen, is
// worded again in place when what it speaks of changes before the next press:
// the form's badge box drawn again for another badge or none, on a change of
// badge in another tab say (withdrawBadgeOption, after it notes a ticked box),
// the browser forgotten, in another tab say (forgetForm), and a redraw drawing
// the form again with its values (restoreDraft). It is worded by the rule it
// was first worded by, kept with it with the image release took off and whether
// it is the notice of a replaced badge, read as things stand then: the badge
// held, the box, the poster key and the text and image the form holds then
// (formHolds), so the notice of a replaced badge names the badge held then, or
// none, a badge stored since an expired or revoked refusal makes it that
// notice, and after Forget this browser it is the server's words; the notice of
// a replaced badge is the form's one refusal for the badge, so a note left
// since goes. Only a notice still in the words the form was told is worded
// again, its text changed in place, so the focus, on its dismiss button too,
// stays where it is; words that come out the same are not said again, and a
// refusal said before anything was sent whose words no longer hold is taken
// off, what was added after it, a failed redraw's error say, kept. A failed
// redraw that keeps the page notes the words it says, with what it added after
// the refusal, so they are worded again there too.
func TestARefusalOnScreenIsWordedAgainForWhatChanges(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if said := squash(withoutComments(jsFunction(t, script, "noticeSaid"))); said != "function noticeSaid() { "+
		`return el.notice.hidden ? "" : el.notice.querySelector(".notice-text").textContent; }` {
		t.Errorf("noticeSaid no longer reads the words on screen, or none when the notice is hidden: %s", said)
	}
	if holds := squash(withoutComments(jsFunction(t, script, "formHolds"))); holds != "function formHolds(form) { "+
		`const body = field(form, "body"); const image = field(form, "image"); `+
		"return { text: Boolean(body && body.value.trim()), image: Boolean(image && image.files.length) }; }" {
		t.Errorf("formHolds no longer reads the text and image a form holds now: %s", holds)
	}
	if reword := squash(withoutComments(jsFunction(t, script, "rewordRefusal"))); reword != "function rewordRefusal(form) { "+
		"const refused = refusalNotices.get(form); const said = noticeSaid(); "+
		"if (!refused || !refused.told || said !== refused.told) { return; } "+
		"const { text, image } = formHolds(form); const words = refused.notice(refused.off, form, text, image); "+
		"if (refused.replaced()) { badgeWithdrawn.delete(form); } "+
		`const told = [words, refused.after].filter(Boolean).join(" "); `+
		"if (words) { refused.told = told; } else { refusalNotices.delete(form); } "+
		`if (!told) { clearNotice(); } else if (told !== said) { el.notice.querySelector(".notice-text").textContent = told; } }` {
		t.Errorf("rewordRefusal no longer words a refusal still on screen again, in place, by its own rule as things stand "+
			"now, taking off a note for the badge and a refusal that no longer holds: %s", reword)
	}
	// Called where the badge box is drawn again, the browser forgotten, and the
	// form drawn again by a redraw, and nowhere else.
	if forget := squash(withoutComments(jsFunction(t, script, "forgetForm"))); !strings.HasSuffix(forget,
		"badgeWithdrawn.delete(form); drawBadgeOption(form); forgottenAt.set(form, browserForgets); rewordRefusal(form); }") ||
		strings.Count(code, "rewordRefusal(") != 4 || strings.Count(code, "noticeSaid()") != 5 || strings.Count(code, "formHolds(") != 3 {
		t.Error("rewordRefusal is no longer called by forgetForm, once the form is forgotten, by withdrawBadgeOption and " +
			"by restoreDraft alone, or the words on screen or what a form holds are read somewhere new")
	}
	// The submit handler keeps the rule, the image release took off and whether
	// it is the notice of a replaced badge with every refusal it keeps, one
	// rule for a form told on screen and one told later
	// (TestARequestGoesOnlyFromAFormOnScreen,
	// TestCaptchaRefusalSaysWhatTheFormStillHolds).
	submit := squash(withoutComments(submitHandler(t, script)))
	if strings.Count(submit, "replaced: replacedNow, off: imageOff,") != 2 || strings.Count(submit, "replacedNow()") != 1 ||
		strings.Count(submit, "!shown &&") != 0 {
		t.Error("the submit handler no longer keeps one rule for whether a refusal is the notice of a replaced badge, read " +
			"as it is worded, with the image release took off, for a form on screen and one off it")
	}
}

// A press refused before anything is sent, an empty one or one the badge's note
// stops, made on a posting form still in sight while a redraw loads (a post
// link's lookup into its own thread, Update, another page of the board's index,
// Back say), is answered on its form, as one that is sent is: the press is
// counted, so the redraw carries the form itself (carriedDraft), and the
// refusal is kept with it as a rule (refuseUnsent). It is left up while the
// redraw loads, through the render that draws the thread after a post link's
// lookup too (render), and while it is still the notice on screen
// (unsentOnScreen) the page that redraw draws, if it draws that form again,
// words it again for the form drawn, leaving it as it is in words that come out
// the same, Update say, with the focus on its button if it was lost
// (restoreDraft), and takes it off when its words no longer hold, the reader
// having written something since an empty press say, the box the note said was
// not ticked having been ticked since, the form noted afresh for another badge,
// or the browser forgotten since. One the reader has dismissed, or that another
// notice has taken the place of, is said no more. A page drawn for another
// address, the Verify page among them, a page drawn with no form, and the age
// screen or an error drawn in the page's place take it off while it is still
// the notice on screen, the form it speaks of having gone, and every press
// takes out what the one before kept, so a refusal is said only on its own
// form, for the press it answers. A page kept for the form whose redraw will
// not load keeps it with the error after it (render).
func TestAPressRefusedBeforeSendingIsSaidOnTheFormDrawn(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if unsent := squash(withoutComments(jsFunction(t, script, "refuseUnsent"))); unsent != "function refuseUnsent(form, words, text, image) { "+
		"const notice = (off, on, hasText, hasImage) => { const said = words(on, hasText, hasImage); "+
		`return said ? imageTakenOff(said, off, { hasText, isThread: on.dataset.kind === "thread" }) : ""; }; `+
		`const told = notice("", form, text, image); `+
		`refusalNotices.set(form, { notice, replaced: () => false, off: "", told, after: "", unsent: true }); showNotice(told); }` {
		t.Errorf("refuseUnsent no longer says a refusal before anything is sent and keeps how to word it, as unsent, for "+
			"the form a redraw draws: %s", unsent)
	}
	if empty := squash(withoutComments(jsFunction(t, script, "emptyPostNotice"))); empty != "function emptyPostNotice(form) { "+
		`const image = field(form, "image"); `+
		`return image && !image.disabled ? "Write something or attach an image." : "Write something to post."; }` {
		t.Errorf("emptyPostNotice no longer offers an image only where the form it is said to takes one: %s", empty)
	}
	if note := squash(withoutComments(jsFunction(t, script, "badgeNoteWords"))); note != "function badgeNoteWords(withdrawn) { "+
		"const forgets = browserForgets; return (on) => { "+
		`const box = field(on, "badge"); if (forgets !== browserForgets || (box && box.checked) || badgeWithdrawn.has(on)) { return ""; } `+
		"return badgeWithdrawnNotice(on, withdrawn); }; }" {
		t.Errorf("badgeNoteWords no longer words the badge's note for the form it is said to, or says nothing once the "+
			"browser is forgotten, the box ticked or the form noted again: %s", note)
	}
	submit := squash(withoutComments(submitHandler(t, script)))
	inOrder(t, "the submit handler", submit,
		"presses.set(form, (presses.get(form) ?? 0) + 1); refocusedForms.delete(form); refusalNotices.delete(form);",
		"if (!body.trim() && !file) { "+
			`refuseUnsent(form, (on, text, image) => (text || image ? "" : emptyPostNotice(on)), false, false); `+
			`(field(form, "body") || form).focus(); return; }`,
		"refuseUnsent(form, badgeNoteWords(withdrawn), Boolean(body.trim()), Boolean(file));",
		"const release = holdForm(form);",
	)
	if strings.Count(code, "refuseUnsent(") != 3 || strings.Count(code, "emptyPostNotice(") != 2 ||
		strings.Count(code, "badgeNoteWords(") != 2 || strings.Count(code, ".unsent") != 2 ||
		strings.Count(code, "Write something or attach an image.") != 1 {
		t.Error("a refusal before anything is sent is said or kept somewhere besides the empty press and the badge's note, " +
			"or read as unsent somewhere besides restoreDraft and unsentOnScreen")
	}
	// It is that form's refusal on screen only while it is the notice on screen
	// in the words it was last told: dismissed, or another notice said in its
	// place, it is not.
	if on := squash(withoutComments(jsFunction(t, script, "unsentOnScreen"))); on != "function unsentOnScreen(form) { "+
		"const refused = form ? refusalNotices.get(form) : undefined; "+
		"return refused && refused.unsent && refused.told && refused.told === noticeSaid() ? refused : undefined; }" {
		t.Errorf("unsentOnScreen no longer reads a refusal said before sending as on screen only while it is the "+
			"notice on screen: %s", on)
	}
	// A render that keeps the page on screen for the form leaves it up as it
	// begins, and every other begins without it; the age screen and an error
	// drawn in the page's place and the Verify page (render), and a page drawn
	// for another address and one drawn with no form (restoreDraft), take it
	// off, the form it speaks of having gone.
	render := squash(withoutComments(jsFunction(t, script, "render")))
	inOrder(t, "render", render,
		"renderNav(route); if (!(keepPage() && unsentOnScreen(carriedDraft(draft).held))) { clearNotice(); } verifyRefusal = null;",
		`if (age !== "ok" && !ungated) { clearNotice(); leavePage(); renderAgeGate(age); return; }`,
		"if (draft && unsentOnScreen(draft.held)) { clearNotice(); } leavePage(); el.main.innerHTML = emptyStateHTML(message, board);",
		`if (route.view === "verify") { if (!overtaken(route)) { const carried = carriedDraft(draft); `+
			"if (carried && unsentOnScreen(carried.held)) { clearNotice(); } focusHash(); } return; }",
	)
	if strings.Count(code, "unsentOnScreen(") != 6 {
		t.Error("a refusal said before sending is read as on screen somewhere besides render's start, its error and its " +
			"Verify page, and restoreDraft's page for another address and page with no form")
	}
	// restoreDraft words it again for the form a press during the redraw
	// carried (answeredForm) while it is still the notice on screen, as any
	// refusal there, and gives that form's button the focus while it is
	// (TestARequestGoesOnlyFromAFormOnScreen pins the words and the focus).
	restore := squash(withoutComments(jsFunction(t, script, "restoreDraft")))
	inOrder(t, "restoreDraft", restore,
		"if (!draft || draft.key !== routeKey(state.route)) { if (draft && unsentOnScreen(draft.held)) { clearNotice(); } return; }",
		"answeredForm = heldForms.has(held) ? null : held;",
		"if (!form) { if (unsentOnScreen(answeredForm)) { clearNotice(); } keepUnsentDraft(draft); return; }",
		"const refused = answeredForm ? refusalNotices.get(answeredForm) : undefined;",
		"const unchanged = Boolean(refused && said) && said === (unsaid ? words : refused.told);",
		"if (unchanged) { ",
		"const focused = unsaid ? unsaid.focused : refused && refused.unsent ? refusalNotices.has(form) : "+
			"refocusedForms.has(answeredForm);",
	)
	if carried := squash(withoutComments(jsFunction(t, script, "carriedDraft"))); carried != "function carriedDraft(draft) { "+
		"if (!draft || !draft.from || (presses.get(draft.from) ?? 0) === draft.pressed) { return draft; } "+
		"return { key: draft.key, held: draft.from, forgets: draft.forgets }; }" {
		t.Errorf("carriedDraft no longer carries a form pressed while the redraw loads as that form, held: %s", carried)
	}
}

// The captcha's worker is a script from /lib/ and its arithmetic is the
// browser's own Web Crypto, so the policy every response carries needed no
// exception for it: workers fall back to default-src 'self', and nothing needs
// eval, WebAssembly or a blob: worker. A widget or a solver that wanted any of
// those would have to loosen the policy first, and this test with it.
func TestContentSecurityPolicyNeedsNoExceptionForTheCaptcha(t *testing.T) {
	if !strings.HasPrefix(contentSecurityPolicy, "default-src 'self';") {
		t.Errorf("the CSP no longer starts with default-src 'self': %s", contentSecurityPolicy)
	}
	for _, banned := range []string{"unsafe-eval", "wasm-unsafe-eval", "blob:", "worker-src", "child-src", "script-src"} {
		if strings.Contains(contentSecurityPolicy, banned) {
			t.Errorf("the CSP mentions %s: %s", banned, contentSecurityPolicy)
		}
	}
}

// A code that has been sent is what the Verify page is for until the code is
// confirmed or given up, whether or not the browser already holds a badge: a
// badge is replaced only once the new one is confirmed. So verifyViewHTML
// decides the code step before it looks at a held badge, and on verifyStep.step
// alone. A badge holder who pressed "Verify another address" and sent a code
// once got the badge panel back instead, with nowhere to type the code: the
// code step came after the panel, and sending the code dropped the flag that
// held the panel off.
func TestVerifyCodeBoxShowsWhileACodeIsPending(t *testing.T) {
	script := appScript(t)
	view := withoutComments(jsFunction(t, script, "verifyViewHTML"))

	// The view's own branches, in order: the ones at its top level, indented
	// once.
	var conditions []string
	for _, m := range regexp.MustCompile(`(?m)^  if \((.+)\) \{$`).FindAllStringSubmatch(view, -1) {
		conditions = append(conditions, m[1])
	}
	const codeStep = `verifyStep.step === "code"`
	at := -1
	for i, c := range conditions {
		if strings.Contains(c, `"code"`) {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("verifyViewHTML has no top-level branch for the code step; its branches are %q", conditions)
	}
	if conditions[at] != codeStep {
		t.Errorf("the code step is drawn when %q; want exactly %q, so nothing but a pending code decides it",
			conditions[at], codeStep)
	}
	for _, c := range conditions[:at] {
		if c != "!v.enabled" {
			t.Errorf("verifyViewHTML checks %q before the code step; only !v.enabled may come first, "+
				"or a pending code can be hidden behind another panel", c)
		}
	}
	// Nor can anything inside the code step, or between the branches, stand
	// in for the code box: the step's first statement returns it, the
	// not-configured page is the only one returned before it, and the view
	// reads verifyStep without ever setting it. A gate nested in the branch —
	// a held badge drawn instead, with the step put back — once brought the
	// bug back with every check above passing.
	opener := "\n  if (" + codeStep + ") {\n"
	start := strings.Index(view, opener)
	if start < 0 {
		t.Fatalf("verifyViewHTML has no line opening the code step as %q", strings.TrimSpace(opener))
	}
	body := strings.TrimLeft(view[start+len(opener):], " \t\n")
	if !strings.HasPrefix(body, "return `") {
		first, _, _ := strings.Cut(body, "\n")
		t.Errorf("the code step starts with %q; want it to return the code box, with nothing decided first", first)
	}
	// What it returns is the code box whatever is held: the template asks
	// nothing about a badge, or about the step it was asked for over.
	if box, _, ok := strings.Cut(body, "`;\n"); !ok {
		t.Error("the code step's template does not end where this test can find it")
	} else {
		for _, banned := range []string{"v.badge", "replacing"} {
			if strings.Contains(box, banned) {
				t.Errorf("the code step's template reads %s, so a pending code's box can depend on it:\n%s", banned, box)
			}
		}
		if !strings.Contains(box, `data-verify="confirm"`) || !strings.Contains(box, `id="f-code"`) {
			t.Errorf("the code step does not return the code box:\n%s", box)
		}
		// Banning names was not enough: a gate on state.verification.badge
		// reads no "v.badge", and with the form's markup still in the template
		// it passed every check above while the box was not drawn. So what the
		// template interpolates is a list this test knows — fixed markup, the
		// badge the code would earn, the wait while a request is out, keep,
		// which adds the Keep my badge button to the form's actions and nothing
		// else (TestReplacingABadgeCanGoBackToIt), and the note saying why a
		// badge held here went, which reads badgeLost alone and is held whole
		// by TestAGoneBadgeIsSaidToHaveGone — and it is one template, not
		// pieces joined around a condition.
		allowed := []string{
			"${VERIFY_INTRO}",
			`${badgeLostHTML("The code on its way earns you a new one.")}`,
			"${verifiedBadgeHTML(verifyStep.label)}",
			"${VERIFY_REFUSED_HTML}",
			"${wait}",
			`${busy ? VERIFY_CHECKING : "Verify"}`,
			"${keep}",
			"${forgetBrowserHTML()}",
		}
		for rest := box; ; {
			at := strings.Index(rest, "${")
			if at < 0 {
				break
			}
			rest = rest[at:]
			known := ""
			for _, a := range allowed {
				if strings.HasPrefix(rest, a) {
					known = a
					break
				}
			}
			if known == "" {
				what, _, _ := strings.Cut(rest, "\n")
				t.Errorf("the code step's template interpolates %s; a pending code's box may interpolate only %q", what, allowed)
				known = "${"
			}
			rest = rest[len(known):]
		}
		if n := strings.Count(box, "`"); n != 1 {
			t.Errorf("the code step's return has %d backticks; want the one opening its template, so that no condition "+
				"is joined into it:\n%s", n, box)
		}
	}
	if n := len(regexp.MustCompile(`\breturn\b`).FindAllString(view[:start], -1)); n != 1 {
		t.Errorf("verifyViewHTML returns %d times before the code step; want once, for the not-configured page", n)
	}
	if verifyStepSet.MatchString(view) {
		t.Error("verifyViewHTML sets verifyStep; it draws the step it is given and decides nothing about it")
	}

	// Nor can what the view reads before the code step stand in for a held
	// badge, so everything up to the step is held line for line: v is the
	// state as loaded, the one page drawn before the step is the one for
	// verification off or not known to be on, which says it is not configured
	// only once the server has said so (answered), and busy, wait and keep say
	// only whether a request is out and whether to offer Keep my badge. A v
	// worked out from the held badge, saying verification was off while a code
	// was pending, once put "not configured" in place of the box with every
	// check above passing.
	// And v.enabled and v.answered are the server's answers and nothing else:
	// loadVerification, which sets them, is held whole, nothing else in app.js
	// sets state.verification, and neither is named anywhere but there, here,
	// in the state the page starts with, and in readStoredBadge's one read of
	// whether verification is on, before it reads a badge another tab stored
	// (TestForgettingTheBadgeReachesEveryTab). A request that gets no answer
	// changes neither. Taken as an answer of "off", the GET given up after a
	// Verify drew "not configured" where the badge just stored belonged, focus
	// fell to the page, and the badge was taken off every posting form, and the
	// footer's Verify link away, for the rest of the visit.
	holdLines(t, "verifyViewHTML before the code step", jsLines(view), "function verifyViewHTML() {", "if ("+codeStep+") {",
		[]string{
			"const v = state.verification;",
			"if (!v.enabled) {",
			"return `",
			`<div class="board-head"><h1>Institutional verification</h1></div>`,
			"${VERIFY_INTRO}",
			`<p class="empty">${v.answered ? "Verification is not configured on this server." : esc(UNREACHABLE)}</p>`,
			"${forgetBrowserHTML()}`;",
			"}",
			"const busy = verifyBusy === verifyStep;",
			`const wait = busy ? " disabled" : "";`,
			"const keep = v.badge",
			"? `<button class=\"btn subtle\" type=\"button\" data-action=\"verify-keep\"${wait}>Keep my badge</button>`",
			`: "";`,
		})
	load := jsFunction(t, script, "loadVerification")
	if got, want := jsLines(load), []string{
		"async function loadVerification() {",
		"let answer = null;",
		"try {",
		`answer = await api("/api/verify", { signal: verifyDeadline() });`,
		"} catch {",
		"}",
		"const answered = !!answer || state.verification.answered;",
		"const enabled = answer ? !!answer.enabled : state.verification.enabled;",
		"state.verification = { answered, enabled, badge: enabled ? readBadge() : null };",
		"offerHeldBadge(true);",
		"if (el.verifyFooter) {",
		"el.verifyFooter.hidden = !enabled;",
		"}",
		"return state.verification;",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("loadVerification runs\n\t%s\nwant it to take whether verification is on from the server's answers "+
			"alone, and to keep what the server said last when a request gets none:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	code := withoutComments(script)
	if n := len(regexp.MustCompile(`\bstate\.verification\s*=[^=]`).FindAllString(code, -1)); n != 1 {
		t.Errorf("app.js sets state.verification %d times; want once, in loadVerification", n)
	}
	const initial = "\n  verification: { answered: false, enabled: false, badge: null },\n"
	if !strings.Contains(code, initial) {
		t.Errorf("the state the page starts with no longer holds %q", strings.TrimSpace(initial))
	}
	const storedRead = "state.verification.badge = state.verification.enabled ? readBadge() : null;"
	if strings.Count(code, storedRead) != 1 || !strings.Contains(jsFunction(t, script, "readStoredBadge"), storedRead) {
		t.Errorf("readStoredBadge no longer reads whether verification is on with %q, alone outside loadVerification", storedRead)
	}
	held := withoutComments(load) + view[:start] + initial + storedRead
	for _, name := range []string{"enabled", "answered"} {
		named := regexp.MustCompile(`\b` + name + `\b`)
		if extra := len(named.FindAllString(code, -1)) - len(named.FindAllString(held, -1)); extra != 0 {
			t.Errorf("app.js names %s %d times outside loadVerification, verifyViewHTML before the code step, "+
				"the state the page starts with and readStoredBadge's read; what the server said about verification "+
				"is its answer, and nothing else may set it", name, extra)
		}
	}

	// And the code step is what a sent code leads to, and what is drawn, and
	// the badge an issued code leads to is drawn the same way, through
	// renderVerify. A gate on a held badge in either place once passed every
	// check above: one that set the address step after a code was sent, then
	// one that returned before the code box was drawn, or before renderVerify
	// drew the step it found, each with the form's markup and every step this
	// test knows still in place. Each left a badge holder with a pending code
	// and nowhere to type it. So what runs once each answer is in is held here
	// statement for statement, and anything added there fails this test:
	// change these lists only with the reason the new statement cannot hide a
	// step. renderVerify is held whole by TestVerifyStepIsDrawnForAReaderArriving,
	// the refusals by TestVerifyRefusalsAreSaidInTheForm, and every place that
	// sets verifyStep by TestVerifyAnswersBelongToTheStepTheyWereSentFrom.
	//
	// offerHeldBadge was added to the badge's answer: it redraws the badge box
	// of a posting form on screen for the badge just stored, for a reader who
	// went on to a board before the answer, and it cannot hide a step. It
	// reads no step, draws nothing of the Verify page, and on the Verify page,
	// which has no posting form, finds nothing to do; it is held whole by
	// TestCaptchaIsAskedForOnlyWhenSomeoneWrites.
	//
	// A code is sent by sendWithCaptcha, once the anti-spam puzzle, if the
	// server asks for one, is solved, and its answer is what that returns,
	// after its last argument, onScreen. A confirmation's answer comes after
	// its finally, which counts the confirmation off the step it was sent from
	// (codeSpentElsewhere) and draws nothing.
	submit := jsLines(jsFunction(t, script, "submitVerification"))
	confirmed := `body: JSON.stringify({ code: String(data.get("code") || "") }),`
	holdLines(t, "the answer to a code sent", submit, "onScreen,", "}", []string{
		");",
		"if (verifyStep !== sentFrom) return;",
		stepToCode,
		"if (!onVerifyPage()) return;",
		"drawVerifyStep();",
		`showVerifyField("f-code");`,
		"return;",
	})
	holdLines(t, "the answer to a code confirmed", submit, confirmed, "} catch (err) {", []string{
		"});",
		"} finally {",
		"sentFrom.confirming -= 1;",
		"}",
		"if (verifyStep !== sentFrom) return;",
		"storeCredential(issued.credential);",
		"state.verification.badge = readBadge();",
		"offerHeldBadge();",
		stepToAddress,
		"if (!onVerifyPage()) return;",
		"if (await renderVerify()) showHeldBadge();",
	})

	// And what runs on the code box from the moment it is drawn is held whole,
	// however far down it calls. Holding the answer statement for statement
	// left the two functions it runs on the box just drawn, showVerifyField and
	// revealClearOfHeader under it, checked for a line and a few words: a gate
	// on a held badge in either took the code form out of the page, and a badge
	// holder who sent a code had nowhere to type it, with every check here
	// passing. The box is drawn in two places. The answer to Send code draws it
	// with drawVerifyStep (of verifyViewHTML, only the part up to the code
	// step's return runs) and shows it with showVerifyField. renderVerify draws
	// whichever step the page is on by the time the server has answered, for a
	// reader arriving (render()) among others, and gives it its refusal with
	// sayVerifyRefusal (one for a wrong code, say); what its callers run on
	// what it drew is followed too: in render(), the take-off of a refusal said
	// before anything was sent (carriedDraft, unsentOnScreen, clearNotice) and
	// focusHash; showHeldBadge after Verify, confirmBrowserForgotten after
	// Forget this browser, and showNotice after a forget, or a code step spent,
	// heard from another tab. And the box can be drawn while renderVerify is
	// still asking: the answer to Send code, landing while render() asks for a
	// badge holder who then pressed Verify in the header. What renderVerify
	// waits on then runs on the box once its answer is in: the rest of api()
	// after its fetch, of loadVerification, which reads the badge again
	// (readBadge), and of renderVerify, which titles the page (announce). A
	// gate on a held badge in announce took the box away with every check here
	// passing. Every top-level function those reach is found by following the
	// calls down, not listed by hand, and each is held whole, here or by the
	// test named, so a function new to that path fails here until it is held
	// too. What runs on the box with no call from any of these — the page's
	// listeners and timers, and the answers to other requests — is not
	// followed; TestOnlyKnownPlacesTellWhetherABadgeIsHeld holds every place
	// that names what can tell whether a badge is held instead.
	graph := jsCallGraph(t, script)
	if end := strings.Index(view[start:], "`;\n"); end < 0 {
		t.Error("the code step's return does not end where this test can find it")
	} else {
		graph["verifyViewHTML"] = jsCallees(view[:start+end])
		delete(graph["verifyViewHTML"], "verifyViewHTML")
	}
	// loadVerification, once its answer is in, brings the posting forms on
	// screen into line with the badge it read (offerHeldBadge): it reads no
	// step and draws nothing of the Verify page, which has no posting form, so
	// on a pending code's box it finds nothing to do. It starts no puzzle
	// there either: loadVerification passes startNothing (held above),
	// offerHeldBadge passes it on, and withdrawBadgeOption starts one only
	// without it (both held whole by the tests named), so the puzzle's code is
	// not followed from it. A refusal a posting form was told is worded again
	// only for a posting form, while it is still on screen (rewordRefusal).
	delete(graph["withdrawBadgeOption"], "prepareCaptcha")
	heldBy := map[string]string{
		"drawVerifyStep":          "TestVerifyStepIsDrawnForAReaderArriving",
		"verifyViewHTML":          "this test, up to the code step and the step's template",
		"badgeLostHTML":           "TestAGoneBadgeIsSaidToHaveGone",
		"sayVerifyRefusal":        "TestVerifyRefusalsAreSaidInTheForm",
		"showHeldBadge":           "TestAGoneBadgeIsSaidToHaveGone",
		"showVerifyField":         "this test",
		"revealClearOfHeader":     "this test",
		"refuseVerification":      "this test",
		"field":                   "this test",
		"verifiedBadgeHTML":       "this test",
		"esc":                     "this test",
		"focusHash":               "this test",
		"confirmBrowserForgotten": "this test",
		"forgetBrowserHTML":       "this test, as fixed markup",
		"unreachableError":        "TestVerifyAnswersBelongToTheStepTheyWereSentFrom",
		"readBadge":               "this test",
		"storedCredential":        "this test",
		"forgetCredential":        "this test",
		"badgeNumber":             "TestPostingFormSendsOnlyTheBadgeItNames",
		"onVerifyPage":            "TestVerifyAnswersDrawOnlyOnTheVerifyPage",
		"announce":                "this test",
		"verifyStepOnScreen":      "TestVerifyStepIsDrawnForAReaderArriving",
		"overtaken":               "TestAHeldFormSurvivesARedrawOfItsPage",
		"showNotice":              "this test",
		"offerHeldBadge":          "TestCaptchaIsAskedForOnlyWhenSomeoneWrites",
		"withdrawBadgeOption":     "TestABadgeTakenFromUnderAFormIsSaidAtItsNextPress",
		"drawBadgeOption":         "TestPostingFormSendsOnlyTheBadgeItNames",
		"badgeOffered":            "TestPostingFormSendsOnlyTheBadgeItNames",
		"badgeOptionHTML":         "this test",
		"storageGet":              "this test",
		"captchaFormNeedsIt":      "this test",
		"captchaFormHasWriting":   "this test",
		"rewordRefusal":           "TestARefusalOnScreenIsWordedAgainForWhatChanges",
		"noticeSaid":              "TestARefusalOnScreenIsWordedAgainForWhatChanges",
		"formHolds":               "TestARefusalOnScreenIsWordedAgainForWhatChanges",
		"clearNotice":             "this test",
		"carriedDraft":            "TestAPressRefusedBeforeSendingIsSaidOnTheFormDrawn",
		"unsentOnScreen":          "TestAPressRefusedBeforeSendingIsSaidOnTheFormDrawn",
	}
	roots := []string{"drawVerifyStep", "showVerifyField", "sayVerifyRefusal", "carriedDraft", "unsentOnScreen",
		"clearNotice", "focusHash", "showHeldBadge", "confirmBrowserForgotten", "showNotice"}
	for _, f := range []struct{ name, resumes string }{
		{"api", "res = await fetch(path, init);"},
		{"loadVerification", `answer = await api("/api/verify", { signal: verifyDeadline() });`},
		{"renderVerify", "await loadVerification();"},
	} {
		src := withoutComments(jsFunction(t, script, f.name))
		at := strings.Index(src, f.resumes)
		if at < 0 || strings.Contains(src[:at], "await ") {
			t.Errorf("%s does not first wait at %q, where this test follows what it runs once the answer is in",
				f.name, f.resumes)
			continue
		}
		for callee := range jsCallees(src[at+len(f.resumes):]) {
			if graph[callee] != nil {
				roots = append(roots, callee)
			}
		}
	}
	reached := jsReached(t, graph, roots...)
	for _, name := range reached {
		if heldBy[name] == "" {
			t.Errorf("%s runs on a pending code's box once it is drawn, and nothing holds it whole: a gate on a held "+
				"badge in it can take the box away with every check passing; hold it here", name)
		}
	}
	var listed []string
	for name := range heldBy {
		listed = append(listed, name)
	}
	slices.Sort(listed)
	for _, name := range listed {
		if !slices.Contains(reached, name) {
			t.Errorf("%s no longer runs on a pending code's box once it is drawn; this test's list of what does is out "+
				"of date (it reaches %v)", name, reached)
		}
	}
	for _, f := range []struct {
		name string
		want []string
	}{
		{"showVerifyField", []string{
			"function showVerifyField(id) {",
			"const input = document.getElementById(id);",
			"if (!input) return;",
			"revealClearOfHeader(input.form);",
			"input.focus({ preventScroll: true });",
			"}",
		}},
		{"revealClearOfHeader", []string{
			"function revealClearOfHeader(el) {",
			"const gap = 12;",
			`const header = document.querySelector(".site-header");`,
			"const top = Math.max(0, header ? header.getBoundingClientRect().bottom : 0) + gap;",
			"const bottom = window.innerHeight - gap;",
			"const box = el.getBoundingClientRect();",
			"if (box.top < top || box.height > bottom - top) {",
			"window.scrollBy(0, box.top - top);",
			"} else if (box.bottom > bottom) {",
			"window.scrollBy(0, box.bottom - bottom);",
			"}",
			"}",
		}},
		{"refuseVerification", []string{
			"function refuseVerification(form, message, target, { invalid = false, select = false } = {}) {",
			`const why = form.querySelector(".verify-refused");`,
			"why.textContent = message;",
			"why.hidden = false;",
			"revealClearOfHeader(form);",
			"verifyShown.at = performance.now();",
			"if (!target) return;",
			`if (invalid) target.setAttribute("aria-invalid", "true");`,
			"target.focus({ preventScroll: true });",
			"if (select) target.select();",
			"}",
		}},
		{"field", []string{
			"function field(form, name) {",
			"const node = form.elements.namedItem(name);",
			"return node instanceof Element ? node : null;",
			"}",
		}},
		{"verifiedBadgeHTML", []string{
			"function verifiedBadgeHTML(label) {",
			"return `<span class=\"badge verified\" title=\"Institutional email verified; not linked to this poster\">" +
				"Verified: ${esc(label)}</span>`;",
			"}",
		}},
		{"esc", []string{
			"function esc(value) {",
			`return String(value ?? "").replace(/[&<>"']/g, (c) => ESCAPES[c]);`,
			"}",
		}},
		{"focusHash", []string{
			"function focusHash() {",
			"if (!window.location.hash) {",
			"return;",
			"}",
			"const target = document.getElementById(window.location.hash.slice(1));",
			"if (target) {",
			`target.scrollIntoView({ block: "center" });`,
			`target.classList.add("flash");`,
			`if (target.hasAttribute("tabindex")) {`,
			"target.focus({ preventScroll: true });",
			"}",
			"}",
			"}",
		}},
		{"confirmBrowserForgotten", []string{
			"function confirmBrowserForgotten() {",
			`const done = document.getElementById("forget-browser-done");`,
			"if (!done) return;",
			"done.textContent = BROWSER_FORGOTTEN;",
			"done.hidden = false;",
			"revealClearOfHeader(done);",
			"done.focus({ preventScroll: true });",
			"}",
		}},
		{"clearNotice", []string{
			"function clearNotice() {",
			"el.notice.hidden = true;",
			`el.notice.textContent = "";`,
			"}",
		}},
		{"showNotice", []string{
			"function showNotice(message) {",
			"el.notice.innerHTML =",
			`'<span class="notice-text"></span>' +`,
			`'<button class="notice-dismiss" type="button" data-action="dismiss-notice" aria-label="Dismiss">&times;</button>';`,
			`el.notice.querySelector(".notice-text").textContent = message;`,
			"el.notice.hidden = false;",
			"}",
		}},
		{"badgeOptionHTML", []string{
			"function badgeOptionHTML(held) {",
			`if (!held) return "";`,
			"return `<label class=\"sage\"><input type=\"checkbox\" name=\"badge\" aria-describedby=\"f-badge-hint\" /> " +
				"post with ${verifiedBadgeHTML(held.label)}</label>",
			`<span class="hint" id="f-badge-hint">A badged post gets its own ID in each thread. A name or`,
			"tripcode you also use without the badge would tie those posts to it, so the badge starts",
			"with the name empty.</span>`;",
			"}",
		}},
		{"storageGet", []string{
			"function storageGet(key) {",
			"try {",
			"return localStorage.getItem(key);",
			"} catch {",
			"return null;",
			"}",
			"}",
		}},
		{"captchaFormNeedsIt", []string{
			"function captchaFormNeedsIt(form) {",
			"if (!state.site.captcha || !(form instanceof HTMLFormElement)) {",
			"return false;",
			"}",
			"if (form.dataset.verify) {",
			`return form.dataset.verify === "request";`,
			"}",
			`const badge = field(form, "badge");`,
			"return Boolean(form.dataset.kind) && !(badge && badge.checked);",
			"}",
		}},
		{"captchaFormHasWriting", []string{
			"function captchaFormHasWriting(form) {",
			"const written = (name) => {",
			"const box = field(form, name);",
			"return Boolean(box && box.value.trim());",
			"};",
			`const image = field(form, "image");`,
			`return written("subject") || written("body") || Boolean(image && image.files && image.files.length);`,
			"}",
		}},
		{"announce", []string{
			"function announce(title) {",
			"document.title = title === \"educhan\" ? title : `${title} — educhan`;",
			"el.status.textContent = title;",
			"}",
		}},
		{"readBadge", []string{
			"function readBadge() {",
			"const token = storedCredential();",
			"if (!token) {",
			"return null;",
			"}",
			`const parts = token.split(".");`,
			`if (parts.length !== 3 || parts[0] !== "v1") {`,
			"forgetCredential();",
			"return null;",
			"}",
			"try {",
			`const base64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");`,
			`const padded = base64 + "=".repeat((4 - (base64.length % 4)) % 4);`,
			"const bytes = Uint8Array.from(atob(padded), (c) => c.charCodeAt(0));",
			"const claims = JSON.parse(new TextDecoder().decode(bytes));",
			"if (!claims.label || !claims.exp) {",
			"forgetCredential();",
			"return null;",
			"}",
			"const expires = new Date(claims.exp * 1000);",
			"if (expires <= new Date()) {",
			"forgetCredential();",
			`badgeLost = "Your badge has expired.";`,
			"return null;",
			"}",
			"return { token, label: claims.label, expires, number: badgeNumber(token) };",
			"} catch {",
			"forgetCredential();",
			"return null;",
			"}",
			"}",
		}},
		{"storedCredential", []string{
			"function storedCredential() {",
			"try {",
			`return localStorage.getItem(CREDENTIAL_STORAGE_KEY) || "";`,
			"} catch {",
			`return "";`,
			"}",
			"}",
		}},
		{"forgetCredential", []string{
			"function forgetCredential() {",
			"try {",
			"localStorage.removeItem(CREDENTIAL_STORAGE_KEY);",
			"} catch {",
			"}",
			"state.verification.badge = null;",
			"}",
		}},
	} {
		if got := jsLines(jsFunction(t, script, f.name)); !slices.Equal(got, f.want) {
			t.Errorf("%s runs\n\t%s\nwant exactly\n\t%s\nIt runs on a pending code's box once the box is drawn, and a "+
				"gate on a held badge in it takes the box away with every other check passing",
				f.name, strings.Join(got, "\n\t"), strings.Join(f.want, "\n\t"))
		}
	}
	// Forget this browser's block is fixed markup, whatever it says: one
	// template, nothing interpolated into it, and nothing else run.
	forget := jsLines(jsFunction(t, script, "forgetBrowserHTML"))
	if all := strings.Join(forget, "\n"); len(forget) < 4 || forget[0] != "function forgetBrowserHTML() {" ||
		forget[1] != "return `" || !strings.HasSuffix(forget[len(forget)-2], "`;") || forget[len(forget)-1] != "}" ||
		strings.Count(all, "`") != 2 || strings.Contains(all, "${") {
		t.Errorf("forgetBrowserHTML runs\n\t%s\nwant it to return one template of fixed markup and do nothing else: it "+
			"is drawn into a pending code's box", strings.Join(forget, "\n\t"))
	}

	// Following the calls down from those functions finds none of what runs
	// once each call that drew the box returns, up to the browser: the rest of
	// each function waiting on it, and of each listener. Code there is in no
	// top-level function, or in one the calls lead up to rather than down to,
	// and a gate on a held badge in either of two places took the box away
	// with every check here passing: in the page's submit listener after
	// submitVerification, the path that hid the box first, for a badge holder
	// who pressed Send code; and in what render() ran after drawing the Verify
	// page, for one who came back to it. So what follows each drawing is held
	// too, line for line, up to where the browser takes over.
	//
	// The answer to Send code ends where it draws (held above), and
	// submitVerification's finally is held by
	// TestVerifyAnswersBelongToTheStepTheyWereSentFrom. The page's submit
	// listener, which waits on it, is held up to the end of its Verify branch,
	// and it is the one listener for a Verify form's submit: the age screen's
	// is the only other.
	holdLines(t, "the page's submit listener up to the end of its Verify branch", jsLines(submitHandler(t, script)),
		`el.main.addEventListener("submit", async (event) => {`, "}", []string{
			"event.preventDefault();",
			"const form = event.target;",
			"if (form.dataset.verify) {",
			"await submitVerification(form);",
			"return;",
		})
	if n := len(regexp.MustCompile(`addEventListener\("submit"|\bonsubmit\b`).FindAllString(code, -1)); n != 2 {
		t.Errorf("app.js listens for a submit %d times; want twice, the age screen and the page, whose listener sends "+
			"a Verify form and is held here", n)
	}

	// renderVerify is called from eight places, and what each runs once it
	// returns is held: the answer to Verify (held above, with showHeldBadge
	// whole), the two forget buttons and the follows of a forget and of a code
	// step spent in another tab (forgottenElsewhere, codeSpentElsewhere), whose
	// callbacks are held here whole, and render(), below. The other two, a
	// change of badge in another tab (followCredentialStorage) and a badged
	// post refused (submit), call it as a statement of its own, with nothing
	// waiting on it or chained to it: what follows it runs before it has
	// asked the server, let alone drawn.
	if n := strings.Count(code, "renderVerify()"); n != 9 {
		t.Errorf("app.js names renderVerify() %d times; want nine: its definition, render(), the answer to Verify, "+
			"the two forget buttons, the two follows of another tab, each held from where it returns, and the two "+
			"redraws of the badge panel that nothing waits on", n)
	}
	if n := strings.Count(squash(code), panelRedrawn); n != 2 || !strings.HasSuffix(panelRedrawn, "{ renderVerify(); }") {
		t.Errorf("app.js draws the badge panel again, with nothing waiting on it, %d times; want two", n)
	}
	for _, then := range []string{
		"renderVerify().then((drawn) => {\n" +
			"      if (drawn) {\n" +
			"        showNotice(BROWSER_FORGOTTEN_ELSEWHERE);\n" +
			"      }\n" +
			"    });\n" +
			"  }\n" +
			"}\n",
		"renderVerify().then((drawn) => {\n" +
			"      if (drawn) {\n" +
			"        showNotice(CODE_SPENT_ELSEWHERE);\n" +
			"      }\n" +
			"    });\n" +
			"  }\n" +
			"}\n",
		"renderVerify().then((drawn) => {\n" +
			"      const lost = drawn && document.getElementById(\"badge-lost\");\n" +
			"      if (lost) {\n" +
			"        revealClearOfHeader(lost);\n" +
			"        lost.focus({ preventScroll: true });\n" +
			"      }\n" +
			"    });\n" +
			"    return;\n",
		"renderVerify().then((drawn) => {\n" +
			"      if (drawn) confirmBrowserForgotten();\n" +
			"    });\n" +
			"    return;\n",
	} {
		if strings.Count(code, then) != 1 {
			t.Errorf("app.js does not run exactly this once a forget button's, or a follow of another tab's, renderVerify returns:\n%s", then)
		}
	}

	// render() leaves the Verify page once it is drawn, with two things run on
	// it, and only when no newer render has overtaken it, after finish, which
	// beginLoading hands it: a refusal said before anything was sent, at a
	// press made while the page loaded, is taken off while it is still the
	// notice on screen, as a page drawn for another address takes it off
	// (carriedDraft, unsentOnScreen and clearNotice, which read no badge and
	// change only the notice), and focusHash jumps to a fragment. Each of those
	// and finish is held whole, and so is the skeleton's timer beginLoading
	// sets, which can fire once the page is drawn: it draws nothing over a page
	// drawn since the load began, and asks render's skeleton (whether a held
	// form keeps the page) only of the page that was there then. What finish
	// runs, endLoading, is held whole by TestAHeldFormSurvivesARedrawOfItsPage.
	// What render() runs after drawing any other page — enhancePosts,
	// restoreDraft and what they reach — serves every page, so it is not run on
	// this one. An overtaken render drew nothing: the notice is the newer
	// render's to leave or take off, and its jump to a fragment would move the
	// page the reader went to.
	renderLines := jsLines(jsFunction(t, script, "render"))
	holdLines(t, "render() once the Verify page is drawn", renderLines, "} finally {", "if (resolved !== null) {",
		[]string{
			"finish();",
			"}",
			`if (route.view === "verify") {`,
			"if (!overtaken(route)) {",
			"const carried = carriedDraft(draft);",
			"if (carried && unsentOnScreen(carried.held)) {",
			"clearNotice();",
			"}",
			"focusHash();",
			"}",
			"return;",
			"}",
		})
	if !slices.Contains(renderLines, "const finish = beginLoading({ skeleton: () => !keepPage() });") ||
		len(regexp.MustCompile(`\bfinish\b`).FindAllString(strings.Join(renderLines, "\n"), -1)) != 2 {
		t.Error("render() does not take finish from beginLoading and call it in its finally alone")
	}
	if got, want := jsLines(jsFunction(t, script, "beginLoading")), []string{
		"function beginLoading({ skeleton = () => true } = {}) {",
		"const begun = rendersBegun;",
		`el.main.setAttribute("aria-busy", "true");`,
		"const dimmed = el.main.firstChild;",
		"loadingTimer = setTimeout(() => {",
		"if (begun === rendersBegun && el.main.firstChild === dimmed && skeleton()) {",
		`el.main.innerHTML = '<div class="skeleton"><div class="skeleton-row"></div><div class="skeleton-row"></div>` +
			`<div class="skeleton-row"></div></div>';`,
		"leavePage();",
		"}",
		"}, 250);",
		"return () => {",
		"if (begun === rendersBegun) {",
		"endLoading();",
		"}",
		"};",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("beginLoading runs\n\t%s\nwant exactly\n\t%s\nWhat it hands render() runs once the Verify page is "+
			"drawn, a pending code's box included", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}

	// navigate waits on render(), and is held whole: nothing runs after it.
	if got, want := jsLines(jsFunction(t, script, "navigate")), []string{
		"async function navigate(href, { replace = false } = {}) {",
		"const url = new URL(href, window.location.origin);",
		"if (replace) {",
		`window.history.replaceState(null, "", url);`,
		"} else {",
		`window.history.pushState(null, "", url);`,
		"}",
		"await render();",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("navigate runs\n\t%s\nwant exactly\n\t%s\nIt waits on render(), which can draw a pending code's box, "+
			"so nothing may run after it", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	// And nothing else waits on either. Every other call of render() or
	// navigate() is a statement of its own, with nothing waiting on it or
	// chained to it, so its caller has run to its end before the Verify page
	// is drawn: render() draws it only once the server has answered. The two
	// other waits are render's own for a post number and a new post's for its
	// thread, and each draws a thread.
	waits := []string{
		"await render();",
		"await render({ draft });",
		"await navigate(`${threadPath(created.board, created.thread_no)}#p${created.no}`);",
	}
	waited := map[string]int{}
	named := regexp.MustCompile(`(?:^|[^.\w$])(?:render|navigate)\b`)
	alone := regexp.MustCompile(`^(?:render|navigate)\((.*)\);$`)
	for _, line := range jsLines(script) {
		if !named.MatchString(line) ||
			strings.HasPrefix(line, "async function render(") || strings.HasPrefix(line, "async function navigate(") {
			continue
		}
		if slices.Contains(waits, line) {
			waited[line]++
			continue
		}
		// A call alone on its line: what is in its parentheses never closes
		// them early, so nothing is chained to the call or follows it.
		if m := alone.FindStringSubmatch(line); m != nil {
			depth := 0
			for _, c := range m[1] {
				if c == '(' {
					depth++
				} else if c == ')' {
					depth--
				}
				if depth < 0 {
					break
				}
			}
			if depth == 0 {
				continue
			}
		}
		t.Errorf("app.js names render or navigate in %q; want each call a statement of its own, with nothing waiting "+
			"on it or chained to it, or one of %q, each once", line, waits)
	}
	for _, line := range waits {
		if waited[line] > 1 {
			t.Errorf("app.js runs %q %d times; want once: what follows another such wait runs on a page render() "+
				"drew, a pending code's box among them", line, waited[line])
		}
	}
}

// A pending code's box shows whether or not a badge is held, so nothing may
// decide on a held badge whether it stays.
// TestVerifyCodeBoxShowsWhileACodeIsPending follows the calls from what draws
// the box, from what its callers run on it, and from what renderVerify waits
// on, and holds each function it reaches whole. Much else runs on the box with
// no call from any of those: the page's listeners — on a click into the code
// box (the click listener, isInternalLink), on a scroll (hideQuotePreview),
// on a key, a change, a pointer, a hover or a drag — its timers, and the
// answers to requests sent from other pages. Followed down, those reach
// render() and every page, which no test can hold whole, and a gate on a held
// badge in announce, hideQuotePreview or isInternalLink took the box out of
// the page with every other check passing. So what can tell whether a badge
// is held is named only where this test lists, top-level block by block, each
// as often as it lists: the verification state or a badge, read as a property
// (state.verification, .badge off anything), destructured or bound under its
// own name ({ verification: { badge } } = state), or named in a string; the
// state's reader loadVerification, and readBadge and storedCredential;
// localStorage, and the key the credential is kept under; and the markup a
// held badge alone draws — Keep my badge, the badge panel's #verify-badge, a
// posting form's badge box and the number its slot carries. Bare names are
// counted in the code alone: the copy that says "badge" on every page is
// left out (jsCodeText). A gate on a held badge that names any of them,
// wherever it is put, fails here; a destructured read in hideQuotePreview
// took the code box away while this test counted property reads alone. What
// it cannot see is a name made at run time — state[k] for a k computed, the
// page's text read back for its wording — which review has to catch. It is a
// floor, like the rest of this file: change a count only with the reason the
// new place cannot take a step off the page.
func TestOnlyKnownPlacesTellWhetherABadgeIsHeld(t *testing.T) {
	script := appScript(t)
	// Read in the source, strings included, since markup and a key are
	// strings: the property reads, the readers, the storage, and the markup.
	tells := regexp.MustCompile(`\.verification\b|\.badge\b|\breadBadge\(|\bstoredCredential\(|\blocalStorage\b|` +
		`\bCREDENTIAL_STORAGE_KEY\b|educhan:credential|verify-keep|verify-badge|data-badge-option|\bbadgeOffered\(|` +
		"\\bbadgeNumbers?\\b|[\"'`](?:badge|verification)[\"'`]")
	// Read in the code alone: the state and a badge by their bare names, as a
	// destructuring, a binding or a key names them, and the state's reader.
	// A name read as a property (.badge) is counted above, not again here.
	names := regexp.MustCompile(`(?:^|[^.\w$])(?:verification|badge)\b|\bloadVerification\(`)
	// Line for line: a comment line is emptied, not taken out.
	source := strings.Split(regexp.MustCompile(`(?m)^[ \t]*//.*$`).ReplaceAllString(script, ""), "\n")
	code := strings.Split(jsCodeText(script), "\n")
	if len(source) != len(code) {
		t.Fatalf("app.js has %d lines and its code %d; jsCodeText keeps every line", len(source), len(code))
	}
	// The code is app.js with what is not code blanked, and nothing else: its
	// function declarations read as app.js's do, the copy is gone, and what a
	// template interpolates, or follows a regular expression holding quotes,
	// is still there.
	for i, line := range source {
		if strings.HasPrefix(line, "function ") || strings.HasPrefix(line, "async function ") {
			if !strings.Contains(line, `"`) && code[i] != line {
				t.Fatalf("jsCodeText reads app.js line %d, %q, as %q; it has lost its place in the source", i+1, line, code[i])
			}
		}
	}
	codeText := strings.Join(code, "\n")
	for _, words := range []string{"Institutional verification", "Keep my badge", "data-action"} {
		if strings.Contains(codeText, words) {
			t.Errorf("jsCodeText leaves %q, from app.js's strings, in its code", words)
		}
	}
	for _, kept := range []string{"${esc(badgeLost)}", "${verifiedBadgeHTML(v.badge.label)}", "state.verification = {",
		"(c) => ESCAPES[c]"} {
		if !strings.Contains(codeText, kept) {
			t.Errorf("jsCodeText takes %q, which is code, out of app.js's code", kept)
		}
	}
	got := map[string]int{}
	for _, s := range jsTopLevel(strings.Join(source, "\n")) {
		n := len(tells.FindAllString(s.text, -1))
		for _, line := range code[s.line : s.line+strings.Count(s.text, "\n")] {
			n += len(names.FindAllString(line, -1))
		}
		if n > 0 {
			got[s.first] += n
		}
	}
	want := map[string]int{
		// The page's state, which starts with no badge known of.
		"const state = {": 2,
		// Where the credential is kept, read, stored (noting what this tab
		// stored, for a forget heard from another tab: storedHere) and
		// forgotten, and the badge read off it and numbered.
		`const CREDENTIAL_STORAGE_KEY = "educhan:credential";`: 2,
		"function storedCredential() {":                        3,
		"function storeCredential(token) {":                    3,
		"function forgetCredential() {":                        4,
		"function readBadge() {":                               3,
		"const badgeNumbers = new Map();":                      1,
		"function badgeNumber(token) {":                        5,
		// What else the browser keeps: the age answer, the post key, and
		// forgetting the browser, which empties the credential before it
		// removes it, the emptying being what every other tab hears the forget
		// by (emptyCredential, followCredentialStorage), and whether this tab
		// can read storage, asked at a forget's message (storageAnswers).
		"function storageGet(key) {":        1,
		"function storageSet(key, value) {": 1,
		"function storageRemove(key) {":     1,
		"function posterKey() {":            2,
		"function forgetBrowser() {":        1,
		"function emptyCredential() {":      6,
		"function storageAnswers() {":       2,
		// The badge the server put on a post, which says nothing of the one
		// held here.
		"function postHTML(post, context) {": 2,
		// A posting form, which the Verify page never has: its badge box and
		// what it offers, the box drawn again for the badge held now, the
		// notices a form whose ticked badge went is told, a draft carried
		// across a re-render, the badge box ticked, whether the form needs the
		// anti-spam puzzle (a Verify form is answered by its data-verify,
		// before any badge box is read), and the post sent.
		"function formHTML({ kind, slug, thread = null, open = false }) {":      5,
		"function badgeOptionHTML(held) {":                                      1,
		"function badgeOffered(form) {":                                         2,
		"function drawBadgeOption(form) {":                                      5,
		"function withdrawBadgeOption(form, startNothing) {":                    2,
		"function badgeWithdrawnNotice(form, { key }) {":                        2,
		"function badgeNoteWords(withdrawn) {":                                  1,
		"function badgeReplacedNotice(form, { hasText, hasImage, imageOff }) {": 4,
		"function formDraft(form) {":                                            6,
		"function restoreDraft(draft) {":                                        7,
		"function captchaFormNeedsIt(form) {":                                   4,
		`el.main.addEventListener("change", (event) => {`:                       1,
		`el.main.addEventListener("submit", async (event) => {`:                 14,
		// What another tab did to the badge or the browser, heard here: a
		// change of the badge storage holds (followCredentialStorage,
		// readStoredBadge) and a forget (forgetOnThisPage). Each takes a code
		// step off the page only where its code is done with: a badge newly
		// verified in another tab spent it or the code that replaced it
		// (codeSpentElsewhere, held by TestForgettingTheBadgeReachesEveryTab),
		// and a forget drops the step with the rest of what the browser held,
		// which would greet the next person at the computer with the last one's
		// institution (held by TestForgettingTheBrowserCancelsWhatIsOnItsWay).
		"function followCredentialStorage(event) {": 2,
		"function readStoredBadge() {":              8,
		"function forgetOnThisPage() {":             2,
		// Asking the server whether verification is on, for the posting
		// forms, when the first page past the age screen loads.
		"async function loadSite() {": 1,
		// The Verify page, held where a pending code's box is concerned by
		// TestVerifyCodeBoxShowsWhileACodeIsPending and the tests it names:
		// whether verification is on and the badge held, the page drawn once
		// the server has said, the page's steps, the answer to Verify, the
		// badge shown once kept or issued, Keep my badge in the page's click
		// listener, and the buttons a press aimed at the step before is let
		// go on.
		"async function loadVerification() {":             7,
		"async function renderVerify() {":                 1,
		"function verifyViewHTML() {":                     7,
		"async function submitVerification(form) {":       3,
		"function showHeldBadge() {":                      1,
		`document.addEventListener("click", (event) => {`: 6,
		"const VERIFY_MOVES = [":                          1,
	}
	var blocks []string
	for first := range got {
		blocks = append(blocks, first)
	}
	for first := range want {
		if got[first] == 0 {
			blocks = append(blocks, first)
		}
	}
	slices.Sort(blocks)
	for _, first := range blocks {
		if got[first] != want[first] {
			t.Errorf("the block opening %q names what tells whether a badge is held %d times; this test knows %d. "+
				"A gate on a held badge there can take a pending code's box off the page with every other check "+
				"passing", first, got[first], want[first])
		}
	}
}

// jsTopLevel splits code into its top-level statements by the first column:
// each runs from a line that starts there to the next such line, and a line
// there that only closes a statement (with }, ) or ]) belongs to the one it
// closes. It returns each statement's first line, its text and the number of
// its first line in code (from 0), in order.
func jsTopLevel(code string) []struct {
	first, text string
	line        int
} {
	var statements []struct {
		first, text string
		line        int
	}
	for i, line := range strings.Split(code, "\n") {
		if line != "" && !strings.ContainsAny(line[:1], " \t})]") {
			statements = append(statements, struct {
				first, text string
				line        int
			}{line, "", i})
		}
		if n := len(statements); n > 0 {
			statements[n-1].text += line + "\n"
		}
	}
	return statements
}

// jsCodeText is src with all that is not code in it blanked to spaces:
// comments, the text of string and template literals, and regular expression
// literals. Quotes and backticks stay, and so does a template's ${...}, read
// as code. Newlines stay too, so its lines are src's lines. A name found in it
// is a name the code uses, not a word in the copy a page shows.
func jsCodeText(src string) string {
	out := []byte(src)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	isWord := func(c byte) bool {
		return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	// A slash starts a regular expression where a value is expected — at the
	// start, after an operator, an opening bracket or a keyword that takes a
	// value — and divides after a name, a number or a closing bracket.
	takesValue := []string{"return", "typeof", "case", "do", "else", "in", "of", "new", "delete", "void", "throw",
		"instanceof", "yield", "await"}
	valueNext := true
	inText := false  // in a template literal's text
	var opened []int // the brace depth each open ${ of a template started at
	depth := 0
	for i := 0; i < len(src); {
		c := src[i]
		if inText {
			switch {
			case c == '\\':
				blank(i)
				if i+1 < len(src) {
					blank(i + 1)
				}
				i += 2
			case c == '`':
				inText, valueNext = false, false
				i++
			case c == '$' && i+1 < len(src) && src[i+1] == '{':
				inText, valueNext = false, true
				opened = append(opened, depth)
				depth++
				i += 2
			default:
				blank(i)
				i++
			}
			continue
		}
		switch {
		case strings.HasPrefix(src[i:], "//"):
			for ; i < len(src) && src[i] != '\n'; i++ {
				blank(i)
			}
		case strings.HasPrefix(src[i:], "/*"):
			stop := len(src)
			if end := strings.Index(src[i+2:], "*/"); end >= 0 {
				stop = i + 2 + end + 2
			}
			for ; i < stop; i++ {
				blank(i)
			}
		case c == '\'' || c == '"':
			for i++; i < len(src) && src[i] != c && src[i] != '\n'; i++ {
				if src[i] == '\\' && i+1 < len(src) {
					blank(i)
					i++
				}
				blank(i)
			}
			i++
			valueNext = false
		case c == '`':
			inText = true
			i++
		case c == '/' && valueNext:
			// Up to the slash that closes it, which a slash in a class [...]
			// does not, and then its flags.
			blank(i)
			class := false
			for i++; i < len(src) && src[i] != '\n'; i++ {
				if src[i] == '\\' && i+1 < len(src) {
					blank(i)
					i++
				} else if src[i] == '[' {
					class = true
				} else if src[i] == ']' {
					class = false
				} else if src[i] == '/' && !class {
					break
				}
				blank(i)
			}
			if i < len(src) && src[i] == '/' {
				blank(i)
				i++
			}
			for ; i < len(src) && isWord(src[i]); i++ {
				blank(i)
			}
			valueNext = false
		case c == '{':
			depth++
			valueNext = true
			i++
		case c == '}':
			depth--
			if n := len(opened); n > 0 && opened[n-1] == depth {
				opened = opened[:n-1]
				inText = true
			}
			valueNext = true
			i++
		case isWord(c):
			j := i
			for j < len(src) && isWord(src[j]) {
				j++
			}
			valueNext = slices.Contains(takesValue, src[i:j])
			i = j
		case c == ')' || c == ']':
			valueNext = false
			i++
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		default:
			valueNext = true
			i++
		}
	}
	return string(out)
}

// verifyStepSet finds each place app.js moves the Verify page to a step, and
// the steps below are the ones it moves to: the address step it starts on, the
// code step a sent code leads to, the address step asked for over a held badge,
// and that step again after the server has dropped the code being typed.
var verifyStepSet = regexp.MustCompile(`\bverifyStep\s*=[^=][^;]*;`)

const (
	stepToAddress   = `verifyStep = { step: "email", label: "" };`
	stepToCode      = `verifyStep = { step: "code", label: sent.label || "" };`
	stepToReplacing = `verifyStep = { step: "email", label: "", replacing: true };`
	stepToEnded     = `verifyStep = { step: "email", label: "", replacing: true, ended: CODE_ENDED_NOTES[err.message] };`
)

// When the server answers a code with a refusal that drops the verification —
// nothing in progress, the code expired, too many wrong guesses — the page
// takes the code box down and goes back to the address step, so a reader is
// never left typing codes into a box nothing is waiting on. It knows those
// refusals by the server's words, so they are checked against what the server
// actually answers, and a wrong code, which leaves the verification pending,
// must not be among them. That holds for the wrong code that spends the last
// attempt too: the server answers it as the end, so a box left up after a
// wrong code still takes the right one.
func TestClientKnowsWhenNoCodeIsPending(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	submit := withoutComments(jsFunction(t, script, "submitVerification"))
	names := []string{"NO_CODE_PENDING", "CODE_EXPIRED", "CODE_GUESSES_SPENT"}
	words := map[string]string{}
	for _, name := range names {
		words[name] = jsString(t, script, name)
		if n := strings.Count(code, `"`+words[name]+`"`); n != 1 {
			t.Errorf("app.js spells out %q %d times; want once, as %s", words[name], n, name)
		}
	}
	ended := `const ended = !requesting && [NO_CODE_PENDING, CODE_EXPIRED, CODE_GUESSES_SPENT].includes(err.message);`
	at := strings.Index(submit, ended)
	if at < 0 {
		t.Fatal("submitVerification no longer recognises the refusals that end a pending verification")
	}
	branch := submit[at:]
	if !strings.Contains(branch, `verifyStep = { step: "email", label: "", replacing: true, ended: CODE_ENDED_NOTES[err.message] };`) {
		t.Error("submitVerification does not go back to the address step when no code is pending")
	}

	h := verifyServer(t, nil)
	var refusals []string
	check := func(name string, wantStatus, status int, got string) {
		t.Helper()
		refusals = append(refusals, got)
		if status != wantStatus || got != words[name] {
			t.Errorf("%s: the server answered %d %q, want %d %q", name, status, got, wantStatus, words[name])
		}
	}

	status, out := h.confirmCode("123456")
	check("NO_CODE_PENDING", http.StatusBadRequest, status, out.Error)

	if status, out := h.requestCode("late@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request code: %d %s", status, out.Error)
	}
	sent := h.mail.last(t)
	h.exec(`UPDATE verification_pending SET expires_at = now() - interval '1 minute'`)
	status, out = h.confirmCode(sent.Code)
	check("CODE_EXPIRED", http.StatusBadRequest, status, out.Error)

	if status, out := h.requestCode("guess@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request code: %d %s", status, out.Error)
	}
	sent = h.mail.last(t)
	wrong := 0
	var wrongWords string
	for i := 0; wrong < maxVerifyAttempts-1; i++ {
		guess := fmt.Sprintf("%0*d", verifyCodeDigits, i)
		if guess == sent.Code {
			continue
		}
		wrong++
		status, out := h.confirmCode(guess)
		if status != http.StatusBadRequest {
			t.Fatalf("wrong code %d: status %d (%s), want 400", wrong, status, out.Error)
		}
		wrongWords = out.Error
	}
	// The wrong code that spends the last attempt ends the verification, and
	// is answered so: after it not even the right code gets through.
	last := "999999"
	if last == sent.Code {
		last = "999998"
	}
	status, out = h.confirmCode(last)
	check("CODE_GUESSES_SPENT", http.StatusTooManyRequests, status, out.Error)
	status, out = h.confirmCode(sent.Code)
	check("NO_CODE_PENDING", http.StatusBadRequest, status, out.Error)

	// A wrong code leaves the verification pending, so the box stays for the
	// right one.
	for _, ending := range refusals {
		if wrongWords == ending {
			t.Errorf("a wrong code is answered %q, which the page reads as nothing pending", wrongWords)
		}
	}
	if wrongWords == "" {
		t.Error("no wrong code was answered")
	}
}

// A code that has expired reaches a browser as NO_CODE_PENDING, not as
// CODE_EXPIRED: the cookie a code is confirmed against lasts exactly as long
// as the code, so by the time the code has expired the browser has dropped the
// cookie, and the server cannot tell which verification is meant. CODE_EXPIRED
// is left the moment between the two. So the note NO_CODE_PENDING leaves in
// the address step says first that the code expired. It once said only that
// the server was no longer waiting for a code from this browser, and a reader
// whose code had run out was never told so.
func TestAnExpiredCodeIsSaidToHaveExpired(t *testing.T) {
	script := appScript(t)
	h := verifyServer(t, nil)

	if status, out := h.requestCode("late@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request code: %d %s", status, out.Error)
	}
	sent := h.mail.last(t)
	var pending *http.Cookie
	for _, c := range (&http.Response{Header: h.lastHeader}).Cookies() {
		if c.Name == pendingCookieName {
			pending = c
		}
	}
	if pending == nil {
		t.Fatal("requesting a code set no pending cookie")
	}
	if ttl := int(h.app.cfg.VerifyCodeTTL.Seconds()); pending.MaxAge != ttl {
		t.Errorf("the pending cookie lasts %ds and the code %ds; this test, and the note it holds the page to, "+
			"rest on the browser dropping the cookie as the code expires", pending.MaxAge, ttl)
	}

	// The code expires, and the browser drops the cookie with it.
	h.exec(`UPDATE verification_pending SET expires_at = now() - interval '1 minute'`)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	h.http.Jar = jar
	status, out := h.confirmCode(sent.Code)
	if want := jsString(t, script, "NO_CODE_PENDING"); status != http.StatusBadRequest || out.Error != want {
		t.Errorf("an expired code from a browser that has dropped its cookie is answered %d %q; want 400 %q",
			status, out.Error, want)
	}

	note := regexp.MustCompile(`(?m)^  \[NO_CODE_PENDING\]: "([^"]+)",$`).FindStringSubmatch(script)
	if note == nil {
		t.Fatal("CODE_ENDED_NOTES has no note for NO_CODE_PENDING")
	}
	if !strings.HasPrefix(note[1], "That code has expired") {
		t.Errorf("NO_CODE_PENDING's note is %q; an expired code is what it nearly always means, so it says that first", note[1])
	}
}

// Asking to verify another address puts the badge panel away, and with it
// "Forget this credential", while the badge itself stays held and usable until
// a new one is confirmed. Both steps of replacing it — the address and the
// code — offer to keep it, which brings the panel back; without that the only
// way back was a reload.
func TestReplacingABadgeCanGoBackToIt(t *testing.T) {
	script := appScript(t)
	view := withoutComments(jsFunction(t, script, "verifyViewHTML"))

	if !strings.Contains(view, "const keep = v.badge\n") {
		t.Fatal("verifyViewHTML no longer offers to keep a held badge, and only a held one")
	}
	code := strings.Index(view, `if (verifyStep.step === "code") {`)
	panel := strings.Index(view, "if (v.badge && !verifyStep.replacing) {")
	address := strings.Index(view, `data-verify="request"`)
	if code < 0 || panel < 0 || address < 0 || !(code < panel && panel < address) {
		t.Fatal("verifyViewHTML no longer draws the code step, the badge panel and the address step in that order")
	}
	if n := strings.Count(view[code:panel], "${keep}"); n != 1 {
		t.Errorf("the code step offers to keep the badge %d times; want once", n)
	}
	if n := strings.Count(view[address:], "${keep}"); n != 1 {
		t.Errorf("the address step offers to keep the badge %d times; want once", n)
	}

	handler := regexp.MustCompile(`(?s)if \(event\.target\.closest\('\[data-action="verify-keep"\]'\)\) \{(.*?)\n  \}`).
		FindStringSubmatch(withoutComments(script))
	if handler == nil {
		t.Fatal(`app.js has no click handler for data-action="verify-keep"`)
	}
	// Back to the badge panel: the address step, not asked for over the badge.
	if !strings.Contains(handler[1], `verifyStep = { step: "email", label: "" };`) {
		t.Errorf("keeping the badge does not go back to the badge panel:%s", handler[1])
	}
}

// Verification requests can outlast the page that sent them: a reader who
// presses Send code or Verify and then follows a link is on another page when
// the answer comes. Every answer submitVerification handles — the code sent,
// the badge issued, a refusal — draws on the Verify page only when it is still
// the one on screen (onVerifyPage); otherwise it only sets verifyStep, so that
// coming back shows the step it left. Drawing regardless once put the Verify
// address form over the board list a reader had gone to, under that page's
// address and title. Keep my badge, Verify another address and Use a different
// address ask it too: one pressed on a Verify page the reader has just left,
// still on screen while the next page loads, does nothing, while the forget
// buttons still forget there and draw nothing (renderVerify).
//
// onVerifyPage asks that and nothing else, and is held whole: every answer
// and renderVerify draw only when it says yes, so a held badge weighed in it
// would keep a pending code's box off the page with every drawing pinned in
// place. Checked only for the route test it contains, it passed with one.
func TestVerifyAnswersDrawOnlyOnTheVerifyPage(t *testing.T) {
	script := appScript(t)
	if got, want := jsLines(jsFunction(t, script, "onVerifyPage")), []string{
		"function onVerifyPage() {",
		`return state.route.view === "verify";`,
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("onVerifyPage runs\n\t%s\nwant it to ask whether the page on screen is the Verify page, and nothing else:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}

	submit := withoutComments(jsFunction(t, script, "submitVerification"))
	// Each stretch of code that runs once an answer is in: after an await of
	// the API, or of sendWithCaptcha, which sends a code request once its
	// anti-spam puzzle is solved, or in the catch that takes its refusal. The
	// age refusal before the first one is drawn before anything is sent, while
	// the page is certainly on screen.
	resumes := regexp.MustCompile(`await api\(|await sendWithCaptcha\(|\} catch \(err\) \{`).FindAllStringIndex(submit, -1)
	if len(resumes) != 3 {
		t.Fatalf("submitVerification resumes after an answer in %d places; this test knows three "+
			"(the code sent, the badge issued, the catch)", len(resumes))
	}
	draws := regexp.MustCompile(`el\.main\.innerHTML\s*=|drawVerifyStep\(|renderVerify\(|showNotice\(|refuseVerification\(|` +
		`sayVerifyRefusal\(|\.focus\(|scrollIntoView\(`)
	for i, r := range resumes {
		end := len(submit)
		if i+1 < len(resumes) {
			end = resumes[i+1][0]
		}
		stretch := submit[r[0]:end]
		draw := draws.FindStringIndex(stretch)
		if draw == nil {
			t.Errorf("stretch %d of submitVerification draws nothing; this test expects each answer to be shown:\n%s", i+1, stretch)
			continue
		}
		at := strings.Index(stretch, "if (!onVerifyPage())")
		if at < 0 || at > draw[0] {
			t.Errorf("stretch %d of submitVerification draws (%q) without first checking that the Verify page is "+
				"still on screen:\n%s", i+1, stretch[draw[0]:draw[1]], stretch)
		}
	}

	// An issued badge is drawn through renderVerify, which asks the server
	// whether verification is on before it draws: one more request the reader
	// can leave during, so it asks onVerifyPage again once that is in
	// (TestVerifyStepIsDrawnForAReaderArriving holds it whole), and the
	// confirmation after forgetting the browser waits on what it says.
	if !strings.Contains(withoutComments(script), "renderVerify().then((drawn) => {\n      if (drawn) confirmBrowserForgotten();") {
		t.Error("forgetting the browser confirms it whether or not the Verify page was drawn")
	}

	// The step buttons draw only on the Verify page too. Keep my badge, Use a
	// different address and Verify another address, pressed on a Verify page
	// the reader has just left, still on screen while the next page loads, do
	// nothing, as the forget buttons draw nothing there (renderVerify): a step
	// drawn there would cover the page they went to, and a Send code pressed on
	// it would go from that page. Every drawing of a step is one of these,
	// renderVerify's or an answer's, and asks first.
	code := withoutComments(script)
	for name, body := range map[string]string{
		"Keep my badge": verifyClickHandler(t, script, `if (event.target.closest('[data-action="verify-keep"]')) {`),
		"Verify another address and Use a different address": verifyClickHandler(t, script, `if (restart) {`),
	} {
		if !strings.HasPrefix(body, "\n    event.preventDefault();\n    if (!onVerifyPage()) return;\n") ||
			strings.Count(body, "drawVerifyStep();") != 1 {
			t.Errorf("%s draws a Verify step without first checking that the Verify page is on screen:%s", name, body)
		}
	}
	if strings.Count(code, "drawVerifyStep();") != 5 || strings.Count(submit, "drawVerifyStep();") != 2 ||
		strings.Count(withoutComments(jsFunction(t, script, "renderVerify")), "drawVerifyStep();") != 1 {
		t.Error("a Verify step is drawn somewhere besides renderVerify, the two answers of submitVerification that " +
			"draw one, Keep my badge and the two buttons that start again, which this test reads")
	}
}

// render() draws the Verify page for a reader arriving at it — from another
// page, by Back, or by Verify in the header while already there — through
// renderVerify, which asks the server one thing before it draws. An answer can
// land while it asks and draw its step in place: the code box once a code is
// sent, the address step once the server drops the code being typed, the badge
// once it is issued; so can a step button on the page being redrawn. That
// drawing was then drawn over, by the loading skeleton that render() puts up
// when a page is slow, or by the page the reader had just left, its own answer
// landing late. renderVerify, which drew nothing once the step had moved while
// it asked, then left the skeleton or that page under the Verify address, and
// a pending code had nowhere to be typed until the reader went somewhere else.
//
// So every drawing of a Verify step goes through drawVerifyStep, which notes
// the step and the drawing; renderVerify leaves a step it did not draw alone
// only while that drawing is still on the page (verifyStepOnScreen), and draws
// it again otherwise; render() calls it with no condition of its own; and the
// skeleton stands in only for the view it dims, never for one drawn since.
// What renderVerify leaves alone is what it once wiped: Forget this browser's
// confirmation, said while an issued badge's late redraw was on its way. And
// the page is titled whichever way it goes, since render() has drawn nothing
// else for it.
func TestVerifyStepIsDrawnForAReaderArriving(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	holdLines(t, "render()'s Verify page", jsLines(jsFunction(t, script, "render")), `case "verify":`, "break;",
		[]string{"await renderVerify();"})

	if got, want := jsLines(jsFunction(t, script, "renderVerify")), []string{
		"async function renderVerify() {",
		"const step = verifyStep;",
		"const route = state.route;",
		"await loadVerification();",
		"if (!onVerifyPage() || overtaken(route)) return false;",
		`announce("Institutional verification");`,
		"if (verifyStep !== step && verifyStepOnScreen()) return false;",
		"drawVerifyStep();",
		`const form = el.main.querySelector("form[data-verify]");`,
		"if (form) sayVerifyRefusal(form);",
		"return true;",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("renderVerify runs\n\t%s\nwant exactly\n\t%s\nIt draws the step the page is on unless the reader has left, "+
			"or a newer render has begun, or that step was drawn in place since it asked and is still on screen, and it "+
			"decides nothing else: a "+
			"gate on a held badge here once hid a pending code's box",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	// Why a badge went (badgeLost) is said in the step drawn, so it is put
	// away once a step is drawn, wherever that was done from
	// (TestAGoneBadgeIsSaidToHaveGone); it changes nothing in what was drawn.
	// Nor does what /verify, drawn in place of another page, takes from it:
	// that page's thread and its open image, and what the page on screen was
	// drawn for (drawnFor, TestARequestGoesOnlyFromAFormOnScreen).
	if got, want := jsLines(jsFunction(t, script, "drawVerifyStep")), []string{
		"function drawVerifyStep() {",
		"el.main.innerHTML = verifyViewHTML();",
		"verifyShown = { step: verifyStep, node: el.main.firstElementChild, at: performance.now() };",
		"state.thread = null;",
		"expandedLink = null;",
		"if (onVerifyPage()) {",
		"drawnFor = { path: renderedPath, route: state.route, thread: null };",
		"}",
		`badgeLost = "";`,
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("drawVerifyStep runs\n\t%s\nwant it to draw the step, note the step, the drawing and when, forget the "+
			"page it replaced and note what it drew for while the Verify page is on screen, and put away what the step "+
			"said about a badge gone:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	if got, want := jsLines(jsFunction(t, script, "verifyStepOnScreen")), []string{
		"function verifyStepOnScreen() {",
		"return verifyShown.step === verifyStep && el.main.contains(verifyShown.node);",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("verifyStepOnScreen runs\n\t%s\nwant it to ask whether the drawing made for the step the page is on "+
			"is still in the page:\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	// Every drawing of a step goes through drawVerifyStep, so that what it
	// notes is what is on the page.
	if !strings.Contains(code, "\nlet verifyShown = { step: null, node: null, at: -Infinity };\n") {
		t.Error("verifyShown does not start with no step drawn")
	}
	if n := len(regexp.MustCompile(`\bverifyShown\s*=[^=]`).FindAllString(code, -1)); n != 2 {
		t.Errorf("app.js sets verifyShown %d times; want two: its declaration, and in drawVerifyStep", n)
	}
	if n := len(regexp.MustCompile(`\bverifyViewHTML\(\)`).FindAllString(code, -1)); n != 2 {
		t.Errorf("app.js names verifyViewHTML() %d times; want two: its definition, and the one draw, in drawVerifyStep", n)
	}

	// The skeleton replaces the view it was put up for, and nothing drawn
	// since: a step drawn in place while the page loaded keeps the field it
	// put the reader in.
	loading := jsLines(jsFunction(t, script, "beginLoading"))
	dimmed := slices.Index(loading, "const dimmed = el.main.firstChild;")
	timer := slices.Index(loading, "loadingTimer = setTimeout(() => {")
	if dimmed < 0 || timer != dimmed+1 || len(loading) < timer+3 ||
		loading[timer+1] != "if (begun === rendersBegun && el.main.firstChild === dimmed && skeleton()) {" ||
		!strings.HasPrefix(loading[timer+2], `el.main.innerHTML = '<div class="skeleton">`) {
		t.Errorf("beginLoading's skeleton does not first check that the page still shows the view it dims:\n\t%s",
			strings.Join(loading, "\n\t"))
	}
	if n := strings.Count(code, `class="skeleton"`); n != 1 {
		t.Errorf("app.js draws the skeleton in %d places; this test knows beginLoading's", n)
	}
}

// When the server drops the code being typed, the code box gives way to the
// address step, and the reason is said in that step: at the top of its form,
// where the box was, read out with the address field and scrolled into view.
// Said only in the notice, it was a screen above the form on a phone, so the
// box turned into an empty address form with nothing on screen to say why.
func TestEndedCodeSaysWhyWhereTheBoxWas(t *testing.T) {
	script := appScript(t)
	notes := regexp.MustCompile(`(?s)\nconst CODE_ENDED_NOTES = \{\n(.*?)\n\};\n`).FindStringSubmatch(script)
	if notes == nil {
		t.Fatal("app.js has no CODE_ENDED_NOTES saying why the code box went")
	}
	for _, name := range []string{"NO_CODE_PENDING", "CODE_EXPIRED", "CODE_GUESSES_SPENT"} {
		if !regexp.MustCompile(`(?m)^  \[` + name + `\]: "[A-Z][^"]+\.",$`).MatchString(notes[1]) {
			t.Errorf("CODE_ENDED_NOTES has no sentence for %s:\n%s", name, notes[1])
		}
	}

	view := withoutComments(jsFunction(t, script, "verifyViewHTML"))
	if !strings.Contains(view, `<p class="verify-ended" id="f-email-ended">${esc(verifyStep.ended)}`) {
		t.Error("verifyViewHTML no longer draws verifyStep.ended, escaped, as the f-email-ended note")
	}
	if !strings.Contains(view, `const endedRef = verifyStep.ended ? "f-email-ended f-verify-refused" : "f-verify-refused";`) {
		t.Error("the address field is no longer described by the note, so a screen reader would not hear why")
	}
	address := strings.Index(view, `data-verify="request"`)
	if address < 0 {
		t.Fatal("verifyViewHTML no longer draws the address step")
	}
	form := view[address:]
	if end := strings.Index(form, "</form>"); end >= 0 {
		form = form[:end]
	}
	note, field := strings.Index(form, "${ended}"), strings.Index(form, `id="f-email"`)
	if note < 0 || field < 0 || note > field {
		t.Error("the address step does not say why the code box went at the top of its form, above the address field")
	}
	if !strings.Contains(form, `required aria-describedby="${endedRef}" />`) {
		t.Error("the address field does not carry endedRef")
	}

	submit := withoutComments(jsFunction(t, script, "submitVerification"))
	if !strings.Contains(submit, `ended: CODE_ENDED_NOTES[err.message] };`) {
		t.Error("submitVerification does not keep the reason with the step it goes back to")
	}
	at := strings.LastIndex(submit, "if (ended) {")
	if at < 0 {
		t.Fatal("submitVerification no longer draws the address step when no code is pending")
	}
	branch := submit[at:]
	branch = branch[:strings.Index(branch, "return;")]
	for _, want := range []string{`getElementById("f-email-ended")`, `scrollIntoView(`, `focus({ preventScroll: true })`} {
		if !strings.Contains(branch, want) {
			t.Errorf("going back to the address step does not bring the reason into view (no %s):\n%s", want, branch)
		}
	}
	if strings.Contains(branch, "showNotice(") {
		t.Errorf("going back to the address step says why in the notice as well, twice on a desktop screen:\n%s", branch)
	}
}

// The buttons that change the Verify page's step in place put away the notice,
// which was about the step being left: "that code is not right" stayed over a
// fresh address form after "Use a different address".
func TestVerifyStepButtonsClearTheNotice(t *testing.T) {
	script := withoutComments(appScript(t))
	for _, opener := range []string{
		`if (event.target.closest('[data-action="verify-keep"]')) {`,
		`if (restart) {`,
	} {
		m := regexp.MustCompile(`(?s)\n  ` + regexp.QuoteMeta(opener) + `(.*?)\n  \}\n`).FindStringSubmatch(script)
		if m == nil {
			t.Errorf("app.js has no click handler opening %q", opener)
			continue
		}
		if !strings.Contains(m[1], "clearNotice();") {
			t.Errorf("the handler opening %q leaves the notice up:%s", opener, m[1])
		}
	}
}

// The Verify page draws each step in place, so the button just pressed is
// gone from under the reader's finger and another can be drawn where it was,
// for the second tap of a double tap to press: on a phone, Use a different
// address and then Keep my badge, which undid the reader's choice; on a wide
// screen, Verify another address and then Forget this browser, which took the
// badge, the post key and the name. So every button the page draws, each of
// which moves it to another step or forgets what the browser holds, lets a
// press go for VERIFY_SETTLE_MS once a step has been drawn: drawVerifyStep,
// which every drawing of a step goes through, notes when
// (TestVerifyStepIsDrawnForAReaderArriving holds it whole), and the page's
// click listener lets the press go before any of their handlers runs. So does
// the 18+ attestation, the one box the page draws, which the reader alone
// ticks (TestBadgeAttestationIsNeverPreselectedOrKept): a double click on Use
// a different address ticked it, the address step having been brought clear
// of the header and its label moved under the pointer. A refusal said in a
// step's form moves the step too, though nothing is drawn, and starts the
// wait again (refuseVerification). CI runs no browser, so
// the time is held to a span that covers a double tap and a second tap from a
// reader who saw nothing change, and no more than a second, which a reader
// who meant to press a button just drawn need not wait out.
func TestVerifyButtonsLetGoAPressAimedAtTheStepBefore(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	m := regexp.MustCompile(`(?m)^const VERIFY_SETTLE_MS = (\d+);$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal("app.js has no number constant VERIFY_SETTLE_MS, how long a Verify button lets a press go once a step " +
			"is drawn")
	}
	if settle, _ := strconv.Atoi(m[1]); settle < 400 || settle > 1000 {
		t.Errorf("VERIFY_SETTLE_MS is %d; want 400 to 1000: a double tap's second press came 80 to 300 ms after "+
			"the first, and a second press from a reader who saw nothing change can come later", settle)
	}

	// The buttons it covers are every button the Verify page draws with an
	// action: Keep my badge, Use a different address, Verify another
	// address, Forget this credential and Forget this browser.
	drawn := map[string]bool{}
	for _, f := range []string{"verifyViewHTML", "forgetBrowserHTML"} {
		for _, b := range regexp.MustCompile(`<button[^>]*\bdata-action="([\w-]+)"`).FindAllStringSubmatch(
			withoutComments(jsFunction(t, script, f)), -1) {
			drawn[b[1]] = true
		}
	}
	moves := regexp.MustCompile(`(?s)\nconst VERIFY_MOVES = \[\n(.*?)\n\]\.join\(", "\);\n`).FindStringSubmatch(code)
	if moves == nil {
		t.Fatal("app.js has no VERIFY_MOVES, the list of the Verify page's buttons, as this test reads it")
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(moves[1], "\n") {
		a := regexp.MustCompile(`^  '\[data-action="([\w-]+)"\]',$`).FindStringSubmatch(line)
		if a == nil {
			t.Errorf("VERIFY_MOVES holds %q; want one data-action selector a line", line)
			continue
		}
		listed[a[1]] = true
	}
	for _, action := range []string{"verify-keep", "verify-restart", "verify-again", "forget-verification", "forget-browser"} {
		if !drawn[action] {
			t.Errorf("the Verify page no longer draws a button for %s; this test expects it to", action)
		}
	}
	for action := range drawn {
		if !listed[action] {
			t.Errorf("the Verify page draws a button for %s that VERIFY_MOVES leaves out, so a press aimed at the "+
				"step before can land on it", action)
		}
	}
	for action := range listed {
		if !drawn[action] {
			t.Errorf("VERIFY_MOVES lists %s, which the Verify page does not draw", action)
		}
	}

	// The attestation is the one box the page draws, wherever its markup comes
	// from, and it sits inside the label VERIFY_ATTESTATION names, with no
	// other label naming it: a press anywhere on the box or its sentence is
	// then a press on that label, which pressedTooSoon lets go. A box drawn
	// outside it, or ticked through another label, could be ticked by a press
	// aimed at the step before.
	if !strings.Contains(code, "\nconst VERIFY_ATTESTATION = \"label.attest\";\n") {
		t.Error(`app.js has no VERIFY_ATTESTATION = "label.attest", the attestation's label, as this test reads it`)
	}
	markup := ""
	for _, f := range []string{"verifyViewHTML", "forgetBrowserHTML", "badgeLostHTML"} {
		markup += withoutComments(jsFunction(t, script, f))
	}
	for _, s := range jsTopLevel(code) {
		if strings.HasPrefix(s.first, "const VERIFY_INTRO = ") || strings.HasPrefix(s.first, "const VERIFY_REFUSED_HTML = ") {
			markup += s.text
		}
	}
	boxes := regexp.MustCompile(`<input[^>]*\btype="(?:checkbox|radio)"[^>]*>`).FindAllStringIndex(markup, -1)
	if len(boxes) != 1 {
		t.Errorf("the Verify page draws %d boxes; this test knows one, the 18+ attestation", len(boxes))
	}
	for _, at := range boxes {
		box := markup[at[0]:at[1]]
		open := strings.LastIndex(markup[:at[0]], "<label")
		var label string
		if open >= 0 && !strings.Contains(markup[open:at[0]], "</label>") {
			label = markup[open : open+strings.Index(markup[open:], ">")+1]
		}
		if !regexp.MustCompile(`^<label class="attest"[\s>]`).MatchString(label) {
			t.Errorf("the Verify page draws %s outside a label.attest, so a press aimed at the step before can tick it", box)
			continue
		}
		id := regexp.MustCompile(`\bid="([\w-]+)"`).FindStringSubmatch(box)
		if id == nil {
			t.Errorf("the Verify page draws %s with no id, as this test reads it", box)
			continue
		}
		if n := strings.Count(code, `for="`+id[1]+`"`) - strings.Count(label, `for="`+id[1]+`"`); n != 0 {
			t.Errorf("app.js names %s in %d labels besides the label.attest around it; a press on one of them "+
				"ticks it without being let go", id[1], n)
		}
	}

	// A press on one of them that soon is one pressedTooSoon says yes to, by
	// when the reader pressed, and the page's click listener lets it go, doing
	// nothing else, before it handles any of them; let go on the attestation,
	// a click leaves the box as it was. Its mousedown is let go too, so the
	// focus stays where the step just drawn put it: the button the press
	// landed on took it otherwise, and on a wide screen that was Forget this
	// browser, for the reader's next Enter to press.
	if got, want := jsLines(jsFunction(t, script, "pressedTooSoon")), []string{
		"function pressedTooSoon(event) {",
		"const aimed = event.target.closest(VERIFY_MOVES) || event.target.closest(VERIFY_ATTESTATION);",
		"return !!aimed && event.timeStamp - verifyShown.at < VERIFY_SETTLE_MS;",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("pressedTooSoon runs\n\t%s\nwant it to say whether a press is on one of the Verify page's buttons, "+
			"or on its attestation, within VERIFY_SETTLE_MS of a step being drawn:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	for _, name := range []string{"VERIFY_MOVES", "VERIFY_ATTESTATION"} {
		if n := strings.Count(code, name); n != 2 {
			t.Errorf("app.js names %s %d times; want two, its declaration and pressedTooSoon's", name, n)
		}
	}
	listener := pageClickListener(t, script)
	const guard = "if (pressedTooSoon(event)) {"
	holdLines(t, "the press let go", jsLines(listener), guard, "}", []string{"event.preventDefault();", "return;"})
	at := strings.Index(listener, "\n  "+guard+"\n")
	if at < 0 {
		t.Fatalf("the page's click listener does not open, at its own level, %q", guard)
	}
	for action := range drawn {
		if first := strings.Index(listener, `data-action="`+action+`"`); first >= 0 && first < at {
			t.Errorf("the page's click listener handles %s before it lets a press aimed at the step before go", action)
		}
	}
	const pressed = "\ndocument.addEventListener(\"mousedown\", (event) => {\n" +
		"  if (pressedTooSoon(event)) event.preventDefault();\n" +
		"});\n"
	if !strings.Contains(code, pressed) {
		t.Errorf("app.js does not let go the mousedown of a press pressedTooSoon lets go, as\n%s", pressed)
	}
	if n := strings.Count(code, "pressedTooSoon("); n != 3 {
		t.Errorf("app.js names pressedTooSoon( %d times; want three: its declaration, the mousedown and the click", n)
	}

	// When a step was drawn: verifyShown says so from the first drawing on,
	// in the clock an event's timeStamp is read in, and before any drawing
	// no press is let go.
	if !strings.Contains(code, "\nlet verifyShown = { step: null, node: null, at: -Infinity };\n") {
		t.Error("verifyShown does not start with no step drawn, at no time")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "drawVerifyStep")),
		"verifyShown = { step: verifyStep, node: el.main.firstElementChild, at: performance.now() };") {
		t.Error("drawVerifyStep does not note when it drew the step")
	}

	// A refusal said in a step's form moves the step under the reader's
	// finger, though nothing is drawn: the sentence opens above the buttons,
	// and the form is brought clear of the header, which scrolls the page
	// down or up. So refuseVerification starts the wait again once it has
	// done both, before anything in it can return. On a phone, the second tap
	// of a double tap on Verify, a wrong code refused, pressed Keep my badge,
	// which the scroll had brought up into Verify's place, and the code box
	// went with the code still pending; and the second tap on Send code with
	// the 18+ box unticked ticked it, its label brought down into Send code's
	// place, under the refusal saying why it had to be ticked.
	refuse := jsLines(jsFunction(t, script, "refuseVerification"))
	const restart = "verifyShown.at = performance.now();"
	shown := slices.Index(refuse, "revealClearOfHeader(form);")
	if shown < 0 || slices.Index(refuse, restart) != shown+1 {
		t.Errorf("refuseVerification runs\n\t%s\nwant it to start the wait again (%s) straight after it brings the "+
			"form, with the refusal said in it, into view clear of the header", strings.Join(refuse, "\n\t"), restart)
	}
	if n := strings.Count(code, "verifyShown.at ="); n != 1 {
		t.Errorf("app.js sets verifyShown.at alone %d times; want one, in refuseVerification: a drawing notes "+
			"when with the step and the drawing (drawVerifyStep)", n)
	}
	// Everything else that scrolls the Verify page clear of the header does
	// it straight after a step is drawn, within the wait that drawing started
	// (TestVerifyStepsAreShownClearOfTheHeader): showVerifyField,
	// showHeldBadge, confirmBrowserForgotten and Forget this credential's
	// branch of the page's click listener. A new place that scrolls it is one
	// to ask whether it moves a step already on screen, as a refusal does.
	var scrollers []string
	for name, callees := range jsCallGraph(t, script) {
		if callees["revealClearOfHeader"] {
			scrollers = append(scrollers, name)
		}
	}
	slices.Sort(scrollers)
	want := []string{"confirmBrowserForgotten", "refuseVerification", "showHeldBadge", "showVerifyField"}
	if !slices.Equal(scrollers, want) {
		t.Errorf("the functions that bring something on the Verify page into view clear of the header are %v; this "+
			"test knows %v, of which refuseVerification alone moves a step already on screen and starts the wait "+
			"again", scrollers, want)
	}
	if n := strings.Count(code, "revealClearOfHeader("); n != 6 {
		t.Errorf("app.js names revealClearOfHeader( %d times; this test knows six: its declaration, the four "+
			"functions above and Forget this credential's branch of the page's click listener", n)
	}
}

// pageClickListener is the page's own click listener, the one that handles
// its buttons and links; the text spoiler's listener, registered before it,
// opens the same way.
func pageClickListener(t *testing.T, script string) string {
	t.Helper()
	const opener = "\ndocument.addEventListener(\"click\", (event) => {\n  if (event.defaultPrevented || event.button !== 0"
	start := strings.Index(script, opener)
	if start < 0 || strings.Count(script, opener) != 1 {
		t.Fatal("app.js has no one click listener for the page's own buttons and links, as this test reads it")
	}
	end := strings.Index(script[start:], "\n});\n")
	if end < 0 {
		t.Fatal("the page's click listener does not close where this test can find it")
	}
	return script[start+1 : start+end+len("\n});")]
}

// verifyClickHandler returns the body of the click handler that opens with
// opener, a line indented once in the page's click listener.
func verifyClickHandler(t *testing.T, script, opener string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\n  ` + regexp.QuoteMeta(opener) + `(.*?)\n  \}\n`).FindStringSubmatch(withoutComments(script))
	if m == nil {
		t.Fatalf("app.js has no click handler opening %q", opener)
	}
	return m[1]
}

// A Verify answer belongs to the step it was sent from. The reader can move
// the page to another step while one is on its way — Forget this browser sits
// outside the form and stays live — and an answer that then acted on the step
// it left undid the reader's choice: a badge stored after the browser was
// forgotten, under the confirmation that it held none, for whoever used the
// computer next; the code box back over a badge the reader had chosen to keep;
// that badge replaced by the new one. So submitVerification notes verifyStep
// before it sends, and each answer is dropped, before it stores, sets or draws
// anything, once verifyStep is no longer that object. The answers are a code
// request's, once its puzzle is solved and the request sent
// (sendWithCaptcha), a confirmation's, and a refusal of either. That only
// works while every change of step is a new object, never an edit of the old
// one, and the places that make one are the ones this test lists. Three marks
// on a step say what is on its way from it, not which step it is: its puzzle
// being solved (solving), its confirmation out (confirming), and that
// confirmation found spent by another tab's verification (spentElsewhere).
// The form's own other buttons wait on the answer with its submit button — in
// the form that sent it, and in the step drawn again for a reader who leaves
// the page and comes back before the answer.
func TestVerifyAnswersBelongToTheStepTheyWereSentFrom(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	submit := withoutComments(jsFunction(t, script, "submitVerification"))

	noted := strings.Index(submit, "const sentFrom = verifyStep;")
	first := -1
	if at := regexp.MustCompile(`await (api|sendWithCaptcha)\(`).FindStringIndex(submit); at != nil {
		first = at[0]
	}
	if noted < 0 || first < 0 || noted > first {
		t.Fatal("submitVerification does not note the step it is sent from before its first request")
	}
	// Each stretch starts where its answer is in: past the whole of the code
	// request's call, whose callbacks label the button while the puzzle is
	// solved, which is not an answer.
	resumes := regexp.MustCompile(`await api\(|(?s)await sendWithCaptcha\(.*?\n\s*onScreen,\n\s*\);|\} catch \(err\) \{`).
		FindAllStringIndex(submit, -1)
	if len(resumes) != 3 || !strings.HasPrefix(submit[resumes[0][0]:], "await sendWithCaptcha(") {
		t.Fatalf("submitVerification resumes after an answer in %d places; this test knows three "+
			"(the code sent, the badge issued, the catch)", len(resumes))
	}
	acts := regexp.MustCompile(`storeCredential\(|state\.verification|verifyStep\s*=[^=]|verifyRefusal\s*=[^=]|` +
		`el\.main\.innerHTML\s*=|drawVerifyStep\(|renderVerify\(|showNotice\(|refuseVerification\(|sayVerifyRefusal\(|` +
		`offerHeldBadge\(|codeSpentElsewhere\(|\.focus\(|scrollIntoView\(|\.disabled\s*=|\.textContent\s*=`)
	const check = "if (verifyStep !== sentFrom) return;"
	for i, r := range resumes {
		end := len(submit)
		if i+1 < len(resumes) {
			end = resumes[i+1][0]
		}
		stretch := submit[r[1]:end]
		act := acts.FindStringIndex(stretch)
		if act == nil {
			t.Errorf("stretch %d of submitVerification does nothing with its answer; this test expects each to:\n%s", i+1, stretch)
			continue
		}
		if at := strings.Index(stretch, check); at < 0 || at > act[0] {
			t.Errorf("stretch %d of submitVerification acts on its answer (%q) without first checking that the page is "+
				"still on the step it was sent from:\n%s", i+1, stretch[act[0]:act[1]], stretch)
		}
	}

	marks := map[string]bool{".solving": true, ".confirming": true, ".spentElsewhere": true}
	for _, m := range regexp.MustCompile(`\b(?:verifyStep|sentFrom)(\.\w+|\[[^\]]*\])\s*[-+]?=[^=]|Object\.assign\(\s*(?:verifyStep|sentFrom)\b`).
		FindAllStringSubmatch(code, -1) {
		if !marks[m[1]] {
			t.Errorf("app.js edits verifyStep in place (%q); a change of step has to be a new object, or an answer "+
				"from the step that was left is taken for the new one", m[0])
		}
	}
	assigned := verifyStepSet.FindAllString(code, -1)
	if len(assigned) == 0 {
		t.Fatal("app.js never sets verifyStep; this test reads the assignments")
	}
	for _, a := range assigned {
		if !regexp.MustCompile(`^verifyStep\s*=\s*\{`).MatchString(a) {
			t.Errorf("verifyStep is set as %q; every step is a new object literal", a)
		}
	}
	// Every place that moves the page to a step, and the step it moves to: the
	// step the page starts on, the three answers, the three buttons that move
	// the page while an answer may be on its way, a forget of the browser, here
	// or heard from another tab (forgetOnThisPage), and another tab's
	// verification of a badge, which spends the code asked for here
	// (codeSpentElsewhere). Forget this browser moves the page through
	// forgetOnThisPage, and sets no step of its own. A new place has to be
	// added here, with its reason: a gate on a held badge that set the address
	// step after a code was sent, or in renderVerify's redraw, once took a
	// badge holder's code box away with every other check passing.
	if !strings.Contains(code, "\nlet "+stepToAddress+"\n") {
		t.Errorf("verifyStep does not start as %q", stepToAddress)
	}
	places := []struct {
		name, body string
		want       []string
	}{
		{"submitVerification", submit, []string{stepToCode, stepToAddress, stepToEnded}},
		{"Forget this browser", verifyClickHandler(t, script, `if (event.target.closest('[data-action="forget-browser"]')) {`),
			nil},
		{"forgetOnThisPage", withoutComments(jsFunction(t, script, "forgetOnThisPage")), []string{stepToAddress}},
		{"codeSpentElsewhere", withoutComments(jsFunction(t, script, "codeSpentElsewhere")), []string{stepToAddress}},
		{"Forget this credential", verifyClickHandler(t, script, `if (event.target.closest('[data-action="forget-verification"]')) {`),
			[]string{stepToAddress}},
		{"Keep my badge", verifyClickHandler(t, script, `if (event.target.closest('[data-action="verify-keep"]')) {`),
			[]string{stepToAddress}},
		{"Verify another address and Use a different address", verifyClickHandler(t, script, `if (restart) {`),
			[]string{stepToReplacing}},
	}
	known := 1
	for _, p := range places {
		known += len(p.want)
		if got := verifyStepSet.FindAllString(p.body, -1); !slices.Equal(got, p.want) {
			t.Errorf("%s sets verifyStep as %q; want %q, so an answer on its way is not taken for the step "+
				"the page moved to, and the page moves nowhere else", p.name, got, p.want)
		}
	}
	if len(assigned) != known {
		t.Errorf("app.js sets verifyStep in %d places; this test knows %d: %q", len(assigned), known, assigned)
	}

	// Keep my badge and Use a different address are in the form, and wait.
	disabled := strings.Index(submit, "const buttons = [...form.querySelectorAll(\"button\")];\n  for (const b of buttons) b.disabled = true;")
	if disabled < 0 || disabled > first {
		t.Error("submitVerification does not hold the form's other buttons while its request is on its way")
	}
	// They wait in the step drawn again too. A reader who leaves the page and
	// comes back while the request is out gets the step drawn afresh, so the
	// wait is kept outside the form, in verifyBusy, from before the request
	// until its answer is in, and verifyViewHTML draws the step waiting from
	// it. Drawn live, Keep my badge or Use a different address there dropped
	// the badge the server was issuing — on the same address, the badge held
	// here was retired with it — and a second Send code left the first code's
	// box up over the second's. No second request goes out from a step whose
	// first is out, and Forget this browser, outside the form, never waits:
	// it has to win.
	if !strings.Contains(code, "\nlet verifyBusy = null;\n") {
		t.Fatal("app.js does not keep verifyBusy, the step a Verify request is out from")
	}
	if !strings.HasPrefix(submit, "async function submitVerification(form) {\n  if (verifyBusy === verifyStep) return;\n") {
		t.Error("submitVerification does not first refuse to send from a step whose request is still out")
	}
	ageRefused := strings.Index(submit, "refuseVerification(form, BADGE_AGE_REFUSAL, adult, { invalid: true });\n    return;")
	busy := strings.Index(submit, "\n  verifyBusy = sentFrom;\n")
	tried := strings.Index(submit, "\n  try {\n")
	if ageRefused < 0 || busy < ageRefused || tried < busy || first < tried {
		t.Error("submitVerification does not set verifyBusy once it is sure to send, before the try that clears it")
	}
	if !regexp.MustCompile(`\n  \} finally \{\s*if \(verifyBusy === sentFrom\) verifyBusy = null;\n    release\(\);\n  \}\n\}$`).MatchString(submit) {
		t.Error("submitVerification does not end by clearing verifyBusy once its answer is in, and only its own wait, " +
			"and then letting the next request go")
	}
	if n := len(regexp.MustCompile(`\bverifyBusy\s*=[^=]`).FindAllString(code, -1)); n != 3 {
		t.Errorf("app.js sets verifyBusy %d times; want three: its declaration, and in submitVerification", n)
	}
	// One request at a time across steps, too. An answer from a step the
	// reader has left is dropped, but the pending-code cookie it carries still
	// lands, and the browser keeps whichever cookie landed last. After Forget
	// this browser mid-Send code, a second Send code from the fresh address
	// step could land first: its code box was then on screen while the server
	// checked codes against the first address, and the right code was refused
	// until the attempts ran out. So a request waits until submitVerification
	// is done with the one before it, whichever step that was from
	// (verifyRequest); it is sent only if the page is still on its step then;
	// and it lets the next one go in the finally, whatever its own answer was.
	// Nothing returns between taking its turn and the try, or the next request
	// would wait for ever.
	if !strings.Contains(code, "\nlet verifyRequest = Promise.resolve();\n") {
		t.Fatal("app.js does not keep verifyRequest, the last Verify request, for the next one to wait on")
	}
	queued := strings.Index(submit, "\n  const earlier = verifyRequest;\n")
	turn := strings.Index(submit, "\n  verifyRequest = new Promise((resolve) => {\n    release = resolve;\n  });\n")
	waited := -1
	if at := regexp.MustCompile(`\n  try \{\s*await earlier;\n    if \(verifyStep !== sentFrom\) return;\n`).FindStringIndex(submit); at != nil {
		waited = at[0]
	}
	if busy < 0 || queued < busy || turn < queued || waited < turn || first < waited {
		t.Error("submitVerification does not wait for the Verify request before it, and check that the page is " +
			"still on its step, before it sends its own")
	} else if strings.Contains(submit[queued:waited], "return") {
		t.Errorf("submitVerification can return between taking its turn and the try that hands it on:\n%s",
			submit[queued:waited])
	}
	if n := len(regexp.MustCompile(`\bverifyRequest\s*=[^=]`).FindAllString(code, -1)); n != 2 {
		t.Errorf("app.js sets verifyRequest %d times; want two: its declaration, and in submitVerification", n)
	}
	// The posting form's submit handler has a release of its own (holdForm's),
	// a local of that handler.
	if n := len(regexp.MustCompile(`\brelease\b`).FindAllString(submit, -1)); n != 3 {
		t.Errorf("submitVerification names release %d times; want three: declared, set and called", n)
	}
	if !strings.Contains(submit, "button.textContent = requesting ? VERIFY_SENDING : VERIFY_CHECKING;") {
		t.Error("submitVerification's waiting labels are not VERIFY_SENDING and VERIFY_CHECKING, which the redrawn step shows")
	}
	caught := submit[strings.Index(submit, "} catch (err) {"):]
	shown := strings.Index(caught, `const shown = form.isConnected ? form : el.main.querySelector("form[data-verify]");`)
	live := strings.Index(caught, `for (const b of shown.querySelectorAll("button")) b.disabled = false;`)
	if shown < 0 || live < shown || !strings.Contains(caught, `shown.querySelector('button[type="submit"]').textContent = label;`) {
		t.Error("a refusal that leaves the step does not bring back the buttons of the form on screen, " +
			"which is the step drawn again, waiting, for a reader who came back")
	}

	view := withoutComments(jsFunction(t, script, "verifyViewHTML"))
	for _, want := range []string{
		"const busy = verifyBusy === verifyStep;",
		`const wait = busy ? " disabled" : "";`,
		`<button class="btn primary" type="submit"${wait}>${busy ? VERIFY_CHECKING : "Verify"}</button>`,
		`<button class="btn primary" type="submit"${wait}>${busy ? (verifyStep.solving ? VERIFY_SOLVING : VERIFY_SENDING) : "Send code"}</button>`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("verifyViewHTML does not draw a step whose request is out as waiting (no %s)", want)
		}
	}
	buttons := regexp.MustCompile(`<button[^>]*>`)
	for _, step := range []string{`data-verify="confirm"`, `data-verify="request"`} {
		at := strings.Index(view, step)
		if at < 0 {
			t.Fatalf("verifyViewHTML has no form %s", step)
		}
		form := view[at:]
		form = form[:strings.Index(form, "</form>")]
		if !strings.Contains(form, "${keep}") {
			t.Errorf("the form %s no longer offers Keep my badge, which this test expects to wait with it", step)
		}
		for _, b := range buttons.FindAllString(form, -1) {
			if !strings.Contains(b, "${wait}") {
				t.Errorf("in the form %s, %s does not wait while the step's request is out", step, b)
			}
		}
	}
	keep := buttons.FindString(view[strings.Index(view, "const keep = v.badge\n"):])
	if !strings.Contains(keep, `data-action="verify-keep"`) || !strings.Contains(keep, "${wait}") {
		t.Errorf("Keep my badge does not wait while the step's request is out: %s", keep)
	}
	if forget := withoutComments(jsFunction(t, script, "forgetBrowserHTML")); strings.Contains(forget, "disabled") ||
		strings.Contains(forget, "${wait}") {
		t.Error("Forget this browser waits on a Verify request; it has to win over one")
	}

	holdVerifyRequestsToADeadline(t, script)
}

// holdVerifyRequestsToADeadline holds the wait for the Verify request before
// to an end. A Send code or a Verify whose answer never came — a connection
// gone silent — held every later request at await earlier, from whichever
// step, the fresh one Forget this browser draws included, which read Sending…
// with its buttons off until the reader reloaded. So every Verify request is
// given up once VERIFY_TIMEOUT_MS has passed: verifyDeadline's signal goes
// with it, and api() turns that signal firing, before the answer or while it
// is coming in, into the unreachable refusal, which lets the next request go.
// What submitVerification waits on once it has taken its turn is the request
// before, its own request, and renderVerify, which waits only on
// loadVerification, which waits only on its own request; each of those three
// requests carries the deadline, and api() hands it to fetch with the rest of
// what the caller passed. A code request goes through sendWithCaptcha, which
// waits on the puzzle and then on the request; the puzzle waits on its
// challenge, asked for with a deadline of its own, and on its workers, which
// give up after CAPTCHA_TIMEOUT_MS. The request's deadline is made as it is
// sent, in the call that sends it, so the puzzle's time is not counted
// against it. And the deadline is longer than the server takes to answer or
// give up, so a request given up is one whose answer was not coming: given up
// sooner, a badge the server was issuing would be lost.
func holdVerifyRequestsToADeadline(t *testing.T, script string) {
	t.Helper()
	code := withoutComments(script)
	awaited := regexp.MustCompile(`\bawait\s+([\w$.]+)`)
	for _, f := range []struct {
		name string
		want []string
	}{
		{"submitVerification", []string{"earlier", "sendWithCaptcha", "api", "renderVerify"}},
		{"sendWithCaptcha", []string{"ready", "send"}},
		{"solveCaptcha", []string{"api", "runCaptchaWorkers"}},
		{"renderVerify", []string{"loadVerification"}},
		{"loadVerification", []string{"api"}},
	} {
		var got []string
		for _, m := range awaited.FindAllStringSubmatch(withoutComments(jsFunction(t, script, f.name)), -1) {
			got = append(got, m[1])
		}
		if !slices.Equal(got, f.want) {
			t.Errorf("%s waits on %q; want %q, each bounded, or a Verify request can wait on it for ever", f.name, got, f.want)
		}
	}

	var paths []string
	for _, call := range jsCalls(t, code, "api") {
		path, ok := strings.CutPrefix(call, `api("/api/verify`)
		if !ok {
			if strings.Contains(call, "/api/verify") {
				t.Errorf("a request to /api/verify is not spelled as this test reads them: %s", call)
			}
			continue
		}
		path, _, _ = strings.Cut(path, `"`)
		paths = append(paths, "/api/verify"+path)
		if !strings.Contains(call, "signal: verifyDeadline(),") && !strings.Contains(call, "{ signal: verifyDeadline() }") {
			t.Errorf("the request to /api/verify%s is not given up in time (no signal: verifyDeadline()):\n%s", path, call)
		}
	}
	if want := []string{"/api/verify", "/api/verify/request", "/api/verify/confirm"}; !slices.Equal(paths, want) {
		t.Errorf("app.js makes Verify requests to %q; this test knows %q", paths, want)
	}
	if n := len(regexp.MustCompile(`[^\w$.]fetch\(`).FindAllString(code, -1)); n != 1 {
		t.Errorf("app.js calls fetch %d times; want once, in api(), which every Verify request goes through", n)
	}

	if got, want := jsLines(jsFunction(t, script, "verifyDeadline")), []string{
		"function verifyDeadline(stop) {",
		"const deadline = new AbortController();",
		"setTimeout(() => deadline.abort(), VERIFY_TIMEOUT_MS);",
		`if (stop) stop.addEventListener("abort", () => deadline.abort(), { once: true });`,
		"return deadline.signal;",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("verifyDeadline runs\n\t%s\nwant a signal that fires once VERIFY_TIMEOUT_MS has passed, "+
			"or when stop does:\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	if got, want := jsLines(jsFunction(t, script, "unreachableError")), []string{
		"function unreachableError() {",
		"const err = new Error(UNREACHABLE);",
		"err.unreachable = true;",
		"return err;",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("unreachableError runs\n\t%s\nwant the refusal a request with no answer is said as:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	// The signal reaches fetch only because api() builds what it hands fetch
	// by spreading the caller's options. Built field by field, or with signal
	// left out, it carried no deadline, and every check above still passed
	// while a Verify request whose answer never came again held every later
	// one, the step Forget this browser draws included, at Sending… until a
	// reload. So everything api() runs up to its fetch is held line for line.
	api := jsLines(jsFunction(t, script, "api"))
	holdLines(t, "api() up to its fetch", api, "async function api(path, options = {}) {", "res = await fetch(path, init);", []string{
		"const init = { ...options, headers: { ...(options.headers || {}) } };",
		"if (init.body !== undefined && !(init.body instanceof FormData)) {",
		`init.headers["Content-Type"] = "application/json";`,
		"}",
		"let res;",
		"try {",
	})
	holdLines(t, "api() when fetch fails", api, "res = await fetch(path, init);", "}", []string{
		"} catch {",
		"throw unreachableError();",
	})
	holdLines(t, "api() when the answer's body cannot be read", api, ": await res.json().catch(() => {", "});", []string{
		"if (init.signal && init.signal.aborted) throw unreachableError();",
		"return null;",
	})

	m := regexp.MustCompile(`(?m)^const VERIFY_TIMEOUT_MS = (\d+);$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal("app.js has no number constant VERIFY_TIMEOUT_MS; this test reads it")
	}
	deadline, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	serverSrc, err := os.ReadFile(filepath.Join("..", "..", "cmd", "server", "main.go"))
	if err != nil {
		t.Fatalf("reading the server's timeouts: %v", err)
	}
	seconds := func(name string) int {
		m := regexp.MustCompile(`\n\t\t` + name + `:\s+(\d+) \* time\.Second,\n`).FindSubmatch(serverSrc)
		if m == nil {
			t.Fatalf("cmd/server/main.go does not set %s as a number of seconds; this test reads it", name)
		}
		n, _ := strconv.Atoi(string(m[1]))
		return n
	}
	// A request's headers are read within ReadHeaderTimeout, and its answer
	// is written within WriteTimeout of that or not at all.
	server := (seconds("ReadHeaderTimeout") + seconds("WriteTimeout")) * 1000
	if deadline <= server {
		t.Errorf("VERIFY_TIMEOUT_MS is %d; the server can still be answering until %d ms, and a badge it issues "+
			"after the page has given up is one nobody holds", deadline, server)
	}
	// And not much longer: after Forget this browser, the step it draws waits
	// this long on a request whose answer is not coming.
	if deadline > server+30000 {
		t.Errorf("VERIFY_TIMEOUT_MS is %d, more than half a minute past the %d ms the server can take; a reader "+
			"waits that long behind a request whose answer is not coming", deadline, server)
	}
}

// jsCalls returns each call of the top-level function name in code, from its
// name to the parenthesis that closes it. A method call (x.name()) is not one.
// Quoted strings are skipped over, so a parenthesis in one does not count.
func jsCalls(t *testing.T, code, name string) []string {
	t.Helper()
	var calls []string
	for _, at := range regexp.MustCompile(`(?:^|[^\w$.])`+regexp.QuoteMeta(name)+`\(`).FindAllStringIndex(code, -1) {
		start := at[1] - len(name) - 1
		depth, end := 0, -1
		for i := at[1] - 1; i < len(code) && end < 0; i++ {
			switch c := code[i]; c {
			case '"', '\'', '`':
				for i++; i < len(code) && code[i] != c; i++ {
					if code[i] == '\\' {
						i++
					}
				}
			case '(':
				depth++
			case ')':
				if depth--; depth == 0 {
					end = i + 1
				}
			}
		}
		if end < 0 {
			t.Fatalf("a call of %s in app.js does not close: %.80s", name, code[start:])
		}
		calls = append(calls, code[start:end])
	}
	if len(calls) == 0 {
		t.Fatalf("app.js never calls %s; this test reads its calls", name)
	}
	return calls
}

// On a phone the notice at the top of the page is a screen above the Verify
// form, so what was said only there changed nothing in view: after a wrong
// code the box still held it, focus had fallen to the page and only the
// button's label had flickered; the 18+ refusal and Forget this browser's
// confirmation were out of sight too. Each is said where the reader is
// looking instead. A refusal goes in the form, under its fields and above its
// buttons, describes the fields, and puts the reader back in the field it is
// about, a wrong code selected to be typed again. The confirmation goes under
// the button that did it.
func TestVerifyRefusalsAreSaidInTheForm(t *testing.T) {
	script := appScript(t)
	view := withoutComments(jsFunction(t, script, "verifyViewHTML"))
	submit := withoutComments(jsFunction(t, script, "submitVerification"))

	if !strings.Contains(script, "\nconst VERIFY_REFUSED_HTML = `<p class=\"verify-refused\" id=\"f-verify-refused\" role=\"alert\" hidden></p>`;\n") {
		t.Fatal("app.js has no VERIFY_REFUSED_HTML for the Verify forms' refusals")
	}
	for _, step := range []string{`data-verify="confirm"`, `data-verify="request"`} {
		at := strings.Index(view, step)
		if at < 0 {
			t.Fatalf("verifyViewHTML has no form %s", step)
		}
		form := view[at:]
		form = form[:strings.Index(form, "</form>")]
		refused := strings.Index(form, "${VERIFY_REFUSED_HTML}")
		actions := strings.Index(form, `<div class="actions">`)
		if n := strings.Count(form, "${VERIFY_REFUSED_HTML}"); n != 1 || actions < refused ||
			strings.Contains(form[refused:], "<input") {
			t.Errorf("the form %s does not say its refusal once, under its fields and above its buttons:\n%s", step, form)
		}
		for _, input := range regexp.MustCompile(`<input[^>]*>`).FindAllString(form, -1) {
			if !strings.Contains(input, `aria-describedby="f-verify-refused"`) && !strings.Contains(input, `aria-describedby="${endedRef}"`) {
				t.Errorf("in the form %s, %s is not described by the form's refusal", step, input)
			}
		}
	}
	if !strings.Contains(view, `const endedRef = verifyStep.ended ? "f-email-ended f-verify-refused" : "f-verify-refused";`) {
		t.Error("the address field is not described by the form's refusal")
	}

	// submitVerification says nothing in the notice itself, and three of the
	// functions it calls can. storeCredential, when the browser will not store
	// a badge just issued, which nothing else on the page could say.
	// codeSpentElsewhere, for a confirmation refused once another tab has
	// verified a badge: the code step goes, and what is drawn is not that
	// step's form but the badge panel for the badge the other tab verified,
	// with no form to say it in, so it says why the code box went in the
	// notice, as it does when it hears of that badge with no confirmation out.
	// And holdForm, read as a call graph: its release switches back an image
	// picker a redraw drew switched off (redrawnPickers), which reaches the
	// notice, and a Verify form has no picker and is never carried across a
	// redraw. offerHeldBadge, which a badge just issued runs for a reader gone
	// on to a page with a posting form, says nothing then: a form whose ticked
	// box it unticks is told at its next press (badgeWithdrawn). Every other
	// refusal is said in the form. Looking for a literal showNotice( alone
	// passed while the guide said submitVerification raised no notice at all,
	// so the calls are followed to every top-level function they reach.
	if strings.Contains(submit, "showNotice(") {
		t.Errorf("submitVerification says something in the notice, a screen above the form on a phone:\n%s", submit)
	}
	if got, want := noticeCallees(t, script, submit), []string{"codeSpentElsewhere", "holdForm", "storeCredential"}; !slices.Equal(got, want) {
		t.Errorf("submitVerification calls %v, which say something in the notice; want %v, about a badge just issued, "+
			"one verified in another tab, and a picker no Verify form has", got, want)
	}
	if caught := strings.Index(submit, "} catch (err) {"); caught < 0 {
		t.Error("submitVerification has no catch for its refusals; this test reads it")
	} else if got := noticeCallees(t, script, submit[caught:]); !slices.Equal(got, []string{"codeSpentElsewhere"}) {
		t.Errorf("a refusal of a Verify request reaches %v, which say something in the notice, not in the form; "+
			"want only codeSpentElsewhere, for a code another tab's verification spent", got)
	}
	if !strings.Contains(submit, "refuseVerification(form, BADGE_AGE_REFUSAL, adult, { invalid: true });") {
		t.Error("the page's own 18+ refusal is not said under the box")
	}
	// What a refusal from the server does, statement for statement. One that
	// ends the pending code goes back to the address step and says why there
	// (TestEndedCodeSaysWhyWhereTheBoxWas). Any other is kept with the step it
	// refused (verifyRefusal) and said in the form on screen, about its field.
	// The form on screen can be the step drawn again for a reader who came
	// back while the answer was on its way; it can also be none yet, while
	// render() is still asking the server before it draws the step, and then
	// the refusal, said nowhere, was lost: the code box came back with live
	// buttons and nothing to say the code typed was wrong. So renderVerify says
	// the refusal kept with the step once it draws it, and does so again if
	// the step is drawn again on the same visit, which Verify in the header
	// does while the form is on screen. Before any of that, the form's fields
	// are let go (holdForm); a confirmation whose code another tab's
	// verification spent goes as codeSpentElsewhere says; and a code request
	// not sent, its form gone from the screen by the time its puzzle was solved
	// (sendWithCaptcha), says nothing and lets the step drawn again go.
	holdLines(t, "a refusal from the server", jsLines(submit), "} catch (err) {", "} finally {", []string{
		"unhold(false);",
		"if (verifyStep !== sentFrom) return;",
		"if (sentFrom.spentElsewhere) {",
		"codeSpentElsewhere();",
		"return;",
		"}",
		"const ended = !requesting && [NO_CODE_PENDING, CODE_EXPIRED, CODE_GUESSES_SPENT].includes(err.message);",
		"if (ended) {",
		stepToEnded,
		"}",
		"if (!onVerifyPage()) return;",
		"if (ended) {",
		"drawVerifyStep();",
		`const why = document.getElementById("f-email-ended");`,
		`if (why) why.scrollIntoView({ block: "center" });`,
		`const input = document.getElementById("f-email");`,
		"if (input) input.focus({ preventScroll: true });",
		"return;",
		"}",
		`const shown = form.isConnected ? form : el.main.querySelector("form[data-verify]");`,
		"if (err.notSent) {",
		"if (shown) {",
		`for (const b of shown.querySelectorAll("button")) b.disabled = false;`,
		`shown.querySelector('button[type="submit"]').textContent = label;`,
		"}",
		"return;",
		"}",
		"const refusedForAge = err.status === 400 && err.message === BADGE_AGE_REFUSED;",
		"verifyRefusal = {",
		"step: sentFrom,",
		`message: refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code" }),`,
		`redrawn: refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code", kept: false }),`,
		`about: refusedForAge ? "adult" : requesting ? "email" : "code",`,
		"invalid: err.status === 400,",
		"select: !requesting && err.status === 400,",
		"};",
		"if (!shown) return;",
		`for (const b of shown.querySelectorAll("button")) b.disabled = false;`,
		`shown.querySelector('button[type="submit"]').textContent = label;`,
		"sayVerifyRefusal(shown, shown === form);",
	})
	if got, want := jsLines(jsFunction(t, script, "sayVerifyRefusal")), []string{
		"function sayVerifyRefusal(form, sentIt = false) {",
		"const refused = verifyRefusal;",
		"if (!refused || refused.step !== verifyStep) return;",
		"refuseVerification(form, sentIt ? refused.message : refused.redrawn, field(form, refused.about), refused);",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("sayVerifyRefusal runs\n\t%s\nwant it to say the refusal kept with the step the page is on, and "+
			"only that one:\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	render := withoutComments(jsFunction(t, script, "renderVerify"))
	drawn, said := strings.Index(render, "drawVerifyStep();"),
		strings.Index(render, "const form = el.main.querySelector(\"form[data-verify]\");\n  if (form) sayVerifyRefusal(form);")
	if drawn < 0 || said < drawn {
		t.Errorf("renderVerify does not say the refusal kept with the step once it has drawn it:\n%s", render)
	}
	// The refusal is kept for one visit to the page, as the notice is, and
	// not past the form being sent again.
	if !strings.Contains(script, "\nlet verifyRefusal = null;\n") {
		t.Error("app.js does not keep verifyRefusal, the refusal said in the step's form")
	}
	if n := len(regexp.MustCompile(`\bverifyRefusal\s*=[^=]`).FindAllString(withoutComments(script), -1)); n != 4 {
		t.Errorf("app.js sets verifyRefusal %d times; want four: its declaration, in submitVerification, "+
			"in clearVerifyRefusal and in render()", n)
	}
	if !strings.HasPrefix(withoutComments(jsFunction(t, script, "clearVerifyRefusal")),
		"function clearVerifyRefusal(form) {\n  verifyRefusal = null;\n") {
		t.Error("sending the form again does not forget the refusal it had, so the step drawn again would say it")
	}
	if !regexp.MustCompile(`\n  if \(!\(keepPage\(\) && unsentOnScreen\(carriedDraft\(draft\)\.held\)\)\) \{\n    clearNotice\(\);\n  \}\n\s*verifyRefusal = null;\n`).MatchString(withoutComments(jsFunction(t, script, "render"))) {
		t.Error("render() does not put away a refusal said in a Verify form with the notice, so a later visit says it again")
	}
	if clear, sent := strings.Index(submit, "clearVerifyRefusal(form);"), regexp.MustCompile(`await (api|sendWithCaptcha)\(`).FindStringIndex(submit); clear < 0 || sent == nil || clear > sent[0] {
		t.Error("a refusal stays up while the form is sent again")
	}
	refuse := withoutComments(jsFunction(t, script, "refuseVerification"))
	for _, want := range []string{"why.textContent = message;", "why.hidden = false;", "revealClearOfHeader(form);",
		`target.setAttribute("aria-invalid", "true");`, "target.focus(", "target.select();"} {
		if !strings.Contains(refuse, want) {
			t.Errorf("refuseVerification does not %s:\n%s", want, refuse)
		}
	}

	forget := withoutComments(jsFunction(t, script, "forgetBrowserHTML"))
	button, done := strings.Index(forget, `data-action="forget-browser"`), strings.Index(forget, `id="forget-browser-done"`)
	if button < 0 || done < button {
		t.Error("Forget this browser has nowhere under its button to say it is done")
	}
	confirm := withoutComments(jsFunction(t, script, "confirmBrowserForgotten"))
	for _, want := range []string{"done.textContent = BROWSER_FORGOTTEN;", "done.hidden = false;", "revealClearOfHeader(done);", "done.focus("} {
		if !strings.Contains(confirm, want) {
			t.Errorf("confirmBrowserForgotten does not %s:\n%s", want, confirm)
		}
	}
	handler := verifyClickHandler(t, script, `if (event.target.closest('[data-action="forget-browser"]')) {`)
	if strings.Contains(handler, "showNotice(") || !strings.Contains(handler, "confirmBrowserForgotten();") {
		t.Errorf("Forget this browser is not confirmed under its button:%s", handler)
	}
}

// A Verify step can be drawn again while its request is out: the reader
// presses Verify in the header, or goes to a board and comes back. The step
// drawn again starts with its fields empty, since nothing typed is kept with
// the step (the address least of all), and the refusal kept with the step is
// said there. Said as the form that sent the request says it, a request that
// got no answer — a dropped connection, or one given up after
// VERIFY_TIMEOUT_MS — told the reader "What you wrote is still in the box."
// under an empty address field or code box, with the focus in it. So that
// sentence is said only in the form that sent the request, which still holds
// what was typed, and every other form is told the refusal without it.
func TestVerifyStepDrawnAgainIsNotToldWhatWasTypedIsThere(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	// errorMessage says what was written is still in the box, or for a post of
	// only an image that the image is still attached, only when it is told so
	// (kept), which it is by default, for the posting forms that stay on
	// screen whatever is refused; and nothing else in app.js says what was
	// written is. A post of only an image that a redraw took off (imageOff) is
	// told neither: nothing of it is left. A Verify form has no image.
	errMsg := jsLines(jsFunction(t, script, "errorMessage"))
	if want := "function errorMessage(err, " +
		`{ hasText = true, hasImage = true, imageOff = "", action = "post", kept = true } = {}) {`; errMsg[0] != want {
		t.Errorf("errorMessage starts\n\t%s\nwant\n\t%s", errMsg[0], want)
	}
	holdLines(t, "errorMessage, for a request that got no answer", errMsg, "if (err.unreachable) {", "}", []string{
		"return nothingLeft || !kept",
		"? UNREACHABLE",
		": hasText",
		"? `${UNREACHABLE} What you wrote is still in the box.`",
		": `${UNREACHABLE} Your image is still attached.`;",
	})
	if n := strings.Count(code, "What you wrote is still in the box"); n != 1 {
		t.Errorf("app.js says what was written is still in the box %d times; want once, in errorMessage, when told "+
			"it is", n)
	}

	// The refusal kept with the step holds what each form is told: message for
	// the one that sent the request, redrawn, with kept false, for any other
	// (held line for line by TestVerifyRefusalsAreSaidInTheForm). Nothing else
	// tells errorMessage the text has gone.
	submit := withoutComments(jsFunction(t, script, "submitVerification"))
	redrawn := `redrawn: refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code", kept: false }),`
	if !strings.Contains(submit, "\n      "+redrawn+"\n") {
		t.Errorf("the refusal kept with a Verify step has no redrawn message without the box's sentence (%s):\n%s",
			redrawn, submit)
	}
	if n := strings.Count(code, "kept: false"); n != 1 {
		t.Errorf("app.js tells errorMessage %d times that what was written has gone; want once, for a Verify step "+
			"drawn again", n)
	}

	// sayVerifyRefusal says redrawn unless told the form sent the request, and
	// one call tells it so: the catch in submitVerification, about the form
	// that sent it, when that is the form on screen. renderVerify draws the
	// step afresh before it says the refusal, so its form never sent anything;
	// it does that after the sending form has said its own when Verify in the
	// header was pressed just before the answer came.
	say := withoutComments(jsFunction(t, script, "sayVerifyRefusal"))
	if !strings.Contains(say, "function sayVerifyRefusal(form, sentIt = false) {") ||
		!strings.Contains(say, "sentIt ? refused.message : refused.redrawn") {
		t.Errorf("sayVerifyRefusal does not tell a form that did not send the request the refusal without the box's "+
			"sentence:\n%s", say)
	}
	var calls []string
	for _, m := range regexp.MustCompile(`[^\w$.]sayVerifyRefusal\(([^()]*)\)`).FindAllStringSubmatch(code, -1) {
		calls = append(calls, m[1])
	}
	slices.Sort(calls)
	if want := []string{"form", "form, sentIt = false", "shown, shown === form"}; !slices.Equal(calls, want) {
		t.Errorf("sayVerifyRefusal is declared and called as %q; want %q: only the form that sent the request is "+
			"told it did", calls, want)
	}
	if !strings.Contains(submit, `const shown = form.isConnected ? form : el.main.querySelector("form[data-verify]");`) ||
		!strings.Contains(submit, "sayVerifyRefusal(shown, shown === form);") {
		t.Errorf("submitVerification does not tell the form it says its refusal in whether that form sent it:\n%s", submit)
	}
	render := withoutComments(jsFunction(t, script, "renderVerify"))
	if !strings.Contains(render, "drawVerifyStep();\n  const form = el.main.querySelector(\"form[data-verify]\");\n  if (form) sayVerifyRefusal(form);") {
		t.Errorf("renderVerify says the refusal kept with the step other than in the step it has just drawn:\n%s", render)
	}
}

// On a phone the header wraps to three rows and stays at the top of the
// screen, and the Verify page draws each step at the scroll position the last
// one left. focus() and scrollIntoView() take an element under the header for
// one in view, so a badge holder who had scrolled down to "Verify another
// address" got the address field, then the code box with the sentence above
// it, then the new badge, all drawn under the header, with only the buttons
// below them in sight. So everything a Verify step draws in place, or says, is
// brought into view by revealClearOfHeader, which measures the header, and
// nothing on the page is focused in a way that scrolls by itself.
func TestVerifyStepsAreShownClearOfTheHeader(t *testing.T) {
	script := appScript(t)

	// What this test asks of revealClearOfHeader, showVerifyField and
	// refuseVerification is why each is written as it is. Each also runs on a
	// pending code's box, so TestVerifyCodeBoxShowsWhileACodeIsPending holds
	// all three whole: checked here for a line or a few words alone, a gate on
	// a held badge in showVerifyField or revealClearOfHeader took the box away.
	reveal := withoutComments(jsFunction(t, script, "revealClearOfHeader"))
	for _, want := range []string{`document.querySelector(".site-header")`, ".getBoundingClientRect().bottom",
		"window.innerHeight", "window.scrollBy("} {
		if !strings.Contains(reveal, want) {
			t.Errorf("revealClearOfHeader does not measure the header and scroll clear of it (no %s):\n%s", want, reveal)
		}
	}
	if strings.Contains(reveal, "scrollIntoView(") || strings.Contains(reveal, "focus(") {
		t.Errorf("revealClearOfHeader leans on scrollIntoView or focus, which do not know about the header:\n%s", reveal)
	}

	field := withoutComments(jsFunction(t, script, "showVerifyField"))
	if !regexp.MustCompile(`revealClearOfHeader\(input\.form\);\n\s*input\.focus\(\{ preventScroll: true \}\);`).MatchString(field) {
		t.Errorf("showVerifyField does not bring the step's form into view clear of the header before it focuses "+
			"the field:\n%s", field)
	}
	held := withoutComments(jsFunction(t, script, "showHeldBadge"))
	if !strings.Contains(held, `document.getElementById("verify-badge")`) ||
		!regexp.MustCompile(`revealClearOfHeader\(held\);\n\s*held\.focus\(\{ preventScroll: true \}\);`).MatchString(held) {
		t.Errorf("showHeldBadge does not bring the badge held into view clear of the header and focus it:\n%s", held)
	}
	view := withoutComments(jsFunction(t, script, "verifyViewHTML"))
	if !strings.Contains(view, `<p id="verify-badge" tabindex="-1">Your badge in this browser is ${verifiedBadgeHTML(v.badge.label)}`) {
		t.Error("the badge panel's first line, the badge held, is not #verify-badge, focusable from script")
	}
	confirm := withoutComments(jsFunction(t, script, "confirmBrowserForgotten"))
	if !strings.Contains(confirm, "revealClearOfHeader(done);\n  done.focus({ preventScroll: true });") {
		t.Errorf("Forget this browser's confirmation is not brought into view clear of the header:\n%s", confirm)
	}
	refuse := withoutComments(jsFunction(t, script, "refuseVerification"))
	shown, focused := strings.Index(refuse, "revealClearOfHeader(form);"), strings.Index(refuse, "target.focus(")
	if shown < 0 || focused < shown {
		t.Errorf("a refusal's form is not brought into view clear of the header before its field is focused:\n%s", refuse)
	}

	// Every place a step is drawn in place of another, and what shows it:
	// the code box, the address field, or the badge held, each clear of the
	// header. The address step that takes a code box's place when the server
	// has dropped the code centres the note saying why, which puts it clear of
	// the header on any screen (TestEndedCodeSaysWhyWhereTheBoxWas).
	places := []struct{ name, body string }{
		{"submitVerification", withoutComments(jsFunction(t, script, "submitVerification"))},
		{"Forget this credential", verifyClickHandler(t, script, `if (event.target.closest('[data-action="forget-verification"]')) {`)},
		{"Forget this browser", verifyClickHandler(t, script, `if (event.target.closest('[data-action="forget-browser"]')) {`)},
		{"Keep my badge", verifyClickHandler(t, script, `if (event.target.closest('[data-action="verify-keep"]')) {`)},
		{"Verify another address and Use a different address", verifyClickHandler(t, script, `if (restart) {`)},
	}
	shows := []string{`showVerifyField("f-code");`, `showVerifyField("f-email");`, "showHeldBadge();",
		`const why = document.getElementById("f-email-ended");`}
	drawn := 0
	for _, p := range places {
		for _, at := range regexp.MustCompile(`drawVerifyStep\(\);\n`).FindAllStringIndex(p.body, -1) {
			drawn++
			next := strings.TrimLeft(p.body[at[1]:], " \t\n")
			if !slices.ContainsFunc(shows, func(s string) bool { return strings.HasPrefix(next, s) }) {
				line, _, _ := strings.Cut(next, "\n")
				t.Errorf("%s draws a Verify step and goes on with %q, without bringing it into view clear of the header",
					p.name, line)
			}
		}
		for _, at := range regexp.MustCompile(`renderVerify\(\)`).FindAllStringIndex(p.body, -1) {
			drawn++
			before, rest := p.body[:at[0]], p.body[at[0]:]
			switch {
			case strings.HasSuffix(before, "if (await ") && strings.HasPrefix(rest, "renderVerify()) showHeldBadge();"):
			case strings.HasPrefix(rest, "renderVerify().then((drawn) => {"):
				then, _, _ := strings.Cut(rest, "\n    });")
				if !strings.Contains(then, "revealClearOfHeader(") && !strings.Contains(then, "confirmBrowserForgotten();") {
					t.Errorf("%s draws the Verify page without bringing what it drew into view clear of the header:\n%s",
						p.name, then)
				}
			default:
				line, _, _ := strings.Cut(rest, "\n")
				t.Errorf("%s draws the Verify page as %q, without bringing what it drew into view clear of the header",
					p.name, line)
			}
		}
	}
	if drawn != 7 {
		t.Errorf("the Verify page is drawn in place %d times; this test knows seven", drawn)
	}

	// Nor does anything there scroll the page another way: every focus is
	// without scrolling, and the one scrollIntoView left is the centred note.
	all := []string{field, held, confirm, refuse}
	for _, p := range places {
		all = append(all, p.body)
	}
	for _, body := range all {
		for _, at := range regexp.MustCompile(`\.focus\(`).FindAllStringIndex(body, -1) {
			if !strings.HasPrefix(body[at[0]:], ".focus({ preventScroll: true })") {
				line, _, _ := strings.Cut(body[at[0]:], "\n")
				t.Errorf("the Verify page focuses with %q, which scrolls without knowing about the header", line)
			}
		}
	}
	scrolls := regexp.MustCompile(`\w+\.scrollIntoView\([^)]*\)`).FindAllString(strings.Join(all, "\n"), -1)
	if !slices.Equal(scrolls, []string{`why.scrollIntoView({ block: "center" })`}) {
		t.Errorf("the Verify page scrolls with %q; want only the ended code's centred note, and revealClearOfHeader "+
			"otherwise", scrolls)
	}
}

// A posting form offers the badge this browser held when it was drawn, and
// the badge can change under it: Verify pressed for another address, and the
// reader gone on to a board before the answer came. The form went on saying
// "post with" the badge before while the post it sent carried the new
// credential, so a box ticked for one institution put the post out under
// another. So the box sits in a slot that names the badge it offers, by a
// number this page gives each credential it reads (badgeNumber); the badge's
// answer brings the slot of any form on screen into line with the badge just
// stored (offerHeldBadge, held in submitVerification by
// TestVerifyCodeBoxShowsWhileACodeIsPending), drawing it again unticked
// (drawBadgeOption); a post goes only under the badge its box named, and a
// form whose ticked box was drawn again is told so at its next press and sends
// nothing (badgeWithdrawn); and a draft carried across a re-render ticks the
// box again only for the badge it was ticked for.
func TestPostingFormSendsOnlyTheBadgeItNames(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	// The box is drawn in one place, and every form draws it in its slot.
	box := `<input type="checkbox" name="badge"`
	if n := strings.Count(code, box); n != 1 || !strings.Contains(jsFunction(t, script, "badgeOptionHTML"), box) {
		t.Errorf("app.js draws the badge box in %d places; want one, badgeOptionHTML", n)
	}
	if n := len(regexp.MustCompile(`[^\w]badgeOptionHTML\(`).FindAllString(code, -1)) - 1; n != 2 {
		t.Errorf("app.js draws badgeOptionHTML %d times; want two, in formHTML's slot and in drawBadgeOption", n)
	}
	form := withoutComments(jsFunction(t, script, "formHTML"))
	if !strings.Contains(form, "const held = state.verification.badge;\n"+
		"  const badge = `<span class=\"badge-option\" data-badge-option=\"${held ? held.number : \"\"}\">${badgeOptionHTML(held)}</span>`;") {
		t.Errorf("formHTML does not draw the badge box in a slot naming the badge it offers:\n%s", form)
	}
	if css, err := fs.ReadFile(webFS(), "app.css"); err != nil {
		t.Fatalf("reading app.css: %v", err)
	} else if !strings.Contains(string(css), "\n.badge-option { display: contents; }\n") {
		t.Error("the badge slot takes room of its own in the form's actions")
	}

	// The number is the page's own, one per credential, however often it is
	// read: a form drawn before the Verify page read the same badge again
	// still names it.
	if !strings.Contains(code, "\nconst badgeNumbers = new Map();\n") {
		t.Error("app.js does not keep badgeNumbers, the page's numbers for the credentials it has read")
	}
	if got, want := jsLines(jsFunction(t, script, "badgeNumber")), []string{
		"function badgeNumber(token) {",
		"if (!badgeNumbers.has(token)) badgeNumbers.set(token, String(badgeNumbers.size + 1));",
		"return badgeNumbers.get(token);",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("badgeNumber runs\n\t%s\nwant one number per credential, kept for the visit:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "readBadge")),
		"return { token, label: claims.label, expires, number: badgeNumber(token) };") {
		t.Error("readBadge does not number the badge it reads")
	}
	if got, want := jsLines(jsFunction(t, script, "badgeOffered")), []string{
		"function badgeOffered(form) {",
		`const slot = form.querySelector("[data-badge-option]");`,
		`return slot ? slot.dataset.badgeOption : "";`,
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("badgeOffered runs\n\t%s\nwant the number its slot names:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}

	// What brings a form on screen into line, held whole: it reads no Verify
	// step and draws nothing but the form's badge slot and the form's note on
	// the anti-spam check, which is why it may run in the badge's answer, for
	// every posting form on screen (offerHeldBadge, through
	// withdrawBadgeOption). A box it draws into a held form is switched off
	// with the rest, until release (TestAPendingFormIsHeld). It says whether
	// the box it drew again was ticked, for withdrawBadgeOption to note. The
	// focus a redrawn box had goes to the box drawn in its place, or the form's
	// button, not to the page.
	if got, want := jsLines(jsFunction(t, script, "drawBadgeOption")), []string{
		"function drawBadgeOption(form) {",
		"const held = state.verification.badge;",
		`const number = held ? held.number : "";`,
		`const slot = form.querySelector("[data-badge-option]");`,
		"if (!slot || slot.dataset.badgeOption === number) return false;",
		`const box = slot.querySelector('input[name="badge"]');`,
		"const ticked = Boolean(box && box.checked);",
		"const focused = slot.contains(document.activeElement);",
		"slot.dataset.badgeOption = number;",
		"slot.innerHTML = badgeOptionHTML(held);",
		`const drawn = slot.querySelector('input[name="badge"]');`,
		"if (drawn && heldForms.has(form)) {",
		"drawn.disabled = true;",
		"heldForms.get(form).push(drawn);",
		"}",
		`const check = form.querySelector("[data-check-hint]");`,
		"if (check) check.textContent = held ? CHECK_HINT_WITH_BADGE : CHECK_HINT;",
		`const next = drawn || form.querySelector('button[type="submit"]');`,
		"if (focused && next) next.focus({ preventScroll: true });",
		"return ticked;",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("drawBadgeOption runs\n\t%s\nwant it to draw again, unticked, a badge box naming a badge no longer "+
			"held, and nothing else:\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	if n := len(regexp.MustCompile(`\.dataset\.badgeOption\s*=[^=]`).FindAllString(code, -1)); n != 1 {
		t.Errorf("app.js sets a slot's badge %d times; want once, in drawBadgeOption, which draws the box with it", n)
	}

	// A post goes under the badge its box named, or not at all; the check
	// comes before the credential is chosen for the request. A box ticked for
	// a badge no longer held is drawn again, unticked, and noted; and a form
	// noted so, then or since it was drawn, is told at this press and sends
	// nothing.
	submit := withoutComments(submitHandler(t, script))
	named := strings.Index(submit, "  const badgeBox = field(form, \"badge\");\n  const offered = state.verification.badge;\n"+
		"  if (badgeBox && badgeBox.checked && !(offered && badgeOffered(form) === offered.number)) {\n"+
		"    withdrawBadgeOption(form, false);\n  }\n")
	told := strings.Index(submit, "  if (badgeWithdrawn.has(form)) {\n")
	held := strings.Index(submit, "const held = wantsBadge ? state.verification.badge : null;")
	headers := strings.Index(submit, "const headers = held ?")
	if named < 0 || told < named || held < told || headers < held {
		t.Errorf("the submit handler does not refuse a badge its box did not name before it chooses the credential:\n%s", submit)
	} else if body, _, _ := strings.Cut(submit[told:], "\n  }\n"); !strings.HasSuffix(body, "return;") {
		t.Errorf("the submit handler goes on with a badge its box did not name:\n%s", body)
	}

	// A post refused because the badge it carried expired or was revoked
	// forgets that credential, and only while it is still the one stored.
	// Another can have been stored while the post was on its way: a Verify
	// answer in this tab, or a badge issued in another, which is what revoked
	// this one. Forgetting whatever was stored deleted that badge, which the
	// server hands out once and cannot show again. So the forms are brought
	// into line with the badge held now instead (offerHeldBadge, and
	// drawBadgeOption for the form that sent the post, or withdrawBadgeOption
	// for one off the screen by then), and the notice says that badge is
	// offered (badgeReplacedNotice) rather than repeating the server's words,
	// which send the reader to verify again. Neither is done in a browser
	// forgotten since the press: a badge stored after a forget is someone
	// else's.
	holdLines(t, "a post refused for the badge it carried", jsLines(submit),
		"const reason = err.payload && err.payload.credential;",
		"const uploadsOff = err.status === 403 && err.message === UPLOADS_SWITCHED_OFF;", []string{
			"let replaced = false;",
			`if (held && (reason === "expired" || reason === "revoked")) {`,
			"if (forgets === browserForgets && storedCredential() === held.token) {",
			"forgetCredential();",
			"badgeLost =",
			`reason === "expired"`,
			`? "Your badge has expired."`,
			`: "That badge was replaced by a newer verification.";`,
			"} else if (forgets === browserForgets) {",
			"readStoredBadge();",
			"}",
			"replaced = forgets === browserForgets && state.verification.badge !== null;",
			"offerHeldBadge(false, form);",
			`const panel = el.main.querySelector('[data-action="forget-verification"]');`,
			`if (onVerifyPage() && ageStatus() === "ok" && panel) {`,
			"renderVerify();",
			"}",
			"if (shown) {",
			"drawBadgeOption(form);",
			"if (replaced) {",
			"badgeWithdrawn.delete(form);",
			"}",
			"if (captchaFormNeedsIt(form) && captchaFormHasWriting(form)) {",
			"prepareCaptcha();",
			"}",
			"} else {",
			"withdrawBadgeOption(form, true);",
			"}",
			"}",
		})
	if n := strings.Count(submit, "forgetCredential("); n != 1 {
		t.Errorf("the submit handler forgets the credential in %d places; want one, for the credential refused", n)
	}
	if !strings.Contains(submit, "\n    const replacedNow = () => (replaced || (badgeRefused && state.verification.badge !== null)) && !forgotten();\n") ||
		!strings.Contains(submit, "\n        : replacedNow()\n"+
			"          ? badgeReplacedNotice(on, { hasText: text, hasImage: image, imageOff: off })\n"+
			"          : errorMessage(err, { hasText: text, hasImage: image, imageOff: off });\n") {
		t.Errorf("a post refused for a badge replaced since does not say which badge is offered now:\n%s", submit)
	}
	if said := withoutComments(jsFunction(t, script, "badgeReplacedNotice")); !strings.Contains(said, "Verified: ${held.label}") ||
		strings.Contains(strings.ToLower(said+jsString(t, script, "BADGE_REPLACED")), "verify again") {
		t.Errorf("badgeReplacedNotice does not say which badge is held now, or sends the reader to verify again:\n%s", said)
	}

	// A draft ticks the box again only for the badge it was ticked for, and
	// nothing else ticks it. A form noted as having had its ticked box drawn
	// again carries that note instead (badgeWithdrawn), so the form drawn in
	// its place is told at its next press too.
	if !strings.Contains(withoutComments(jsFunction(t, script, "formDraft")),
		`const ticked = badgeWithdrawn.get(form) || (badge && badge.checked ? { number: badgeOffered(form) } : null);`) {
		t.Error("a draft does not note which badge its box was ticked for")
	}
	restore := withoutComments(jsFunction(t, script, "restoreDraft"))
	if !strings.Contains(restore, "const same = Boolean(ticked && badge && ticked.number === badgeOffered(form));\n"+
		"  if (badge) badge.checked = same;\n") {
		t.Errorf("a draft ticks the badge box again whichever badge the form offers:\n%s", restore)
	}
	if n := len(regexp.MustCompile(`\.checked\s*=[^=]`).FindAllString(code, -1)) -
		len(regexp.MustCompile(`\b(sage|spoiler|adult)\.checked\s*=[^=]`).FindAllString(code, -1)); n != 1 {
		t.Errorf("app.js ticks or unticks a box %d times besides sage, spoiler and the attestation; want once, "+
			"the badge box in restoreDraft", n)
	}
}

// Forget this credential draws the address step in place of the badge panel,
// which takes the button and the focus with it. It once left a keyboard on
// the page itself, with nothing to tell a screen reader that the badge had
// gone. So the address step opens by saying so (badgeLost), and that is
// brought into view clear of the header and focused, as Forget this browser's
// confirmation is.
func TestForgettingTheCredentialSaysSo(t *testing.T) {
	script := appScript(t)
	if said := jsString(t, script, "CREDENTIAL_FORGOTTEN"); !regexp.MustCompile(`^[A-Z][^"]*badge\.$`).MatchString(said) {
		t.Errorf("CREDENTIAL_FORGOTTEN is %q; want a sentence saying the badge has gone", said)
	}
	// The note, focusable from script, is badgeLostHTML's, held whole by
	// TestAGoneBadgeIsSaidToHaveGone.
	view := withoutComments(jsFunction(t, script, "verifyViewHTML"))
	address, opened := strings.Index(view, `data-verify="request"`), -1
	if address >= 0 {
		opened = strings.LastIndex(view[:address], "return `")
	}
	if opened < 0 || !strings.Contains(view[opened:address], `${badgeLostHTML("Verify again to get a new one.")}`) {
		t.Errorf("the address step does not open by saying why the badge went:\n%s", view)
	}
	handler := verifyClickHandler(t, script, `if (event.target.closest('[data-action="forget-verification"]')) {`)
	lost, drawn := strings.Index(handler, "badgeLost = CREDENTIAL_FORGOTTEN;"), strings.Index(handler, "renderVerify()")
	if lost < 0 || drawn < lost {
		t.Errorf("Forget this credential does not say the badge has gone in the step it draws:%s", handler)
	}
	if !strings.Contains(handler, "renderVerify().then((drawn) => {\n"+
		"      const lost = drawn && document.getElementById(\"badge-lost\");\n"+
		"      if (lost) {\n"+
		"        revealClearOfHeader(lost);\n"+
		"        lost.focus({ preventScroll: true });\n"+
		"      }\n"+
		"    });") {
		t.Errorf("Forget this credential does not bring what it said into view clear of the header and focus it:%s", handler)
	}
}

// A badge can go while the Verify page is on another step: it runs out while
// a code for another address is on its way, or another tab forgets it. Keep my
// badge drew the badge panel from the badge read before, so one that had
// expired came back as if it were live, "good until" a time gone by, and went
// on being offered on posting forms for the server to refuse. And the code
// step, drawn again once the expiry was noticed, never said it, while
// renderVerify put away what it would have said: the Keep my badge button went
// with no word why. So Keep my badge reads the badge again before it draws,
// and a badge gone leaves the address step, which says why, shown and focused
// in the panel's place; both steps that can be drawn without the badge say why
// it went (badgeLostHTML); and that is put away only by drawing a step
// (drawVerifyStep), so it is said once, whichever way the step was drawn.
func TestAGoneBadgeIsSaidToHaveGone(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	keep := verifyClickHandler(t, script, `if (event.target.closest('[data-action="verify-keep"]')) {`)
	holdLines(t, "Keep my badge", jsLines(keep), "clearNotice();", "return;", []string{
		"state.verification.badge = readBadge();",
		"if (!state.verification.badge && !badgeLost) badgeLost = CREDENTIAL_FORGOTTEN;",
		stepToAddress,
		"drawVerifyStep();",
		"showHeldBadge();",
	})
	if got, want := jsLines(jsFunction(t, script, "showHeldBadge")), []string{
		"function showHeldBadge() {",
		`const held = document.getElementById("verify-badge") || document.getElementById("badge-lost");`,
		"if (!held) return;",
		"revealClearOfHeader(held);",
		"held.focus({ preventScroll: true });",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("showHeldBadge runs\n\t%s\nwant it to show and focus the badge held, or why there is none:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}

	// What says why, held whole: it reads badgeLost and nothing else, so it can
	// sit in the code step's template without deciding anything there
	// (TestVerifyCodeBoxShowsWhileACodeIsPending), and it can be focused.
	if got, want := jsLines(jsFunction(t, script, "badgeLostHTML")), []string{
		"function badgeLostHTML(then) {",
		"return badgeLost ? `<p class=\"verify-note\" id=\"badge-lost\" tabindex=\"-1\">${esc(badgeLost)} ${esc(then)}</p>` : \"\";",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("badgeLostHTML runs\n\t%s\nwant it to say why the badge went, focusable, and nothing else:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	view := withoutComments(jsFunction(t, script, "verifyViewHTML"))
	codeStep := strings.Index(view, `if (verifyStep.step === "code") {`)
	panel := strings.Index(view, "if (v.badge && !verifyStep.replacing) {")
	address, opened := strings.Index(view, `data-verify="request"`), -1
	if address >= 0 {
		opened = strings.LastIndex(view[:address], "return `")
	}
	if codeStep < 0 || panel < codeStep || opened < panel {
		t.Fatal("verifyViewHTML no longer draws the code step, the badge panel and the address step in that order")
	}
	if n := strings.Count(view, "${badgeLostHTML("); n != 2 {
		t.Errorf("verifyViewHTML says why a badge went %d times; want twice, in the code step and the address step", n)
	}
	if !strings.Contains(view[codeStep:panel], `${badgeLostHTML("The code on its way earns you a new one.")}`) {
		t.Errorf("the code step does not say why a badge held here went:\n%s", view[codeStep:panel])
	}
	if !strings.Contains(view[opened:address], `${badgeLostHTML("Verify again to get a new one.")}`) {
		t.Errorf("the address step does not open by saying why a badge held here went:\n%s", view[opened:address])
	}

	// Put away by drawing a step and nowhere else: drawVerifyStep, held whole
	// by TestVerifyStepIsDrawnForAReaderArriving, and its declaration. Or by
	// forgetting the browser (forgetOnThisPage), here or in another tab: why
	// the last person's badge went would greet the next with that person's
	// institution.
	if n := len(regexp.MustCompile(`\bbadgeLost\s*=\s*""`).FindAllString(code, -1)); n != 3 ||
		!strings.Contains(withoutComments(jsFunction(t, script, "forgetOnThisPage")), "\n  badgeLost = \"\";\n") {
		t.Errorf("app.js puts badgeLost away %d times; want three: its declaration, in drawVerifyStep and in "+
			"forgetOnThisPage", n)
	}
	if !strings.Contains(code, "\nlet badgeLost = \"\";\n") {
		t.Error("badgeLost does not start empty")
	}
}
