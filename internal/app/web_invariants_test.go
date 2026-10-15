package app

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

	// The Pass asks the same question in its own two functions, held to the
	// same rules by TestPassAttestationIsNeverPreselectedOrKept.
	passAttest := withoutComments(jsFunction(t, script, "passAttestHTML"))
	passSubmit := withoutComments(jsFunction(t, script, "submitPass"))
	if all, here := strings.Count(withoutComments(script), "adult"),
		strings.Count(view, "adult")+strings.Count(submit, "adult")+
			strings.Count(passAttest, "adult")+strings.Count(passSubmit, "adult"); all != here {
		t.Errorf("app.js mentions the attestation %d times outside verifyViewHTML, submitVerification, "+
			"passAttestHTML and submitPass", all-here)
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

// The Pass is held to the credential's rules: its code leaves the browser on
// a post only from the submit handler, only on an unbadged post, and never
// through api(); otherwise only in the body of the Pass page's own requests.
func TestOnlyThePostingRequestCarriesThePass(t *testing.T) {
	script := appScript(t)
	submit := submitHandler(t, script)

	if uses, inSubmit := strings.Count(script, "PASS_HEADER"), strings.Count(submit, "PASS_HEADER"); uses != 2 || inSubmit != 1 {
		t.Errorf("PASS_HEADER appears %d times in app.js, %d of them in the submit handler; "+
			"want the declaration and one use, in the submit handler", uses, inSubmit)
	}
	// Never beside the credential: the Pass is read only when no badge is held.
	if !strings.Contains(submit, "const pass = held ? null : storedPass();") ||
		!strings.Contains(submit, "headers[PASS_HEADER] = pass.code;") {
		t.Error("the submit handler no longer keeps the Pass off badged posts")
	}
	api := withoutComments(jsFunction(t, script, "api"))
	for _, banned := range []string{"storedPass", "PASS_STORAGE_KEY", "PASS_HEADER"} {
		if strings.Contains(api, banned) {
			t.Errorf("api() reads %s; every request would carry the Pass", banned)
		}
	}
	// Where the code may go besides a post: the body of a renewal
	// (submitPass) and of a check of the Pass held (checkHeldPass), back into
	// storage with the expiry a claim brought (keepClaimed), and onto the
	// screen when the visitor asks to see it (showPassCode). Each is a
	// deliberate use; a new one is a new place the code goes.
	code := withoutComments(script)
	allowed := 0
	for _, name := range []string{"submitPass", "checkHeldPass", "keepClaimed", "showPassCode"} {
		allowed += strings.Count(withoutComments(jsFunction(t, script, name)), "held.code")
	}
	if n := strings.Count(code, "held.code"); n != allowed {
		t.Errorf("app.js reads the stored code (held.code) %d times, %d of them in submitPass, checkHeldPass, "+
			"keepClaimed and showPassCode; want none elsewhere", n, allowed)
	}
	if check := withoutComments(jsFunction(t, script, "checkHeldPass")); !strings.Contains(check,
		`api("/api/pass/activate", { method: "POST", body: JSON.stringify({ pass: held.code }) })`) {
		t.Error("checkHeldPass no longer sends the code only in the body of /api/pass/activate")
	}
	if show := withoutComments(jsFunction(t, script, "showPassCode")); strings.Count(show, "held.code") != 1 ||
		!strings.Contains(show, "${esc(held.code)}") || strings.Contains(show, "api(") {
		t.Error("showPassCode does more with the code than put it, escaped, on the screen")
	}
}

// Buying a Pass takes the badge's attestation, by the badge's rules: an
// unticked, unrequired box on every render, read once and stored nowhere, and
// the server's refusal recognised in its own words.
func TestPassAttestationIsNeverPreselectedOrKept(t *testing.T) {
	script := appScript(t)
	attest := withoutComments(jsFunction(t, script, "passAttestHTML"))
	submit := withoutComments(jsFunction(t, script, "submitPass"))

	box := regexp.MustCompile(`<input[^>]*\bname="adult"[^>]*>`).FindString(attest)
	if box == "" || !strings.Contains(box, `type="checkbox"`) {
		t.Fatalf("passAttestHTML has no adult checkbox: %q", box)
	}
	for _, banned := range []string{"checked", "required"} {
		if regexp.MustCompile(`\s` + banned + `\b`).MatchString(box) {
			t.Errorf("the Pass attestation box is %s: %s", banned, box)
		}
	}
	for name, body := range map[string]string{"passAttestHTML": attest, "submitPass": submit} {
		for _, banned := range []string{"localStorage", "sessionStorage", "document.cookie", "indexedDB"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s uses %s; the attestation is never stored", name, banned)
			}
		}
	}
	if !strings.Contains(submit, "showNotice(PASS_AGE_REFUSAL);") {
		t.Error("submitPass no longer refuses on its own when the box is unticked")
	}
	if !strings.Contains(script, `const PASS_AGE_REFUSED = "`+passAdultRequired+`";`) {
		t.Errorf("PASS_AGE_REFUSED in app.js is not the server's refusal %q", passAdultRequired)
	}
}

