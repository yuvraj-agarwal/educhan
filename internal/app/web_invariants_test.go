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
	// (submitPass) and of a check of the Pass held (checkHeldPass, and
	// confirmRenewal after a renewal was collected), back into storage with
	// the expiry a claim or a check brought (keepClaimed, confirmRenewal), and
	// onto the screen when the visitor asks to see it (showPassCode). The
	// readers are
	// pinned rather than a variable name: the stored Pass is read through
	// storedPass, only in these functions, and its key and header are spelled
	// out once each, so nothing reads it any other way. Each is a deliberate
	// use; a new one is a new place the code goes.
	code := withoutComments(script)
	readers := []string{
		"storePass",      // reads back what it stored
		"showPassLink",   // whether to show the header's link
		"passViewHTML",   // whether to draw the held Pass
		"showPassCode",   // the screen, on request
		"keepClaimed",    // storage, with a claim's expiry
		"checkHeldPass",  // the body of /api/pass/activate
		"confirmRenewal", // the body of /api/pass/activate, after a renewal
		"submitPass",     // the body of a renewal
	}
	allowed := strings.Count(submit, "storedPass(")
	for _, name := range readers {
		allowed += strings.Count(withoutComments(jsFunction(t, script, name)), "storedPass(")
	}
	if n := strings.Count(code, "storedPass(") - strings.Count(code, "function storedPass("); n != allowed {
		t.Errorf("app.js reads the stored Pass (storedPass()) %d times, %d of them in the submit handler and %s; "+
			"want none elsewhere", n, allowed, strings.Join(readers, ", "))
	}
	for literal, name := range map[string]string{`"educhan:pass"`: "PASS_STORAGE_KEY", `"X-Educhan-Pass"`: "PASS_HEADER"} {
		if n := strings.Count(code, literal); n != 1 || !strings.Contains(code, "const "+name+" = "+literal+";") {
			t.Errorf("app.js spells out %s %d times; want once, as %s", literal, n, name)
		}
	}
	keyUsers := 0
	for _, name := range []string{"storedPass", "storePass", "markPassExpired", "forgetPass"} {
		keyUsers += strings.Count(withoutComments(jsFunction(t, script, name)), "PASS_STORAGE_KEY")
	}
	if n := strings.Count(code, "PASS_STORAGE_KEY"); n != keyUsers+1 {
		t.Errorf("PASS_STORAGE_KEY appears %d times in app.js; want its declaration and %d uses, in storedPass, "+
			"storePass, markPassExpired and forgetPass", n, keyUsers)
	}
	for _, name := range []string{"checkHeldPass", "confirmRenewal"} {
		check := withoutComments(jsFunction(t, script, name))
		if !strings.Contains(check, `api("/api/pass/activate", { method: "POST", body: JSON.stringify({ pass: held.code }) })`) ||
			strings.Count(check, "api(") != 1 || strings.Contains(check, "${held.code") || strings.Contains(check, "${esc(held.code") {
			t.Errorf("%s no longer sends the code only in the body of /api/pass/activate, or puts it in its words", name)
		}
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
		for _, banned := range []string{"localStorage", "sessionStorage", "storageSet", "document.cookie", "indexedDB"} {
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

// passForgottenCheck is how collectPass and redeemPassReissue drop an answer
// that arrived after this browser stopped collecting (passForgets).
var passForgottenCheck = regexp.MustCompile(`if \(forgets !== passForgets\) \{\s*return;\s*\}`)

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
	if !strings.Contains(keep, "storePass(res.pass, res.expires_at)") {
		t.Error("keepClaimed no longer stores an issued Pass")
	}
	// A renewal is of the Pass its checkout was for, which may not be the one
	// held now: its expiry is never written onto the held Pass on trust. The
	// held code is checked again, and what the server says of it is stored.
	if !strings.Contains(keep, "if (res.status === \"renewed\") {\n    return \"renewed\";\n  }\n  if (res.status !== \"issued\") {") ||
		strings.Contains(keep, "storePass(held.code") {
		t.Error("keepClaimed writes a renewal's expiry onto the Pass this browser holds")
	}
	confirm := withoutComments(jsFunction(t, script, "confirmRenewal"))
	checked := strings.Index(confirm, `await api("/api/pass/activate"`)
	if checked < 0 || !strings.Contains(confirm[checked:], "storePass(res.pass, res.expires_at);") ||
		regexp.MustCompile(`storePass\([^)]*renewed`).MatchString(confirm) {
		t.Error("confirmRenewal no longer stores the expiry the server gives for the held code, and only that")
	}
	for name, call := range map[string]string{
		"collectPass":       "await confirmRenewal(res)",
		"redeemPassReissue": "await confirmRenewal(res, true)",
	} {
		body := withoutComments(jsFunction(t, script, name))
		kept, confirmed := strings.Index(body, "keepClaimed(res"), strings.Index(body, `const notice = news === "renewed" ? `+call+` : "";`)
		if confirmed < kept {
			t.Errorf("%s no longer checks the held Pass after a renewal, once what the claim brought is kept", name)
		}
	}
	// A Pass removed while it was being checked is not stored back, or
	// removed again in its name: the check of what is held comes between the
	// answer and anything that writes.
	afterCheck := confirm[checked:]
	still := strings.Index(afterCheck, "if (!still || still.code !== held.code) {\n    return another;\n  }")
	if still < 0 || !strings.Contains(afterCheck, "const still = storedPass();") ||
		strings.Index(afterCheck, "storePass(") < still || strings.Index(afterCheck, "forgetPass()") < still {
		t.Error("confirmRenewal writes what it checked into a browser whose Pass has changed meanwhile")
	}
	for _, check := range []string{"state.route", "passVisit"} {
		if strings.Contains(keep, check) {
			t.Errorf("keepClaimed reads %s; a reader who moved on would lose the code", check)
		}
	}
	// From the answer to the store, nothing may ask where the reader is or
	// leave: a failed request is only noted (failed = err), and dealt with
	// after.
	for _, name := range []string{"collectPass", "redeemPassReissue"} {
		body := withoutComments(jsFunction(t, script, name))
		asked := strings.Index(body, `await api("/api/pass/claim"`)
		kept := strings.Index(body, "keepClaimed(res")
		if asked < 0 || kept < asked {
			t.Fatalf("%s no longer keeps what a claim brought as soon as it has it", name)
		}
		between := body[asked:kept]
		if !strings.Contains(between, "} catch (err) {\n      failed = err;\n    }") &&
			!strings.Contains(between, "} catch (err) {\n    failed = err;\n  }") {
			t.Errorf("%s does more on a failed claim than note it before storing what an answer brought", name)
		}
		// The one check allowed before the store is whether this browser
		// stopped collecting while the claim was on its way, which is the
		// reader saying to keep nothing, not a question of where they are
		// (TestPassClaimsCarryANonceAndCanBeDropped pins it).
		between = passForgottenCheck.ReplaceAllString(between, "")
		for _, check := range []string{"return", "state.route", "passVisit", "visit", "await api(", "showClaimed"} {
			if strings.Contains(between[len(`await api(`):], check) {
				t.Errorf("%s has %q between the answer and storing what it brought", name, check)
			}
		}
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
	call := regexp.MustCompile(`if \(([^{]*)\) \{\s*collectPass\(visit\);`).FindStringSubmatch(render)
	if call == nil || !strings.Contains(call[1], "info.claim || passNoncePending()") || strings.Contains(call[1], "back") || strings.Contains(call[1], "paid") {
		t.Errorf("renderPass no longer collects whenever the server says there is a purchase to collect, "+
			"or a claim's answer may have been lost: %q", call)
	}
	collect := withoutComments(jsFunction(t, script, "collectPass"))
	for _, want := range []string{"for (;;)", "setTimeout(resolve, wait)",
		"wait = Math.min(Math.round(wait * 1.5), PASS_CLAIM_LONGEST_WAIT_MS);"} {
		if !strings.Contains(collect, want) {
			t.Errorf("collectPass no longer asks again, with a growing pause, while the page is open (%s)", want)
		}
	}
	first, err := strconv.Atoi(regexp.MustCompile(`(?m)^const PASS_CLAIM_FIRST_WAIT_MS = (\d+);$`).FindStringSubmatch(script)[1])
	if err != nil {
		t.Fatal(err)
	}
	longest, err := strconv.Atoi(regexp.MustCompile(`(?m)^const PASS_CLAIM_LONGEST_WAIT_MS = (\d+);$`).FindStringSubmatch(script)[1])
	if err != nil {
		t.Fatal(err)
	}
	if first < 1000 || longest <= first {
		t.Errorf("the claim's pause starts at %d ms and grows to %d ms; want it to start at a second or more and grow", first, longest)
	}

	// Collecting stands in for the forms only while this browser has no Pass;
	// with one, what is still open is said beside it, and its renewal stays.
	view := withoutComments(jsFunction(t, script, "passViewHTML"))
	if !strings.Contains(view, "const open = info.enabled && info.claim === true && !passStartedAgain;") {
		t.Error("passViewHTML no longer draws the collecting state while the server has a purchase to collect")
	}
	stop := regexp.MustCompile(`if \(open && !has\) \{\s*parts\.push\(passCollectingHTML\(info\)\);\s*return parts\.join\(""\);`).FindStringIndex(view)
	buy, activate := strings.Index(view, `data-pass="buy"`), strings.Index(view, `data-pass="activate"`)
	if stop == nil || buy < stop[0] || activate < stop[0] {
		t.Error("passViewHTML offers the buy or activate form while a purchase is being collected")
	}
	if !strings.Contains(view, "if (open) {\n    parts.push(passOpenCheckoutHTML());") {
		t.Error("passViewHTML no longer says a checkout is still open beside the Pass this browser has")
	}
	if held := withoutComments(jsFunction(t, script, "passHeldHTML")); strings.Contains(held, "!collecting") ||
		!strings.Contains(held, "} else if (info.enabled) {") {
		t.Error("passHeldHTML no longer offers a renewal beside a checkout still open")
	}
	for _, name := range []string{"passCollectingHTML", "passOpenCheckoutHTML"} {
		panel := withoutComments(jsFunction(t, script, name))
		if strings.Contains(panel, "<form") || strings.Contains(panel, "data-pass") {
			t.Errorf("%s has a form to pay with", name)
		}
		// The note says when an attempt failed, and is cleared by the next answer.
		if !strings.Contains(panel, `<p class="muted" id="pass-collect-note" role="status"></p>`) {
			t.Errorf("%s's note is not announced", name)
		}
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "showClaimed")), `note.textContent = "";`) {
		t.Error("an answer no longer clears the note a failed attempt left")
	}
	if strings.Contains(withoutComments(jsFunction(t, script, "passOpenCheckoutHTML")), "Collecting your Pass") {
		t.Error("a browser that has a Pass is told its Pass is being collected")
	}
	// A checkout gives the browser a new claim, and what was under the old
	// one is no longer collected here: both panels say so before the reader
	// starts another, and nothing promises an earlier payment is added on.
	for name, want := range map[string]string{
		"passCollectingHTML":   "Once you buy again, this browser stops collecting the earlier checkout.",
		"passOpenCheckoutHTML": "Starting another checkout here, a renewal included, stops this browser collecting this one.",
	} {
		if !strings.Contains(words(withoutComments(jsFunction(t, script, name))), want) {
			t.Errorf("%s no longer says that another checkout stops this one being collected here", name)
		}
	}
	if section := words(withoutComments(passSection(t, script))); strings.Contains(section, "added to your Pass") {
		t.Error("the Pass page promises an earlier payment is added to the Pass a later checkout buys")
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

// A Pass that is no good on a post is treated as no Pass, and the created
// post's answer says why ("pass"), so the page stops sending it: an unknown
// one is forgotten and one that ran out is kept to renew. No post is refused
// for its Pass any more, so nothing reads a refusal for one.
func TestPostMadeWithANoGoodPassSaysSo(t *testing.T) {
	submit := withoutComments(submitHandler(t, appScript(t)))
	for _, want := range []string{
		`if (pass && created.pass === "invalid") {` + "\n      forgetPass();",
		`} else if (pass && created.pass === "expired") {` + "\n      markPassExpired(pass.code);",
	} {
		if !strings.Contains(submit, want) {
			t.Errorf("the submit handler no longer deals with a created post's answer about its Pass: %q", want)
		}
	}
	if strings.Contains(submit, "err.payload.pass") || strings.Contains(submit, "passReason") {
		t.Error("the submit handler still reads a refusal for a post's Pass, which the server no longer sends")
	}
	if !strings.Contains(words(submit), "Your post was sent, but the Pass in this browser") {
		t.Error("a post made with a Pass that is no good no longer says so")
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
	if !strings.Contains(withoutComments(jsFunction(t, script, "keepClaimed")), "const kept = storePass(res.pass, res.expires_at);") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "passIssuedHTML")), "const here = issued.kept") {
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
	t.Run("drawn again when brought back from the back-forward cache, behind the age screen", func(t *testing.T) {
		show := withoutComments(jsBlock(t, script, `window.addEventListener("pageshow", (event) => {`, "});"))
		if !strings.Contains(show, `event.persisted && state.route.view === "pass"`) || !strings.Contains(show, "render();") {
			t.Error("the Pass page is not drawn again when Back brings it out of the cache")
		}
		// render asks the age screen first; renderPass would draw the page, and
		// fetch and spend what it does, over a screen not yet answered or a
		// refusal.
		if strings.Contains(show, "renderPass(") {
			t.Error("Back draws the Pass page without asking the age screen first")
		}
		render := withoutComments(jsFunction(t, script, "render"))
		gate, pass := strings.Index(render, "const age = ageStatus();"), strings.Index(render, "renderPass(")
		if gate < 0 || pass < gate {
			t.Error("render draws the Pass page before it asks the age screen")
		}
	})
	t.Run("Renew with the Pass gone from this browser starts no purchase", func(t *testing.T) {
		submit := withoutComments(jsFunction(t, script, "submitPass"))
		gone, checkout := strings.Index(submit, `if (kind === "renew" && !held) {`), strings.Index(submit, `"/api/pass/checkout"`)
		if gone < 0 || checkout < gone || !strings.Contains(submit, "if (kind === \"renew\") {\n      body.renew = held.code;") {
			t.Error("Renew on Stripe buys a new Pass when the one it was for has left this browser")
		}
	})
	t.Run("a post with a run-out Pass promises no renewal", func(t *testing.T) {
		// Sales may be paused; the Pass page says what can be done.
		if submit := words(withoutComments(submitHandler(t, script))); strings.Contains(submit, "You can renew it on the Pass page") ||
			!strings.Contains(submit, "The Pass page says whether it can be renewed.") {
			t.Error("a post made with a Pass that ran out promises a renewal the Pass page may not offer")
		}
	})
	t.Run("the FAQ says every request that holds an address", func(t *testing.T) {
		faq := words(faqAnswer(t, script, "faq-anonymity"))
		if !strings.Contains(faq, "When you post, ask for a badge code, start buying a Pass or check a Pass code, the server holds your IP address in its memory") {
			t.Error("the FAQ no longer lists every request whose address flood control holds")
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
		!strings.Contains(withoutComments(jsFunction(t, script, "redeemPassReissue")),
			`body: JSON.stringify({ reissue: token, nonce: passNonce(PASS_REISSUE_NONCE_STORAGE_KEY) })`) {
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
	m := regexp.MustCompile(`(?m)^const ` + name + ` =\s+"([^"\\]*)";$`).FindStringSubmatch(script)
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

// A code just handed over is drawn on every drawing of the Pass page until the
// reader leaves it, so no redraw, poll, Back or button loses a code the server
// will not show again, even one the browser could not store; and the page
// never says a Pass was renewed when this browser has none.
func TestPassPageKeepsACodeJustHandedOver(t *testing.T) {
	script := appScript(t)
	keep := withoutComments(jsFunction(t, script, "keepClaimed"))
	if !strings.Contains(keep, "passJustIssued = {") {
		t.Error("keepClaimed no longer keeps a code just handed over for the page to draw")
	}
	// The page says a Pass here was renewed only once the server, asked about
	// the code held here, gives it the renewal's expiry.
	confirm := words(withoutComments(jsFunction(t, script, "confirmRenewal")))
	if !strings.Contains(confirm, "if (!held) { return `${by} renewed a Pass this browser doesn't have") {
		t.Error("a renewal with no Pass in this browser is said to renew the Pass here")
	}
	// A renewal a reissue link brought is not said to be a payment from this
	// browser: the link may have been paid for anywhere.
	if !strings.Contains(confirm, `async function confirmRenewal(renewed, reissued = false) {`) ||
		!strings.Contains(confirm, `const by = reissued ? "This link" : "A payment from this browser";`) ||
		!strings.Contains(confirm, "const another = `${by} renewed a Pass until ${until}.`;") ||
		strings.Count(confirm, "A payment from this browser") != 1 {
		t.Error("confirmRenewal no longer says a renewal came from a reissue link when it did")
	}
	renewed := "`Your Pass is renewed until"
	asked := strings.Index(confirm, `await api("/api/pass/activate"`)
	compared := strings.Index(confirm, "if (checked.getTime() === new Date(renewed.expires_at).getTime() && !(held.expires && held.expires >= checked)) {")
	if strings.Count(words(withoutComments(passSection(t, script))), renewed) != 1 ||
		asked < 0 || compared < asked || strings.Index(confirm, renewed) < compared {
		t.Error("the page says the Pass here was renewed without checking the code held here")
	}
	view := withoutComments(jsFunction(t, script, "passViewHTML"))
	if !strings.Contains(view, "const issued = passJustIssued;") || !strings.Contains(view, "parts.push(passIssuedHTML(issued));") {
		t.Error("passViewHTML no longer draws the code just handed over")
	}
	if strings.Contains(view, "function passViewHTML(info, ") {
		t.Error("passViewHTML takes the code just handed over from its caller again, so a redraw loses it")
	}
	if n := strings.Count(withoutComments(script), "passJustIssued = null;"); n != 4 {
		t.Errorf("a code just handed over is dropped in %d places; want its declaration, leaving the Pass page, "+
			"removing the Pass and forgetting the browser", n)
	}
	render := withoutComments(jsFunction(t, script, "render"))
	if !strings.Contains(render, `if (state.route.view === "pass" && route.view !== "pass") {`) ||
		!strings.Contains(render, "if (passJustIssued && passJustIssued.kept) {\n      passJustIssued = null;") {
		t.Error("render no longer drops a code just handed over only once the reader leaves the Pass page, and keeps one this browser could not store")
	}
	issued := withoutComments(jsFunction(t, script, "passIssuedHTML"))
	if !strings.Contains(words(issued), "educhan keeps no copy of it: this browser can show it again only until its data is cleared.") {
		t.Error("the panel of a code just handed over no longer says the browser can show it again")
	}
}

// A browser that has a Pass is asked before a reissue link replaces it, and a
// code handed over in place of a different one shows that one beside it.
func TestPassReissueLinkAsksBeforeReplacingAPass(t *testing.T) {
	script := appScript(t)
	view := withoutComments(jsFunction(t, script, "passViewHTML"))
	if !strings.Contains(view, "if (passReissueToken && held && info.enabled) {\n    parts.push(passReissueAskHTML());") {
		t.Error("passViewHTML no longer asks before a reissue link replaces the Pass this browser has")
	}
	render := withoutComments(jsFunction(t, script, "renderPass"))
	if !strings.Contains(render, `if (passReissueToken && info.enabled && !document.getElementById("pass-reissue-ask")) {`) {
		t.Error("renderPass spends a reissue link the page is still asking about")
	}
	code := withoutComments(script)
	if !strings.Contains(code, `const reissue = event.target.closest('[data-action="reissue-pass"]');`) ||
		!strings.Contains(code, `event.target.closest('[data-action="skip-reissue"]')`) {
		t.Error("the question about a reissue link has no answer")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "keepClaimed")), `replaced: kept && held ? held.code : "",`) ||
		!strings.Contains(withoutComments(jsFunction(t, script, "passIssuedHTML")), "${esc(issued.replaced)}") {
		t.Error("a code handed over in place of another no longer shows the one it replaced")
	}
}

// Every claim carries the collection's nonce, which the page keeps until an
// answer says nothing more is waiting, so a claim whose answer was lost is
// answered again with the same code. A shared computer can drop the claim:
// forgetting the browser, removing the Pass and the collecting panels all ask
// the server to, since the claim is a cookie the page cannot see, after the
// page has dropped the nonces and stopped collecting. The nonces, like the
// code they are spent for, go nowhere but the bodies of the claims.
func TestPassClaimsCarryANonceAndCanBeDropped(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	collect := withoutComments(jsFunction(t, script, "collectPass"))
	if !strings.Contains(collect, "body: JSON.stringify({ nonce: passNonce(PASS_NONCE_STORAGE_KEY) }),") {
		t.Error("collectPass no longer sends the collection's nonce")
	}
	if n := strings.Count(collect, "dropPassNonce(PASS_NONCE_STORAGE_KEY);"); n != 2 ||
		!strings.Contains(collect, "if (res.claim !== true) {\n        dropPassNonce(PASS_NONCE_STORAGE_KEY);") {
		t.Error("collectPass drops the nonce other than once nothing more is waiting, or the claim is refused")
	}
	// A collection stopped (forgetPassClaim) or replaced sends no claim and
	// draws no nonce: the check comes first in every turn of the loop.
	stopped := strings.Index(collect, "for (;;) {\n    if (visit !== passVisit || state.route.view !== \"pass\") {\n      return;\n    }")
	if stopped < 0 || strings.Index(collect, "passNonce(") < stopped {
		t.Error("collectPass can send a claim, and draw a nonce, after collecting has stopped")
	}
	reissue := withoutComments(jsFunction(t, script, "redeemPassReissue"))
	if !strings.Contains(reissue, "body: JSON.stringify({ reissue: token, nonce: passNonce(PASS_REISSUE_NONCE_STORAGE_KEY) }),") ||
		strings.Count(reissue, "dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);") != 2 {
		t.Error("redeemPassReissue no longer sends the link's own nonce, or no longer drops it once answered")
	}
	if n := len(regexp.MustCompile(`\bpassNonce\(`).FindAllString(code, -1)); n != 3 {
		t.Errorf("passNonce( appears %d times; want its definition and the bodies of a claim and a reissue", n)
	}
	// The keys are spelled out once, and used only by the nonce helpers, the
	// two requests that send a nonce, and forgetting; passNonces only by the
	// helpers.
	nonceUsers := map[string][]string{
		"PASS_NONCE_STORAGE_KEY":         {"passNoncePending", "collectPass", "forgetPassClaim"},
		"PASS_REISSUE_NONCE_STORAGE_KEY": {"redeemPassReissue", "forgetPassClaim"},
		"passNonces":                     {"passNonce", "passNoncePending", "dropPassNonce"},
	}
	for name, users := range nonceUsers {
		want := 1
		for _, user := range users {
			want += strings.Count(withoutComments(jsFunction(t, script, user)), name)
		}
		if n := len(regexp.MustCompile(`\b`+name+`\b`).FindAllString(code, -1)); n != want {
			t.Errorf("%s appears %d times in app.js; want its declaration and %d uses, in %s", name, n, want-1, strings.Join(users, ", "))
		}
	}
	// The code a claim handed over is read only to tell it from another, and
	// drawn only on its own panel.
	if n, want := strings.Count(code, "passJustIssued.code"), strings.Count(withoutComments(jsFunction(t, script, "keepClaimed")), "passJustIssued.code"); n != want {
		t.Errorf("passJustIssued.code is read %d times, %d of them in keepClaimed; want none elsewhere", n, want)
	}
	issuedCode := regexp.MustCompile(`\bissued\.code\b`)
	want := len(issuedCode.FindAllString(withoutComments(jsFunction(t, script, "passIssuedHTML")), -1)) +
		len(issuedCode.FindAllString(withoutComments(jsFunction(t, script, "passViewHTML")), -1))
	if n := len(issuedCode.FindAllString(code, -1)); n != want {
		t.Errorf("issued.code is read %d times, %d of them in passIssuedHTML and passViewHTML; want none elsewhere", n, want)
	}
	load := withoutComments(jsFunction(t, script, "loadPassInfo"))
	if !strings.Contains(load, `const info = await api("/api/pass");`) || strings.Count(load, "api(") != 1 {
		t.Error("loadPassInfo sends GET /api/pass with something added")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "newPassNonce")), "crypto.getRandomValues(new Uint8Array(16))") {
		t.Error("the nonce is no longer 16 random bytes")
	}
	forget := withoutComments(jsFunction(t, script, "forgetPassClaim"))
	asked := strings.Index(forget, `await api("/api/pass/claim", { method: "POST", body: JSON.stringify({ forget: true }) })`)
	if asked < 0 {
		t.Error("forgetPassClaim no longer asks the server to drop the claim")
	}
	for _, first := range []string{"passVisit += 1;", "passForgets += 1;", "dropPassNonce(PASS_NONCE_STORAGE_KEY);", "dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);"} {
		if at := strings.Index(forget, first); at < 0 || at > asked {
			t.Errorf("forgetPassClaim no longer does %q before it asks the server", first)
		}
	}
	// An answer already on its way when this browser stopped collecting is
	// dropped, not stored or drawn: the count of forgets is read before each
	// claim is sent, and compared once the answer is in, before anything is
	// kept, and again after a renewal was checked, before anything is drawn.
	for _, name := range []string{"collectPass", "redeemPassReissue"} {
		body := withoutComments(jsFunction(t, script, name))
		read := strings.Index(body, "const forgets = passForgets;")
		asked := strings.Index(body, `await api("/api/pass/claim"`)
		caught := strings.Index(body, "failed = err;")
		checks := passForgottenCheck.FindAllStringIndex(body, -1)
		kept := strings.Index(body, "keepClaimed(res")
		confirmed := strings.Index(body, "await confirmRenewal(")
		drawn := strings.Index(body, "showClaimed(")
		if read < 0 || read > asked || len(checks) != 2 ||
			checks[0][0] < caught || checks[0][0] > kept ||
			checks[1][0] < confirmed || checks[1][0] > drawn {
			t.Errorf("%s can store or draw an answer that arrived after this browser stopped collecting", name)
		}
	}
	if n := len(regexp.MustCompile(`\bpassForgets\b`).FindAllString(code, -1)); n != 8 {
		t.Errorf("passForgets appears %d times; want its declaration, forgetPassClaim's count, and a read and two "+
			"checks in each of collectPass and redeemPassReissue", n)
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "forgetBrowser")), "return forgetPassClaim();") {
		t.Error("Forget this browser no longer drops a purchase still being collected")
	}
	remove := code[strings.Index(code, `if (event.target.closest('[data-action="forget-pass"]')) {`):]
	remove = remove[:strings.Index(remove, "\n  }\n")]
	if !strings.Contains(remove, "const dropped = forgetPassClaim();") {
		t.Error("Remove the Pass no longer drops a purchase still being collected")
	}
	// Drawn at once, from what is already known, so the code is off the screen
	// before the visitor walks away, and focus goes to what took its place.
	if !strings.Contains(remove, "drawPass({ ...lastPassInfo, claim: false });") || strings.Contains(remove, "renderPass(") ||
		!strings.Contains(remove, "passFocus(") {
		t.Error("Remove the Pass no longer draws the page again at once, with focus on what took its place")
	}
	for _, name := range []string{"passCollectingHTML", "passOpenCheckoutHTML"} {
		if !strings.Contains(withoutComments(jsFunction(t, script, name)), "${passStopCollectingHTML()}") {
			t.Errorf("%s no longer offers to stop collecting here", name)
		}
	}
	stop := code[strings.Index(code, `const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');`):]
	stop = stop[:strings.Index(stop, "\n    return;\n  }")]
	if !strings.Contains(stop, "forgetPassClaim().then((done) => {") {
		t.Error("Stop collecting it here no longer drops the claim")
	}
	for name, text := range map[string]string{
		"Forget this browser": words(jsFunction(t, script, "forgetBrowserHTML")),
		"the FAQ":             words(faqAnswer(t, script, "faq-pass")),
		"the held Pass":       words(jsFunction(t, script, "passHeldHTML")),
	} {
		if !strings.Contains(text, "collecting") {
			t.Errorf("%s no longer says it stops the browser collecting a Pass on its way", name)
		}
	}
}

