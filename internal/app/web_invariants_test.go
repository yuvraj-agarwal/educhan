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
	// Nor is a Pass that has run out sent: it is kept to be renewed, and on a
	// post it would be a code travelling for nothing.
	if !strings.Contains(submit, "if (pass && !pass.expired) {\n    headers[PASS_HEADER] = pass.code;") {
		t.Error("the submit handler sends a Pass that has run out")
	}
	api := withoutComments(jsFunction(t, script, "api"))
	for _, banned := range []string{"storedPass", "PASS_STORAGE_KEY", "PASS_HEADER"} {
		if strings.Contains(api, banned) {
			t.Errorf("api() reads %s; every request would carry the Pass", banned)
		}
	}
	// Where the code may go besides a post: the body of a renewal
	// (submitPass) and of a check of the Pass held (checkHeldPass, and
	// recheckPass after a renewal was collected or an answer was lost), back
	// into storage with the expiry a claim or a check brought (keepClaimed,
	// recheckPass), and onto the screen when the visitor asks to see it
	// (showPassCode). The readers are pinned rather than a variable name: the
	// stored Pass is read through storedPass, only in these functions, and its
	// key and header are spelled out once each, so nothing reads it any other
	// way. Each is a deliberate use; a new one is a new place the code goes.
	code := withoutComments(script)
	readers := []string{
		"storePass",           // reads back what it stored
		"showPassLink",        // whether to show the header's link
		"passViewHTML",        // whether to draw the held Pass
		"showPassCode",        // the screen, on request
		"keepClaimed",         // storage, with a claim's expiry
		"checkHeldPass",       // the body of /api/pass/activate
		"confirmRenewal",      // whether a Pass is held to check after a renewal
		"recheckPass",         // whether the Pass checked is still held
		"passAfterLostAnswer", // whether a Pass is held to check after a lost answer
		"passStillHeld",       // whether an answer is still about the Pass held
		"followPassStorage",   // whether a code just handed over is still held
		"submitPass",          // the body of a renewal
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
	for _, name := range []string{"storedPass", "storePass", "markPassExpired", "forgetPass", "followPassStorage"} {
		keyUsers += strings.Count(withoutComments(jsFunction(t, script, name)), "PASS_STORAGE_KEY")
	}
	if n := strings.Count(code, "PASS_STORAGE_KEY"); n != keyUsers+1 {
		t.Errorf("PASS_STORAGE_KEY appears %d times in app.js; want its declaration and %d uses, in storedPass, "+
			"storePass, markPassExpired, forgetPass and followPassStorage", n, keyUsers)
	}
	// followPassStorage only compares the key of a storage event with it, and
	// reads the Pass itself through storedPass.
	if follow := withoutComments(jsFunction(t, script, "followPassStorage")); strings.Count(follow, "PASS_STORAGE_KEY") != 1 ||
		!strings.Contains(follow, "if (event.key !== PASS_STORAGE_KEY && event.key !== null) {") ||
		strings.Contains(follow, "oldValue") || strings.Contains(follow, "newValue") {
		t.Error("followPassStorage reads the stored Pass other than through storedPass")
	}
	for _, name := range []string{"checkHeldPass", "recheckPass"} {
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

// passForgottenCheck is how collectPass drops an answer that arrived after
// this browser stopped collecting (passForgets), and redeemPassReissue one
// that arrived after Remove the Pass or Forget this browser (passForgotten).
var passForgottenCheck = regexp.MustCompile(`if \((?:forgets !== passForgets|passForgotten\(forgets\))\) \{\s*return;\s*\}`)

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
	recheck := withoutComments(jsFunction(t, script, "recheckPass"))
	checked := strings.Index(recheck, `await api("/api/pass/activate"`)
	if checked < 0 || !strings.Contains(recheck[checked:], "storePass(res.pass, res.expires_at);") ||
		!strings.Contains(confirm, "const found = await recheckPass(held);") ||
		strings.Contains(confirm, "storePass(") || strings.Contains(recheck, "renewed") {
		t.Error("confirmRenewal no longer stores the expiry the server gives for the held code (recheckPass), and only that")
	}
	for name, call := range map[string]string{
		"collectPass":       `const notice = news === "renewed" ? await confirmRenewal(res) : "";`,
		"redeemPassReissue": "if (news === \"renewed\") {\n    notice = await confirmRenewal(res, true);",
	} {
		body := withoutComments(jsFunction(t, script, name))
		kept, confirmed := strings.Index(body, "keepClaimed(res"), strings.Index(body, call)
		if kept < 0 || confirmed < kept {
			t.Errorf("%s no longer checks the held Pass after a renewal, once what the claim brought is kept", name)
		}
	}
	// A Pass removed while it was being checked is not stored back, or
	// removed again in its name: the check of what is held comes between the
	// answer and anything that writes.
	afterCheck := recheck[checked:]
	still := strings.Index(afterCheck, "if (passForgotten(forgets) || !still || still.code !== held.code) {\n    return { state: \"changed\" };\n  }")
	if still < 0 || !strings.Contains(afterCheck, "const still = storedPass();") ||
		strings.Index(afterCheck, "storePass(") < still || strings.Index(afterCheck, "forgetPass()") < still {
		t.Error("recheckPass writes what it checked into a browser whose Pass has changed meanwhile")
	}
	// Nor into one forgotten meanwhile, here or in another tab, or while that
	// is on its way, whatever it holds by then.
	if read := strings.Index(recheck, "const forgets = passCodeForgets;"); read < 0 || read > checked {
		t.Error("recheckPass no longer notes the forgets before it asks, so it can store into a browser forgotten meanwhile")
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
	if !strings.Contains(withoutComments(jsFunction(t, script, "showClaimed")), "function showClaimed(claim, news, notice = \"\") {\n  if (!passPageShown()) {\n    return;\n  }") {
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
	if call == nil || !strings.Contains(call[1], "info.claim || passNoncePending(PASS_NONCE_STORAGE_KEY)") || strings.Contains(call[1], "back") || strings.Contains(call[1], "paid") {
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
	// A failed attempt says the page will ask again only in the drawing whose
	// collection that is: one overtaken by a newer drawing is about to stop,
	// and its note would land in the newer drawing's panel.
	if strings.Count(collect, "note.textContent = ") != 1 ||
		!strings.Contains(collect, "if (note && visit === passVisit) {\n        note.textContent = `${passErrorText(failed)} This page will ask again shortly.`;") {
		t.Error("collectPass writes its failed attempt into the note of a drawing that is not its own")
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
	buy := strings.Index(view, `data-pass="buy"`)
	activate := regexp.MustCompile(`(?m)^  parts\.push\(passCodeFieldHTML\(held\)\);$`).FindStringIndex(view)
	if stop == nil || buy < stop[0] || activate == nil || activate[0] < stop[0] {
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
	open := words(withoutComments(jsFunction(t, script, "passOpenCheckoutHTML")))
	if strings.Contains(open, "Collecting your Pass") {
		t.Error("a browser that has a Pass is told its Pass is being collected")
	}
	// Back from paying for a renewal, before Stripe confirms it, the page
	// cannot tell that payment from a checkout left unpaid, so it says what
	// either comes to, and never that the payment just made is unpaid.
	if strings.Contains(open, "hasn't been paid") || strings.Contains(open, "Another checkout") ||
		!strings.Contains(open, "If you've just paid, Stripe is confirming the payment") ||
		!strings.Contains(open, "If you left it without paying, it stays open on Stripe") {
		t.Error("passOpenCheckoutHTML tells a buyer back from paying that their checkout is unpaid")
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
	if !strings.Contains(withoutComments(jsFunction(t, script, "passViewHTML")), "parts.push(passCodeFieldHTML(held));") ||
		!strings.Contains(words(withoutComments(jsFunction(t, script, "passCodeFieldHTML"))),
			`if (held && held.expired) { label = "Have a newer code, or a reissue code? Enter it";`) {
		t.Error("passViewHTML no longer offers to enter a newer code for a Pass that has run out here")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "passHeldHTML")), `data-action="check-pass"`) ||
		!strings.Contains(code, "checkHeldPass(checkPass);") {
		t.Error("a Pass that has run out here can no longer be checked again")
	}
	submit := withoutComments(submitHandler(t, script))
	if !strings.Contains(submit, "markPassExpired(sent.code);") || strings.Contains(submit, "storePass(pass.code") {
		t.Error("a post refused for an expired Pass no longer marks it run out without a date")
	}
	if stored := withoutComments(jsFunction(t, script, "storedPass")); !strings.Contains(stored, "if (saved.expired === true) {") ||
		!strings.Contains(stored, "return { code: saved.code, expires: null, expired: true };") {
		t.Error("storedPass no longer reads a Pass marked run out")
	}
}

// An answer about the Pass held that arrives after that Pass left this
// browser (Remove the Pass, Forget this browser, here or in another tab, or
// another code stored meanwhile) acts on nothing: a code check does not store
// it back, and a post's answer neither removes nor marks run out a Pass that
// is not the one it was sent with. passStillHeld compares the code held now
// with the one held when the request went. A browser that held no Pass can
// tell nothing from that, so an answer on the Pass page is also dropped once
// Remove or Forget has been pressed since its request went, or while one is
// on its way (passForgotten): a code typed into an empty browser is not
// stored again after Forget this browser.
func TestPassAnswerAboutAPassThatLeftIsDropped(t *testing.T) {
	script := appScript(t)
	if still := withoutComments(jsFunction(t, script, "passStillHeld")); !strings.Contains(still, "const now = storedPass();") ||
		!strings.Contains(still, `return (now ? now.code : "") === (before ? before.code : "");`) {
		t.Error("passStillHeld no longer compares the code held now with the one held before")
	}
	if forgotten := withoutComments(jsFunction(t, script, "passForgotten")); !strings.Contains(forgotten,
		"return forgets !== passCodeForgets || passCodeForgetting !== null;") {
		t.Error("passForgotten no longer drops an answer once Remove or Forget was pressed meanwhile, or is on its way")
	}
	// Each writer of an answer checks first: the forgets are noted before the
	// request, and every storePass, forgetPass, markPassExpired and passLost
	// after it comes after both checks. Use this code here (its expired branch
	// included), Check it again, and a reissue code.
	for name, guard := range map[string]string{
		"checkHeldPass":     "if (passForgotten(forgets) || !passStillHeld(held)) {",
		"submitPass":        "if (dropped || passForgotten(forgets) || !passStillHeld(held)) {",
		"redeemPassReissue": "if (passForgotten(forgets)) {",
	} {
		body := withoutComments(jsFunction(t, script, name))
		read := strings.Index(body, "const forgets = passCodeForgets;")
		asked := regexp.MustCompile(`await api\((request\.path|"/api/pass/activate"|"/api/pass/claim")`).FindStringIndex(body)
		if asked == nil || read < 0 || read > asked[0] {
			t.Errorf("%s no longer notes the forgets before its request", name)
			continue
		}
		after := body[asked[0]:]
		guarded := strings.Index(after, guard)
		for _, write := range []string{"storePass(", "forgetPass()", "markPassExpired(", "passLost = ", "keepClaimed("} {
			if at := strings.Index(after, write); at >= 0 && (guarded < 0 || at < guarded) {
				t.Errorf("%s calls %s on an answer before checking it is still about the Pass held, and nothing forgot it", name, write)
			}
		}
	}
	// A code typed in, a reissue code, Check it again or a checkout asked for
	// while a forget is on its way, here or in another tab, waits for every one
	// before the forgets are noted and the request goes, so its answer is not
	// dropped for a forget pressed before it was asked for. But the forgets are
	// noted first when the button is pressed, and one that comes while it
	// waits, Remove the Pass or Forget this browser pressed after it, here or
	// in another tab, means it sends nothing: the button comes back, and the
	// notice says why. Noted only after the wait, a renewal would send the code
	// Remove took away, and a reissue code be spent and its Pass stored after
	// Forget. A Stop alone, which only a tab that has given up waiting can
	// send, is said apart, since nothing was removed or forgotten. Each wait
	// ends a turn after the forgets, so what Stop says once it is answered
	// does not take the place of what the press says.
	settled := withoutComments(jsFunction(t, script, "passForgetsSettled"))
	if !strings.Contains(settled, "const codes = passCodeForgets;\n  const claims = passForgets;\n"+
		"  while (passForgetting) {\n    await passForgetting;\n    await new Promise((resolve) => setTimeout(resolve));\n  }\n"+
		"  if (codes !== passCodeForgets) {\n    return PASS_FORGOTTEN_MEANWHILE;\n  }\n"+
		"  return claims !== passForgets ? PASS_STOPPED_MEANWHILE : \"\";\n}") {
		t.Error("passForgetsSettled no longer waits for every forget on its way, and says what came meanwhile")
	}
	for name, dropped := range map[string]string{
		"submitPass": "const meanwhile = await passForgetsSettled();\n  if (meanwhile) {\n    button.disabled = false;\n    button.textContent = label;\n" +
			"    if (passPageShown()) {\n      refocus(button);\n      showNotice(meanwhile);\n    }\n    return;\n  }",
		"redeemPassReissue": "const meanwhile = await passForgetsSettled();\n  if (meanwhile) {\n    if (passPageShown()) {\n      showNotice(meanwhile);\n    }\n    return;\n  }",
		"checkHeldPass": "const meanwhile = await passForgetsSettled();\n  if (meanwhile) {\n    button.disabled = false;\n" +
			"    if (passPageShown()) {\n      refocus(button);\n      showNotice(meanwhile);\n    }\n    return;\n  }",
	} {
		body := withoutComments(jsFunction(t, script, name))
		waited, noted := strings.Index(body, dropped), strings.Index(body, "const forgets = passCodeForgets;")
		asked := regexp.MustCompile(`await api\(`).FindStringIndex(body)
		if waited < 0 || noted < waited || asked == nil || asked[0] < noted || strings.Count(body, "passForgetsSettled()") != 1 {
			t.Errorf("%s sends its request while a forget is on its way, or after a Remove or Forget pressed while it waited", name)
			continue
		}
		// What passForgetsSettled notes is what had been forgotten when the
		// button was pressed: nothing is awaited before it but submitPass's
		// hand-over of a reissue code, which waits in redeemPassReissue.
		before := strings.Replace(body[:waited], "await redeemPassReissue(typed);", "", 1)
		if strings.Contains(before, "await ") || strings.Contains(before, ".then(") {
			t.Errorf("%s waits before it notes the forgets, so one pressed after its button counts as one pressed before", name)
		}
	}
	if n := strings.Count(withoutComments(script), "passForgetsSettled()"); n != 4 ||
		!strings.Contains(script, "\nconst PASS_FORGOTTEN_MEANWHILE = \"Nothing was sent: the Pass was removed, or this browser forgotten") ||
		!strings.Contains(script, "\nconst PASS_STOPPED_MEANWHILE = \"Nothing was sent: this browser stopped collecting") {
		t.Errorf("passForgetsSettled() appears %d times; want its definition and the three presses that wait on it, "+
			"which say nothing was sent, and why, when a forget came meanwhile", n)
	}
	// Each is called straight from the press: the click, or the submit
	// handler, which hands a Pass form over before it awaits anything.
	code := withoutComments(script)
	if !strings.Contains(code, "    checkHeldPass(checkPass);\n") ||
		!strings.Contains(withoutComments(submitHandler(t, script)), "  const form = event.target;\n  if (form.dataset.verify) {\n"+
			"    await submitVerification(form);\n    return;\n  }\n  if (form.dataset.pass) {\n    await submitPass(form);") {
		t.Error("Check it again or a Pass form waits before it reaches passForgetsSettled, which then notes forgets pressed after it")
	}
	// One asked for before the forget and dropped for it gives the button that
	// waited its focus back, rather than leaving it on the top of the document.
	if !strings.Contains(withoutComments(jsFunction(t, script, "submitPass")),
		"if (dropped || passForgotten(forgets) || !passStillHeld(held)) {\n    button.disabled = false;\n    button.textContent = label;\n"+
			"    if (passPageShown()) {\n      refocus(button);\n    }\n    return;\n  }") {
		t.Error("submitPass drops an answer for a forget and leaves focus on the top of the document")
	}
	// A checkout still on its way when Remove the Pass, Forget this browser or
	// Stop collecting it here is pressed, here or in another tab, sets a claim
	// that forget never saw. It is not followed to Stripe, and that claim is
	// dropped by the order its answer names: never by a forget of every claim,
	// which would take a checkout's started after the forget, nor by the
	// forget's marks and counters, which would stop other tabs collecting what
	// is rightly theirs. Both counters are noted when the request goes, once no
	// forget is on its way, so only a forget pressed after it counts.
	checkout := withoutComments(jsFunction(t, script, "submitPass"))
	noted := strings.Index(checkout, "const forgets = passCodeForgets;\n  const claims = passForgets;\n")
	asked := strings.Index(checkout, "res = await api(request.path,")
	late := strings.Index(checkout, "const dropped = Boolean(res) && kind !== \"activate\" && (claims !== passForgets || passForgotten(forgets));\n"+
		"  if (dropped) {\n    forgetPassOrder(res.order);\n  }\n")
	guard := strings.Index(checkout, "if (dropped || passForgotten(forgets) || !passStillHeld(held)) {")
	assign := strings.Index(checkout, "window.location.assign(res.url);")
	if noted < 0 || asked < noted || noted < strings.Index(checkout, "await passForgetsSettled();") || late < asked || guard < late || assign < guard ||
		len(regexp.MustCompile(`\bpassForgets\b`).FindAllString(checkout, -1)) != 2 {
		t.Error("a checkout answered after a forget pressed since it went keeps the claim it set, or is followed to Stripe")
	}
	order := withoutComments(jsFunction(t, script, "forgetPassOrder"))
	named := strings.Index(order, `api("/api/pass/claim", { method: "POST", body: JSON.stringify({ forget: true, order }) })`)
	if checked := strings.Index(order, "if (typeof order !== \"string\" || !order) {\n    return;\n  }"); checked < 0 || named < checked ||
		strings.Count(order, "api(") != 1 {
		t.Error("forgetPassOrder no longer drops the one claim its order names, or asks for a forget of every claim when there is no name")
	}
	for _, kept := range []string{"storageSet(", "localStorage", "PASS_FORGET_STORAGE_KEY", "passVisit", "passForgets", "passCodeForgets",
		"dropPassNonce(", "waitOnPassForget(", "heedPassForget("} {
		if strings.Contains(order, kept) {
			t.Errorf("forgetPassOrder does %q: the order's name is sent once and kept nowhere, and the drop stops nothing else", kept)
		}
	}
	if strings.Count(code, "forgetPassOrder(") != 2 || strings.Count(code, ".order") != 1 {
		t.Error("a checkout's order is read, or forgotten by name, other than where a checkout answered after a forget is dropped")
	}
	// What offers Remove the Pass and Forget this browser says so, as a
	// shared computer's reader needs to hear it.
	for name, want := range map[string][2]string{
		"Forget this browser": {"forgetBrowserHTML", "It also stops this browser collecting a Pass bought here that hasn't arrived yet, " +
			"even one whose checkout was still opening;"},
		"the held Pass": {"passHeldHTML", "Removing it also stops this browser collecting anything bought here that hasn't " +
			"arrived yet, even a checkout still opening."},
	} {
		if !strings.Contains(words(jsFunction(t, script, want[0])), want[1]) {
			t.Errorf("%s no longer says a checkout still opening is stopped too (%q)", name, want[1])
		}
	}
	if want := "Either also stops the browser collecting a Pass bought there that hasn't arrived yet, even one whose " +
		"checkout was still opening, so the next person can't collect it;"; !strings.Contains(words(faqAnswer(t, script, "faq-pass")), want) {
		t.Errorf("the FAQ's Pass entry no longer says a checkout still opening is stopped too (%q)", want)
	}
	submit := withoutComments(submitHandler(t, script))
	sent := strings.Index(submit, "const sent = pass && passStillHeld(pass) ? pass : null;")
	if sent < 0 || sent > strings.Index(submit, "forgetPass();") || sent > strings.Index(submit, "markPassExpired(") ||
		strings.Contains(submit, "pass && created.pass") {
		t.Error("a post's answer about its Pass acts on whatever Pass is held when it arrives")
	}
}

// What arrives after a wait on the Pass page (a code checked, a reissue code
// spent, a claim, Check now, Stop collecting it here, Remove's request, a
// copy, another tab's forget) draws the page, moves focus or says a notice
// only while the reader is still on it (passPageShown); one who moved on to a
// board or the Verify page meanwhile stays where they are, with what was
// still right to store stored. Each drawing after an await, or in a .then,
// is preceded by that check since the wait; the async functions make it once,
// at their own level, after their last wait, so no branch goes without it.
func TestPassAnswersDrawOnlyOnThePassPage(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if shown := withoutComments(jsFunction(t, script, "passPageShown")); !strings.Contains(shown, `return state.route.view === "pass";`) {
		t.Error("passPageShown no longer asks whether the reader is on the Pass page")
	}
	// The one place the Pass page's code asks where the reader is.
	if n := strings.Count(withoutComments(passSection(t, script)), "state.route.view"); n != 1 {
		t.Errorf("the Pass section reads state.route.view %d times; want once, in passPageShown", n)
	}
	handler := func(opener string) string {
		start := strings.Index(code, opener)
		if start < 0 {
			t.Fatalf("app.js has no %q; this test reads it", opener)
		}
		block := code[start:]
		return block[:strings.Index(block, "\n    return;\n  }")]
	}
	submit := withoutComments(jsFunction(t, script, "submitPass"))
	// The reissue code's branch hands over to redeemPassReissue, checked on
	// its own, and returns.
	reissue := strings.Index(submit, "if (isReissueCode(typed)) {")
	reissueEnd := strings.Index(submit[reissue:], "\n    }\n") + reissue
	if reissue < 0 || !strings.Contains(submit[reissue:reissueEnd], "await redeemPassReissue(typed);") {
		t.Fatal("submitPass no longer hands a reissue code over to redeemPassReissue; this test reads around it")
	}
	submit = submit[:reissue] + submit[reissueEnd:]
	bodies := map[string]string{
		"renderPass":           withoutComments(jsFunction(t, script, "renderPass")),
		"checkHeldPass":        withoutComments(jsFunction(t, script, "checkHeldPass")),
		"submitPass":           submit,
		"redeemPassReissue":    withoutComments(jsFunction(t, script, "redeemPassReissue")),
		"followPassForget":     withoutComments(jsFunction(t, script, "followPassForget")),
		"forgetPassOrder":      withoutComments(jsFunction(t, script, "forgetPassOrder")),
		"Remove the Pass":      handler(`if (event.target.closest('[data-action="forget-pass"]')) {`),
		"Stop collecting":      handler(`const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');`),
		"Copy the code":        handler(`if (event.target.closest('[data-action="copy-pass"]')) {`),
		"Check now":            handler(`if (event.target.closest('[data-action="collect-pass"]')) {`),
		"another tab's change": withoutComments(jsFunction(t, script, "followPassStorage")),
	}
	waits := regexp.MustCompile(`\bawait |\.then\(|\.catch\(`)
	draws := regexp.MustCompile(`\b(drawPass|redrawPass|passFocus|focusPassPanel|showNotice|refocus|renderPass|announce)\(|location\.assign\(`)
	gates := regexp.MustCompile(`passPageShown\(\)|if \(drawn\) \{`)
	for name, body := range bodies {
		waited := waits.FindAllStringIndex(body, -1)
		gated := gates.FindAllStringIndex(body, -1)
		if len(gated) == 0 {
			t.Errorf("%s never asks whether the reader is still on the Pass page", name)
			continue
		}
		for _, draw := range draws.FindAllStringIndex(body, -1) {
			last := -1
			for _, wait := range waited {
				if wait[0] < draw[0] {
					last = wait[0]
				}
			}
			if last < 0 {
				continue // drawn in the click itself, before any wait
			}
			ok := false
			for _, gate := range gated {
				ok = ok || (gate[0] > last && gate[0] < draw[0])
			}
			if !ok {
				t.Errorf("%s draws, focuses or says something after a wait (%q) without asking first whether the reader "+
					"is still on the Pass page", name, body[draw[0]:draw[1]])
			}
		}
	}
	// The async functions ask once, at their own level, after their last
	// wait: a check inside one branch would leave the others drawing over
	// another page.
	for name, gate := range map[string]string{
		"renderPass":        "\n  if (visit !== passVisit || !passPageShown()) {\n    return false;\n  }",
		"checkHeldPass":     "\n  if (!passPageShown()) {\n    button.disabled = false;\n    return;\n  }",
		"submitPass":        "\n  if (!passPageShown()) {\n    button.disabled = false;\n    button.textContent = label;\n    return;\n  }",
		"redeemPassReissue": "\n  if (!passPageShown()) {\n    return;\n  }",
	} {
		body := bodies[name]
		waited := waits.FindAllStringIndex(body, -1)
		at := strings.Index(body, gate)
		if at < 0 || len(waited) == 0 || at < waited[len(waited)-1][0] {
			t.Errorf("%s no longer asks, at its own level and after its last wait, whether the reader is still on the Pass page", name)
		}
	}
	// A checkout opened that the reader is not then sent to, because they
	// left the page, the Pass held changed or a forget came first, is one left
	// behind, as if they had left Stripe without paying: the forms come back
	// over it, and it is not drawn as a payment Stripe has yet to confirm. One
	// a forget came first for has its claim dropped as well (forgetPassOrder,
	// pinned in TestPassAnswerAboutAPassThatLeftIsDropped).
	left := strings.Index(submit, "if (res && kind !== \"activate\") {\n    passStartedAgain = true;\n  }")
	if guarded := strings.Index(submit, "if (dropped || passForgotten(forgets) || !passStillHeld(held)) {"); left < 0 || guarded < left ||
		!strings.Contains(submit, "passStartedAgain = false;\n    window.location.assign(res.url);") {
		t.Error("a checkout answered after the reader left the Pass page, or after a forget, is drawn as one to collect")
	}
	// Check now moves focus only if its own drawing happened, which renderPass
	// says; the claim's answer is shown through showClaimed, which asks first.
	if !strings.Contains(bodies["Check now"], "renderPass().then((drawn) => {\n      if (drawn) {\n        passFocus(") ||
		!strings.Contains(bodies["renderPass"], "collectPass(visit);\n  }\n  return true;\n}") {
		t.Error("Check now moves focus once its drawing was overtaken, or the reader left the page")
	}
}

// A Pass that is no good on a post is treated as no Pass, and the created
// post's answer says why ("pass"), so the page stops sending it: an unknown
// one is forgotten and one that ran out is kept to renew. No post is refused
// for its Pass any more, so nothing reads a refusal for one.
func TestPostMadeWithANoGoodPassSaysSo(t *testing.T) {
	submit := withoutComments(submitHandler(t, appScript(t)))
	for _, want := range []string{
		`if (sent && created.pass === "invalid") {` + "\n      forgetPass();",
		`} else if (sent && created.pass === "expired") {` + "\n      markPassExpired(sent.code);",
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
		if !strings.Contains(body, "kept = storePass(res.pass, res.expires_at);") || !strings.Contains(body, ": PASS_NOT_KEPT") {
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
		// Without that answer, a Pass held here is still drawn, to show its code
		// or remove it, which needs nothing from the server, with no Renew and
		// nothing said about sales; so is the code box, and the error after them.
		block := view[failed:]
		block = block[:strings.Index(block, "\n  }\n")]
		held := strings.Index(block, "if (held) {\n      parts.push(passHeldHTML({ enabled: false }, held, shown));\n    }")
		box := strings.Index(block, "parts.push(passCodeFieldHTML(held));")
		said := strings.Index(block, "parts.push(`<p class=\"empty\">${esc(passErrorText(info.failed))}</p>`);")
		if held < 0 || box < held || said < box || strings.Contains(block, "PASS_PAUSED") || strings.Contains(block, "not on sale") ||
			!strings.Contains(block, "return parts.join(\"\");") {
			t.Error("a Pass page that could not load hides the Pass held here, or offers it for sale")
		}
		// An answer from educhan on such a page, to a code typed in, a reissue
		// code or Check it again, has the page asked for again, which would
		// otherwise go on saying educhan couldn't be reached under what educhan
		// has just answered.
		refresh := withoutComments(jsFunction(t, script, "refreshFailedPass"))
		if !strings.Contains(refresh, "if (!lastPassInfo.failed) {\n    return;\n  }\n  renderPass().then((drawn) => {") {
			t.Error("refreshFailedPass no longer asks for the Pass page again after an answer on one that could not load")
		}
		// The new drawing moves focus only when it dropped it to the top of the
		// document: focus the reader took elsewhere meanwhile, to the header or
		// the notice, stays where they put it.
		if !strings.Contains(refresh, "if (drawn && (!document.activeElement || document.activeElement === document.body)) {\n"+
			"      passFocus(...selectors);\n    }") {
			t.Error("refreshFailedPass pulls focus back into the Pass page from wherever the reader moved it")
		}
		for name, want := range map[string]int{"submitPass": 3, "redeemPassReissue": 1, "checkHeldPass": 3} {
			if n := strings.Count(withoutComments(jsFunction(t, script, name)), "refreshFailedPass("); n != want {
				t.Errorf("%s asks for a Pass page that could not load again after %d of its answers; want %d", name, n, want)
			}
		}
		if !strings.Contains(withoutComments(jsFunction(t, script, "redeemPassReissue")), "if (answered) {\n    refreshFailedPass(") {
			t.Error("redeemPassReissue asks for a Pass page that could not load again without an answer from educhan")
		}
	})
	t.Run("the server's words are sentences", func(t *testing.T) {
		// The page goes on after them ("This page will ask again shortly."),
		// so they start with a capital and end with a full stop, whatever the
		// status, a proxy's status line included.
		text := withoutComments(jsFunction(t, script, "passErrorText"))
		if !strings.Contains(text, "const said = err.message.trim();") ||
			!strings.Contains(text, "const text = said.charAt(0).toUpperCase() + said.slice(1);") ||
			!strings.Contains(text, "return /[.!?]$/.test(text) ? text : `${text}.`;") || strings.Contains(text, "err.status === 403 ?") {
			t.Error("passErrorText no longer makes every message from the server a sentence")
		}
	})
	t.Run("what was said about a Pass gone goes with Forget and Remove", func(t *testing.T) {
		// The note is the last holder's news; the next person at a shared
		// computer is not told about a Pass that was never theirs. It is set
		// empty in its declaration, on leaving the page, and by these two,
		// pressed here or in another tab (followPassForget, for a "code:"
		// mark).
		code := withoutComments(script)
		remove := code[strings.Index(code, `if (event.target.closest('[data-action="forget-pass"]')) {`):]
		remove = remove[:strings.Index(remove, "\n  }\n")]
		follow := withoutComments(jsFunction(t, script, "followPassForget"))
		if !strings.Contains(withoutComments(jsFunction(t, script, "forgetBrowser")), "passLost = \"\";") ||
			!strings.Contains(remove, "passLost = \"\";") || strings.Count(code, "passLost = \"\";") != 5 ||
			!regexp.MustCompile(`if \(code\) \{[^}]*passLost = "";`).MatchString(follow) {
			t.Error("Forget this browser or Remove the Pass, here or in another tab, leaves what the page said about a Pass that has gone")
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
		// Only the rules and the FAQ, which fetch nothing, are read past an
		// unanswered screen; the Pass page asks the server about this browser's
		// purchase and its code.
		if !strings.Contains(script, "\nconst UNGATED_VIEWS = new Set([\"rules\", \"faq\"]);\n") {
			t.Error("UNGATED_VIEWS lets more than the rules and the FAQ past the age screen")
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

// A reissue code is typed or pasted into the Pass page's code box, so it is in
// no address, and no history, bookmark or Referer can keep it. It goes to the
// server once, in the body of a claim with its own nonce, is kept in memory
// only until an answer arrives, and goes with Remove the Pass, Forget this
// browser and leaving the page. The page knows it from a Pass code by the
// server's own word and length.
func TestPassReissueCodeIsTypedAndNeverKept(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	for _, banned := range []string{"reissue=", "takePassReissueToken", "passReissueToken"} {
		if strings.Contains(code, banned) {
			t.Errorf("app.js has %q; a reissue code never rides in an address", banned)
		}
	}
	if strings.Contains(withoutComments(passSection(t, script)), "location.hash") {
		t.Error("the Pass page reads the address's fragment; a reissue code never rides in an address")
	}
	if got := jsString(t, script, "REISSUE_CODE_WORD"); got != reissueCodePrefix {
		t.Errorf("REISSUE_CODE_WORD is %q; the server's reissue codes start %q", got, reissueCodePrefix)
	}
	if !strings.Contains(script, "\nconst REISSUE_CODE_LENGTH = "+strconv.Itoa(reissueCodeLen)+";\n") {
		t.Errorf("REISSUE_CODE_LENGTH is not the server's %d", reissueCodeLen)
	}
	if got := jsString(t, script, "PASS_REISSUE_SPENT"); got != passReissueSpent {
		t.Errorf("PASS_REISSUE_SPENT is %q; the server refuses a spent reissue code with %q", got, passReissueSpent)
	}
	if is := withoutComments(jsFunction(t, script, "isReissueCode")); !strings.Contains(is,
		"return bare.startsWith(REISSUE_CODE_WORD) || bare.length === REISSUE_CODE_LENGTH;") {
		t.Error("isReissueCode no longer knows a reissue code by its word or its length")
	}

	// The box: never filled in by the browser, and long enough for a code
	// written out with its word and dashes.
	field := withoutComments(jsFunction(t, script, "passCodeFieldHTML"))
	box := regexp.MustCompile(`<input[^>]*\bid="f-pass-code"[^>]*>`).FindString(field)
	for _, want := range []string{`autocomplete="off"`, `autocapitalize="characters"`, `spellcheck="false"`, `type="text"`} {
		if !strings.Contains(box, want) {
			t.Errorf("the code box has lost %s: %s", want, box)
		}
	}
	if m := regexp.MustCompile(`maxlength="(\d+)"`).FindStringSubmatch(box); m == nil {
		t.Error("the code box has no maxlength")
	} else if n, _ := strconv.Atoi(m[1]); n < len(formatReissueCode(strings.Repeat("0", reissueCodeLen))) {
		t.Errorf("the code box takes %d characters, fewer than a reissue code written out", n)
	}

	// Sent once, in a claim's body, beside its own nonce, and stored nowhere.
	redeem := withoutComments(jsFunction(t, script, "redeemPassReissue"))
	if n := strings.Count(code, "reissue: typed"); n != 1 ||
		!strings.Contains(redeem, "body: JSON.stringify({ reissue: typed, nonce: passNonce(PASS_REISSUE_NONCE_STORAGE_KEY) }),") ||
		len(regexp.MustCompile(`\btyped\b`).FindAllString(redeem, -1)) != 2 {
		t.Error("the reissue code goes somewhere other than the body of one claim")
	}
	submit := withoutComments(jsFunction(t, script, "submitPass"))
	if !strings.Contains(submit, "if (isReissueCode(typed)) {") || strings.Count(submit, "redeemPassReissue(") != 1 ||
		strings.Count(code, "redeemPassReissue(") != 2 {
		t.Error("a reissue code is spent other than from the code box")
	}
	for name, body := range map[string]string{
		"redeemPassReissue": redeem,
		"submitPass":        submit,
		"forgetTypedCode":   withoutComments(jsFunction(t, script, "forgetTypedCode")),
		"drawPass":          withoutComments(jsFunction(t, script, "drawPass")),
	} {
		for _, banned := range []string{"storageSet(", "localStorage", "sessionStorage", "document.cookie", "indexedDB", "history."} {
			if strings.Contains(body, banned) {
				t.Errorf("%s uses %s; what is typed in the code box is kept in memory only", name, banned)
			}
		}
	}
	// What the box holds is kept only in passCodeTyped, which only the box's
	// drawing and forgetTypedCode touch.
	typed := regexp.MustCompile(`\bpassCodeTyped\b`)
	want := 1 + len(typed.FindAllString(withoutComments(jsFunction(t, script, "drawPass")), -1)) +
		len(typed.FindAllString(withoutComments(jsFunction(t, script, "forgetTypedCode")), -1))
	if n := len(typed.FindAllString(code, -1)); n != want {
		t.Errorf("passCodeTyped appears %d times; want its declaration and its uses in drawPass and forgetTypedCode (%d)", n, want-1)
	}
	// It goes once an answer arrives (none when educhan could not be reached
	// or failed), and with Remove the Pass, Forget this browser and leaving
	// the page.
	if !strings.Contains(redeem, "const answered = Boolean(res) || (!failed.unreachable && failed.status < 500);") ||
		!strings.Contains(redeem, "if (answered) {\n    forgetTypedCode();\n  }") || strings.Count(redeem, "forgetTypedCode();") != 1 {
		t.Error("redeemPassReissue keeps the code, or drops it, other than when an answer arrives")
	}
	// Emptying the box empties both what is kept of it and the box itself; a
	// drawing of the page reads the box into passCodeTyped first and puts it
	// back after, so a drawing does not lose it, and does not bring back what
	// forgetTypedCode emptied either.
	if forget := withoutComments(jsFunction(t, script, "forgetTypedCode")); !strings.Contains(forget, "passCodeTyped = \"\";") ||
		!strings.Contains(forget, `const box = document.getElementById("f-pass-code");`) ||
		!strings.Contains(forget, "if (box) {\n    box.value = \"\";\n  }") {
		t.Error("forgetTypedCode no longer empties both the code box and what is kept of it")
	}
	if draw := withoutComments(jsFunction(t, script, "drawPass")); !strings.Contains(draw, "if (box) {\n    passCodeTyped = box.value;\n  }") ||
		!strings.Contains(draw, "again.value = passCodeTyped;") ||
		strings.Index(draw, "passCodeTyped = box.value;") > strings.Index(draw, "el.main.innerHTML = passViewHTML(info);") ||
		strings.Index(draw, "again.value = passCodeTyped;") < strings.Index(draw, "el.main.innerHTML = passViewHTML(info);") {
		t.Error("drawPass no longer carries what is typed in the code box across a drawing")
	}
	remove := code[strings.Index(code, `if (event.target.closest('[data-action="forget-pass"]')) {`):]
	remove = remove[:strings.Index(remove, "\n  }\n")]
	render := withoutComments(jsFunction(t, script, "render"))
	leaving := render[strings.Index(render, `if (state.route.view === "pass" && route.view !== "pass") {`):]
	for name, body := range map[string]string{
		"Remove the Pass":      remove,
		"Forget this browser":  withoutComments(jsFunction(t, script, "forgetBrowser")),
		"leaving the page":     leaving[:strings.Index(leaving, "\n  }\n")],
		"leaving the document": withoutComments(jsBlock(t, script, `window.addEventListener("pagehide", () => {`, "});")),
	} {
		if !strings.Contains(body, "forgetTypedCode();") {
			t.Errorf("%s leaves what was typed in the code box", name)
		}
	}
	// Remove draws the page again, so the box is emptied before that drawing
	// can carry its text across.
	if strings.Index(remove, "forgetTypedCode();") > strings.Index(remove, "drawPass(") {
		t.Error("Remove the Pass draws the page again with the code box's text")
	}
}

// The code box takes a code however it was typed or pasted, and the page
// sends it as the server writes it: in capitals, without whatever stood
// between its groups (CODE_SEPARATORS, which
// TestPageAndServerSkipTheSameSeparators holds to the server's own set), with
// the letters the server reads as digits written as those digits after a
// reissue code's word (CODE_FOLDS, which TestPageAndServerFoldTheSameLetters
// holds to the server's reading), and grouped with dashes as formatPassCode
// and formatReissueCode group it. What reaches the server does not depend on
// which separators it skips or which letters it folds, and a code the page
// reads as one kind is sent as that kind.
func TestPassPageSendsCodesAsTheServerWritesThem(t *testing.T) {
	script := appScript(t)
	if !strings.Contains(script, "\nconst PASS_CODE_LENGTH = "+strconv.Itoa(passCodeLen)+";\n") {
		t.Errorf("PASS_CODE_LENGTH is not the server's %d", passCodeLen)
	}
	chars := withoutComments(jsFunction(t, script, "codeCharacters"))
	if !strings.Contains(chars, "for (const c of typed.toUpperCase()) {\n    if (!CODE_SEPARATORS.includes(c)) {\n      bare += c;") {
		t.Error("codeCharacters no longer reads a code in capitals with the separators left out")
	}
	passGroup := strings.Index(formatPassCode(strings.Repeat("0", passCodeLen)), "-")
	reissueGroup := strings.Index(strings.TrimPrefix(formatReissueCode(strings.Repeat("0", reissueCodeLen)), reissueCodePrefix+"-"), "-")
	if folded := withoutComments(jsFunction(t, script, "foldedCode")); !strings.Contains(folded,
		"for (const c of text) {\n    folded += CODE_FOLDS[c] ?? c;") {
		t.Error("foldedCode no longer writes each letter the server reads as a digit as that digit")
	}
	canonical := withoutComments(jsFunction(t, script, "canonicalCode"))
	for _, want := range []string{
		"const bare = codeCharacters(typed);",
		`const word = bare.startsWith(REISSUE_CODE_WORD) ? REISSUE_CODE_WORD : "";`,
		"const rest = foldedCode(bare.slice(word.length));",
		"if (rest.length === REISSUE_CODE_LENGTH) {\n    return [REISSUE_CODE_WORD, ...codeGroups(rest, " + strconv.Itoa(reissueGroup) + ")].join(\"-\");",
		"if (bare.length === PASS_CODE_LENGTH) {\n    return codeGroups(word + rest, " + strconv.Itoa(passGroup) + ").join(\"-\");",
		"return word + rest;",
	} {
		if !strings.Contains(canonical, want) {
			t.Errorf("canonicalCode no longer writes a code as the server does (%q)", want)
		}
	}
	if groups := withoutComments(jsFunction(t, script, "codeGroups")); !strings.Contains(groups,
		"for (let at = 0; at < text.length; at += size) {\n    groups.push(text.slice(at, at + size));") {
		t.Error("codeGroups no longer cuts a code into groups of the size given")
	}
	// Both requests a typed code goes in carry that form: what is read out of
	// the box is made canonical first, and nothing else reads the box.
	submit := withoutComments(jsFunction(t, script, "submitPass"))
	read := strings.Index(submit, `const typed = canonicalCode(String(field(form, "pass").value || ""));`)
	if read < 0 || strings.Index(submit, "if (isReissueCode(typed)) {") < read ||
		!strings.Contains(submit, "await redeemPassReissue(typed);") ||
		!strings.Contains(submit, `request = { path: "/api/pass/activate", body: { pass: typed } };`) {
		t.Error("submitPass sends a typed code other than as the server writes it")
	}
	code := withoutComments(script)
	if n := strings.Count(code, "canonicalCode("); n != 2 || strings.Count(code, `field(form, "pass")`) != 1 {
		t.Errorf("canonicalCode( appears %d times; want its definition and the one read of the code box, in submitPass", n)
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

// Forget this browser takes the poster key, the name, the badge and the Pass
// out of storage, but only the Pass, and what goes with it, is followed in
// the browser's other tabs (followPassStorage, followPassForget): a tab
// already open keeps a key it made (pagePosterKey), the badge
// (state.verification) and the name in a form it drew until it is reloaded,
// and a post from it writes the key and the name back to storage, for every
// page loaded after it. The Verify page and the FAQ's shared-computer advice
// say so, and to close those tabs first. A tab that followed the rest as well
// would make that advice say less than the page does, so this fails then too,
// for the wording to change with it.
func TestForgetThisBrowserSaysWhatOtherTabsKeep(t *testing.T) {
	script := appScript(t)
	listeners := regexp.MustCompile(`addEventListener\("storage", (\w+)\)`).FindAllStringSubmatch(withoutComments(script), -1)
	if len(listeners) != 2 || listeners[0][1] != "followPassStorage" || listeners[1][1] != "followPassForget" {
		t.Errorf("the page follows other tabs' storage with %v; the Verify page and the FAQ say only the Pass is followed", listeners)
	}
	for name, text := range map[string]string{
		"Forget this browser":  words(jsFunction(t, script, "forgetBrowserHTML")),
		"the FAQ on anonymity": words(faqAnswer(t, script, "faq-anonymity")),
		"the FAQ on badges":    words(faqAnswer(t, script, "faq-badge")),
	} {
		wants := []string{"close educhan's other tabs", "A tab left open can keep", "a post from it puts"}
		if name == "Forget this browser" {
			wants = []string{"Other educhan tabs open in this browser let go of the Pass too, but can keep the key, the name " +
				"and any badge they have, and a post from one of them puts the key and the name back, so close them first."}
		}
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s no longer says what a tab left open keeps after Forget this browser (%q)", name, want)
			}
		}
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
	// A renewal a reissue code brought is not said to be a payment from this
	// browser: the renewal it collected may have been paid for anywhere.
	if !strings.Contains(confirm, `async function confirmRenewal(renewed, reissued = false) {`) ||
		!strings.Contains(confirm, `const by = reissued ? "This reissue code" : "A payment from this browser";`) ||
		!strings.Contains(confirm, "const another = `${by} renewed a Pass until ${until}.`;") ||
		strings.Count(confirm, "A payment from this browser") != 1 {
		t.Error("confirmRenewal no longer says a renewal came from a reissue code when it did")
	}
	renewed := "`Your Pass is renewed until"
	asked := strings.Index(confirm, "const found = await recheckPass(held);")
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
	if n := strings.Count(withoutComments(script), "passJustIssued = null;"); n != 5 {
		t.Errorf("a code just handed over is dropped in %d places; want its declaration, leaving the Pass page, "+
			"removing the Pass, forgetting the browser, and another tab removing or replacing it", n)
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

// A reissue code typed in the code box is the reader's say-so, so it is
// spent at once, whatever this browser holds, and a code handed over in place
// of a different one shows that one beside it, to be written down. The box is
// on the page whether or not a Pass is held; with a good one held, a Pass
// code typed there is refused rather than put in its place unseen.
func TestPassReissueCodeReplacesAPassAndShowsIt(t *testing.T) {
	script := appScript(t)
	view := withoutComments(jsFunction(t, script, "passViewHTML"))
	field := regexp.MustCompile(`(?m)^  parts\.push\(passCodeFieldHTML\(held\)\);$`).FindStringIndex(view)
	if field == nil || strings.Contains(view[:field[0]], "if (!held || held.expired) {") {
		t.Error("passViewHTML no longer offers the code box whatever this browser holds")
	}
	submit := withoutComments(jsFunction(t, script, "submitPass"))
	reissue := strings.Index(submit, "if (isReissueCode(typed)) {")
	refused := strings.Index(submit, "if (held && !held.expired) {")
	if reissue < 0 || refused < reissue || strings.Contains(submit[reissue:refused], "held") {
		t.Error("a reissue code typed in a browser that holds a Pass is not spent at once")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "keepClaimed")), `replaced: kept && held ? held.code : "",`) ||
		!strings.Contains(withoutComments(jsFunction(t, script, "passIssuedHTML")), "${esc(issued.replaced)}") {
		t.Error("a code handed over in place of another no longer shows the one it replaced")
	}
	if !strings.Contains(words(jsFunction(t, script, "passIssuedHTML")), "If it was for the Pass this reissue code was for, it no longer works.") {
		t.Error("the panel of a reissued code no longer says the code it replaced may have stopped working")
	}
}

// Every claim carries the collection's nonce, which the page keeps until an
// answer says nothing more is waiting, so a claim whose answer was lost is
// answered again with the same code. A reissue code's nonce is kept until an
// answer says the code issued or renewed a Pass, or until Remove the Pass or
// Forget this browser, and never dropped on a refusal, since an earlier try
// may have reached the server. A shared computer can drop the claim:
// forgetting the browser, removing the Pass and the collecting panels all ask
// the server to, since the claim is a cookie the page cannot see, after the
// page has dropped the nonces (Stop collecting it here only the collection's)
// and stopped collecting, and nothing collects again until the server has the
// request. The nonces, like the codes they are spent for, go nowhere but the
// bodies of the claims.
func TestPassClaimsCarryANonceAndCanBeDropped(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	collect := withoutComments(jsFunction(t, script, "collectPass"))
	if !strings.Contains(collect, "body: JSON.stringify({ nonce: passNonce(PASS_NONCE_STORAGE_KEY) }),") {
		t.Error("collectPass no longer sends the collection's nonce")
	}
	// Dropped once nothing more is waiting, or the claim is refused, and kept
	// when educhan could not be reached or failed, so the claim is asked
	// again with it.
	refused := regexp.MustCompile(`\} else if \(failed\.status === 400 \|\| failed\.status === 403\) \{\s*dropPassNonce\(PASS_NONCE_STORAGE_KEY\);`)
	if n := strings.Count(collect, "dropPassNonce(PASS_NONCE_STORAGE_KEY);"); n != 2 ||
		!strings.Contains(collect, "if (res.claim !== true) {\n        dropPassNonce(PASS_NONCE_STORAGE_KEY);") ||
		!refused.MatchString(collect) {
		t.Error("collectPass drops the nonce other than once nothing more is waiting, or the claim is refused")
	}
	// A collection stopped (forgetPassClaim) or replaced sends no claim and
	// draws no nonce, and none is sent while the server is still to drop the
	// claim: both checks come first in every turn of the loop.
	stopped := strings.Index(collect, "for (;;) {\n    if (passForgetting) {\n      await passForgetting;\n    }\n"+
		"    if (visit !== passVisit || !passPageShown()) {\n      return;\n    }")
	if stopped < 0 || strings.Index(collect, "passNonce(") < stopped {
		t.Error("collectPass can send a claim, and draw a nonce, after collecting has stopped or while the claim is being dropped")
	}
	reissue := withoutComments(jsFunction(t, script, "redeemPassReissue"))
	if !strings.Contains(reissue, "body: JSON.stringify({ reissue: typed, nonce: passNonce(PASS_REISSUE_NONCE_STORAGE_KEY) }),") {
		t.Error("redeemPassReissue no longer sends the reissue code's own nonce")
	}
	// Kept when educhan could not be reached or failed, so pressing again
	// asks with the same nonce and is handed the same code, and kept after a
	// refusal too, so the right code typed after a mistyped retry still asks
	// with the nonce of a try whose answer was lost: dropped only once an
	// answer says the code issued or renewed a Pass.
	if strings.Count(reissue, "dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);") != 1 ||
		!strings.Contains(reissue, "if (res && (res.status === \"issued\" || res.status === \"renewed\")) {\n    dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);\n") {
		t.Error("redeemPassReissue drops its nonce on a refusal, or on a failure that brought no answer, or keeps it once a Pass is issued or renewed")
	}
	// Whether a try with that nonce may have gone unanswered is marked beside
	// it: read before each try, so it says only what earlier tries left, and
	// put there before the claim is sent, so a closed page leaves it too. An
	// answer takes off only the mark its own try put there, and the mark goes
	// with the nonce: a lost answer, a mistyped retry and then the right code
	// are still a retry after a lost answer, and two refusals with nothing
	// lost between them are only refusals.
	againAt := strings.Index(reissue, "const again = passNoncePending(PASS_REISSUE_UNANSWERED_STORAGE_KEY);")
	markAt := strings.Index(reissue, "\n  passNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);")
	askAt := strings.Index(reissue, `await api("/api/pass/claim"`)
	if againAt < 0 || markAt < againAt || askAt < markAt ||
		!strings.Contains(reissue, "dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);\n    dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);\n"+
			"  } else if (answered && !again) {\n    dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);\n  }") ||
		strings.Count(reissue, "dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);") != 2 || strings.Count(reissue, "passNoncePending(") != 1 {
		t.Error("redeemPassReissue no longer takes a refusal for a retry after a lost answer only when a try went unanswered")
	}
	if !strings.Contains(code, `const PASS_REISSUE_UNANSWERED_STORAGE_KEY = "educhan:pass-reissue-unanswered";`) {
		t.Error("the mark of an unanswered reissue try is no longer kept under its own key")
	}
	if n := len(regexp.MustCompile(`\bpassNonce\(`).FindAllString(code, -1)); n != 4 {
		t.Errorf("passNonce( appears %d times; want its definition, the bodies of a claim and a reissue, and the reissue's mark of a try unanswered", n)
	}
	// The keys are spelled out once, and used only by the two requests that
	// send a nonce, renderPass's look for one kept from an unanswered claim,
	// and forgetting, here or in another tab; passNonces only by the helpers.
	nonceUsers := map[string][]string{
		"PASS_NONCE_STORAGE_KEY":              {"renderPass", "collectPass", "forgetPassClaim", "followPassForget"},
		"PASS_REISSUE_NONCE_STORAGE_KEY":      {"redeemPassReissue", "forgetPassClaim", "followPassForget"},
		"PASS_REISSUE_UNANSWERED_STORAGE_KEY": {"redeemPassReissue", "forgetPassClaim", "followPassForget"},
		"passNonces":                          {"passNonce", "passNoncePending", "dropPassNonce"},
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
	justReads := 0
	for _, name := range []string{"keepClaimed", "followPassStorage"} {
		justReads += strings.Count(withoutComments(jsFunction(t, script, name)), "passJustIssued.code")
	}
	if n := strings.Count(code, "passJustIssued.code"); n != justReads {
		t.Errorf("passJustIssued.code is read %d times, %d of them in keepClaimed and followPassStorage; want none elsewhere", n, justReads)
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
	for _, first := range []string{"passVisit += 1;", "passForgets += 1;", "dropPassNonce(PASS_NONCE_STORAGE_KEY);",
		"if (emptyBoxes) {\n    passCodeForgets += 1;\n    dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);\n" +
			"    dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);\n  }"} {
		if at := strings.Index(forget, first); at < 0 || at > asked {
			t.Errorf("forgetPassClaim no longer does %q before it asks the server", first)
		}
	}
	// It says nothing is left to collect here only once the server has the
	// request, or sells no Pass: unreachable, the claim is still this
	// browser's, and the page has to say so.
	answered := regexp.MustCompile(`await api\("/api/pass/claim", \{ method: "POST", body: JSON\.stringify\(\{ forget: true \}\) \}\);\s*` +
		`return true;\s*\} catch \(err\) \{\s*return err\.status === 403;\s*\}`)
	if !answered.MatchString(forget) {
		t.Error("forgetPassClaim says the claim was dropped when the server never had the request")
	}
	// Until then nothing collects again: renderPass waits on the request
	// before it asks about the claim, as collectPass does before each claim,
	// and Stop collecting it here takes Check now off with it. The mark comes
	// off once the request is answered, and what the click goes on with
	// waits for every other forget on its way too.
	if !strings.Contains(forget, "waitOnPassForget(\n    mark,\n    emptyBoxes,\n    request.then(() => {\n      try {\n"+
		"        localStorage.removeItem(mark);") ||
		!strings.Contains(forget, "return passForgetting.then(() => request);\n}") {
		t.Error("forgetPassClaim no longer leaves the request it sent for the page to wait on")
	}
	// The page waits on every forget on its way, this tab's and other tabs',
	// each under its own mark, so none takes another's place: a forget sent
	// while an earlier one is still on its way (Remove the Pass pressed before
	// Stop's is answered, here or in another tab) leaves the page waiting on
	// both, whichever answer comes first, or a checkout started once Remove's
	// was answered could have its claim dropped by Stop's, arriving after it.
	// Each is taken off before anything that waits on it goes on, so a wait
	// that ends sees only the forgets still on their way.
	wait := withoutComments(jsFunction(t, script, "waitOnPassForget"))
	if !strings.Contains(wait, "const wait = { code };\n  wait.done = over.then(() => {\n    if (passForgetWaits.get(key) === wait) {\n"+
		"      passForgetWaits.delete(key);\n      settlePassForgetting();\n    }\n  });\n  passForgetWaits.set(key, wait);\n  settlePassForgetting();") {
		t.Error("waitOnPassForget no longer keeps every forget on its way for the page to wait on, or lets a wait go on before it is taken off")
	}
	made := withoutComments(jsFunction(t, script, "settlePassForgetting"))
	if !strings.Contains(made, "for (const wait of passForgetWaits.values()) {\n    all.push(wait.done);\n    if (wait.code) {\n      codes.push(wait.done);") ||
		!strings.Contains(made, "passForgetting = all.length ? Promise.all(all) : null;\n  passCodeForgetting = codes.length ? Promise.all(codes) : null;") {
		t.Error("passForgetting no longer waits for every forget on its way, or passCodeForgetting for every one that takes the Pass")
	}
	for name, n := range map[string]int{"passForgetting = ": 2, "passCodeForgetting = ": 2, "passForgetWaits.set(": 1, "waitOnPassForget(": 3} {
		if got := strings.Count(code, name); got != n {
			t.Errorf("app.js does %q %d times; want %d: declared, then set only by settlePassForgetting, "+
				"for waitOnPassForget, which forgetPassClaim and heedPassForget call", name, got, n)
		}
	}
	render := withoutComments(jsFunction(t, script, "renderPass"))
	waited := strings.Index(render, "if (passForgetting) {\n    await passForgetting;\n  }")
	if loaded := strings.Index(render, "await loadPassInfo();"); waited < 0 || loaded < waited {
		t.Error("renderPass asks about the claim while it is still being dropped, and can collect it again")
	}
	// An answer already on its way when this browser stopped collecting is
	// dropped, not stored or drawn: the count of forgets is read before each
	// claim is sent, and compared once the answer is in, before anything is
	// kept, and again after every wait on the Pass held (a renewal checked,
	// or a lost answer's), before anything is drawn.
	for name, counter := range map[string]string{"collectPass": "passForgets", "redeemPassReissue": "passCodeForgets"} {
		body := withoutComments(jsFunction(t, script, name))
		read := strings.Index(body, "const forgets = "+counter+";")
		asked := strings.Index(body, `await api("/api/pass/claim"`)
		caught := strings.Index(body, "failed = err;")
		kept := strings.Index(body, "keepClaimed(res")
		checks := passForgottenCheck.FindAllStringIndex(body, -1)
		draws := regexp.MustCompile(`\b(showClaimed|drawPass)\(`).FindAllStringIndex(body, -1)
		waits := regexp.MustCompile(`await (confirmRenewal|passAfterLostAnswer)\(`).FindAllStringIndex(body, -1)
		ok := read >= 0 && read < asked && len(checks) > 0 && len(draws) > 0 && len(waits) == 2 &&
			checks[0][0] > caught && checks[0][0] < kept && checks[0][0] < draws[0][0]
		for _, wait := range waits {
			next := len(body)
			for _, draw := range draws {
				if draw[0] > wait[0] {
					next = draw[0]
					break
				}
			}
			checked := false
			for _, check := range checks {
				checked = checked || (check[0] > wait[0] && check[0] < next)
			}
			ok = ok && checked
		}
		if !ok {
			t.Errorf("%s can store or draw an answer that arrived after this browser stopped collecting", name)
		}
	}
	for counter, users := range map[string][]string{
		"passForgets":        {"forgetPassClaim", "collectPass", "followPassForget", "redeemPassReissue", "submitPass", "passForgetsSettled"},
		"passCodeForgets":    {"forgetPassClaim", "followPassForget", "passForgotten", "recheckPass", "redeemPassReissue", "checkHeldPass", "submitPass", "passForgetsSettled"},
		"passCodeForgetting": {"settlePassForgetting", "passForgotten"},
	} {
		uses := 1
		for _, name := range users {
			uses += len(regexp.MustCompile(`\b`+counter+`\b`).FindAllString(withoutComments(jsFunction(t, script, name)), -1))
		}
		if n := len(regexp.MustCompile(`\b`+counter+`\b`).FindAllString(code, -1)); n != uses {
			t.Errorf("%s appears %d times; want its declaration and its uses in %s, %d in all", counter, n, strings.Join(users, ", "), uses)
		}
	}
	// A reissue answer lands through Stop collecting it here, but what it says
	// about the claim was true before Stop dropped it, so it is not drawn back
	// once this browser has stopped collecting meanwhile.
	redeem := withoutComments(jsFunction(t, script, "redeemPassReissue"))
	if read, asked := strings.Index(redeem, "const claims = passForgets;"), strings.Index(redeem, `await api("/api/pass/claim"`); read < 0 || read > asked ||
		!strings.Contains(redeem, "drawPass(res && claims === passForgets ? { ...lastPassInfo, claim: res.claim === true } : lastPassInfo);") {
		t.Error("a reissue answer that arrives after Stop collecting it here draws the claim Stop dropped back on the page")
	}
	// Remove the Pass and Forget this browser have the browser's other tabs
	// empty their code boxes too (followPassForget); Stop collecting it here
	// leaves them.
	if !strings.Contains(withoutComments(jsFunction(t, script, "forgetBrowser")), "return forgetPassClaim(true);") {
		t.Error("Forget this browser no longer drops a purchase still being collected, here and in the browser's other tabs")
	}
	remove := code[strings.Index(code, `if (event.target.closest('[data-action="forget-pass"]')) {`):]
	remove = remove[:strings.Index(remove, "\n  }\n")]
	if !strings.Contains(remove, "const dropped = forgetPassClaim(true);") {
		t.Error("Remove the Pass no longer drops a purchase still being collected, here and in the browser's other tabs")
	}
	// Drawn at once, from what is already known, so the code is off the screen
	// before the visitor walks away, and focus goes to what took its place.
	if !strings.Contains(remove, "drawPass({ ...lastPassInfo, claim: false });") || strings.Contains(remove, "renderPass(") ||
		!strings.Contains(remove, "passFocus(") {
		t.Error("Remove the Pass no longer draws the page again at once, with focus on what took its place")
	}
	// Every panel drawn while something is collected here offers to stop it,
	// the forms drawn again over a checkout left (Start again, or back from
	// Stripe's cancel) included.
	for _, name := range []string{"passCollectingHTML", "passOpenCheckoutHTML", "passEarlierCheckoutHTML"} {
		if !strings.Contains(withoutComments(jsFunction(t, script, name)), "${passStopCollectingHTML()}") {
			t.Errorf("%s no longer offers to stop collecting here", name)
		}
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "passViewHTML")), "if (info.claim === true && passStartedAgain) {\n    parts.push(passEarlierCheckoutHTML());") {
		t.Error("the forms drawn over a checkout left behind no longer offer to stop collecting it")
	}
	stop := code[strings.Index(code, `const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');`):]
	stop = stop[:strings.Index(stop, "\n    return;\n  }")]
	disabled := strings.Index(stop, "const waiting = [stopClaim, ...el.main.querySelectorAll('[data-action=\"collect-pass\"]')];\n"+
		"    for (const button of waiting) {\n      button.disabled = true;\n    }")
	dropped := strings.Index(stop, "const dropped = forgetPassClaim();")
	if dropped < 0 || !strings.Contains(stop, "dropped.then((done) => {") {
		t.Error("Stop collecting it here no longer drops the claim")
	}
	if disabled < 0 || dropped < disabled {
		t.Error("Stop collecting it here leaves Check now to collect what it is dropping")
	}
	// A drawing made while the claim is being dropped, here or in another
	// tab, by a reissue answer say, keeps Check now and Stop off as Stop's own
	// click left them, and the other tab takes off those already drawn; Stop's
	// answer gives back those drawn meanwhile with its own.
	collectButtons := `const PASS_COLLECT_BUTTONS = '[data-action="collect-pass"], [data-action="stop-pass-claim"]';`
	offWhileStopping := "if (passForgetting) {\n    for (const button of el.main.querySelectorAll(PASS_COLLECT_BUTTONS)) {\n      button.disabled = true;\n    }\n  }"
	draw := withoutComments(jsFunction(t, script, "drawPass"))
	followStop := withoutComments(jsFunction(t, script, "followPassForget"))
	if !strings.Contains(code, collectButtons) || strings.Index(draw, offWhileStopping) < strings.Index(draw, "el.main.innerHTML = passViewHTML(info);") ||
		!strings.Contains(followStop, "for (const button of el.main.querySelectorAll(PASS_COLLECT_BUTTONS)) {\n    button.disabled = true;\n  }") ||
		strings.Index(followStop, "button.disabled = true;") < strings.Index(followStop, "passForgetting = waited;") ||
		!strings.Contains(stop, "for (const button of [...waiting, ...el.main.querySelectorAll(PASS_COLLECT_BUTTONS)]) {\n        button.disabled = false;") {
		t.Error("a Pass page drawn while this browser is stopping collecting offers Check now and Stop again")
	}
	// Turned away, the claim is still this browser's, and the page goes on
	// collecting it, as the notice and the panel say. The collection it goes
	// on with is the one forgetPassClaim has just begun by ending the last, so
	// the visit is read after it: read before, it is never the page's, and
	// nothing collects again.
	if visit := strings.Index(stop, "const visit = passVisit;"); visit < 0 || visit < dropped || strings.Count(stop, "passVisit") != 2 {
		t.Error("a Stop collecting it here that could not reach educhan reads the visit before forgetPassClaim ends it, and never collects again")
	}
	failed := regexp.MustCompile(`if \(!done && visit === passVisit\) \{\s*collectPass\(visit\);\s*\}`)
	said := regexp.MustCompile(`if \(!done\) \{\s*refocus\(stopClaim\);\s*showNotice\("Couldn't reach educhan, so this browser is still collecting it\.[^"]*"\);\s*return;\s*\}`)
	if !failed.MatchString(stop) || !said.MatchString(stop) {
		t.Error("a Stop collecting it here that could not reach educhan no longer goes on collecting, as it says")
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

// A claim or a reissue code asked again with a nonce kept from a try whose
// answer never arrived may have renewed the Pass held, in that lost answer,
// and a renewal hands over no code, so the server refuses the retry. The page
// then checks the held code, as after a renewal it collects, and says a date
// that moved on in words that claim nothing about where the time came from;
// a reissue code refused then is not said to need replacing. A check turned
// away by the per-address cooldown a campus shares is asked once more, after
// the wait the server names.
func TestPassLostRenewalAnswerIsCheckedOnThePassHeld(t *testing.T) {
	script := appScript(t)
	for name, refused := range map[string]string{
		"collectPass":       "} else if (failed.status === 400 || failed.status === 403) {",
		"redeemPassReissue": "} else if (failed && again && (failed.status === 400 || failed.status === 403)) {",
	} {
		body := withoutComments(jsFunction(t, script, name))
		again := strings.Index(body, "const again = passNoncePending(")
		asked := strings.Index(body, `await api("/api/pass/claim"`)
		at := strings.Index(body, refused)
		checked := strings.Index(body, "await passAfterLostAnswer(")
		if again < 0 || asked < again || at < asked || checked < at {
			t.Errorf("%s no longer checks the Pass held when a retry with a kept nonce is refused", name)
		}
	}
	collect := withoutComments(jsFunction(t, script, "collectPass"))
	if !strings.Contains(collect, `const notice = again ? await passAfterLostAnswer() : "";`) {
		t.Error("collectPass checks the Pass held after a refusal it has no reason to doubt")
	}
	// What a claim brought, and what a check after a lost answer found, is
	// said even when the page was drawn again while it was awaited (Check
	// now, say), which would otherwise drop the notice and leave the old date
	// on screen; an answer with nothing to say is left to the newer drawing.
	for _, shown := range []string{"if (visit === passVisit || news) {", "if (visit === passVisit || notice) {"} {
		if strings.Count(collect, shown) != 1 {
			t.Errorf("collectPass no longer shows what an answer brought once the page has been drawn again (%q)", shown)
		}
	}
	after := withoutComments(jsFunction(t, script, "passAfterLostAnswer"))
	if !strings.Contains(after, "const found = await recheckPass(held);") ||
		!strings.Contains(after, "if (found.state === \"dated\" && !(held.expires && held.expires >= new Date(found.expires))) {\n"+
			"    return `Your Pass is now good until ${passUntilText(found.expires)}.`;") {
		t.Error("passAfterLostAnswer no longer stores and says a date the Pass held moved on to")
	}
	for _, claim := range []string{"renew", "pay", "reissue", "bought"} {
		if strings.Contains(strings.ToLower(after), claim) {
			t.Errorf("passAfterLostAnswer says %q, which the page cannot know", claim)
		}
	}
	// Kept from a try with no answer, or from one refused, a nonce says only
	// that an earlier try did not issue or renew a Pass: it may have reached
	// the server and renewed one, or not, and this code may be mistyped. The
	// notice says both, and neither that it was used nor that it needs
	// replacing.
	reissue := words(withoutComments(jsFunction(t, script, "redeemPassReissue")))
	retried := `if (failed.message === PASS_REISSUE_SPENT && again) { showNotice( "This reissue code didn't work this time. ` +
		`If an earlier try reached educhan, it may have renewed your Pass already; otherwise check it against the one you were sent: ` +
		`it may be mistyped, used, run out or cancelled." );`
	if !strings.Contains(reissue, retried) {
		t.Error("a reissue code refused when asked again is said to need replacing, though it may have renewed the Pass, " +
			"or to have been used, though it may be mistyped")
	}
	// A Pass held that no longer works, once the retry is refused, is what an
	// earlier try that gave it a new code leaves, and the right reissue code
	// asked again with the kept nonce hands that code over: the reader is told
	// to enter it again, not only that the Pass has gone.
	removed := `notice = await passAfterLostAnswer( "The Pass that was in this browser no longer works, so it has been removed. ` +
		`An earlier try may have reached educhan and given it a new code: enter the reissue code again, checked against the one you were sent, ` +
		`to get that code. Otherwise the reissue code may be mistyped, used, run out or cancelled." );`
	if !strings.Contains(reissue, removed) || !strings.Contains(after, "if (found.state === \"invalid\") {\n    return gone;\n  }") ||
		!strings.Contains(script, "\nasync function passAfterLostAnswer(gone = \"The Pass that was in this browser is no longer valid, so it has been removed.\") {") {
		t.Error("a reissue code refused when asked again, with the Pass held found gone, no longer says to enter the code again")
	}
	recheck := withoutComments(jsFunction(t, script, "recheckPass"))
	if !strings.Contains(recheck, "for (let attempt = 0; attempt < 2 && !res; attempt += 1) {") ||
		!strings.Contains(recheck, "const seconds = err.status === 429 && err.payload && err.payload.retry_after;") ||
		!strings.Contains(recheck, "await new Promise((resolve) => setTimeout(resolve, Math.min(seconds, 5) * 1000));") {
		t.Error("a Pass checked after a renewal or a lost answer is no longer asked again once, after a 429's wait")
	}
}

// Copy the code puts the Pass on the clipboard, where it would outlive Remove
// the Pass and Forget this browser on a shared computer. Both empty it, inside
// their click, since writing to the clipboard takes the reader's gesture, when
// this tab copied the code since it was loaded, nothing has been copied or cut
// in this tab since, and the browser still holds it. The page cannot read the
// clipboard without a prompt, so what it empties is whatever is on it by
// then, and the page says so, and what it cannot reach.
func TestPassCopiedHereIsEmptiedFromTheClipboard(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	copied := code[strings.Index(code, `if (event.target.closest('[data-action="copy-pass"]')) {`):]
	copied = copied[:strings.Index(copied, "\n    return;\n  }")]
	// Noted once the copy has succeeded, and only then: a refused copy leaves
	// the reader's own clipboard, which is not the page's to empty. What is
	// noted is the code copied, so a tab that removes or replaces the Pass
	// can be told apart from one that renews it (followPassStorage).
	if !regexp.MustCompile(`const copying = code\.textContent\.trim\(\);\s*navigator\.clipboard\.writeText\(copying\)\.then\(\s*\(\) => \{\s*passCodeCopied = copying;`).MatchString(copied) ||
		strings.Count(code, "passCodeCopied = copying;") != 1 {
		t.Error("Copy the code no longer notes the code this tab put on the clipboard, or notes it when it did not")
	}
	// Set only there, and otherwise only ever cleared: by emptying the
	// clipboard, by anything copied or cut in this tab since, and when the
	// browser no longer holds the code copied.
	if n := len(regexp.MustCompile(`\bpassCodeCopied = `).FindAllString(code, -1)); n != 5 ||
		!strings.Contains(code, "\nlet passCodeCopied = \"\";\n") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "followPassStorage")), "if (!now || now.code !== passCodeCopied) {\n    passCodeCopied = \"\";\n  }") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "forgetCopiedPassCode")),
			"if (selected && (event.type === \"copy\" || editable)) {\n    passCodeCopied = \"\";\n  }") {
		t.Errorf("passCodeCopied is set in %d places; want its declaration, Copy the code, emptying the clipboard, "+
			"a copy or cut in this tab, and another tab's removal or replacement of the code copied", n)
	}
	// Anything copied or cut in this tab after the code takes its place on the
	// clipboard, so Remove and Forget leave it there. Copy the code's own
	// write sets off neither event.
	for _, kind := range []string{"copy", "cut"} {
		if !strings.Contains(code, "\ndocument.addEventListener(\""+kind+"\", forgetCopiedPassCode);\n") {
			t.Errorf("a %s in this tab no longer stops Remove and Forget emptying what it put on the clipboard", kind)
		}
	}
	// But only one that certainly put something there: a copy or a cut with
	// nothing selected, or a cut that removes nothing, sets the event off and
	// leaves the clipboard as it was, the code included, and one that may have
	// is left uncounted, so Remove and Forget err toward emptying it. A copy
	// counts when the selection holds something: a text box's own, where it
	// can be read; the document's, for an email or number box, which does not
	// say; and anywhere else (a checkbox, the file picker, a button or the
	// page) the document's selection's own text. Chromium goes on reporting a
	// text box's text as the document's selection once the focus has left the
	// box, in a collapsed range that holds none, so a Ctrl+C on a button after
	// a click in the code box copies nothing and must not count. A cut counts
	// only in a box that can be typed in, neither read-only nor disabled, or in
	// editable content.
	taken := words(withoutComments(jsFunction(t, script, "forgetCopiedPassCode")))
	if !strings.Contains(taken, "function forgetCopiedPassCode(event) { const target = event.target; "+
		"const box = target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement ? target : null; "+
		"const readable = box !== null && box.selectionStart !== null; "+
		`const typed = readable || (box !== null && (box.type === "email" || box.type === "number")); `+
		"const selection = document.getSelection(); "+
		"const range = selection && selection.rangeCount ? selection.getRangeAt(0) : null; "+
		"const selected = readable ? box.selectionStart !== box.selectionEnd "+
		`: typed ? String(selection || "") !== "" : String(range || "") !== ""; `+
		"const editable = box ? typed && !box.readOnly && !box.disabled "+
		": target instanceof HTMLElement && target.isContentEditable; "+
		`if (selected && (event.type === "copy" || editable)) {`) {
		t.Error("a copy or a cut that may have put nothing on the clipboard counts as one that took the code's place there")
	}
	empty := withoutComments(jsFunction(t, script, "emptyCopiedPassCode"))
	if !strings.Contains(empty, "if (!passCodeCopied) {\n    return;\n  }\n  passCodeCopied = \"\";") ||
		!strings.Contains(empty, `navigator.clipboard.writeText("").catch(() => {});`) {
		t.Error("emptyCopiedPassCode no longer empties the clipboard of a code this tab copied, and only then")
	}
	// Called in the click itself, before anything is awaited and outside any
	// callback: a clipboard write after the forget's answer, in a .then, has
	// lost the reader's gesture, which Safari and Firefox require. The handler
	// for Remove is not async, and forgetBrowser is called first thing in
	// Forget's and returns the forget's promise without waiting on it.
	remove := code[strings.Index(code, `if (event.target.closest('[data-action="forget-pass"]')) {`):]
	remove = remove[:strings.Index(remove, "\n  }\n")]
	emptied := strings.Index(remove, "emptyCopiedPassCode();")
	if emptied < 0 || strings.Contains(remove, "await ") || emptied > strings.Index(remove, "const dropped = forgetPassClaim(true);") ||
		emptied > strings.Index(remove, ".then(") || strings.Count(remove, "emptyCopiedPassCode") != 1 {
		t.Error("Remove the Pass no longer empties the clipboard of a code copied here while the click allows it")
	}
	browser := withoutComments(jsFunction(t, script, "forgetBrowser"))
	emptied = strings.Index(browser, "emptyCopiedPassCode();")
	forgetClick := code[strings.Index(code, `if (event.target.closest('[data-action="forget-browser"]')) {`):]
	forgetClick = forgetClick[:strings.Index(forgetClick, "\n  }\n")]
	if !strings.Contains(script, "\nfunction forgetBrowser() {") || emptied < 0 ||
		emptied > strings.Index(browser, "return forgetPassClaim(true);") ||
		strings.Contains(browser, "await ") || strings.Contains(browser, ".then(") ||
		!regexp.MustCompile(`^if \(event\.target\.closest\('\[data-action="forget-browser"\]'\)\) \{\s*event\.preventDefault\(\);\s*const dropped = forgetBrowser\(\);`).MatchString(forgetClick) {
		t.Error("Forget this browser no longer empties the clipboard of a code copied here while the click allows it")
	}
	if n := strings.Count(code, "emptyCopiedPassCode()"); n != 3 {
		t.Errorf("emptyCopiedPassCode() appears %d times; want its definition, Remove the Pass and Forget this browser", n)
	}
	// The promise is as narrow as the flag, and says what it costs: the
	// button named, a code it copied since educhan was last opened in this
	// tab, with nothing copied in the tab since, and not one another tab has
	// removed or replaced since; whatever is on the clipboard by then is
	// emptied, a copy the page cannot see took anything included, and a code
	// copied by hand is not.
	for name, text := range map[string]string{
		"the held Pass":       words(jsFunction(t, script, "passHeldHTML")),
		"Forget this browser": words(jsFunction(t, script, "forgetBrowserHTML")),
		"the FAQ":             words(faqAnswer(t, script, "faq-pass")),
	} {
		for _, want := range []string{"Copy the code", "since you last opened educhan in", "copied nothing in",
			"whatever is on it by then", "unless another tab removed or replaced the Pass meanwhile", "even after a copy in",
			"that educhan can't see took anything, such as one from an email box in some browsers",
			"copied any other way, by hand included, stays"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s no longer says which copies of the code are emptied from the clipboard (%q)", name, want)
			}
		}
	}
}

// Remove the Pass or Forget this browser in one tab must not leave a code
// just handed over on another tab's screen. The storage event tells every
// other tab of the browser; each drops the code it was showing once the
// browser no longer holds it, and draws the Pass page again if it is open.
func TestPassRemovedInAnotherTabLeavesThisOne(t *testing.T) {
	script := appScript(t)
	if !strings.Contains(withoutComments(script), "\nwindow.addEventListener(\"storage\", followPassStorage);\n") {
		t.Error("a tab no longer follows the Pass another tab removed or replaced")
	}
	follow := withoutComments(jsFunction(t, script, "followPassStorage"))
	if !strings.Contains(follow, "if (!now || (passJustIssued && passJustIssued.code !== now.code)) {\n    passJustIssued = null;\n  }") ||
		!strings.Contains(follow, `if (passPageShown() && el.main.querySelector("[data-pass-page]")) {`+
			"\n    redrawPass(lastPassInfo, \"#pass-held\", \"form[data-pass] input\");") {
		t.Error("a code just handed over stays on screen after another tab removed or replaced the Pass")
	}
	// A code this tab copied stops counting as its to empty from the
	// clipboard once the browser no longer holds that code, whether or not
	// a code just handed over is on screen.
	if !strings.Contains(follow, "if (!now || now.code !== passCodeCopied) {\n    passCodeCopied = \"\";\n  }") {
		t.Error("a code this tab copied still counts as its to empty after another tab removed or replaced it")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "passViewHTML")), "<div class=\"board-head\" data-pass-page>") {
		t.Error("the Pass page no longer marks itself, so another tab's change can draw over the age screen")
	}
}

// Remove the Pass, Forget this browser and Stop collecting it here, pressed
// in one tab, stop the browser's other tabs as they stop that one: the
// storage event carries a mark the pressing tab keeps while its request is on
// the way, whether or not a Pass was held. Another tab then drops an answer
// to a claim already on its way and sends no claim until the mark goes. For
// Remove and Forget it also drops a reissue code's nonce and an answer about
// a Pass on its way, and empties its code box, so a reissue code typed there
// is not left for the next person at the computer, even when the Pass's own
// removal drew that tab again first. Stop leaves a reissue code alone, here
// and in other tabs: it is about a checkout, and a reissue code it dropped
// would be spent with nothing to show for it.
func TestPassForgottenInAnotherTabStopsThisOne(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if !strings.Contains(code, "\nwindow.addEventListener(\"storage\", followPassForget);\n") {
		t.Error("a tab no longer follows a forget pressed in another tab")
	}
	if n := strings.Count(code, `"educhan:pass-forget"`); n != 1 || !strings.Contains(code, `const PASS_FORGET_STORAGE_KEY = "educhan:pass-forget";`) {
		t.Errorf("app.js spells out the forget mark's key %d times; want once, as PASS_FORGET_STORAGE_KEY", n)
	}
	// The mark is set before the request is sent, under a key of its own (the
	// prefix and a random token), carries whether to empty the code box and
	// when it was set, and is taken off once the request is answered: marks
	// never take each other's place, so every tab hears each one go.
	forget := withoutComments(jsFunction(t, script, "forgetPassClaim"))
	marked := strings.Index(forget, "storageSet(mark, `${emptyBoxes ? \"code\" : \"claim\"}:${Date.now()}`);")
	asked := strings.Index(forget, `await api("/api/pass/claim"`)
	if !strings.Contains(forget, "const mark = `${PASS_FORGET_STORAGE_KEY}:${newPassNonce()}`;") || marked < 0 || marked > asked ||
		!strings.Contains(forget, "request.then(() => {\n      try {\n        localStorage.removeItem(mark);") ||
		strings.Count(forget, "removeItem(") != 1 {
		t.Error("forgetPassClaim no longer tells the browser's other tabs, before it asks the server, that it is forgetting")
	}
	if !strings.Contains(code, "\nfunction forgetPassClaim(emptyBoxes = false) {") || strings.Count(code, "forgetPassClaim(true)") != 2 ||
		strings.Count(code, "forgetPassClaim()") != 1 {
		t.Error("only Remove the Pass and Forget this browser have other tabs empty their code boxes")
	}
	follow := withoutComments(jsFunction(t, script, "followPassForget"))
	gate := strings.Index(follow, "const key = event.key;\n  if (!key || !key.startsWith(`${PASS_FORGET_STORAGE_KEY}:`)) {\n    return;\n  }")
	read := strings.Index(follow, "const mark = event.newValue;")
	if gate != strings.Index(follow, "{")+4 || read < gate || strings.Count(follow, "event.") != 2 {
		t.Error("followPassForget reads a storage event other than a forget mark's")
	}
	// A mark that goes ends the wait on it, and one this tab already waits on
	// (found when it was loaded) is not stopped for twice.
	stop := strings.Index(follow, "if (!mark) {\n    const heard = passForgetHeard.get(key);\n    if (heard) {\n      heard();\n    }\n    return;\n  }\n"+
		"  if (passForgetWaits.has(key)) {\n    return;\n  }")
	for _, step := range []string{"const code = mark.startsWith(\"code:\");", "passVisit += 1;", "passForgets += 1;",
		"dropPassNonce(PASS_NONCE_STORAGE_KEY);",
		"if (code) {\n    passCodeForgets += 1;\n    dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);\n" +
			"    dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);\n    forgetTypedCode();\n    passLost = \"\";\n  }",
		"const waited = heedPassForget(key, mark);"} {
		if at := strings.Index(follow, step); stop < 0 || stop < read || at < stop {
			t.Errorf("followPassForget no longer does %q when another tab forgets", step)
		}
	}
	// Stop ("claim:") leaves a reissue code alone, here and in other tabs: its
	// nonce, the code typed, and an answer on its way, which redeemPassReissue
	// drops only for Remove and Forget (passCodeForgets, not passForgets), and
	// which waits on Stop without taking it for one of them (passCodeForgetting
	// is made only of the forgets that take the Pass).
	for name, steps := range map[string][]string{
		"forgetPassClaim": {"passCodeForgets += 1;", "dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);",
			"dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);"},
		"followPassForget": {"passCodeForgets += 1;", "dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);",
			"dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);", "forgetTypedCode();", "passLost = \"\";"},
	} {
		body := withoutComments(jsFunction(t, script, name))
		for _, only := range steps {
			guarded := regexp.MustCompile(`if \((emptyBoxes|code)\) \{[^}]*` + regexp.QuoteMeta(only))
			if strings.Count(body, only) != 1 || !guarded.MatchString(body) {
				t.Errorf("%s does %q for Stop collecting it here too, which then drops a reissue code being spent", name, only)
			}
		}
	}
	if !strings.Contains(forget, "waitOnPassForget(\n    mark,\n    emptyBoxes,") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "heedPassForget")), `return waitOnPassForget(key, mark.startsWith("code:"), over);`) {
		t.Error("a Stop's mark is waited on as one that takes the Pass, and drops a reissue answer")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "redeemPassReissue")), "const forgets = passCodeForgets;") {
		t.Error("redeemPassReissue drops its answer for Stop collecting it here, leaving a spent reissue code and a dead Pass")
	}
	// It waits for the mark to go, or until a while after it was set, for a
	// tab closed before its answer, and draws the Pass page again from the
	// server only then: if the reader is on it, past the age screen, even when
	// the mark arrived while it was still loading and drew nothing (a skeleton
	// is no Pass page yet), with focus the drawing dropped put back on the page.
	heed := withoutComments(jsFunction(t, script, "heedPassForget"))
	left := withoutComments(jsFunction(t, script, "passForgetLeft"))
	if !strings.Contains(heed, "passForgetHeard.set(key, resolve);\n    setTimeout(resolve, passForgetLeft(mark));") ||
		!strings.Contains(heed, "}).then(() => {\n    passForgetHeard.delete(key);\n  });") ||
		!strings.Contains(left, `const left = Number(mark.slice(mark.indexOf(":") + 1)) + PASS_FORGET_WAIT_MS - Date.now();`) ||
		!strings.Contains(left, "return left > 0 ? Math.min(left, PASS_FORGET_WAIT_MS) : 0;") {
		t.Error("a tab no longer waits for another tab's mark to go, or no longer for at most PASS_FORGET_WAIT_MS after it was set")
	}
	if !regexp.MustCompile(`waited\.then\(\(\) => \{\s*if \(passPageShown\(\) && ageStatus\(\) === "ok"\) \{\s*`+
		`renderPass\(\)\.then\(\(drawn\) => \{\s*if \(drawn\) \{\s*`+
		`if \(!document\.activeElement \|\| document\.activeElement === document\.body\) \{\s*passFocus\(`).MatchString(follow) ||
		strings.Contains(follow, "data-pass-page") {
		t.Error("followPassForget no longer draws the Pass page from the server once the other tab's request is answered, " +
			"with focus the drawing dropped on what took its place")
	}
	// A tab opened while a forget is on its way hears no storage event for it,
	// nor does one brought back from the back-forward cache for a mark set or
	// taken off while it was away, so each looks for the marks there before
	// anything is drawn: one it does not know of is waited on, one whose
	// mark has gone stops being waited on, and one set longer ago than a tab
	// waits, left by a tab closed before its answer, is taken off.
	under := withoutComments(jsFunction(t, script, "heedPassForgetsUnderWay"))
	if !strings.Contains(under, "if (key && key.startsWith(`${PASS_FORGET_STORAGE_KEY}:`)) {\n        marks.set(key, localStorage.getItem(key) || \"\");") ||
		!strings.Contains(under, "for (const [key, heard] of passForgetHeard) {\n    if (!marks.has(key)) {\n      heard();") ||
		!strings.Contains(under, "if (passForgetWaits.has(key)) {\n      continue;\n    }\n    if (passForgetLeft(mark) > 0) {\n      heedPassForget(key, mark);\n    } else {\n"+
			"      try {\n        localStorage.removeItem(key);") {
		t.Error("a tab opened while a forget is on its way no longer waits on it, or no longer takes off a mark left behind")
	}
	if !strings.Contains(code, "\nwindow.addEventListener(\"storage\", followPassForget);\nheedPassForgetsUnderWay();\n") ||
		strings.Index(code, "\nheedPassForgetsUnderWay();\n") > strings.LastIndex(code, "\nrender();\n") ||
		!strings.Contains(withoutComments(jsBlock(t, script, `window.addEventListener("pageshow", (event) => {`, "});")),
			"if (event.persisted) {\n    heedPassForgetsUnderWay();\n  }\n  if (event.persisted && state.route.view === \"pass\") {") ||
		strings.Count(code, "heedPassForgetsUnderWay()") != 3 {
		t.Error("a page opened, or brought back, while a forget is on its way can draw the Pass page before it looks for one")
	}
	// How long at most, as the README says.
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	said := regexp.MustCompile(`or for (\d+) seconds at most, in case the tab that set it was closed first`).FindStringSubmatch(words(string(readme)))
	wait := regexp.MustCompile(`(?m)^const PASS_FORGET_WAIT_MS = (\d+);$`).FindStringSubmatch(script)
	if said == nil || wait == nil || said[1]+"000" != wait[1] {
		t.Errorf("a tab waits PASS_FORGET_WAIT_MS = %v ms for another tab's forget; the README says %v seconds", wait, said)
	}
}