// passSection is the Pass page's code: everything from its banner to the
// next one.
func passSection(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, "/* educhan Pass ---")
	end := strings.Index(script, "/* Rules and FAQ ---")
	if start < 0 || end < start {
		t.Fatal("app.js has no educhan Pass section before the Rules and FAQ one; this test reads it")
	}
	return script[start:end]
}

// faqAnswer is one FAQ entry, from its id to the next entry's.
func faqAnswer(t *testing.T, script, id string) string {
	t.Helper()
	start := strings.Index(script, `id: "`+id+`",`)
	if start < 0 {
		t.Fatalf("app.js has no FAQ entry %q; this test reads it", id)
	}
	end := strings.Index(script[start+1:], `id: "`)
	if end < 0 {
		t.Fatalf("no FAQ entry follows %q; this test reads up to the next one", id)
	}
	return script[start : start+1+end]
}

// words is text with every run of white space made one space, so a sentence
// is found wherever a template literal wrapped it.
func words(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// The server hands a new code over once and keeps nothing to show it again,
// so what a claim brings is stored before anything asks whether the reader
// is still on the Pass page. Only the redraw waits on that.
func TestPassPageStoresAClaimedPassBeforeAnythingElse(t *testing.T) {
	script := appScript(t)
	keep := withoutComments(jsFunction(t, script, "keepClaimed"))
	if !strings.Contains(keep, "storePass(res.pass, res.expires_at)") || !strings.Contains(keep, "storePass(held.code, res.expires_at)") {
		t.Error("keepClaimed no longer stores an issued Pass and a renewed expiry")
	}
	for _, check := range []string{"state.route", "passVisit"} {
		if strings.Contains(keep, check) {
			t.Errorf("keepClaimed reads %s; a reader who moved on would lose the code", check)
		}
	}
	collect := withoutComments(jsFunction(t, script, "collectPass"))
	got, kept := strings.Index(collect, "if (res) {"), strings.Index(collect, "keepClaimed(res)")
	if got < 0 || kept < got {
		t.Fatal("collectPass no longer keeps what a claim brought as soon as it has it")
	}
	for _, check := range []string{"return", "state.route", "passVisit"} {
		if strings.Contains(collect[got:kept], check) {
			t.Errorf("collectPass checks %q before storing what a claim brought", check)
		}
	}
	redeem := withoutComments(jsFunction(t, script, "redeemPassReissue"))
	if !strings.Contains(redeem, "showClaimed({ ...info, claim: res.claim === true }, res, keepClaimed(res), visit);") ||
		strings.Contains(redeem, "state.route") {
		t.Error("redeemPassReissue no longer stores a reissued code before looking at the page")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "showClaimed")), `state.route.view !== "pass"`) {
		t.Error("showClaimed draws the Pass page over whatever the reader moved on to")
	}
}