// After a button on the Pass page draws it again, focus goes to what took the
// button's place, not the top of the document.
func TestPassPageButtonsKeepFocus(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	for _, action := range []string{"collect-pass", "restart-pass", "forget-pass", "skip-reissue"} {
		start := strings.Index(code, `event.target.closest('[data-action="`+action+`"]')`)
		if start < 0 {
			t.Fatalf("no handler for %s", action)
		}
		handler := code[start:]
		handler = handler[:strings.Index(handler, "\n    return;\n  }")]
		if !strings.Contains(handler, "passFocus(") {
			t.Errorf("the %s handler leaves focus at the top of the document", action)
		}
	}
	for _, name := range []string{"checkHeldPass", "submitPass"} {
		if n := strings.Count(withoutComments(jsFunction(t, script, name)), "passFocus("); n < 2 {
			t.Errorf("%s moves focus after %d of its redraws", name, n)
		}
	}
	// What arrives after a button was pressed (Check now's claim, Use the link
	// here) draws the page again through redrawPass, which moves focus that
	// was on the page to what took the button's place.
	redraw := withoutComments(jsFunction(t, script, "redrawPass"))
	if !strings.Contains(redraw, "const focused = el.main.contains(document.activeElement);") ||
		!strings.Contains(redraw, "passFocus(...selectors);") {
		t.Error("redrawPass no longer keeps focus that was on the page")
	}
	for name, want := range map[string]string{
		"showClaimed":       `redrawPass({ ...lastPassInfo, claim }, '[data-action="collect-pass"]', "#pass-held", "form[data-pass] input");`,
		"redeemPassReissue": `redrawPass(lastPassInfo, "#pass-held", "form[data-pass] input");`,
	} {
		body := withoutComments(jsFunction(t, script, name))
		// drawPass( is in redrawPass( too, so equal counts mean no bare drawPass.
		if !strings.Contains(body, want) || strings.Count(body, "drawPass(") != strings.Count(body, "redrawPass(") {
			t.Errorf("%s draws the page again without keeping focus", name)
		}
	}
	if !strings.Contains(code, "redeemPassReissue(token, passVisit);") {
		t.Error("Use the link here no longer spends the link through redeemPassReissue")
	}
	// Use the link here stays enabled while its answer is awaited: a disabled
	// button drops focus to the top of the document, where redrawPass no
	// longer sees it on the page. The token is spent before the request, so a
	// second press does nothing.
	use := code[strings.Index(code, `const reissue = event.target.closest('[data-action="reissue-pass"]');`):]
	use = use[:strings.Index(use, "\n    return;\n  }")]
	if strings.Contains(use, "disabled") || !strings.Contains(use, "passReissueToken = \"\";\n    if (token) {") {
		t.Error("Use the link here disables the button that has focus, or can spend the link twice")
	}
}

