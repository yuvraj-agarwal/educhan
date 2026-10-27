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
	// sets state.verification, and neither is named anywhere but there, here
	// and in the state the page starts with. A request that gets no answer
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
	held := withoutComments(load) + view[:start] + initial
	for _, name := range []string{"enabled", "answered"} {
		named := regexp.MustCompile(`\b` + name + `\b`)
		if extra := len(named.FindAllString(code, -1)) - len(named.FindAllString(held, -1)); extra != 0 {
			t.Errorf("app.js names %s %d times outside loadVerification, verifyViewHTML before the code step, and "+
				"the state the page starts with; what the server said about verification is its answer, and nothing "+
				"else may set it", name, extra)
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
	// TestPostingFormSendsOnlyTheBadgeItNames.
	submit := jsLines(jsFunction(t, script, "submitVerification"))
	requested := `body: JSON.stringify({ email: String(data.get("email") || ""), adult: adult.checked }),`
	confirmed := `body: JSON.stringify({ code: String(data.get("code") || "") }),`
	holdLines(t, "the answer to a code sent", submit, requested, "}", []string{
		"});",
		"if (verifyStep !== sentFrom) return;",
		stepToCode,
		"if (!onVerifyPage()) return;",
		"drawVerifyStep();",
		`showVerifyField("f-code");`,
		"return;",
	})
	holdLines(t, "the answer to a code confirmed", submit, confirmed, "} catch (err) {", []string{
		"});",
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
	// left the two functions it runs on the box just drawn, showVerifyField
	// and revealClearOfHeader under it, checked for a line and a few words: a
	// gate on a held badge in either took the code form out of the page, and a
	// badge holder who sent a code had nowhere to type it, with every check
	// here passing. The box is drawn in two places. The answer to Send code
	// draws it with drawVerifyStep (of verifyViewHTML, only the part up to the
	// code step's return runs) and shows it with showVerifyField. renderVerify
	// draws whichever step the page is on by the time the server has answered,
	// for a reader arriving (render()) among others, and gives it its refusal
	// with sayVerifyRefusal (one for a wrong code, say); what its callers run
	// on what it drew is followed too: focusHash in render(), showHeldBadge
	// after Verify, and confirmBrowserForgotten after Forget this browser.
	// And the box can be drawn while renderVerify is still asking: the answer
	// to Send code, landing while render() asks for a badge holder who then
	// pressed Verify in the header. What renderVerify waits on then runs on
	// the box once its answer is in: the rest of api() after its fetch, of
	// loadVerification, which reads the badge again (readBadge), and of
	// renderVerify, which titles the page (announce). A gate on a held badge
	// in announce took the box away with every check here passing.
	// Every top-level function those reach is found by following the calls
	// down, not listed by hand, and each is held whole, here or by the test
	// named, so a function new to that path fails here until it is held too.
	// What runs on the box with no call from any of these — the page's
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
	}
	roots := []string{"drawVerifyStep", "showVerifyField", "sayVerifyRefusal", "focusHash", "showHeldBadge",
		"confirmBrowserForgotten"}
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

	// renderVerify is called from four places, and what each runs once it
	// returns is held: the answer to Verify (held above, with showHeldBadge
	// whole), the two forget buttons, whose callbacks are held here whole, and
	// render(), below.
	if n := strings.Count(code, "renderVerify()"); n != 5 {
		t.Errorf("app.js names renderVerify() %d times; want five: its definition, render(), the answer to Verify, and "+
			"the two forget buttons, each held from where it returns", n)
	}
	for _, then := range []string{
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
			t.Errorf("app.js does not run exactly this once a forget button's renderVerify returns:\n%s", then)
		}
	}

	// render() leaves the Verify page once it is drawn, with focusHash alone
	// run on it, after finish, which beginLoading hands it; both are held
	// whole. What render() runs after drawing any other page — enhancePosts,
	// restoreDraft and what they reach — serves every page, so it is not run
	// on this one.
	renderLines := jsLines(jsFunction(t, script, "render"))
	holdLines(t, "render() once the Verify page is drawn", renderLines, "} finally {", "if (resolved !== null) {",
		[]string{
			"finish();",
			"}",
			`if (route.view === "verify") {`,
			"focusHash();",
			"return;",
			"}",
		})
	if !slices.Contains(renderLines, "const finish = beginLoading();") ||
		len(regexp.MustCompile(`\bfinish\b`).FindAllString(strings.Join(renderLines, "\n"), -1)) != 2 {
		t.Error("render() does not take finish from beginLoading and call it in its finally alone")
	}
	if got, want := jsLines(jsFunction(t, script, "beginLoading")), []string{
		"function beginLoading() {",
		`el.main.setAttribute("aria-busy", "true");`,
		"const dimmed = el.main.firstChild;",
		"const timer = setTimeout(() => {",
		"if (el.main.firstChild !== dimmed) return;",
		`el.main.innerHTML = '<div class="skeleton"><div class="skeleton-row"></div><div class="skeleton-row"></div>` +
			`<div class="skeleton-row"></div></div>';`,
		"}, 250);",
		"return () => {",
		"clearTimeout(timer);",
		`el.main.removeAttribute("aria-busy");`,
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
		// Where the credential is kept, read, stored and forgotten, and the
		// badge read off it and numbered.
		`const CREDENTIAL_STORAGE_KEY = "educhan:credential";`: 2,
		"function storedCredential() {":                        3,
		"function storeCredential(token) {":                    2,
		"function forgetCredential() {":                        4,
		"function readBadge() {":                               3,
		"const badgeNumbers = new Map();":                      1,
		"function badgeNumber(token) {":                        5,
		// What else the browser keeps: the age answer, the post key, and
		// forgetting the browser.
		"function storageGet(key) {":        1,
		"function storageSet(key, value) {": 1,
		"function posterKey() {":            2,
		"function forgetBrowser() {":        1,
		// The badge the server put on a post, which says nothing of the one
		// held here.
		"function postHTML(post, context) {": 2,
		// A posting form, which the Verify page never has: its badge box and
		// what it offers, a draft carried across a re-render, the badge box
		// ticked, and the post sent.
		"function formHTML({ kind, slug, thread = null, open = false }) {": 5,
		"function badgeOptionHTML(held) {":                                 1,
		"function badgeOffered(form) {":                                    2,
		"function offerHeldBadge() {":                                      5,
		"function sayBadgeUnticked() {":                                    2,
		"function captureDraft() {":                                        6,
		"function restoreDraft(draft) {":                                   7,
		`el.main.addEventListener("change", (event) => {`:                  1,
		`el.main.addEventListener("submit", async (event) => {`:            12,
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
// address and title.
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
	// the API, or in the catch that takes its refusal. The age refusal before
	// the first one is drawn before anything is sent, while the page is
	// certainly on screen.
	resumes := regexp.MustCompile(`await api\(|\} catch \(err\) \{`).FindAllStringIndex(submit, -1)
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
		"await loadVerification();",
		"if (!onVerifyPage()) return false;",
		`announce("Institutional verification");`,
		"if (verifyStep !== step && verifyStepOnScreen()) return false;",
		"drawVerifyStep();",
		`const form = el.main.querySelector("form[data-verify]");`,
		"if (form) sayVerifyRefusal(form);",
		"return true;",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("renderVerify runs\n\t%s\nwant exactly\n\t%s\nIt draws the step the page is on unless the reader has left, "+
			"or that step was drawn in place since it asked and is still on screen, and it decides nothing else: a "+
			"gate on a held badge here once hid a pending code's box",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	// Why a badge went (badgeLost) is said in the step drawn, so it is put
	// away once a step is drawn, wherever that was done from
	// (TestAGoneBadgeIsSaidToHaveGone); it changes nothing in what was drawn.
	if got, want := jsLines(jsFunction(t, script, "drawVerifyStep")), []string{
		"function drawVerifyStep() {",
		"el.main.innerHTML = verifyViewHTML();",
		"verifyShown = { step: verifyStep, node: el.main.firstElementChild, at: performance.now() };",
		`badgeLost = "";`,
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("drawVerifyStep runs\n\t%s\nwant it to draw the step, note the step, the drawing and when, and put away "+
			"what the step said about a badge gone:\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
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
	timer := slices.Index(loading, "const timer = setTimeout(() => {")
	if dimmed < 0 || timer != dimmed+1 || len(loading) < timer+3 ||
		loading[timer+1] != "if (el.main.firstChild !== dimmed) return;" ||
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
// anything, once verifyStep is no longer that object. That only works while
// every change of step is a new object, never an edit of the old one, and the
// places that make one are the ones this test lists. The form's own other
// buttons wait on the answer with its submit button — in the form that sent
// it, and in the step drawn again for a reader who leaves the page and comes
// back before the answer.
func TestVerifyAnswersBelongToTheStepTheyWereSentFrom(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	submit := withoutComments(jsFunction(t, script, "submitVerification"))

	noted := strings.Index(submit, "const sentFrom = verifyStep;")
	first := strings.Index(submit, "await api(")
	if noted < 0 || first < 0 || noted > first {
		t.Fatal("submitVerification does not note the step it is sent from before its first request")
	}
	resumes := regexp.MustCompile(`await api\(|\} catch \(err\) \{`).FindAllStringIndex(submit, -1)
	if len(resumes) != 3 {
		t.Fatalf("submitVerification resumes after an answer in %d places; this test knows three "+
			"(the code sent, the badge issued, the catch)", len(resumes))
	}
	acts := regexp.MustCompile(`storeCredential\(|state\.verification|verifyStep\s*=[^=]|verifyRefusal\s*=[^=]|` +
		`el\.main\.innerHTML\s*=|drawVerifyStep\(|renderVerify\(|showNotice\(|refuseVerification\(|sayVerifyRefusal\(|` +
		`\.focus\(|scrollIntoView\(|\.disabled\s*=|\.textContent\s*=`)
	const check = "if (verifyStep !== sentFrom) return;"
	for i, r := range resumes {
		end := len(submit)
		if i+1 < len(resumes) {
			end = resumes[i+1][0]
		}
		stretch := submit[r[0]:end]
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

	if m := regexp.MustCompile(`\bverifyStep(\.\w+|\[[^\]]*\])\s*=[^=]|Object\.assign\(\s*verifyStep\b`).FindString(code); m != "" {
		t.Errorf("app.js edits verifyStep in place (%q); a change of step has to be a new object, or an answer "+
			"from the step that was left is taken for the new one", m)
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
	// step the page starts on, the three answers, and the four buttons that
	// move the page while an answer may be on its way. A new place has to be
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
			[]string{stepToAddress}},
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
	if n := len(regexp.MustCompile(`\brelease\b`).FindAllString(code, -1)); n != 3 {
		t.Errorf("app.js names release %d times; want three, all in submitVerification: declared, set and called", n)
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
		`<button class="btn primary" type="submit"${wait}>${busy ? VERIFY_SENDING : "Send code"}</button>`,
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
// what the caller passed. And the deadline is longer than the server takes to
// answer or give up, so a request given up is one whose answer was not coming:
// given up sooner, a badge the server was issuing would be lost.
func holdVerifyRequestsToADeadline(t *testing.T, script string) {
	t.Helper()
	code := withoutComments(script)
	awaited := regexp.MustCompile(`\bawait\s+([\w$.]+)`)
	for _, f := range []struct {
		name string
		want []string
	}{
		{"submitVerification", []string{"earlier", "api", "api", "renderVerify"}},
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
		"function verifyDeadline() {",
		"const deadline = new AbortController();",
		"setTimeout(() => deadline.abort(), VERIFY_TIMEOUT_MS);",
		"return deadline.signal;",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("verifyDeadline runs\n\t%s\nwant a signal that fires once VERIFY_TIMEOUT_MS has passed:\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
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

	// submitVerification says nothing in the notice itself, and two of the
	// functions it calls can, both about a badge just issued: storeCredential,
	// when the browser will not store it, which nothing else on the page could
	// say; and offerHeldBadge, for a reader gone on to a page with a posting
	// form whose ticked badge box the new badge unticked (held whole by
	// TestPostingFormSendsOnlyTheBadgeItNames). A refusal reaches neither: the
	// catch says it in the form. Looking for a literal showNotice( alone
	// passed while the guide said submitVerification raised no notice at all,
	// so the calls are followed to every top-level function they reach.
	if strings.Contains(submit, "showNotice(") {
		t.Errorf("submitVerification says something in the notice, a screen above the form on a phone:\n%s", submit)
	}
	if got, want := noticeCallees(t, script, submit), []string{"offerHeldBadge", "storeCredential"}; !slices.Equal(got, want) {
		t.Errorf("submitVerification calls %v, which say something in the notice; want %v, about a badge just issued", got, want)
	}
	if caught := strings.Index(submit, "} catch (err) {"); caught < 0 {
		t.Error("submitVerification has no catch for its refusals; this test reads it")
	} else if got := noticeCallees(t, script, submit[caught:]); len(got) > 0 {
		t.Errorf("a refusal of a Verify request reaches %v, which say something in the notice, not in the form", got)
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
	// does while the form is on screen.
	holdLines(t, "a refusal from the server", jsLines(submit), "} catch (err) {", "} finally {", []string{
		"if (verifyStep !== sentFrom) return;",
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
		"const refusedForAge = err.status === 400 && err.message === BADGE_AGE_REFUSED;",
		"verifyRefusal = {",
		"step: sentFrom,",
		`message: refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code" }),`,
		`redrawn: refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code", kept: false }),`,
		`about: refusedForAge ? "adult" : requesting ? "email" : "code",`,
		"invalid: err.status === 400,",
		"select: !requesting && err.status === 400,",
		"};",
		`const shown = form.isConnected ? form : el.main.querySelector("form[data-verify]");`,
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
	if !regexp.MustCompile(`\n  clearNotice\(\);\n\s*verifyRefusal = null;\n`).MatchString(withoutComments(jsFunction(t, script, "render"))) {
		t.Error("render() does not put away a refusal said in a Verify form with the notice, so a later visit says it again")
	}
	if clear, sent := strings.Index(submit, "clearVerifyRefusal(form);"), strings.Index(submit, "await api("); clear < 0 || clear > sent {
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

	// errorMessage says what was written is still in the box only when it is
	// told so (kept), which it is by default, for the posting forms that
	// stay on screen whatever is refused; and nothing else in app.js says it.
	errMsg := jsLines(jsFunction(t, script, "errorMessage"))
	if want := `function errorMessage(err, { hasText = true, action = "post", kept = true } = {}) {`; errMsg[0] != want {
		t.Errorf("errorMessage starts\n\t%s\nwant\n\t%s", errMsg[0], want)
	}
	holdLines(t, "errorMessage, for a request that got no answer", errMsg, "if (err.unreachable) {", "}", []string{
		"return kept ? `${UNREACHABLE} What you wrote is still in the box.` : UNREACHABLE;",
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
// TestVerifyCodeBoxShowsWhileACodeIsPending); a post goes only under the badge
// its box named; and a draft carried across a re-render ticks the box again
// only for the badge it was ticked for.
func TestPostingFormSendsOnlyTheBadgeItNames(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	// The box is drawn in one place, and every form draws it in its slot.
	box := `<input type="checkbox" name="badge"`
	if n := strings.Count(code, box); n != 1 || !strings.Contains(jsFunction(t, script, "badgeOptionHTML"), box) {
		t.Errorf("app.js draws the badge box in %d places; want one, badgeOptionHTML", n)
	}
	if n := len(regexp.MustCompile(`[^\w]badgeOptionHTML\(`).FindAllString(code, -1)) - 1; n != 2 {
		t.Errorf("app.js draws badgeOptionHTML %d times; want two, in formHTML's slot and in offerHeldBadge", n)
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
	// step and draws nothing but posting forms' badge slots, which is why it
	// may run in the badge's answer. The focus a redrawn box had goes to the
	// box drawn in its place, or the form's button, not to the page.
	if got, want := jsLines(jsFunction(t, script, "offerHeldBadge")), []string{
		"function offerHeldBadge() {",
		"const held = state.verification.badge;",
		`const number = held ? held.number : "";`,
		"let unticked = false;",
		`for (const slot of el.main.querySelectorAll("[data-badge-option]")) {`,
		"if (slot.dataset.badgeOption === number) continue;",
		`const box = slot.querySelector('input[name="badge"]');`,
		"if (box && box.checked) unticked = true;",
		"const focused = slot.contains(document.activeElement);",
		"slot.dataset.badgeOption = number;",
		"slot.innerHTML = badgeOptionHTML(held);",
		`const next = slot.querySelector('input[name="badge"]') || slot.closest("form").querySelector('button[type="submit"]');`,
		"if (focused && next) next.focus({ preventScroll: true });",
		"}",
		"if (unticked) sayBadgeUnticked();",
		"}",
	}; !slices.Equal(got, want) {
		t.Errorf("offerHeldBadge runs\n\t%s\nwant it to draw again, unticked, every badge box naming a badge no longer "+
			"held, and nothing else:\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	if n := len(regexp.MustCompile(`\.dataset\.badgeOption\s*=[^=]`).FindAllString(code, -1)); n != 1 {
		t.Errorf("app.js sets a slot's badge %d times; want once, in offerHeldBadge, which draws the box with it", n)
	}

	// A post goes under the badge its box named, or not at all; the check
	// comes before the credential is chosen for the request.
	submit := withoutComments(submitHandler(t, script))
	held := strings.Index(submit, "const held = wantsBadge ? state.verification.badge : null;")
	named := strings.Index(submit, "if (wantsBadge && !(held && badgeOffered(form) === held.number)) {\n    offerHeldBadge();\n")
	headers := strings.Index(submit, "const headers = held ?")
	if held < 0 || named < held || headers < named {
		t.Errorf("the submit handler does not refuse a badge its box did not name before it chooses the credential:\n%s", submit)
	} else if body, _, _ := strings.Cut(submit[named:], "\n  }\n"); !strings.HasSuffix(body, "return;") {
		t.Errorf("the submit handler goes on with a badge its box did not name:\n%s", body)
	}

	// A post refused because the badge it carried expired or was revoked
	// forgets that credential, and only while it is still the one stored.
	// Another can have been stored while the post was on its way: a Verify
	// answer in this tab, or a badge issued in another, which is what revoked
	// this one. Forgetting whatever was stored deleted that badge, which the
	// server hands out once and cannot show again. So the form is brought into
	// line with the badge held now instead (offerHeldBadge), and the notice
	// says that badge is offered (sayBadgeReplaced) rather than repeating the
	// server's words, which send the reader to verify again.
	holdLines(t, "a post refused for the badge it carried", jsLines(submit),
		"const reason = err.payload && err.payload.credential;",
		"if (err.status === 403 && err.message === IMAGE_LIMIT_REACHED) {", []string{
			"let replacedBy = null;",
			`if (held && (reason === "expired" || reason === "revoked")) {`,
			"if (storedCredential() === held.token) {",
			"forgetCredential();",
			"badgeLost =",
			`reason === "expired"`,
			`? "Your badge has expired."`,
			`: "That badge was replaced by a newer verification.";`,
			"} else {",
			"state.verification.badge = readBadge();",
			"replacedBy = state.verification.badge;",
			"}",
			"offerHeldBadge();",
			"}",
		})
	if n := strings.Count(submit, "forgetCredential("); n != 1 {
		t.Errorf("the submit handler forgets the credential in %d places; want one, for the credential refused", n)
	}
	if !strings.Contains(submit, "    if (replacedBy) {\n      sayBadgeReplaced(replacedBy);\n    } else {\n"+
		"      showNotice(errorMessage(err, { hasText: body.trim() !== \"\" }));\n    }\n") {
		t.Errorf("a post refused for a badge replaced since does not say which badge is offered now:\n%s", submit)
	}
	if said := withoutComments(jsFunction(t, script, "sayBadgeReplaced")); !strings.HasPrefix(said, "function sayBadgeReplaced(held) {\n  showNotice(\n") ||
		!strings.Contains(said, "Verified: ${held.label}") || strings.Contains(strings.ToLower(said), "verify again") {
		t.Errorf("sayBadgeReplaced does not say which badge is held now, or sends the reader to verify again:\n%s", said)
	}

	// A draft ticks the box again only for the badge it was ticked for, and
	// nothing else ticks it.
	if !strings.Contains(withoutComments(jsFunction(t, script, "captureDraft")), `badge: badge && badge.checked ? badgeOffered(form) : "",`) {
		t.Error("a draft does not note which badge its box was ticked for")
	}
	restore := withoutComments(jsFunction(t, script, "restoreDraft"))
	if !strings.Contains(restore, `if (badge) badge.checked = ticked !== "" && ticked === badgeOffered(form);`) {
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
	// by TestVerifyStepIsDrawnForAReaderArriving, and its declaration.
	if n := len(regexp.MustCompile(`\bbadgeLost\s*=\s*""`).FindAllString(code, -1)); n != 2 {
		t.Errorf("app.js puts badgeLost away %d times; want two: its declaration, and in drawVerifyStep", n)
	}
	if !strings.Contains(code, "\nlet badgeLost = \"\";\n") {
		t.Error("badgeLost does not start empty")
	}
}