// A browser with a purchase to collect (GET /api/pass says "claim") asks for
// it on every drawing of the Pass page, not only on the way back from Stripe,
// and goes on asking while the page is open. Meanwhile the page offers no way
// to pay again for the Pass on its way.
func TestPassPageCollectsAPendingPurchase(t *testing.T) {
	script := appScript(t)
	render := withoutComments(jsFunction(t, script, "renderPass"))
	call := regexp.MustCompile(`if \(([^{]*)\) \{\s*collectPass\(info, visit\);`).FindStringSubmatch(render)
	if call == nil || !strings.Contains(call[1], "info.claim") || strings.Contains(call[1], "back") || strings.Contains(call[1], "paid") {
		t.Errorf("renderPass no longer collects whenever the server says there is a purchase to collect: %q", call)
	}
	collect := withoutComments(jsFunction(t, script, "collectPass"))
	for _, want := range []string{"for (;;)", "setTimeout(resolve, wait)", "PASS_CLAIM_LONGEST_WAIT_MS"} {
		if !strings.Contains(collect, want) {
			t.Errorf("collectPass no longer asks again, with a growing pause, while the page is open (%s)", want)
		}
	}

	view := withoutComments(jsFunction(t, script, "passViewHTML"))
	if !strings.Contains(view, "const collecting = info.enabled && info.claim === true && !passStartedAgain;") {
		t.Error("passViewHTML no longer draws the collecting state while the server has a purchase to collect")
	}
	stop := regexp.MustCompile(`passCollectingHTML\(\)\);\s*return parts\.join\(""\);`).FindStringIndex(view)
	buy, activate := strings.Index(view, `data-pass="buy"`), strings.Index(view, `data-pass="activate"`)
	if stop == nil || buy < stop[0] || activate < stop[0] {
		t.Error("passViewHTML offers the buy or activate form while a purchase is being collected")
	}
	if held := withoutComments(jsFunction(t, script, "passHeldHTML")); !strings.Contains(held, "info.enabled && !collecting") {
		t.Error("passHeldHTML offers a renewal while a purchase is being collected")
	}
	if panel := withoutComments(jsFunction(t, script, "passCollectingHTML")); strings.Contains(panel, "<form") || strings.Contains(panel, "data-pass") {
		t.Error("the collecting panel has a form to pay with")
	}
}

// What the Pass page and the FAQ say a Pass is: a way to post without solving
// the captcha, up to an hourly budget everyone using it shares, with the rules
// and the wait between posts as usual. Nothing educhan has not built (a report
// button, a filter), no promise the operator cannot keep (that a buyer is
// never seen), and no other site's product named to readers who may be 13.
func TestPassCopyClaimsOnlyWhatIsBuilt(t *testing.T) {
	script := appScript(t)
	intro := words(withoutComments(jsFunction(t, script, "passIntroHTML")))
	faq := words(faqAnswer(t, script, "faq-pass"))

	banned := regexp.MustCompile(`(?i)4chan|\breport|filter|never sees|refusal limit|content rules|gains? (?:nothing|nobody)|sent only with your posts|lasts a year`)
	for name, text := range map[string]string{
		"the Pass page":        withoutComments(passSection(t, script)),
		"the FAQ's Pass entry": faq,
		"Forget this browser":  jsFunction(t, script, "forgetBrowserHTML"),
	} {
		for _, found := range banned.FindAllString(text, -1) {
			t.Errorf("%s says %q", name, found)
		}
	}
	for _, want := range []string{
		"without solving the captcha",
		"shares that",
		"the rules and the wait between posts apply as usual",
		"stores none of them",
		"Whoever runs educhan can see in Stripe who bought a Pass, but never which posts it was used on",
		"when you renew it or check it",
	} {
		for name, text := range map[string]string{"the Pass page": intro, "the FAQ's Pass entry": faq} {
			if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
				t.Errorf("%s no longer says %q", name, want)
			}
		}
	}
	if !strings.Contains(words(faqAnswer(t, script, "faq-anonymity")), "Stripe's checkout asks for your email") {
		t.Error("the FAQ's anonymity answer no longer says that buying a Pass means giving Stripe an email")
	}
}