// A browser that keeps no site data is not sent to Stripe to pay for a Pass
// it could never collect or keep. The probe is not in submitPass, which may
// store nothing but the Pass.
func TestPassCheckoutNeedsABrowserThatKeepsData(t *testing.T) {
	script := appScript(t)
	submit := withoutComments(jsFunction(t, script, "submitPass"))
	probe, checkout := strings.Index(submit, "if (!passCanBeKept()) {\n      showNotice(PASS_NO_STORAGE);\n      return;"), strings.Index(submit, `"/api/pass/checkout"`)
	if probe < 0 || checkout < probe {
		t.Error("submitPass no longer refuses a checkout in a browser that would keep nothing")
	}
	can := withoutComments(jsFunction(t, script, "passCanBeKept"))
	if !strings.Contains(can, "if (!navigator.cookieEnabled) {") || !strings.Contains(can, `const kept = storageGet(STORAGE_PROBE_KEY) === "1";`) {
		t.Error("passCanBeKept no longer checks both cookies and storage")
	}
	if got := jsString(t, script, "PASS_NO_STORAGE"); got != "This browser isn't letting educhan keep anything, so it couldn't collect or keep a Pass. Allow this site to store data first." {
		t.Errorf("PASS_NO_STORAGE says %q", got)
	}
}