// After a button on the Pass page draws it again, focus goes to what took the
// button's place, not the top of the document; a button disabled while it
// waited, and left in place, gets focus back.
func TestPassPageButtonsKeepFocus(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	for _, action := range []string{"collect-pass", "restart-pass", "forget-pass", "stop-pass-claim"} {
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
	// What arrives after a button was pressed (Check now's claim, say) draws
	// the page again through redrawPass, which moves focus that was on the
	// page to what took the button's place.
	redraw := withoutComments(jsFunction(t, script, "redrawPass"))
	if !strings.Contains(redraw, "const focused = el.main.contains(document.activeElement);") ||
		!strings.Contains(redraw, "passFocus(...selectors);") {
		t.Error("redrawPass no longer keeps focus that was on the page")
	}
	// A drawing set off by something else (a claim's answer, another tab's
	// change to the Pass) leaves a reader typing in the code box where they
	// were, caret and all, and redrawPass does not move them on.
	draw := withoutComments(jsFunction(t, script, "drawPass"))
	if !strings.Contains(draw, "const typing = box && document.activeElement === box ? [box.selectionStart, box.selectionEnd] : null;") ||
		!strings.Contains(draw, "if (typing) {\n      again.focus();\n      again.setSelectionRange(typing[0], typing[1]);") ||
		!strings.Contains(redraw, "if (focused && !el.main.contains(document.activeElement)) {\n    passFocus(...selectors);") {
		t.Error("a drawing of the Pass page takes focus out of the code box while the reader types in it")
	}
	claimed := withoutComments(jsFunction(t, script, "showClaimed"))
	// drawPass( is in redrawPass( too, so equal counts mean no bare drawPass.
	if !strings.Contains(claimed, `redrawPass({ ...lastPassInfo, claim }, '[data-action="collect-pass"]', "#pass-held", "form[data-pass] input");`) ||
		strings.Count(claimed, "drawPass(") != strings.Count(claimed, "redrawPass(") {
		t.Error("showClaimed draws the page again without keeping focus")
	}
	// A reissue code's button waits disabled, which drops focus, so the page
	// drawn with its answer puts focus on what it brought, or back in the box.
	reissue := withoutComments(jsFunction(t, script, "redeemPassReissue"))
	if !strings.Contains(reissue, "if (news === \"issued\") {\n    focusPassPanel(\"pass-issued\");\n  } else {\n"+
		"    passFocus(res ? \"#pass-held\" : \"#f-pass-code\", \"#pass-held\", \"form[data-pass] input\");") {
		t.Error("redeemPassReissue draws the page again without moving focus")
	}
	// Turned away, Check it again and Stop collecting it here stay where they
	// were, enabled again, with focus back on them.
	if !strings.Contains(withoutComments(jsFunction(t, script, "refocus")),
		"if (button.isConnected && (!document.activeElement || document.activeElement === document.body)) {\n    button.focus();") {
		t.Error("refocus no longer gives focus back to a button that lost it while disabled, and only then")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "checkHeldPass")), "button.disabled = false;\n    refocus(button);") {
		t.Error("Check it again, turned away, leaves focus at the top of the document")
	}
	stop := code[strings.Index(code, `const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');`):]
	stop = stop[:strings.Index(stop, "\n    return;\n  }")]
	if !strings.Contains(stop, "if (!done) {\n        refocus(stopClaim);") {
		t.Error("Stop collecting it here, turned away, leaves focus at the top of the document")
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
	if !strings.Contains(submit, `const expired = kind === "activate" && refusal === "expired" && failed.payload.code;`) ||
		!strings.Contains(submit, "} else if (expired) {\n    forgetTypedCode();\n    kept = storePass(failed.payload.code, failed.payload.expires_at);") {
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
	if !strings.Contains(withoutComments(jsFunction(t, script, "submitPass")), "if (failed.status === 403 && failed.message === PASS_NOT_ON_SALE) {\n    drawPass({ ...lastPassInfo, paused: true });") {
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

// What the Pass page and the FAQ say about refunds, reissuing, hours, a
// server's unknown terms and a Pass that is gone, as the server does it.
func TestPassCopyMatchesWhatTheServerDoes(t *testing.T) {
	script := appScript(t)
	intro := words(withoutComments(jsFunction(t, script, "passIntroHTML")))
	faq := words(faqAnswer(t, script, "faq-pass"))
	for name, text := range map[string]string{"the Pass page": intro, "the FAQ's Pass entry": faq} {
		// The payment's reference is kept for refunds and for reissuing a
		// lost code, and the copy names both.
		for _, want := range []string{
			"a refund or a chargeback can take back the time that payment bought",
			"and so that a lost code can be replaced from the payment on your receipt",
			"Hours are counted in UTC",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s no longer says %q", name, want)
			}
		}
		if strings.Contains(text, "cancel the Pass") || strings.Contains(text, "until the hour ends") {
			t.Errorf("%s says a refund cancels the Pass, or that the hour is the reader's own", name)
		}
	}
	// A lost code is reissued from the payment that bought the Pass, or, once
	// that is refunded or disputed, only from one payment left paying for all
	// its time (passReissueNoOrigin), so every promise of a reissue code for a
	// lost code says it may no longer be possible then.
	held := words(jsFunction(t, script, "passHeldHTML"))
	for name, text := range map[string]string{"the held Pass": held, "the FAQ's Pass entry": faq} {
		if !strings.Contains(text, "you'll be sent a reissue code") ||
			!strings.Contains(text, "If the payment that bought the Pass was refunded or disputed, that may no longer be possible.") &&
				!strings.Contains(text, "if the payment that bought it was refunded or disputed, that may no longer be possible.") {
			t.Errorf("%s promises a reissue code for a lost code without saying a refunded purchase can end it", name)
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