// Removing the Pass, or forgetting the browser, loses it for good without its
// code, and both say to write the code down first, so the held Pass's panel
// shows it on request.
func TestPassCodeCanBeShownBeforeItIsRemoved(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	held := withoutComments(jsFunction(t, script, "passHeldHTML"))
	if !strings.Contains(held, `data-action="show-pass"`) || !strings.Contains(held, `id="pass-shown"`) {
		t.Error("the held Pass's panel no longer offers to show its code")
	}
	if !strings.Contains(code, `const showPass = event.target.closest('[data-action="show-pass"]');`) ||
		!strings.Contains(code, "showPassCode(showPass);") {
		t.Error("nothing shows the code when Show the code is pressed")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "showPassCode")), `document.getElementById("pass-shown")`) {
		t.Error("showPassCode no longer fills the panel's slot")
	}
	if !strings.Contains(words(jsFunction(t, script, "forgetBrowserHTML")), "show its code on the") {
		t.Error("Forget this browser no longer says where the code can be seen before it is forgotten")
	}
}

// A Pass this browser holds as run out may have been renewed on another
// device, and one a post was refused for as expired has no date here worth
// keeping. The page offers to check it again and takes a newer code, and the
// refusal marks it run out without inventing a date.
func TestPassRunOutHereIsCheckedWithTheServer(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if !strings.Contains(withoutComments(jsFunction(t, script, "passViewHTML")), "if (!held || held.expired) {") {
		t.Error("passViewHTML no longer offers to enter a newer code for a Pass that has run out here")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "passHeldHTML")), `data-action="check-pass"`) ||
		!strings.Contains(code, "checkHeldPass(checkPass);") {
		t.Error("a Pass that has run out here can no longer be checked again")
	}
	submit := withoutComments(submitHandler(t, script))
	if !strings.Contains(submit, "markPassExpired(pass.code);") || strings.Contains(submit, "storePass(pass.code") {
		t.Error("a post refused for an expired Pass no longer marks it run out without a date")
	}
	if stored := withoutComments(jsFunction(t, script, "storedPass")); !strings.Contains(stored, "if (saved.expired === true) {") ||
		!strings.Contains(stored, "return { code: saved.code, expires: null, expired: true };") {
		t.Error("storedPass no longer reads a Pass marked run out")
	}
}

// Once the captcha is built, a post sent without a Pass needs it solved, so a
// post refused for its Pass says so rather than "post again".
func TestPostRefusedForItsPassSaysToSolveTheCaptcha(t *testing.T) {
	submit := words(withoutComments(submitHandler(t, appScript(t))))
	if n := strings.Count(submit, "Solve the captcha and post again"); n != 2 {
		t.Errorf("the refusals for an invalid and an expired Pass say to solve the captcha %d times; want 2", n)
	}
	if strings.Contains(submit, "Post again to send it without the Pass") {
		t.Error("a post refused for its Pass still says the next press posts without it")
	}
}

// A browser that blocks storage, or has none left, keeps no Pass, and the
// page must not say it does: storing is checked by reading back.
func TestPassPageSaysWhenThisBrowserCannotKeepIt(t *testing.T) {
	script := appScript(t)
	if store := withoutComments(jsFunction(t, script, "storePass")); !strings.Contains(store, "const kept = storedPass();") ||
		!strings.Contains(store, "return Boolean(kept && kept.code === code);") {
		t.Error("storePass no longer reads back what it stored")
	}
	for _, name := range []string{"submitPass", "checkHeldPass"} {
		body := withoutComments(jsFunction(t, script, name))
		if !strings.Contains(body, "const kept = storePass(res.pass, res.expires_at);") || !strings.Contains(body, ": PASS_NOT_KEPT") {
			t.Errorf("%s says the Pass is here without checking it was kept", name)
		}
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "keepClaimed")), "kept: storePass(res.pass, res.expires_at)") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "passIssuedHTML")), "const here = kept") {
		t.Error("a code just handed over is said to be kept without checking")
	}
}