// A code typed in after it ran out is kept, with the date the server has for
// it, so the page offers to renew it.
func TestPassTypedInAfterItRanOutCanBeRenewed(t *testing.T) {
	submit := withoutComments(jsFunction(t, appScript(t), "submitPass"))
	if !strings.Contains(submit, `if (kind === "activate" && refusal === "expired" && err.payload.code) {`+
		"\n      const kept = storePass(err.payload.code, err.payload.expires_at);") {
		t.Error("submitPass no longer keeps a code typed in after it ran out")
	}
	if !strings.Contains(withoutComments(jsFunction(t, appScript(t), "passRunOutText")), `"This Pass has run out. You can renew it below."`) {
		t.Error("a code typed in after it ran out is no longer said to be renewable below")
	}
}

// While sales are paused (EDUCHAN_PASS_SALES=paused) the page says so where
// Buy, Renew and Start again would be, and knows checkout's refusal by the
// server's own words, while a Pass held keeps everything else.
func TestPassPageWhileSalesArePaused(t *testing.T) {
	script := appScript(t)
	if got := jsString(t, script, "PASS_NOT_ON_SALE"); got != passNotOnSale {
		t.Errorf("PASS_NOT_ON_SALE in app.js is %q; the server refuses with %q", got, passNotOnSale)
	}
	if got, want := jsString(t, script, "PASS_PAUSED"), strings.ToUpper(passNotOnSale[:1])+passNotOnSale[1:]+"."; got != want {
		t.Errorf("PASS_PAUSED is %q, want the server's words as a sentence, %q", got, want)
	}
	view := withoutComments(jsFunction(t, script, "passViewHTML"))
	if !strings.Contains(view, "if (!has && info.paused) {\n    parts.push(`<p class=\"empty\">${esc(PASS_PAUSED)}</p>`);\n  } else if (!has) {") {
		t.Error("passViewHTML offers Buy while sales are paused")
	}
	held := withoutComments(jsFunction(t, script, "passHeldHTML"))
	paused, renew := strings.Index(held, "if (info.paused) {\n    renew = `<p class=\"muted\">${esc(PASS_PAUSED)}</p>`;"), strings.Index(held, `data-pass="renew"`)
	if paused < 0 || renew < paused {
		t.Error("passHeldHTML offers Renew while sales are paused")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "passCollectingHTML")), "const restart = info.paused\n    ? \"\"") {
		t.Error("the collecting panel offers Start again while sales are paused")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "submitPass")), "if (err.status === 403 && err.message === PASS_NOT_ON_SALE) {\n      drawPass({ ...lastPassInfo, paused: true });") {
		t.Error("a checkout refused because sales were paused does not draw the page as paused")
	}
}

