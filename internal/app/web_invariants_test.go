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
	"dropCaptcha", "solveCaptcha", "runCaptchaWorkers", "sendWithCaptcha",
}

// A captcha answer is good for one request and kept in memory only: never in
// storage, a cookie or a draft, where it would outlive the page or be found by
// the next person at a shared computer. It is set in one place, cleared in one
// other, and read only where it is handed to a request.
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
	// The two requests that carry an answer may keep other things, a post's
	// name say, but nothing that mentions the answer.
	keeps := regexp.MustCompile(`(` + strings.Join(append(banned, "captureDraft", "keepUnsentDraft"), "|") + `)[^;]*`)
	for name, fn := range map[string]string{
		"the submit handler": submitHandler(t, script),
		"submitVerification": jsFunction(t, script, "submitVerification"),
	} {
		for _, call := range keeps.FindAllString(withoutComments(fn), -1) {
			if lower := strings.ToLower(call); strings.Contains(lower, "answer") || strings.Contains(lower, "captcha") {
				t.Errorf("%s keeps the captcha answer: %s", name, call)
			}
		}
	}
	code := withoutComments(script)
	if n := strings.Count(code, "captcha.answer = "); n != 2 ||
		!strings.Contains(jsFunction(t, script, "solveCaptcha"), "captcha.answer = btoa(") ||
		!strings.Contains(jsFunction(t, script, "dropCaptcha"), `captcha.answer = "";`) {
		t.Errorf("captcha.answer is assigned %d times; want once in solveCaptcha and once, clearing it, in dropCaptcha", n)
	}
	// Read anywhere else, the held answer could go out on a request that
	// should not carry one, a badged post's.
	reads := strings.Count(code, "captcha.answer")
	for _, name := range []string{"captchaAnswer", "dropCaptcha", "solveCaptcha"} {
		reads -= strings.Count(withoutComments(jsFunction(t, script, name)), "captcha.answer")
	}
	if reads != 0 {
		t.Errorf("captcha.answer is used %d times outside captchaAnswer, dropCaptcha and solveCaptcha; a request gets its answer from sendWithCaptcha", reads)
	}
}

// A reader who never starts to write never asks for a captcha: the challenge
// is fetched in one place, and the only things that start a fetch before a
// request needs it are a reader moving into a box to write in, or unticking
// the badge in a form they are writing in, and a post or code request refused
// after the server spent its answer, which the reader is about to send again.
func TestCaptchaIsAskedForOnlyWhenSomeoneWrites(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)

	if n := strings.Count(code, `"/api/captcha"`); n != 1 || !strings.Contains(jsFunction(t, script, "solveCaptcha"), `api("/api/captcha", { signal })`) {
		t.Errorf(`"/api/captcha" appears %d times in app.js; want once, in solveCaptcha`, n)
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
	focus := jsBlock(t, script, `el.main.addEventListener("focusin", (event) => {`, "});")
	send := withoutComments(jsFunction(t, script, "sendWithCaptcha"))
	toggled := withoutComments(jsFunction(t, script, "captchaBadgeToggled"))
	if n := strings.Count(code, "prepareCaptcha()") - 1; n != 3 || !strings.Contains(focus, "prepareCaptcha()") ||
		!strings.Contains(send, `=== "spent") {
          dropCaptcha(answer);
          if (needsIt()) prepareCaptcha();`) ||
		!strings.Contains(toggled, `} else if (captchaFormNeedsIt(box.form)) {
    prepareCaptcha();`) {
		t.Errorf("prepareCaptcha() is called %d times in app.js; want three, from the focusin listener, on unticking the badge, and after a refusal that spent the answer", n)
	}
	if !strings.Contains(focus, `box.matches("textarea, input[type=text], input[type=email]")`) {
		t.Error("the focusin listener no longer starts the puzzle only from a box to write in")
	}
	for _, name := range []string{"render", "formHTML", "verifyViewHTML", "loadSite"} {
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
	toggled := withoutComments(jsFunction(t, script, "captchaBadgeToggled"))
	stop := withoutComments(jsFunction(t, script, "stopCaptcha"))
	waits := withoutComments(jsFunction(t, script, "sendWithCaptcha"))
	change := withoutComments(jsBlock(t, script, `el.main.addEventListener("change", (event) => {`, "});"))
	if !strings.Contains(change, "captchaBadgeToggled(event.target);") ||
		!strings.Contains(toggled, "if (box.checked) {\n    stopCaptcha();") ||
		!strings.Contains(stop, "if (captcha.stop && captcha.waiting === 0) {") ||
		!strings.Contains(waits, "captcha.waiting += 1;") || !strings.Contains(waits, "captcha.waiting -= 1;") {
		t.Error("ticking the badge no longer stops a puzzle nobody is waiting for")
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
	needs := withoutComments(jsFunction(t, script, "captchaFormNeedsIt"))
	if !strings.Contains(needs, "!(badge && badge.checked)") {
		t.Error("captchaFormNeedsIt no longer leaves a badged post's puzzle unsolved")
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

	send := withoutComments(jsFunction(t, script, "sendWithCaptcha"))
	if !strings.Contains(send, "err.payload && err.payload.captcha") || !strings.Contains(send, `reason === "`+captchaMissing+`"`) {
		t.Errorf("sendWithCaptcha no longer reads the refusal's captcha field, or no longer knows %q", captchaMissing)
	}
	// A refusal of the answer drops it and sends once more, once: a loop here
	// would hammer /api/captcha. The answer is dropped after a success, on a
	// captcha refusal, and on a refusal that says it was spent, and nowhere else.
	if !strings.Contains(send, "for (let retried = false; ; retried = true) {") ||
		!strings.Contains(send, "if (retried || !needsIt()) {") || strings.Count(send, "dropCaptcha(answer);") != 3 {
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