// The rest of what the front-end review found on the Pass page, one check
// each.
func TestPassPageDetails(t *testing.T) {
	script := appScript(t)
	t.Run("dates in the reader's time zone, like the badge's", func(t *testing.T) {
		held := withoutComments(jsFunction(t, script, "passHeldHTML"))
		if !strings.Contains(held, "timeHTML(held.expires.toISOString())") || strings.Contains(passSection(t, script), "toLocaleDateString") {
			t.Error("the held Pass's date is not written by timeHTML")
		}
	})
	t.Run("a server that did not answer is not one that sells no Pass", func(t *testing.T) {
		load := withoutComments(jsFunction(t, script, "loadPassInfo"))
		view := withoutComments(jsFunction(t, script, "passViewHTML"))
		failed, off := strings.Index(view, "if (info.failed) {"), strings.Index(view, "The Pass is not on sale on this server.")
		if !strings.Contains(load, "return { enabled: false, failed: err };") || failed < 0 || failed > off {
			t.Error("a Pass page that could not load says the Pass is not on sale")
		}
		if text := withoutComments(jsFunction(t, script, "passErrorText")); !strings.Contains(text, "if (err.unreachable) {\n    return UNREACHABLE;") {
			t.Error("passErrorText no longer says UNREACHABLE for a server that did not answer")
		}
	})
	t.Run("a panel drawn already filled is focused", func(t *testing.T) {
		if !strings.Contains(withoutComments(jsFunction(t, script, "showClaimed")), `focusPassPanel("pass-issued");`) ||
			!strings.Contains(jsFunction(t, script, "passIssuedHTML"), `id="pass-issued" tabindex="-1"`) {
			t.Error("a code just handed over is drawn without moving focus to it")
		}
	})
	t.Run("drawn again when brought back from the back-forward cache", func(t *testing.T) {
		show := withoutComments(jsBlock(t, script, `window.addEventListener("pageshow", (event) => {`, "});"))
		if !strings.Contains(show, `event.persisted && state.route.view === "pass"`) || !strings.Contains(show, "renderPass();") {
			t.Error("the Pass page is not drawn again when Back brings it out of the cache")
		}
	})
	t.Run("the price says when tax is added", func(t *testing.T) {
		if !strings.Contains(withoutComments(jsFunction(t, script, "passIntroHTML")), `info.automatic_tax ? " plus any tax" : ""`) {
			t.Error("the price leaves out that Stripe adds tax")
		}
	})
}

// A reissue link's token rides in the fragment, which reaches no server and no
// Referer. render takes it out of the address before anything else, the age
// screen included, so no history entry or bookmark keeps it, and it leaves the
// page once, in the body of a claim.
func TestPassReissueTokenLeavesTheAddressFirst(t *testing.T) {
	script := appScript(t)
	render := withoutComments(jsFunction(t, script, "render"))
	take := strings.Index(render, "takePassReissueToken();")
	if take < 0 || take > strings.Index(render, "const requested") || take > strings.Index(render, "ageStatus()") {
		t.Error("render no longer takes a reissue token out of the address before anything else")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "takePassReissueToken")), `window.history.replaceState(null, "", pathname + search);`) {
		t.Error("takePassReissueToken leaves the token in the address")
	}
	code := withoutComments(script)
	if n := strings.Count(code, "reissue: token"); n != 1 ||
		!strings.Contains(withoutComments(jsFunction(t, script, "redeemPassReissue")), `body: JSON.stringify({ reissue: token })`) {
		t.Error("the reissue token goes somewhere other than the body of one claim")
	}
	if !strings.Contains(withoutComments(jsBlock(t, script, `window.addEventListener("popstate", () => {`, "});")), `startsWith("#reissue=")`) {
		t.Error("a reissue link opened over the Pass page is taken for a jump within it")
	}
}

// The Pass page sends a buyer who lost a code to the support address on
// Stripe's receipt, which exists only once the operator has turned receipts
// and a public support email on; the README's setup says to.
func TestPassPagePromisesOnlyWhatTheSetupTurnsOn(t *testing.T) {
	if !strings.Contains(words(passSection(t, appScript(t))), "support address on the receipt Stripe emailed you") {
		t.Fatal("the Pass page no longer names the receipt's support address; this test pins the setup that promise needs")
	}
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if setup := words(string(readme)); !strings.Contains(setup, "Turn on Stripe's email receipts and give the account a public support email") {
		t.Error("the README's Pass setup no longer turns on the receipts and support email the Pass page promises")
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
