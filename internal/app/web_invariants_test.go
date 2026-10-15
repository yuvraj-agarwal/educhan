package app

import (
	"io/fs"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
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

// A post about wanting to hurt yourself is answered once with where to get
// help, and "Post anyway" sends the same post again with support_seen, which
// takes it past that one step. The flag belongs to that one submission: the
// submit handler reads it from the button that submitted and puts it on that
// request, and nothing keeps it — not storage, not the draft, not a field or
// attribute on the form — so the next post with the same words is asked
// again, and no browser carries a standing mark that it has seen the help.
func TestSupportSeenIsSentOnlyFromSubmitAndNeverKept(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	submit := withoutComments(submitHandler(t, script))

	// The wire name twice, both in the submit handler: a JSON post and a
	// multipart one.
	if n, in := strings.Count(code, "support_seen"), strings.Count(submit, "support_seen"); n != 2 || in != 2 {
		t.Errorf(`"support_seen" appears %d times in app.js, %d of them in the submit handler; want twice, both there`, n, in)
	}
	for _, want := range []string{
		`const supportSeen = Boolean(event.submitter && event.submitter.matches('[data-action="post-anyway"]'));`,
		`payload.support_seen = true;`,
		`data.set("support_seen", "1");`,
	} {
		if !strings.Contains(submit, want) {
			t.Errorf("the submit handler no longer has %s", want)
		}
	}
	// supportSeen is the submit handler's own, and lives no longer than it.
	if n, in := strings.Count(code, "supportSeen"), strings.Count(submit, "supportSeen"); n != in || in == 0 {
		t.Errorf("supportSeen appears %d times in app.js and %d in the submit handler; want every use there", n, in)
	}
	for _, line := range strings.Split(code, "\n") {
		if !strings.Contains(line, "supportSeen") && !strings.Contains(line, "support_seen") {
			continue
		}
		lower := strings.ToLower(line)
		for _, keeper := range []string{"storage", "dataset", "setattribute", "cookie", "history", "draft", "indexeddb"} {
			if strings.Contains(lower, keeper) {
				t.Errorf("the support flag is kept somewhere: %s", strings.TrimSpace(line))
			}
		}
	}
	// The draft carried across a re-render knows nothing of the support step.
	for _, name := range []string{"captureDraft", "restoreDraft", "keepUnsentDraft", "storageSet", "storageGet", "api"} {
		if body := withoutComments(jsFunction(t, script, name)); strings.Contains(strings.ToLower(body), "support") {
			t.Errorf("%s mentions the support step; the flag must not outlive one submission", name)
		}
	}
	// The panel's button is what says which submission it is: a submit
	// button with no name, so no form data carries it, and no field beside it.
	panel := withoutComments(jsFunction(t, script, "supportPanelHTML"))
	button := regexp.MustCompile(`<button[^>]*data-action="post-anyway"[^>]*>`).FindString(panel)
	if button == "" || !strings.Contains(button, `type="submit"`) || strings.Contains(button, "name=") {
		t.Errorf("Post anyway is no longer an unnamed submit button of the form: %q", button)
	}
	if strings.Contains(panel, "<input") {
		t.Error("the support panel has a field, which every later submission would carry")
	}
}

// The posting form reads the content rules' answers by the server's own words:
// the two refusal outcomes, the block rules it has sentences for, the held
// acknowledgement, and support_seen in the encodings submit sends it in. If
// they drifted apart, a post with crisis language could not be made from the
// page at all, and a held post would send its poster to a page without it.
func TestClientReadsTheContentRulesAnswersTheServerGives(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	for _, want := range []string{
		`outcome === "support"`, `outcome === "block"`, `err.payload.outcome === "block"`,
		`case "` + ruleContact + `":`, `case "` + ruleIDNumber + `":`, `created.held`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("app.js no longer reads %s", want)
		}
	}

	h := testServer(t, nil)
	h.addBoard("words")
	thread := h.createThread("words", "", "", "a thread")
	replies := repliesPath("words", thread.ThreadNo)

	status, out := h.postJSON(replies, map[string]any{"name": "", "body": "call me on 9876543210", "badge": false, "sage": false})
	if status != http.StatusUnprocessableEntity || out["outcome"] != "block" || out["rule"] != ruleContact ||
		out["field"] != "body" || out["match"] != "9876543210" {
		t.Errorf("a phone number: %d %v, want 422 block contact in body", status, out)
	}

	// What submit sends: the form as JSON, and as multipart with an image.
	crisis := map[string]any{"name": "", "body": "i want to die", "badge": false, "sage": false}
	if status, out := h.postJSON(replies, crisis); status != http.StatusUnprocessableEntity || out["outcome"] != "support" {
		t.Errorf("crisis language: %d %v, want 422 support", status, out)
	}
	crisis["support_seen"] = true
	if status, out := h.postJSON(replies, crisis); status != http.StatusCreated {
		t.Errorf("crisis language with support_seen: %d %v, want 201", status, out)
	}
	fields := map[string]string{"name": "", "body": "i want to die"}
	if status, out := h.uploadMap(replies, fields, "pic.png", pngBytes(t, 24, 24)); status != http.StatusUnprocessableEntity || out["outcome"] != "support" {
		t.Errorf("multipart crisis language: %d %v, want 422 support", status, out)
	}
	fields["support_seen"] = "1"
	if status, out := h.uploadMap(replies, fields, "pic.png", pngBytes(t, 24, 24)); status != http.StatusCreated {
		t.Errorf(`multipart crisis language with support_seen "1": %d %v, want 201`, status, out)
	}

	status, out = h.postJSON(threadsPath("words"), map[string]any{"name": "", "body": "how old are you? dm me", "badge": false, "subject": ""})
	if status != http.StatusAccepted || out["held"] != true || out["no"] != nil {
		t.Errorf("a held thread: %d %v, want 202 held with no number", status, out)
	}

	// A match too long to quote whole ends in an ellipsis, which pointAtMatch
	// takes off before it looks for the text and then selects to the end of
	// the run, so the whole of a long link is selected rather than its start.
	link := "https://discord.gg/" + strings.Repeat("abcdefghij", 10)
	status, out = h.postJSON(replies, map[string]any{"name": "", "body": "come chat " + link + " now", "badge": false, "sage": false})
	match, _ := out["match"].(string)
	if status != http.StatusUnprocessableEntity || !strings.HasSuffix(match, "…") ||
		!strings.HasPrefix(link, strings.TrimSuffix(match, "…")) || utf8.RuneCountInString(match) > maxMatchLen {
		t.Errorf("a long link: %d %v, want 422 with the link's start and an ellipsis, at most %d characters", status, out, maxMatchLen)
	}
	point := withoutComments(jsFunction(t, script, "pointAtMatch"))
	for _, want := range []string{`endsWith("…")`, `slice(0, -1)`, `/^\S*/.exec(input.value.slice(end))`} {
		if !strings.Contains(point, want) {
			t.Errorf("pointAtMatch no longer reads a cut match: missing %s", want)
		}
	}
	// The copy it selects is the one the server counted (matchOccurrence):
	// the quote's words with any whitespace between, every search one
	// character on from the last place found, and the occurrence'th.
	for _, want := range []string{
		`{ field: where, match, occurrence }`, `words.join("\\s+"), "g")`, `search.lastIndex = next.index + 1;`,
		`occurrence + 1`,
	} {
		if !strings.Contains(point, want) {
			t.Errorf("pointAtMatch no longer selects the copy the server refused: missing %s", want)
		}
	}
	if strings.Contains(point, "indexOf(") {
		t.Error("pointAtMatch searches with indexOf, which finds the first copy whichever the server refused")
	}
	// The sentence is the server's, which says what it found: a request to
	// talk on another app is not a contact detail.
	blocked := withoutComments(jsFunction(t, script, "blockedMessage"))
	if !strings.Contains(blocked, "{ error, rule, field: where, match }") || !strings.Contains(blocked, "${error.trim()}") {
		t.Error("blockedMessage no longer shows the server's sentence for what it found")
	}
	status, out = h.postJSON(replies, map[string]any{"name": "", "body": "what's your insta?", "badge": false, "sage": false})
	if msg, _ := out["error"].(string); status != http.StatusUnprocessableEntity || out["rule"] != ruleContact ||
		!strings.Contains(msg, "asks people to talk on another app") {
		t.Errorf("an ask: %d %v, want a contact block whose sentence says it is an ask", status, out)
	}
	// The least scroll leaves a field that is on screen but under the sticky
	// header where it is; on a phone the header is three rows deep, and the
	// Name field is right under it.
	reveal := withoutComments(jsFunction(t, script, "revealBelowHeader"))
	if !strings.Contains(point, "revealBelowHeader(input)") || !strings.Contains(reveal, `scrollIntoView({ block: "start" })`) ||
		!strings.Contains(reveal, ".site-header") {
		t.Error("pointAtMatch no longer brings the field out from under the sticky header")
	}
	// So does Go back on the support panel: closing the panel shortens the
	// page, which can leave the message under the header, where a plain
	// focus() does not scroll.
	opener := `const supportBack = event.target.closest('[data-action="support-back"]');`
	i := strings.Index(script, opener)
	if i < 0 {
		t.Fatal("app.js has no Go back handler for the support panel; this test reads it")
	}
	back := withoutComments(script[i:])
	back = back[:strings.Index(back, "return;")]
	if !strings.Contains(back, "message.focus({ preventScroll: true });") || !strings.Contains(back, "revealBelowHeader(message);") {
		t.Error("Go back on the support panel no longer brings the message out from under the sticky header")
	}
}

// A post's answer can arrive after the page was drawn again — "Update" on the
// thread while the reply was on its way — and then the form it was sent from
// is out of the document, with the draft in the form that replaced it. An
// answer written into the old form is seen by nobody: a held reply looks
// unsent and is posted twice, and a block or the support panel never shows.
// After the request, everything is done to liveForm's form.
func TestPostAnswersGoToTheFormOnScreen(t *testing.T) {
	handler := withoutComments(submitHandler(t, appScript(t)))
	i := strings.Index(handler, "await api(")
	if i < 0 {
		t.Fatal("the submit handler no longer awaits api(); this test reads what follows it")
	}
	after := handler[i:]
	if n := strings.Count(after, "liveForm(form, draftKey)"); n != 2 {
		t.Errorf("liveForm(form, draftKey) is called %d times after the request, want once for an answer and once for a refusal", n)
	}
	for _, stale := range []string{
		"showFormNotice(form", "showSupport(form", "pointAtMatch(form", "closeSupport(form",
		"form.reset(", "field(form", "form.querySelector(", "form.contains(",
	} {
		if strings.Contains(after, stale) {
			t.Errorf("the submit handler uses the submitted form after the request (%s), which a re-render may have taken out of the page", stale)
		}
	}
}
