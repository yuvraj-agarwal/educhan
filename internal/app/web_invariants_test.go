package app

import (
	"io/fs"
	"net/http"
	"regexp"
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
// where a request takes it, put back where a request left it unspent or got no
// response, and read nowhere else.
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
// address"), an image picked or dropped on it (not one a re-render puts back),
// unticking the badge on a form whose tick stopped a puzzle or that has
// something written or attached, and a post or code request refused after the
// server spent its answer, which the reader, still on its form, is about to
// send again.
func TestCaptchaIsAskedForOnlyWhenSomeoneWrites(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	if n := strings.Count(code, `"/api/captcha"`); n != 1 || !strings.Contains(jsFunction(t, script, "solveCaptcha"), `api("/api/captcha", { signal })`) {
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
	if n := strings.Count(code, "prepareCaptcha()") - 1; n != 4 ||
		!strings.Contains(focus, `if (box.matches("textarea, input[type=text], input[type=email]") && captchaFormNeedsIt(box.form)) { prepareCaptcha(); }`) ||
		!strings.Contains(send, `=== "spent")) { keepCaptcha(taken); } else if (answer && needsIt() && onScreen()) { prepareCaptcha(); }`) ||
		!strings.Contains(toggled, `if (captchaFormNeedsIt(form) && (stopped || captchaFormHasWriting(form))) { prepareCaptcha(); }`) ||
		!strings.Contains(attached, `if (captchaFormNeedsIt(form) && image && image.files && image.files.length) { prepareCaptcha(); }`) {
		t.Errorf("prepareCaptcha() is called %d times in app.js; want four: from the focusin listener in a box to write "+
			"in a form that needs an answer, on unticking the badge where the tick stopped a puzzle or something is "+
			"written, on an image attached to a form that needs an answer, and after a refusal that spent the answer "+
			"of a form still on screen", n)
	}
	// An image counts once it is attached, picked or dropped; one a re-render
	// puts back (restoreDraft, through setFile) is part of drawing a page, and
	// one dropped on a held form, which setFile does not take, is not attached.
	change := squash(withoutComments(jsBlock(t, script, `el.main.addEventListener("change", (event) => {`, "});")))
	drop := squash(withoutComments(jsBlock(t, script, `document.addEventListener("drop", (event) => {`, "});")))
	if strings.Count(code, "captchaImageAttached(") != 3 ||
		!strings.Contains(change, "clearNotice(); updateFileField(field); captchaImageAttached(event.target.form); }") ||
		!strings.Contains(drop, `if (setFile(field, event.dataTransfer.files[0])) { captchaImageAttached(field.closest("form")); }`) {
		t.Error("an image picked or dropped no longer starts the puzzle, once it is attached, or something else calls captchaImageAttached")
	}
	if set := squash(withoutComments(jsFunction(t, script, "setFile"))); strings.Count(set, "return true;") != 1 ||
		!strings.HasSuffix(set, "input.files = transfer.files; } catch { return false; } updateFileField(field); return true; }") {
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
	// A refusal of the answer drops it and sends once more, once: a loop here
	// would hammer /api/captcha. Which outcomes put the answer back is
	// TestEachCaptchaAnswerGoesOnOneRequest's.
	if !strings.Contains(send, "for (let retried = false; ; retried = true) {") ||
		!strings.Contains(send, "if (retried || !needsIt()) {") {
		t.Error("sendWithCaptcha no longer drops a refused answer and resends exactly once")
	}
	// Any other refusal keeps the answer unless the server says it spent it,
	// and then the next puzzle starts at once.
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
// to CAPTCHA_TIMEOUT_MS for nothing.
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
		`challenge = await api("/api/captcha", { signal });`,
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
// leave meanwhile, for another page. A request whose form has gone by the time
// its answer is ready is not sent, and its answer is put back for the next
// press. A posting form stays on screen across a redraw of its own page, by
// route key, while it is held: the redraw puts it in place of the form the
// new page draws, or keeps the page around it when the new one will not load
// (TestAHeldFormSurvivesARedrawOfItsPage). No redraw carries a /verify form,
// so one leaves the screen at the next render, of /verify too: a code request
// sent from the form a redraw was replacing would leave the page on the
// address step while the code went out.
// Sent or not, a request whose form has left the screen is not navigated from
// and says nothing on the page the reader went to, while a post that did go
// still empties and releases its form, wherever it is.
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
		"taken = takeCaptcha(await captchaAnswer());",
		// A request not sent has not spent its answer, which goes back, and
		// the error says it was not sent.
		"if (!onScreen()) { keepCaptcha(taken); throw notSent(); }",
		"return await send(answer);",
	)
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

	submit := squash(withoutComments(submitHandler(t, script)))
	verify := squash(withoutComments(jsFunction(t, script, "submitVerification")))
	for name, fn := range map[string]string{"the submit handler": submit, "submitVerification": verify} {
		// The page is the one on screen at the press, and sendWithCaptcha
		// checks against it; neither request is sent once the browser is
		// forgotten either (TestForgettingTheBrowserCancelsWhatIsOnItsWay).
		onScreen := "const route = state.route; const onScreen = () => formOnScreen(form, route);"
		sends := "() => onScreen() && !forgotten(), );"
		// A post's release says what it took off (TestCaptchaRefusalSaysWhatTheFormStillHolds).
		released := "} catch (err) { const shown = onScreen(); const imageOff = release(shown); button.textContent = label;"
		if name == "submitVerification" {
			onScreen = "const route = state.route; const forgets = browserForgets; const forgotten = () => forgets !== browserForgets; " +
				"const onScreen = () => formOnScreen(form, route) && !forgotten();"
			sends = "onScreen, );"
			released = "} catch (err) { const shown = onScreen(); release(shown); button.textContent = label;"
		}
		inOrder(t, name, fn,
			onScreen,
			sends,
			released,
			"if (!shown) { return; }",
		)
		catch := fn[strings.Index(fn, "} catch (err) {"):]
		before := catch[:max(strings.Index(catch, "if (!shown) { return; }"), 0)]
		for _, banned := range []string{"showNotice(", "navigate(", "reset(", "el.main", "redrawThreadCounts(", "state.thread", "blockFileField("} {
			if strings.Contains(before, banned) {
				t.Errorf("%s's refusal uses %s before it knows the form is still on screen", name, banned)
			}
		}
	}
	// A post that went from a form since gone leaves the page the reader went
	// to as it is, and a code request does not draw /verify over it.
	// The form a post went from is emptied wherever it is, so a redraw that
	// carried it while it was held does not show the post again. A badge
	// confirmed is offered from then on, /verify on screen or not, so a page
	// the reader goes to while /verify is being drawn again, which that
	// drawing then leaves alone, has it too.
	inOrder(t, "the submit handler", submit, "() => onScreen() && !forgotten(), );", "form.reset();",
		"release(false); if (!onScreen()) { return; } await navigate(")
	inOrder(t, "submitVerification", verify,
		"onScreen, );", "if (!onScreen()) { return; }", "el.main.innerHTML = verifyViewHTML();",
		`await api("/api/verify/confirm"`,
		`storeCredential(issued.credential); state.verification.badge = readBadge(); verifyStep = { step: "email", label: "" }; `+
			"if (!onScreen()) { return; } await renderVerify(route);",
	)
	// And that drawing's GET /api/verify failing withdraws neither the badge
	// nor the feature: a failed request keeps the feature as the last answer
	// had it and reads the badge from storage as an answer would, so /verify
	// shows the badge and every posting form drawn after it offers it. Only a
	// page that has had no answer yet, which starts with the feature off,
	// takes a failure to mean off.
	load := squash(withoutComments(jsFunction(t, script, "loadVerification")))
	if !strings.Contains(load, `{ let enabled; try { enabled = !!(await api("/api/verify")).enabled; } `+
		"catch { enabled = state.verification.enabled; } state.verification = { enabled, badge: enabled ? readBadge() : null };") ||
		!strings.Contains(withoutComments(script), "\n  verification: { enabled: false, badge: null },\n") {
		t.Errorf("a GET /api/verify that fails no longer keeps what the page knew, or a first one no longer means off: %s", load)
	}
}

// No answer is on two requests at once. The server takes an answer from only
// one of two requests that carry it, and refuses the other as used, so a page
// that handed its held answer to a second press while the first was on its
// way, a slow upload say, would have one of its own posts refused, and that
// one, its form gone, dropped without a word. So a request takes the answer
// out of the page as it is handed over, in the same step, and a press
// meanwhile solves a fresh puzzle. The answer goes back only when the request
// left it unspent, or brought no response at all, which may have spent it,
// and only into an empty place while it is still good: the next request is
// then refused as used and solves a fresh one.
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
		"do { taken = takeCaptcha(await captchaAnswer()); } while (!taken);",
		`const answer = taken ? taken.answer : "";`,
		// Not sent: put back.
		"if (!onScreen()) { keepCaptcha(taken); throw notSent(); }",
		// Sent and taken: kept by nothing.
		"try { return await send(answer); } catch (err) {",
		// Refused without the spent header or a captcha reason, or never
		// answered: put back. Refused with the header: the next puzzle starts.
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
// greet the next person with the last one's institution, not the name, and
// not a note that a badge was lost, which would tell them there had been one.
// A code request or a post not sent yet, still waiting on its puzzle, is not
// sent: the post would go under the Name and the poster key the forget took
// away, while its form showed neither. That holds for the second send after a
// refused answer too, whether the forget is heard while the second puzzle is
// solving or while the first send is on its way, before its refusal arrives. A
// post's form is released with its text and says why, whichever it was:
// sendWithCaptcha throws the error notSent makes for a request it did not
// send, which on a form still on screen only the forget causes, and the submit
// handler answers that mark before it words any other error. A redraw on its
// way puts back no Name its draft read before the press, and a form left on
// screen loses the option to post with the badge and says what a form without
// it says about the anti-spam check, rather than offering a badge that has
// gone. Each request and each draft notes browserForgets, and only
// forgetOnThisPage moves it, for a press in this tab or one heard from another
// (TestForgettingTheBrowserReachesEveryTab).
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
		`for (const form of el.main.querySelectorAll("form[data-kind]")) { const name = field(form, "name"); `+
		`if (name) { name.defaultValue = ""; name.value = ""; } dropBadgeOption(form); } }`) {
		t.Error("forgetOnThisPage no longer drops everything the page holds that the forget covers")
	}
	// The option goes with its hint, and the form's note on the anti-spam
	// check, drawn in its own element, says what a form drawn without the
	// option says. The submit handler drops the option the same way when the
	// server refuses the badge as expired or revoked.
	drop := squash(withoutComments(jsFunction(t, script, "dropBadgeOption")))
	inOrder(t, "dropBadgeOption", drop,
		`const box = field(form, "badge"); const option = box && box.closest("label");`,
		"option.remove();", `form.querySelector("#f-badge-hint")?.remove();`,
		`const check = form.querySelector("[data-check-hint]"); if (check) { check.textContent = CHECK_HINT; } }`,
	)
	form := squash(withoutComments(jsFunction(t, script, "formHTML")))
	if !strings.Contains(form, "check = ` <span data-check-hint>${held ? CHECK_HINT_WITH_BADGE : CHECK_HINT}</span>`;") ||
		strings.Contains(form, "anti-spam") {
		t.Error("formHTML no longer draws the note on the anti-spam check in the element dropBadgeOption rewords, from the two constants")
	}
	hint, badgeHint := jsString(t, script, "CHECK_HINT"), jsString(t, script, "CHECK_HINT_WITH_BADGE")
	if strings.Contains(hint, "badge") || !strings.Contains(badgeHint, "badge") || !strings.HasSuffix(badgeHint, hint[1:]) {
		t.Errorf("CHECK_HINT should say nothing of a badge, and CHECK_HINT_WITH_BADGE add only that: %q, %q", hint, badgeHint)
	}
	if n := strings.Count(code, "dropBadgeOption(form);"); n != 2 || !strings.Contains(submitHandler(t, script), "dropBadgeOption(form);") ||
		strings.Count(code, "#f-badge-hint") != 1 {
		t.Errorf("the badge option is taken out of a form %d times besides in dropBadgeOption; want twice, a forget's and the submit handler's", n)
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
	verify := squash(withoutComments(jsFunction(t, script, "submitVerification")))
	inOrder(t, "submitVerification", verify,
		"const forgets = browserForgets; const forgotten = () => forgets !== browserForgets; "+
			"const onScreen = () => formOnScreen(form, route) && !forgotten();",
		"const release = holdForm(form);",
		"onScreen, ); if (forgotten()) { return; } verifyStep = { step: \"code\"",
		`await api("/api/verify/confirm", { method: "POST", body: JSON.stringify({ code: String(data.get("code") || "") }), }); `+
			"if (forgotten()) { return; } storeCredential(issued.credential);",
	)
	if strings.Count(verify, "verifyStep =") != 2 || strings.Count(verify, "storeCredential(") != 1 || strings.Count(verify, "readBadge(") != 1 {
		t.Error("submitVerification keeps what a request brings back somewhere this test does not read")
	}
	// A post is held to the same: not sent once forgotten (sendWithCaptcha asks
	// before each send, and again when a refusal of the answer calls for the
	// second), and what it brings back kept only if nothing has moved
	// browserForgets since the press. A post the forget stopped says so on its
	// form, and the error sendWithCaptcha throws for it, whose message is for
	// nobody, never reaches errorMessage.
	submit := squash(withoutComments(submitHandler(t, script)))
	inOrder(t, "the submit handler", submit,
		"const forgets = browserForgets; const forgotten = () => forgets !== browserForgets;",
		"const release = holdForm(form);",
		"() => onScreen() && !forgotten(), );",
		"if (!held && forgets === browserForgets) { storageSet(NAME_STORAGE_KEY, name); storedHere.set(NAME_STORAGE_KEY, name); }",
		`nameField.value = held || forgets !== browserForgets ? storageGet(NAME_STORAGE_KEY) || "" : name;`,
		`if (reason === "expired" || reason === "revoked") { if (forgets === browserForgets) { forgetCredential(); badgeLost =`,
		`if (!shown) { return; } const box = field(form, "body"); const hasText = Boolean(box && box.value.trim()); if (err.notSent) { `+
			"showNotice(imageTakenOff(FORGOTTEN_BEFORE_SENDING, imageOff, { hasText, isThread })); return; }",
		"showNotice(errorMessage(err, { hasText, imageOff }));",
		"imageTakenOff(errorMessage(err, { hasText, imageOff: off }), off, { hasText, isThread, closed });",
	)
	if strings.Count(submit, "storageSet(") != 1 || strings.Count(submit, "badgeLost") != 1 || strings.Count(submit, "forgetCredential(") != 1 {
		t.Error("the submit handler keeps something, or drops a badge, without checking it against browserForgets")
	}
	send := squash(withoutComments(jsFunction(t, script, "sendWithCaptcha")))
	refused := submit[max(strings.Index(submit, "} catch (err) {"), 0):]
	mark := squash(withoutComments(jsFunction(t, script, "notSent")))
	if mark != `function notSent() { const err = new Error("not sent"); err.notSent = true; return err; }` ||
		strings.Count(code, "notSent") != 5 || strings.Count(send, "throw notSent();") != 2 || strings.Count(code, "notSent()") != 3 ||
		strings.Count(refused, "err.notSent") != 1 || strings.Count(send, "new Error(") != 0 || strings.Count(refused, "showNotice(") != 3 {
		t.Error("an error sendWithCaptcha throws for a request it did not send, before a send or in place of the " +
			"second, is no longer marked, or no longer answered by the submit handler alone and before it words any other error")
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
// open in the browser, where it has BroadcastChannel: each cancels what it has
// on its way and drops what it holds itself, as the tab it was pressed in
// does, and draws /verify again if it shows it. Storage is shared, and was
// cleared by the tab it was pressed in before it told the others, but a tab
// that handled a confirmation or a post in between may have written the
// badge, the poster key or the name back before it heard. So each tab notes
// what it stores of those three, and on hearing removes each that storage
// still holds as it stored it, and touches nothing else: not a badge
// confirmed or a key made in another tab since the forget, however late it
// hears. The one message is a constant that says nothing about anyone, only
// forgetBrowser sends it, and a tab acts on nothing else that arrives.
func TestForgettingTheBrowserReachesEveryTab(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if !regexp.MustCompile(`\nconst FORGET_CHANNEL = "educhan:[a-z-]+";\n`).MatchString(code) ||
		!regexp.MustCompile(`\nconst FORGET_MESSAGE = "[a-z-]+";\n`).MatchString(code) {
		t.Error("the forget channel's name or its message is no longer a constant")
	}
	// One channel, opened once, listening for that message and nothing else.
	open := squash(withoutComments(jsFunction(t, script, "openForgetChannel")))
	if strings.Count(code, "new BroadcastChannel(") != 1 || !strings.Contains(code, "\nconst forgetChannel = openForgetChannel();\n") ||
		!strings.Contains(open, `if (typeof BroadcastChannel !== "function") { return null; } try { `+
			`const channel = new BroadcastChannel(FORGET_CHANNEL); channel.addEventListener("message", (event) => { `+
			`if (event.data === FORGET_MESSAGE) { forgottenElsewhere(); } }); return channel; } catch { return null; }`) {
		t.Error("openForgetChannel no longer opens the one channel, where there is one, and acts only on FORGET_MESSAGE")
	}
	// Posted on in one place, forgetBrowser, after its own work, with the
	// constant: the only other postMessage in app.js gives the captcha's
	// worker its puzzle.
	forget := squash(withoutComments(jsFunction(t, script, "forgetBrowser")))
	inOrder(t, "forgetBrowser", forget,
		"forgetOnThisPage();", "forgetCredential();", "localStorage.removeItem(key);",
		"if (forgetChannel) { forgetChannel.postMessage(FORGET_MESSAGE); } }")
	if strings.Count(code, ".postMessage(") != 2 || strings.Count(code, "forgetChannel.postMessage(") != 1 ||
		strings.Count(withoutComments(jsFunction(t, script, "runCaptchaWorkers")), "worker.postMessage({") != 1 ||
		strings.Count(code, "forgetChannel") != 3 {
		t.Error("something besides forgetBrowser posts on the forget channel, or uses it")
	}
	// A tab that hears it removes what it stored itself, only where storage
	// still holds it as stored, and then forgets what it holds.
	elsewhere := squash(withoutComments(jsFunction(t, script, "forgottenElsewhere")))
	if elsewhere != `function forgottenElsewhere() { for (const [key, value] of storedHere) { `+
		`if (storageGet(key) === value) { storageRemove(key); } } forgetOnThisPage(); `+
		`if (state.route.view === "verify" && ageStatus() === "ok") { renderVerify(state.route).then((drawn) => { `+
		`if (drawn) { showNotice(BROWSER_FORGOTTEN); } }); } }` {
		t.Errorf("a tab that hears the forget no longer removes only what it stored and storage still holds, "+
			"forgets what it holds, or draws /verify again: %s", elsewhere)
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
	for fn, want := range map[string]string{
		"storeCredential": "localStorage.setItem(CREDENTIAL_STORAGE_KEY, token); storedHere.set(CREDENTIAL_STORAGE_KEY, token);",
		"posterKey":       "localStorage.setItem(POSTER_KEY_STORAGE_KEY, pagePosterKey); storedHere.set(POSTER_KEY_STORAGE_KEY, pagePosterKey);",
	} {
		if !strings.Contains(squash(withoutComments(jsFunction(t, script, fn))), want) {
			t.Errorf("%s no longer notes what it stores as it stores it: missing %s", fn, want)
		}
	}
	submit := squash(withoutComments(submitHandler(t, script)))
	if !strings.Contains(submit, "storageSet(NAME_STORAGE_KEY, name); storedHere.set(NAME_STORAGE_KEY, name);") {
		t.Error("the submit handler no longer notes the name it stores as it stores it")
	}
	if !strings.Contains(code, "\nconst storedHere = new Map();\n") || strings.Count(code, "storedHere.set(") != 3 ||
		strings.Count(code, "storedHere") != 6 || strings.Count(code, "of storedHere)") != 1 || strings.Count(code, "storedHere.clear();") != 1 {
		t.Error("storedHere is written, read or cleared somewhere besides where the three are stored, forgottenElsewhere and forgetOnThisPage")
	}
	if strings.Count(code, "forgottenElsewhere(") != 2 || strings.Count(code, "forgetOnThisPage(") != 3 {
		t.Error("forgetOnThisPage or forgottenElsewhere is called from somewhere besides a forget")
	}
}

// From the press until the answer, a post's or a code request's form is
// held: its controls switched off, once it has been read and checked, so
// nothing the reader changes meanwhile (a badge ticked, an address corrected,
// a line added) is left out of what is sent, or wiped by the reset after it.
// A refusal switches back on what was switched off, and nothing else, before
// the recovery that may switch the picker off for good, and gives the focus
// that switching off took back to the form's button, unless the reader has
// put it somewhere since, without scrolling a reader who has scrolled away
// from the form back to it; nothing writes into a held form from outside it:
// not a quote, not a dropped image; and app.css shows the held controls
// switched off.
func TestAPendingFormIsHeld(t *testing.T) {
	script := appScript(t)
	hold := squash(withoutComments(jsFunction(t, script, "holdForm")))
	for _, want := range []string{
		"const controls = Array.from(form.elements).filter((control) => !control.disabled);",
		"for (const control of controls) { control.disabled = true; } heldForms.add(form);",
		"return (refocus) => { heldForms.delete(form); for (const control of controls) { control.disabled = false; }",
	} {
		if !strings.Contains(hold, want) {
			t.Errorf("holdForm no longer switches off exactly the controls that were on, and back: missing %s", want)
		}
	}
	if strings.Count(hold, "form.elements") != 1 || strings.Count(hold, ".disabled = false") != 1 {
		t.Error("holdForm switches on controls it did not switch off")
	}
	// Switching off the focused control leaves the focus nowhere; a form that
	// had it at the press, refused while on screen, has it back on its button,
	// unless the reader moved it during the wait: then it stays where they put
	// it. The page does not scroll to the button: a reader who scrolled away
	// meanwhile, which leaves the focus lost, stays where they are, whether or
	// not that is the top of the page, where the notice is.
	inOrder(t, "holdForm", hold,
		"const focused = form.contains(document.activeElement);",
		"control.disabled = true;",
		"const lost = !document.activeElement || document.activeElement === document.body; "+
			`if (refocus && focused && lost) { form.querySelector('button[type="submit"]')?.focus({ preventScroll: true }); }`,
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
	// straight into the try whose catch releases it: a check that refused
	// after the hold, or any return between the hold and the try, would leave
	// the form switched off for good, and a hold before the file is read would
	// read the picker as switched off and post without the image.
	held := "const release = holdForm(form); button.textContent = sending; clearNotice(); try {"
	inOrder(t, "the submit handler", submit, "const data = new FormData(form);",
		"const file = image && !image.disabled && image.files[0];", "if (!body.trim() && !file) {", "if (wantsBadge && !held) {",
		held, "sendWithCaptcha(", "} catch (err) {", "release(shown);")
	inOrder(t, "submitVerification", verify, "const data = new FormData(form);",
		"if (requesting && !(adult && adult.checked)) {",
		held, "sendWithCaptcha(", `await api("/api/verify/confirm"`, "} catch (err) {", "release(shown);")
	// The button is held with the rest, and released on a refusal, and for a
	// post also after a success, once the form is emptied, so that a redraw
	// does not carry it as still held (TestAHeldFormSurvivesARedrawOfItsPage);
	// after a code request's success the page is replaced.
	for name, fn := range map[string]string{"the submit handler": submit, "submitVerification": verify} {
		want := 1
		if name == "the submit handler" {
			want = 2
		}
		if strings.Count(fn, "release(") != want || strings.Contains(fn, "button.disabled") {
			t.Errorf("%s switches its controls on somewhere besides its refusal and, for a post, its success, or its button apart from them", name)
		}
	}
	inOrder(t, "the submit handler", submit, "sendWithCaptcha(", "form.reset();", "clearFileField(fileField); } release(false);", "} catch (err) {")
	if blocked, released := strings.Index(submit, "blockFileField("), strings.Index(submit, "release(shown);"); blocked < released {
		t.Error("the submit handler switches its controls back on after switching the picker off, which turns the picker back on")
	}

	if quote := withoutComments(jsFunction(t, script, "quoteTarget")); !strings.Contains(quote, `textarea[name="body"]:not(:disabled)`) {
		t.Error("quoteTarget writes a quote into a reply box held while its post is pending")
	}
	if set := squash(withoutComments(jsFunction(t, script, "setFile"))); !strings.Contains(set,
		`if (input.disabled) { if (field.classList.contains("blocked")) { showNotice(field.querySelector(".file-blocked").textContent); } return false; }`) {
		t.Error("setFile takes a file into a picker held while its post is pending, or says nothing for a blocked one")
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
// read like any draft, so one whose post went is drawn empty. Its page is told
// by routeKey, which leaves out the index page, so another page of the same
// board's index carries a held new-thread form as Update does. And when the
// hold ends, success or refusal, release leaves its picker as the new page drew
// one: a redraw that drew it switched off, the thread having reached its image
// limit meanwhile, has the held form's switched off with the same reason,
// rather than offering an image to a thread that takes no more, and the image
// it held taken off, which release reports for the notice to say
// (TestCaptchaRefusalSaysWhatTheFormStillHolds); one that drew it live, the
// limit raised meanwhile, has it switched on, though it was switched off at the
// press; and one that drew none, on a site that takes no images, has it
// switched off with the site's reason. A refusal of the image itself, the image
// limit reached or uploads switched off, then switches it off in the submit
// handler.
func TestAHeldFormSurvivesARedrawOfItsPage(t *testing.T) {
	script := appScript(t)
	if !strings.Contains(withoutComments(script), "\nconst heldForms = new WeakSet();\n") {
		t.Fatal("app.js no longer keeps the held forms in heldForms")
	}
	if key := squash(withoutComments(jsFunction(t, script, "routeKey"))); key !=
		"function routeKey(route) { return `${route.view}:${route.slug || \"\"}:${route.no || \"\"}`; }" {
		t.Errorf("routeKey is no longer the view, the board and the thread, without the index page: %s", key)
	}
	// A draft is keyed by the page on screen, which the render that drew it
	// noted, never by a render begun since that has not drawn: the second of a
	// double click would carry this page's form into the other page's.
	capture := squash(withoutComments(jsFunction(t, script, "captureDraft")))
	inOrder(t, "captureDraft", capture,
		"const key = routeKey(drawnFor.route || state.route);",
		`const form = el.main.querySelector("form[data-kind]");`,
		"return unsent ? { key, open: true, body: unsent.value } : null;",
		"if (heldForms.has(form)) { return { key, held: form }; }",
		"return { key, ...formDraft(form) };",
	)
	if strings.Count(capture, "routeKey(") != 1 || strings.Count(capture, "key") != 4 {
		t.Error("captureDraft keys a draft by something besides the page on screen")
	}
	restore := squash(withoutComments(jsFunction(t, script, "restoreDraft")))
	inOrder(t, "restoreDraft", restore,
		"if (!draft || draft.key !== routeKey(state.route)) { return; }",
		`const form = el.main.querySelector("form[data-kind]");`,
		"if (draft.held) { const held = draft.held; if ( heldForms.has(held) && form && "+
			"form.dataset.kind === held.dataset.kind && form.dataset.thread === held.dataset.thread ) { "+
			`if (held.querySelector("[data-file-field]")) { redrawnPickers.set(held, pickerRefusal(form)); } form.replaceWith(held);`,
		"return; } answered = heldForms.has(held) ? null : held; draft = { key: draft.key, ...formDraft(held) }; }",
		"if (!form) { keepUnsentDraft(draft); return; }",
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
	// A kept page keeps its thread and its open image until a new page is
	// drawn over it, so a refusal answered while the redraw loads updates the
	// page on screen. A render a newer one has overtaken draws nothing, loaded
	// or not, and one that draws notes what the page was drawn for (drawnFor),
	// which a kept page whose redraw will not load gets back, with the error
	// as a notice, never in place of the page: after two redraws that fail, the
	// page is still the one drawn, not the first redraw's half-begun one. Every
	// render begun, the age screen's and the rules' included, ends the loading
	// state of any before it (below). A post link's lookup goes on to the
	// thread unless the reader went somewhere else meanwhile, which a jump
	// within the page, moving only the fragment, is not: the address is
	// compared less its fragment, or a skip link pressed during the lookup
	// would leave the skeleton on screen for good.
	render := squash(withoutComments(jsFunction(t, script, "render")))
	inOrder(t, "render", render,
		"async function render({ draft = captureDraft() } = {}) {",
		"const requested = window.location.pathname + window.location.search; renderedPath = requested; "+
			"const route = parseRoute(); state.route = route; "+
			"rendersBegun += 1; endLoading(); "+
			"const keepPage = Boolean(draft && draft.held && draft.key === routeKey(route)); "+
			"if (!keepPage) { state.thread = null; expandedLink = null; }",
		"renderAgeGate(age); return; }",
		"const finish = beginLoading({ skeleton: !keepPage });",
		"await loadSite();",
		"if (overtaken(route)) { return; } renderNav(route);",
		"} catch (err) { if (overtaken(route)) { return; }",
		"if (keepPage && draft.held.isConnected) { renderedPath = drawnFor.path; state.route = drawnFor.route; "+
			`state.thread = drawnFor.thread; const said = el.notice.hidden ? "" : el.notice.querySelector(".notice-text").textContent; `+
			"showNotice(said ? `${said} ${message}` : message); return; }",
		"expandedLink = null; el.main.innerHTML = emptyStateHTML(message, board);",
		"} finally {",
		"if (!overtaken(route) && window.location.pathname + window.location.search === requested) { "+
			`window.history.replaceState(null, "", resolved); await render({ draft }); } return; }`,
		"if (overtaken(route)) { return; } drawnFor = { path: renderedPath, route, thread: state.thread }; "+
			"expandedLink = null; enhancePosts(el.main); restoreDraft(draft);",
	)
	if n := strings.Count(render, "requested"); n != 3 || strings.Contains(render, "location.href") {
		t.Errorf("requested appears %d times in render; want it taken, less the fragment, noted as renderedPath "+
			"and compared once", n)
	}
	if n := strings.Count(render, "keepPage"); n != 4 {
		t.Errorf("keepPage appears %d times in render; want its declaration, the resets, the skeleton and the failed load", n)
	}
	if over := squash(withoutComments(jsFunction(t, script, "overtaken"))); over !=
		"function overtaken(route) { return state.route !== route; }" {
		t.Errorf("overtaken no longer says whether a newer render has begun: %s", over)
	}
	if code := withoutComments(script); !strings.Contains(code, "\nlet drawnFor = { path: null, route: null, thread: null };\n") ||
		strings.Count(code, "drawnFor") != 6 {
		t.Error("drawnFor is no longer only declared, noted by a render that draws, read by captureDraft and put back by a " +
			"kept page's failed redraw")
	}
	for _, view := range []string{"renderIndex", "renderCatalog", "renderArchive", "renderThread", "renderVerify"} {
		fn := squash(withoutComments(jsFunction(t, script, view)))
		steps := []string{" = await api(", "if (overtaken(route)) { return; }", "el.main.innerHTML = "}
		switch view {
		case "renderThread":
			steps = []string{" = await api(", "if (overtaken(route)) { return; }", "state.thread = detail;", "el.main.innerHTML = "}
		case "renderVerify":
			steps = []string{"async function renderVerify(route) { await loadVerification(); if (overtaken(route)) { return false; } " +
				`el.main.innerHTML = verifyViewHTML(); badgeLost = ""; announce("Institutional verification"); return true; }`}
		}
		inOrder(t, view, fn, steps...)
	}
	// /verify is drawn for the route it was asked for, as that route was before
	// the request: by render, by the confirmation, and after a forget, pressed
	// here or heard from another tab, only while /verify is the page being
	// drawn, whose notice is said only where it drew.
	code := withoutComments(script)
	forgot := "renderVerify(state.route).then((drawn) => { if (drawn) { showNotice(BROWSER_FORGOTTEN); } });"
	if squashed := squash(code); strings.Count(code, "renderVerify(") != 6 || strings.Count(code, "await renderVerify(route);") != 2 ||
		strings.Count(squashed, `if (state.route.view === "verify") { renderVerify(state.route); }`) != 1 ||
		strings.Count(squashed, `if (state.route.view === "verify") { `+forgot+" }") != 1 ||
		strings.Count(squashed, `if (state.route.view === "verify" && ageStatus() === "ok") { `+forgot+" }") != 1 ||
		!strings.Contains(squash(withoutComments(jsFunction(t, script, "render"))), "await renderVerify(route);") {
		t.Error("renderVerify is called somewhere without the route it draws for, or for a page that is not /verify, " +
			"or a forget's notice is said where it did not draw")
	}
	// The loading state is the latest render's: beginning one ends any before
	// it, and a render's skeleton and its finish act only while it is the
	// latest, so an overtaken render neither blanks the newer page nor takes
	// the dimming off one still loading. overtaken would not do: a kept page's
	// failed redraw overtakes itself.
	loading := squash(withoutComments(jsFunction(t, script, "beginLoading")))
	inOrder(t, "beginLoading", loading,
		`function beginLoading({ skeleton = true } = {}) { const begun = rendersBegun; el.main.setAttribute("aria-busy", "true"); `+
			"loadingTimer = setTimeout(() => { if (skeleton && begun === rendersBegun) { el.main.innerHTML =",
		"}, 250); return () => { if (begun === rendersBegun) { endLoading(); } }; }",
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
// before it is solved waits with the button saying what the FAQ says it says.
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
	for name, fn := range map[string]string{
		"the submit handler": submitHandler(t, script),
		"submitVerification": jsFunction(t, script, "submitVerification"),
	} {
		if !strings.Contains(fn, `button.textContent = solving ? "Checking…" : sending;`) {
			t.Errorf("%s no longer says Checking… on its button while it waits for the puzzle", name)
		}
	}
}

// A refused post keeps what its form still holds, and the notice says so in
// the terms of what is there once the hold has ended, never of what was
// pressed: text in the box, or only an image, or nothing, when a redraw while
// the post was held drew its picker switched off and release took the image
// off. release then returns the picker's reason, and every notice after it but
// the two refusals of the image itself, which say so in their own words, adds
// that the image has been taken off and why, and tells a post left with
// nothing to write something instead; a refusal of the thread itself
// (archived, locked, gone) adds only that the image was taken off, since no
// post can go there. A refusal answered before the redraw has drawn ends the
// same: restoreDraft words its notice again with the picker's reason, as it
// would have read had the redraw come first. So a captcha refusal or a
// connection lost says "Your image is still attached" only of an image that
// is, and an archived thread is not answered with "Text replies still work".
func TestCaptchaRefusalSaysWhatTheFormStillHolds(t *testing.T) {
	script := appScript(t)
	message := squash(withoutComments(jsFunction(t, script, "errorMessage")))
	if !strings.HasPrefix(message, `function errorMessage(err, { hasText = true, imageOff = "", action = "post" } = {}) { `+
		`const nothingLeft = !hasText && imageOff !== "";`) {
		t.Errorf("errorMessage no longer tells a post left with nothing from one that kept its text or its image: %.200s", message)
	}
	branch := jsBraced(t, message, "if (err.status === 403 && err.payload && err.payload.captcha) {")
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
		"if (err.unreachable) { return nothingLeft ? UNREACHABLE : hasText ? `${UNREACHABLE} What you wrote is still in the box.` : " +
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
	// The submit handler reads the text from the box after release, and adds
	// what release took off to every notice but the image's own refusals.
	submit := squash(withoutComments(submitHandler(t, script)))
	refused := submit[max(strings.Index(submit, "} catch (err) {"), 0):]
	inOrder(t, "the submit handler", refused,
		"const imageOff = release(shown);",
		`const box = field(form, "body"); const hasText = Boolean(box && box.value.trim());`,
		"showNotice(imageTakenOff(FORGOTTEN_BEFORE_SENDING, imageOff, { hasText, isThread }));",
		"const limitReached = err.status === 403 && err.message === IMAGE_LIMIT_REACHED; if (limitReached) {",
		"if (limitReached || uploadsOff) { showNotice(errorMessage(err, { hasText, imageOff })); return; }",
		`const closed = err.status === 404 || (err.status === 403 && (err.message === "thread is archived" || `+
			`err.message === "thread is locked")); `+
			"const notice = (off) => imageTakenOff(errorMessage(err, { hasText, imageOff: off }), off, { hasText, isThread, closed }); "+
			"refusalNotices.set(form, notice); showNotice(notice(imageOff)); } });",
	)
	code := withoutComments(script)
	if strings.Count(refused, "showNotice(") != 3 || strings.Count(refused, "body.trim()") != 0 ||
		strings.Count(code, "imageTakenOff(") != 4 {
		t.Error("the submit handler words a refusal somewhere besides the three notices this test reads, or from the text pressed, " +
			"or something besides it and restoreDraft says an image was taken off")
	}
	// A held form answered while a redraw of its page loaded, refused before the
	// redraw drew, is read like any draft, and an image the new page takes none
	// of is taken off, its notice saying so as it would have, had the redraw
	// come first, rather than giving way to the picker's reason: the refusal's
	// own notice, still on screen, worded again with that reason by the wording
	// the submit handler kept, and any other with the image's going added.
	restore := squash(withoutComments(jsFunction(t, script, "restoreDraft")))
	inOrder(t, "restoreDraft", restore,
		"let answered = null;",
		"answered = heldForms.has(held) ? null : held; draft = { key: draft.key, ...formDraft(held) }; }",
		`const fileField = form.querySelector("[data-file-field]"); const refusal = answered && draft.file ? pickerRefusal(form) : ""; `+
			`if (refusal) { const said = el.notice.hidden ? "" : el.notice.querySelector(".notice-text").textContent; `+
			"const notice = refusalNotices.get(answered); "+
			`const kept = { hasText: Boolean(draft.body.trim()), isThread: form.dataset.kind === "thread" }; `+
			`showNotice(notice && said === notice("") ? notice(refusal) : said ? imageTakenOff(said, refusal, kept) : refusal); } `+
			"else if (fileField && draft.file && setFile(fileField, draft.file)) {",
	)
	if !strings.Contains(code, "\nconst refusalNotices = new WeakMap();\n") || strings.Count(code, "refusalNotices") != 3 {
		t.Error("refusalNotices is no longer only declared, kept by the submit handler and read by restoreDraft")
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