// The header links to the Pass only on a server that has it, or in a browser
// that holds one, which needs the page to show its code or remove it.
func TestHeaderLinksToThePassOnlyWhereItIs(t *testing.T) {
	shell, err := fs.ReadFile(webFS(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shell), `<a class="verify-link" href="/pass" data-page="pass" data-pass-link hidden>Pass</a>`) {
		t.Error("the header's Pass link is not hidden until the page knows the server has the Pass")
	}
	css, err := fs.ReadFile(webFS(), "app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".verify-link[hidden] { display: none; }") {
		t.Error("the header link's display would override its hidden attribute")
	}
	script := appScript(t)
	if !strings.Contains(withoutComments(jsFunction(t, script, "showPassLink")), "el.passLink.hidden = !(state.site.pass || storedPass());") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "renderNav")), "showPassLink();") {
		t.Error("the header's Pass link is no longer shown for a server with the Pass or a browser holding one")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "loadSite")), "pass: site.pass === true") {
		t.Error("loadSite no longer reads whether the server has the Pass")
	}
}

// What the Pass page and the FAQ say about refunds, hours, a server's unknown
// terms and a Pass that is gone, as the server does it.
func TestPassCopyMatchesWhatTheServerDoes(t *testing.T) {
	script := appScript(t)
	intro := words(withoutComments(jsFunction(t, script, "passIntroHTML")))
	faq := words(faqAnswer(t, script, "faq-pass"))
	for name, text := range map[string]string{"the Pass page": intro, "the FAQ's Pass entry": faq} {
		for _, want := range []string{"a refund or a chargeback can take back the time that payment bought", "Hours are counted in UTC"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s no longer says %q", name, want)
			}
		}
		if strings.Contains(text, "cancel the Pass") || strings.Contains(text, "until the hour ends") {
			t.Errorf("%s says a refund cancels the Pass, or that the hour is the reader's own", name)
		}
	}
	if !strings.Contains(intro, `const term = info.enabled ? passTermText(info.days) : "a set time";`) {
		t.Error("the Pass page says how long a Pass lasts without the server's terms")
	}
	if got := jsString(t, script, "PASS_GONE_REASONS"); got != "It may have run out more than a month ago, or been refunded or replaced." {
		t.Errorf("PASS_GONE_REASONS says %q", got)
	}
	if n := strings.Count(withoutComments(script), "${PASS_GONE_REASONS}"); n != 4 {
		t.Errorf("the reasons a Pass may have gone are given in %d places; want a check, a renewal's check, a post's answer and its notice", n)
	}
}
