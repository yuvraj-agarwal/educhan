package app

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
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
	}
	if n := len(regexp.MustCompile(`\breturn\b`).FindAllString(view[:start], -1)); n != 1 {
		t.Errorf("verifyViewHTML returns %d times before the code step; want once, for the not-configured page", n)
	}
	if verifyStepSet.MatchString(view) {
		t.Error("verifyViewHTML sets verifyStep; it draws the step it is given and decides nothing about it")
	}

	// And the code step is what a sent code leads to, and what is drawn. In
	// the stretch that takes the request's answer it is the one step set, and
	// it is set before the draw, so nothing after it puts another step in its
	// place. renderVerify, which draws the page again for a reader who comes
	// back, sets no step and draws only the view. A gate on a held badge in
	// either place — after the code step is set, or in renderVerify's redraw —
	// once passed every check above; TestVerifyAnswersBelongToTheStepTheyWereSentFrom
	// holds every place that sets verifyStep to the list it knows.
	submit := withoutComments(jsFunction(t, script, "submitVerification"))
	requests := regexp.MustCompile(`await api\(`).FindAllStringIndex(submit, -1)
	if len(requests) != 2 {
		t.Fatalf("submitVerification makes %d requests; this test knows two (the code sent, the badge issued)", len(requests))
	}
	sent := submit[requests[0][0]:requests[1][0]]
	steps := verifyStepSet.FindAllString(sent, -1)
	set, drawn := strings.Index(sent, stepToCode), strings.Index(sent, "el.main.innerHTML = verifyViewHTML();")
	if len(steps) != 1 || steps[0] != stepToCode || drawn < 0 || set > drawn {
		t.Errorf("once a code is sent, submitVerification sets %q; want %q alone, set before the step is drawn:\n%s",
			steps, stepToCode, sent)
	}
	render := withoutComments(jsFunction(t, script, "renderVerify"))
	if verifyStepSet.MatchString(render) {
		t.Errorf("renderVerify sets verifyStep; it draws the step it finds and decides nothing about it:\n%s", render)
	}
	if n := strings.Count(render, "innerHTML"); n != 1 || !strings.Contains(render, "el.main.innerHTML = verifyViewHTML();") {
		t.Errorf("renderVerify draws something besides verifyViewHTML():\n%s", render)
	}
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
func TestVerifyAnswersDrawOnlyOnTheVerifyPage(t *testing.T) {
	script := appScript(t)
	guard := withoutComments(jsFunction(t, script, "onVerifyPage"))
	if !strings.Contains(guard, `state.route.view === "verify"`) {
		t.Fatalf("onVerifyPage no longer asks whether the page on screen is the Verify page:\n%s", guard)
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
	draws := regexp.MustCompile(`el\.main\.innerHTML\s*=|renderVerify\(|showNotice\(|refuseVerification\(|\.focus\(|scrollIntoView\(`)
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
	// can leave during, so it asks onVerifyPage again once that is in, and
	// the confirmation after forgetting the browser waits on what it says.
	render := withoutComments(jsFunction(t, script, "renderVerify"))
	asked := strings.Index(render, "await loadVerification();")
	guarded := strings.Index(render, "if (!onVerifyPage()) return false;")
	drawn := strings.Index(render, "el.main.innerHTML = verifyViewHTML();")
	if asked < 0 || drawn < 0 || guarded < asked || guarded > drawn {
		t.Errorf("renderVerify draws the Verify page without checking, once its request is in, that the page is "+
			"still on screen:\n%s", render)
	}
	if !strings.Contains(withoutComments(script), "renderVerify().then((drawn) => {\n      if (drawn) confirmBrowserForgotten();") {
		t.Error("forgetting the browser confirms it whether or not the Verify page was drawn")
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
// buttons wait on the answer with its submit button.
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
	acts := regexp.MustCompile(`storeCredential\(|state\.verification|verifyStep\s*=[^=]|el\.main\.innerHTML\s*=|` +
		`renderVerify\(|showNotice\(|refuseVerification\(|\.focus\(|scrollIntoView\(|\.disabled\s*=|\.textContent\s*=`)
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

	if strings.Contains(submit, "showNotice(") {
		t.Errorf("submitVerification says something in the notice, a screen above the form on a phone:\n%s", submit)
	}
	if !strings.Contains(submit, "refuseVerification(form, BADGE_AGE_REFUSAL, adult, { invalid: true });") {
		t.Error("the page's own 18+ refusal is not said under the box")
	}
	caught := submit[strings.Index(submit, "} catch (err) {"):]
	for _, want := range []string{
		`const shown = form.isConnected ? form : el.main.querySelector("form[data-verify]");`,
		`const about = field(shown, refusedForAge ? "adult" : requesting ? "email" : "code");`,
		`refuseVerification(shown, refusedForAge ? BADGE_AGE_REFUSAL : errorMessage(err, { action: "code" }), about, {`,
		`select: !requesting && err.status === 400,`,
	} {
		if !strings.Contains(caught, want) {
			t.Errorf("a refusal from the server is not said in the form, about its field (no %s)", want)
		}
	}
	if clear, sent := strings.Index(submit, "clearVerifyRefusal(form);"), strings.Index(submit, "await api("); clear < 0 || clear > sent {
		t.Error("a refusal stays up while the form is sent again")
	}
	refuse := withoutComments(jsFunction(t, script, "refuseVerification"))
	for _, want := range []string{"why.textContent = message;", "why.hidden = false;", "scrollIntoView(",
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
	for _, want := range []string{"done.textContent = BROWSER_FORGOTTEN;", "done.hidden = false;", "scrollIntoView(", "done.focus("} {
		if !strings.Contains(confirm, want) {
			t.Errorf("confirmBrowserForgotten does not %s:\n%s", want, confirm)
		}
	}
	handler := verifyClickHandler(t, script, `if (event.target.closest('[data-action="forget-browser"]')) {`)
	if strings.Contains(handler, "showNotice(") || !strings.Contains(handler, "confirmBrowserForgotten();") {
		t.Errorf("Forget this browser is not confirmed under its button:%s", handler)
	}
}
