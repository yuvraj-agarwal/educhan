package app

import (
	"encoding/json"
	"io/fs"
	"math"
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
		"passHeldRecord",      // what a notice about the Pass held was said beside
		"emptyCopiedHeldCode", // whether the code copied is the Pass Remove takes
		"submitPass",          // the body of a renewal
	}
	allowed := strings.Count(submit, "storedPass(")
	for _, name := range readers {
		allowed += strings.Count(withoutComments(jsFunction(t, script, name)), "storedPass(")
	}
	// The pageshow listener: whether a Pass was stored while the page was in
	// the back-forward cache.
	allowed += strings.Count(withoutComments(jsBlock(t, script, `window.addEventListener("pageshow", (event) => {`, "});")), "storedPass(")
	if n := strings.Count(code, "storedPass(") - strings.Count(code, "function storedPass("); n != allowed {
		t.Errorf("app.js reads the stored Pass (storedPass()) %d times, %d of them in the submit handler, the pageshow listener "+
			"and %s; want none elsewhere", n, allowed, strings.Join(readers, ", "))
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
	// reads the Pass itself through storedPass, but for the code that was
	// stored before the other tab's change, which it reads from the event's
	// old value through passCodeIn, once, to compare with a code just handed
	// over, for a removal, and with a code this tab copied.
	if follow := withoutComments(jsFunction(t, script, "followPassStorage")); strings.Count(follow, "PASS_STORAGE_KEY") != 2 ||
		!strings.Contains(follow, "if (event.key !== PASS_STORAGE_KEY && event.key !== null) {") ||
		!strings.Contains(follow, "const was = passCodeIn(event.oldValue);\n"+
			"  if (event.key === PASS_STORAGE_KEY && event.newValue === null && was) {") ||
		strings.Count(follow, "oldValue") != 1 || strings.Count(follow, "newValue") != 1 ||
		strings.Count(code, "passCodeIn(") != 2 {
		t.Error("followPassStorage reads the stored Pass other than through storedPass")
	}
	if in := withoutComments(jsFunction(t, script, "passCodeIn")); !strings.Contains(in, "const saved = JSON.parse(raw);") ||
		!strings.Contains(in, `return typeof saved.code === "string" && PASS_CODE_PATTERN.test(saved.code) ? saved.code : "";`) ||
		strings.Contains(in, "localStorage") || strings.Contains(in, "storage") {
		t.Error("passCodeIn reads more than the code in what it is given")
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
	if !strings.Contains(submit, "showPassNotice(PASS_AGE_REFUSAL);") {
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
	if !strings.Contains(withoutComments(jsFunction(t, script, "showClaimed")), "function showClaimed(claim, news, notice = \"\", asked = false) {\n  if (!passPageShown()) {\n    return;\n  }") {
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
	call := regexp.MustCompile(`if \(([^{]*)\) \{\s*collectPass\(visit, asked\);`).FindStringSubmatch(render)
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
	show := withoutComments(jsFunction(t, script, "showPassCode"))
	if !strings.Contains(show, `document.getElementById("pass-shown")`) {
		t.Error("showPassCode no longer fills the panel's slot")
	}
	// Hidden again, the code leaves the page, not only the screen, so nothing
	// reading the page finds it there afterwards.
	if !strings.Contains(show, "if (!slot.hidden) {\n    slot.hidden = true;\n    slot.textContent = \"\";\n") {
		t.Error("Hide the code leaves the code in the page")
	}
	// Shown, it takes focus, so a screen reader reads it out: the code in its
	// own slot, not a code just handed over that the page shows beside it.
	if !strings.HasSuffix(show, "  slot.hidden = false;\n  button.textContent = \"Hide the code\";\n"+
		"  button.setAttribute(\"aria-expanded\", \"true\");\n  slot.querySelector(\".pass-code\").focus();\n}") {
		t.Error("Show the code no longer moves focus to the code it shows")
	}
	// The held Pass's code and a code just handed over can be on the page
	// together, so each has an id of its own, and each Copy the code copies
	// the code in its own panel: copying the held Pass's to write it down
	// before Remove must not copy the other.
	if !strings.Contains(show, `<p class="pass-code" id="pass-held-code" tabindex="-1">${esc(held.code)}</p>`) ||
		strings.Count(code, `id="pass-code"`) != 1 || strings.Count(code, `id="pass-held-code"`) != 1 ||
		!strings.Contains(withoutComments(jsFunction(t, script, "passIssuedHTML")), `<p class="pass-code" id="pass-code">${esc(issued.code)}</p>`) {
		t.Error("the held Pass's code shown and a code just handed over no longer have ids of their own")
	}
	copied := code[strings.Index(code, `if (event.target.closest('[data-action="copy-pass"]')) {`):]
	copied = copied[:strings.Index(copied, "\n    return;\n  }")]
	if !strings.Contains(copied, "event.preventDefault();\n    const code = event.target.closest(\".verify-panel\")?.querySelector(\".pass-code\");\n") ||
		strings.Contains(code, `getElementById("pass-code")`) || strings.Contains(code, `getElementById("pass-held-code")`) {
		t.Error("Copy the code no longer copies the code in its own panel")
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
	if stored := withoutComments(jsFunction(t, script, "storedPass")); !strings.Contains(stored, "const marked = saved.expired === true;") ||
		!strings.Contains(stored, "const expires = marked && saved.expires === undefined ? null : new Date(saved.expires);") ||
		!strings.Contains(stored, "return { code: saved.code, expires, expired: marked || expires <= serverNow(), marked };") {
		t.Error("storedPass no longer reads a Pass marked run out")
	}
}

// Whether a Pass has run out is the server's to say, and a computer's clock
// can be minutes off either way. The page keeps, in memory alone, how far the
// server's clock is from this computer's, from the Date header of the latest
// answer sent no-store (every JSON answer is), and judges every Pass by that
// (serverNow): the Pass held (storedPass, which the held panel's status, its
// "good until" and renewal wording, Check it again and every notice read) and
// a code just handed over (passIssuedHTML's ranOut). Before any answer has
// come it has this computer's clock alone. A Pass the server said has run
// out, answering a check of it, is kept marked so with the date it gave, and
// counts as run out whatever any clock says until the server says otherwise;
// a post's answer gives no date, so its mark keeps none.
func TestPassRunOutIsJudgedByTheServersClock(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if !strings.Contains(code, "\nlet serverClockOffset = 0;\n") || strings.Count(code, "serverClockOffset = ") != 2 {
		t.Error("the page no longer starts from this computer's clock, or sets the server's offset other than from an answer")
	}
	if got := withoutComments(jsFunction(t, script, "noteServerClock")); got != "function noteServerClock(res) {\n"+
		"  const date = Date.parse(res.headers.get(\"Date\") || \"\");\n"+
		"  if (Number.isFinite(date) && /\\bno-store\\b/.test(res.headers.get(\"Cache-Control\") || \"\")) {\n"+
		"    serverClockOffset = date - Date.now();\n  }\n}" {
		t.Errorf("noteServerClock no longer keeps the server's offset from the date of an answer no cache kept, and only then:\n%s", got)
	}
	if got := withoutComments(jsFunction(t, script, "serverNow")); got != "function serverNow() {\n"+
		"  return new Date(Date.now() + serverClockOffset);\n}" {
		t.Errorf("serverNow is no longer this computer's clock moved by the server's offset:\n%s", got)
	}
	// Every answer api() has is read, as soon as its headers are in, error
	// answers included, before anything else is done with it.
	if !strings.Contains(withoutComments(jsFunction(t, script, "api")),
		"    err.unreachable = true;\n    throw err;\n  }\n  noteServerClock(res);\n  if (answered) {") ||
		strings.Count(code, "noteServerClock(") != 2 {
		t.Error("api() no longer notes the server's clock from every answer as soon as its headers are in")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "storedPass")),
		"return { code: saved.code, expires, expired: marked || expires <= serverNow(), marked };") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "passIssuedHTML")),
			"const ranOut = !here && new Date(issued.expires) <= serverNow();") {
		t.Error("a Pass held, or a code just handed over, is judged run out by this computer's clock, or not marked so by the server")
	}
	// No Pass judgment reads this computer's clock alone: in the Pass part of
	// the page, Date.now() is read only for the times tabs compare with one
	// another, which every tab of this browser reads by the same clock: the
	// marks they set for one another (passForgetLeft, openPassCheckout), when
	// a forget went (forgetPassOrder's, and the press Remove, Forget and Stop
	// give forgetPassClaim, outside it), and when a request that may find
	// nothing left under the claim went (collectPass, loadPassInfo); new Date()
	// not at all.
	pass := code[strings.Index(code, "/* educhan Pass"):strings.Index(code, "/* Rules and FAQ")]
	if strings.Contains(pass, "new Date()") || strings.Count(pass, "Date.now()") != 5 || strings.Count(code, "serverNow()") != 3 ||
		strings.Count(withoutComments(jsFunction(t, script, "forgetPassClaim")), "Date.now()") != 0 ||
		strings.Count(withoutComments(jsFunction(t, script, "forgetPassOrder")), "const at = Date.now();") != 1 ||
		strings.Count(withoutComments(jsFunction(t, script, "passForgetLeft")), "Date.now()") != 1 ||
		strings.Count(withoutComments(jsFunction(t, script, "openPassCheckout")), "Date.now()") != 1 ||
		strings.Count(withoutComments(jsFunction(t, script, "collectPass")), "const sent = Date.now();") != 1 ||
		strings.Count(withoutComments(jsFunction(t, script, "loadPassInfo")), "const sent = Date.now();") != 1 ||
		strings.Count(code, "const at = Date.now();") != 4 || strings.Count(code, "Date.now()") != 10 {
		t.Error("a Pass is judged by this computer's clock somewhere, not the server's")
	}
	// The server's word that a Pass has run out, answering a check of it, is
	// kept with its date: by Check it again, by the check of the code held
	// after a renewal collected here or after a claim or reissue code whose
	// answer was lost (recheckPass), and by a code typed in. Stored again
	// without it (renewed, say), the mark goes.
	if store := withoutComments(jsFunction(t, script, "storePass")); !strings.HasPrefix(store, "function storePass(code, expires, expired = false) {\n"+
		"  storageSet(PASS_STORAGE_KEY, JSON.stringify(expired ? { code, expires, expired } : { code, expires }));\n") ||
		strings.Count(code, ", failed.payload.expires_at, true);") != 3 || strings.Count(code, "failed.payload.expires_at") != 3 ||
		strings.Count(code, "storePass(") != 9 {
		t.Error("the server's word that a Pass has run out is no longer kept with the date it gave, or is kept for a Pass stored again")
	}
	if got := withoutComments(jsFunction(t, script, "markPassExpired")); got != "function markPassExpired(code) {\n"+
		"  storageSet(PASS_STORAGE_KEY, JSON.stringify({ code, expired: true }));\n}" {
		t.Errorf("a post's word that its Pass has run out is kept with a date it did not give:\n%s", got)
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
		"submitPass":        "if (calledOff || dropped || passForgotten(forgets) || !passStillHeld(held)) {",
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
	// send, or one pressed while a checkout waits on another, is said apart,
	// since nothing was removed or forgotten. Each wait ends a turn after the
	// forgets, so what Stop says once it is answered does not take the place
	// of what the press says. A checkout waits as well until no other checkout
	// is on its way, in any tab, by their marks, so the one pressed last holds
	// the claim cookie: one pressed after a forget is not overtaken by a late
	// answer to one the forget overtook. A press whose document is left while
	// it waits (pagehide) stops waiting and sends nothing, ahead of anything
	// else, and says nothing either, since the reader has gone: brought back
	// from the back-forward cache, it would otherwise go with no new press, a
	// checkout to Stripe and a reissue code spent from a box leaving emptied.
	settled := withoutComments(jsFunction(t, script, "passForgetsSettled"))
	if !strings.Contains(settled, "async function passForgetsSettled(checkout = false) {\n"+
		"  const codes = passCodeForgets;\n  const claims = passForgets;\n  const left = passLeft;\n"+
		"  while (left === passLeft && (passForgetting || (checkout && passCheckoutLeft() > 0))) {\n"+
		"    await (passForgetting || new Promise((resolve) => setTimeout(resolve, PASS_CHECKOUT_LOOK_MS)));\n"+
		"    await new Promise((resolve) => setTimeout(resolve));\n  }\n"+
		"  if (left !== passLeft) {\n    return PASS_LEFT_MEANWHILE;\n  }\n"+
		"  if (codes !== passCodeForgets) {\n    return PASS_FORGOTTEN_MEANWHILE;\n  }\n"+
		"  return claims !== passForgets ? PASS_STOPPED_MEANWHILE : \"\";\n}") {
		t.Error("passForgetsSettled no longer waits for every forget on its way, and for a checkout every other checkout, " +
			"and says what came meanwhile, or no longer stops for a document left while it waits")
	}
	for name, dropped := range map[string]string{
		"submitPass": "const meanwhile = await passForgetsSettled(kind !== \"activate\");\n  if (meanwhile) {\n    button.disabled = false;\n    button.textContent = label;\n" +
			"    if (passPageShown() && meanwhile !== PASS_LEFT_MEANWHILE) {\n      refocus(button);\n      showPassNotice(meanwhile);\n    }\n    return;\n  }",
		"redeemPassReissue": "const meanwhile = await passForgetsSettled();\n  if (meanwhile) {\n" +
			"    if (passPageShown() && meanwhile !== PASS_LEFT_MEANWHILE) {\n      showPassNotice(meanwhile);\n    }\n    return;\n  }",
		"checkHeldPass": "const meanwhile = await passForgetsSettled();\n  if (meanwhile) {\n    button.disabled = false;\n" +
			"    if (passPageShown() && meanwhile !== PASS_LEFT_MEANWHILE) {\n      refocus(button);\n      showPassNotice(meanwhile);\n    }\n    return;\n  }",
	} {
		body := withoutComments(jsFunction(t, script, name))
		waited, noted := strings.Index(body, dropped), strings.Index(body, "const forgets = passCodeForgets;")
		asked := regexp.MustCompile(`await api\(`).FindStringIndex(body)
		if waited < 0 || noted < waited || asked == nil || asked[0] < noted || strings.Count(body, "passForgetsSettled(") != 1 {
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
	if n := strings.Count(withoutComments(script), "passForgetsSettled("); n != 4 ||
		!strings.Contains(script, "\nconst PASS_FORGOTTEN_MEANWHILE = \"Nothing was sent: the Pass was removed, or this browser forgotten") ||
		!strings.Contains(script, "\nconst PASS_STOPPED_MEANWHILE = \"Nothing was sent: this browser stopped collecting") {
		t.Errorf("passForgetsSettled( appears %d times; want its definition and the three presses that wait on it, "+
			"which say nothing was sent, and why, when a forget came meanwhile", n)
	}
	// What a press comes to when its document was left is never said: it is
	// its own word, returned once and set aside by each of the three presses
	// before anything is shown.
	if !strings.Contains(script, "\nconst PASS_LEFT_MEANWHILE = \"left\";\n") ||
		strings.Count(withoutComments(script), "PASS_LEFT_MEANWHILE") != 5 {
		t.Error("PASS_LEFT_MEANWHILE is said, or set aside other than by the three presses that wait")
	}
	// Each is called straight from the press: the click, or the submit
	// handler, which hands a Pass form over before it awaits anything.
	code := withoutComments(script)
	if !strings.Contains(code, "    checkHeldPass(checkPass);\n") ||
		!strings.Contains(withoutComments(submitHandler(t, script)), "  const form = event.target;\n  if (form.dataset.verify) {\n"+
			"    await submitVerification(form);\n    return;\n  }\n  if (form.dataset.pass) {\n    await submitPass(form);") {
		t.Error("Check it again or a Pass form waits before it reaches passForgetsSettled, which then notes forgets pressed after it")
	}
	// One asked for before the forget and dropped for it, or called off, gives
	// the button that waited its focus back, rather than leaving it on the top
	// of the document.
	if !strings.Contains(withoutComments(jsFunction(t, script, "submitPass")),
		"if (calledOff || dropped || passForgotten(forgets) || !passStillHeld(held)) {\n    button.disabled = false;\n    button.textContent = label;\n"+
			"    if (passPageShown()) {\n      refocus(button);\n    }\n    return;\n  }") {
		t.Error("submitPass drops an answer for a forget, or a checkout called off, and leaves focus on the top of the document")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "checkHeldPass")),
		"if (passForgotten(forgets) || !passStillHeld(held)) {\n    button.disabled = false;\n"+
			"    if (passPageShown()) {\n      refocus(button);\n    }\n    return;\n  }") {
		t.Error("Check it again drops an answer for a forget and leaves focus on the top of the document")
	}
	// A checkout still on its way when Remove the Pass, Forget this browser or
	// Stop collecting it here is pressed must not leave a claim that outlives
	// the forget. Pressed in its own tab, the forget calls it off first, if
	// its answer has not arrived, so the browser keeps no cookie from it; if
	// its answer has arrived, the cookie it set is in the browser before the
	// forget's request goes, and is dropped with the rest. Pressed in another
	// tab, the forget may have gone too soon to see that claim: the checkout
	// is not followed to Stripe, and its claim is dropped by the order its
	// answer names, never by a forget of every claim, which would take a
	// checkout's started after the forget, nor by the forget's marks and
	// counters, which would stop other tabs collecting what is rightly theirs.
	// Both counters are noted when the request goes, once no forget is on its
	// way, so only a forget pressed after it counts. The checkout is noted
	// after them, and taken off as soon as its answer is in, before anything
	// else is done with it, so another checkout waiting on it goes on.
	checkout := withoutComments(jsFunction(t, script, "submitPass"))
	noted := strings.Index(checkout, "const forgets = passCodeForgets;\n  const claims = passForgets;\n")
	opened := strings.Index(checkout, "const checkout = kind === \"activate\" ? null : openPassCheckout();\n"+
		"  const options = { method: \"POST\", body: JSON.stringify(request.body) };\n  if (checkout) {\n"+
		"    options.signal = checkout.abort.signal;\n    options.answered = () => {\n      checkout.answered = true;\n    };\n  }\n")
	asked := strings.Index(checkout, "res = await api(request.path, options);\n  } catch (err) {\n    failed = err;\n  }\n"+
		"  const calledOff = Boolean(checkout) && checkout.abort.signal.aborted;\n  if (checkout) {\n    closePassCheckout(checkout);\n  }\n")
	late := strings.Index(checkout, "const dropped = Boolean(res) && kind !== \"activate\" && (claims !== passForgets || passForgotten(forgets));\n"+
		"  if (dropped && !checkout.forgotten) {\n    forgetPassOrder(res.order, passForgetHeardLast);\n  }\n")
	guard := strings.Index(checkout, "if (calledOff || dropped || passForgotten(forgets) || !passStillHeld(held)) {")
	assign := strings.Index(checkout, "window.location.assign(res.url);")
	if noted < 0 || opened < noted || asked < opened || noted < strings.Index(checkout, "await passForgetsSettled(kind !== \"activate\");") ||
		late < asked || guard < late || assign < guard || strings.Count(checkout, "openPassCheckout(") != 1 ||
		strings.Count(checkout, "closePassCheckout(") != 1 || len(regexp.MustCompile(`\bpassForgets\b`).FindAllString(checkout, -1)) != 2 {
		t.Error("a checkout answered after a forget pressed since it went keeps the claim it set, or is followed to Stripe")
	}
	// The answer's headers are noted as they arrive, before its body is read:
	// a checkout is called off only until then, and once they are in, the
	// browser has kept the cookie they set.
	api := withoutComments(jsFunction(t, script, "api"))
	fetched := strings.Index(api, "res = await fetch(path, init);")
	noteAnswer := strings.Index(api, "  if (answered) {\n    answered();\n  }\n  const payload = res.status === 204 ? null : await res.json()")
	if !strings.Contains(api, "const { answered, ...rest } = options;\n") ||
		!strings.Contains(api, "const init = { ...rest, headers: { ...(rest.headers || {}) } };") || fetched < 0 || noteAnswer < fetched {
		t.Error("api() no longer says when an answer's headers are in, before its body is read")
	}
	// Called off: each checkout not yet answered, its request dropped and its
	// mark taken off; for a forget pressed here, one already answered is noted
	// instead, since the forget goes after its cookie and needs no drop by
	// name. Remove, Forget and Stop do it before anything else, so before the
	// forget's request goes, and so does leaving the page; a forget heard of
	// from another tab does not, and leaves the answer to be dropped by name.
	callOff := withoutComments(jsFunction(t, script, "callOffPassCheckouts"))
	if callOff != "function callOffPassCheckouts(forgotten) {\n  for (const checkout of [...passCheckouts]) {\n"+
		"    if (checkout.answered) {\n      checkout.forgotten = checkout.forgotten || forgotten;\n    } else {\n"+
		"      closePassCheckout(checkout);\n      checkout.abort.abort();\n    }\n  }\n}" {
		t.Error("callOffPassCheckouts no longer calls off each checkout not yet answered, and notes the rest for a forget")
	}
	if forget := withoutComments(jsFunction(t, script, "forgetPassClaim")); !strings.HasPrefix(forget,
		"function forgetPassClaim(pressed, at) {\n  callOffPassCheckouts(true);\n  const emptyBoxes = pressed !== \"stop\";\n  passVisit += 1;\n") {
		t.Error("Remove, Forget or Stop sends its forget before calling off a checkout still on its way in its tab")
	}
	if hide := withoutComments(jsBlock(t, script, `window.addEventListener("pagehide", () => {`, "});")); !strings.Contains(hide, "  callOffPassCheckouts(false);\n") {
		t.Error("leaving the page leaves a checkout on its way, whose answer would set a claim nothing here follows")
	}
	// And a press still waiting to go (passForgetsSettled) is called off with
	// it: leaving moves passLeft, which nothing else moves, and every press
	// notes it before it waits.
	if hide := withoutComments(jsBlock(t, script, `window.addEventListener("pagehide", () => {`, "});")); !strings.Contains(hide, "  passLeft += 1;\n") ||
		!strings.Contains(code, "\nlet passLeft = 0;\n") || strings.Count(code, "passLeft += 1;") != 1 ||
		len(regexp.MustCompile(`\bpassLeft\b`).FindAllString(code, -1)) != 5 {
		t.Error("leaving the page leaves a press waiting to go, which goes with no new press once the page is brought back")
	}
	if n := strings.Count(code, "callOffPassCheckouts("); n != 3 {
		t.Errorf("callOffPassCheckouts is called %d times; want its definition, forgetPassClaim and pagehide", n)
	}
	// A checkout's mark holds the time and nothing else, under a key spelled
	// once and read only where it is put up, where it is waited on, and where
	// Forget this browser takes every one off, and every mark is waited on for
	// at most the wait a forget's mark gets.
	if !strings.Contains(code, "\nconst PASS_CHECKOUT_STORAGE_KEY = \"educhan:pass-checkout\";\nconst PASS_CHECKOUT_LOOK_MS = 100;\n") ||
		strings.Count(code, `"educhan:pass-checkout"`) != 1 || strings.Count(code, "PASS_CHECKOUT_STORAGE_KEY") != 4 {
		t.Error("the checkout mark's key is spelled out, or read, other than in openPassCheckout, passCheckoutLeft and forgetBrowser")
	}
	// A mark a tab left behind, crashed or killed before its answer came, says
	// when a checkout was started here. Forget this browser takes every one
	// off, once its forget has called off this tab's own, and a page load takes
	// off one past its wait, by passCheckoutLeft's own test. A tab whose
	// checkout is still on its way puts its mark back as it was when another
	// tab takes it off, so a checkout pressed after the Forget still waits for
	// its answer; one past its wait stays off.
	browser := withoutComments(jsFunction(t, script, "forgetBrowser"))
	called := strings.Index(browser, "  const dropped = forgetPassClaim(\"browser\", at);\n")
	swept := strings.Index(browser, "  pagePosterKey = \"\";\n  try {\n    const marks = [];\n"+
		"    for (let at = 0; at < localStorage.length; at += 1) {\n      const key = localStorage.key(at);\n"+
		"      if (key && key.startsWith(`${PASS_CHECKOUT_STORAGE_KEY}:`)) {\n        marks.push(key);\n      }\n    }\n"+
		"    for (const key of marks) {\n      localStorage.removeItem(key);\n    }\n  } catch {\n\n  }\n  return dropped;\n}")
	if called < 0 || swept < called || strings.Count(browser, "forgetPassClaim(") != 1 {
		t.Error("Forget this browser no longer takes every checkout's mark off once its forget has called off this tab's checkouts")
	}
	if !regexp.MustCompile(`\nheedPassForgetsUnderWay\(\);\s*passCheckoutLeft\(\);\n`).MatchString(code) {
		t.Error("a page load no longer takes off a checkout's mark past its wait")
	}
	if keep := withoutComments(jsFunction(t, script, "keepPassCheckoutMark")); keep != "function keepPassCheckoutMark(event) {\n"+
		"  if (event.newValue !== null || !event.oldValue || passForgetLeft(event.oldValue) === 0) {\n    return;\n  }\n"+
		"  for (const checkout of passCheckouts) {\n    if (checkout.mark === event.key) {\n"+
		"      storageSet(checkout.mark, event.oldValue);\n    }\n  }\n}" ||
		!strings.Contains(code, "\nwindow.addEventListener(\"storage\", keepPassCheckoutMark);\n") {
		t.Error("a checkout on its way no longer puts back its mark when another tab takes it off, so a later checkout can overtake it")
	}
	if open := withoutComments(jsFunction(t, script, "openPassCheckout")); !strings.Contains(open,
		"mark: `${PASS_CHECKOUT_STORAGE_KEY}:${newPassNonce()}`,\n    abort: new AbortController(),\n    answered: false,\n    forgotten: false,\n  };\n"+
			"  storageSet(checkout.mark, `checkout:${Date.now()}`);\n  passCheckouts.add(checkout);\n") {
		t.Error("openPassCheckout no longer marks a checkout with the time alone, or no longer notes it to be called off")
	}
	if closed := withoutComments(jsFunction(t, script, "closePassCheckout")); !strings.Contains(closed,
		"passCheckouts.delete(checkout);\n  try {\n    localStorage.removeItem(checkout.mark);") {
		t.Error("closePassCheckout no longer takes a checkout's mark off")
	}
	left := withoutComments(jsFunction(t, script, "passCheckoutLeft"))
	if !strings.Contains(left, "if (key && key.startsWith(`${PASS_CHECKOUT_STORAGE_KEY}:`)) {\n        marks.push(key);") ||
		!strings.Contains(left, "const wait = passForgetLeft(localStorage.getItem(key) || \"\");\n      if (wait > 0) {\n"+
			"        left = Math.max(left, wait);\n      } else {\n        localStorage.removeItem(key);\n      }") {
		t.Error("passCheckoutLeft no longer waits on every checkout's mark, each for at most the wait, and takes off one older")
	}
	if strings.Count(code, "passCheckoutLeft()") != 3 || strings.Count(code, "openPassCheckout()") != 2 {
		t.Error("a checkout's mark is put up, or waited on, other than by submitPass's checkout through passForgetsSettled " +
			"(and swept as the page loads)")
	}
	order := withoutComments(jsFunction(t, script, "forgetPassOrder"))
	named := strings.Index(order, `api("/api/pass/claim", { method: "POST", body: JSON.stringify({ forget: true, order }) })`)
	if checked := strings.Index(order, "if (typeof order !== \"string\" || !order) {\n    return;\n  }"); checked < 0 || named < checked ||
		strings.Count(order, "api(") != 1 {
		t.Error("forgetPassOrder no longer drops the one claim its order names, or asks for a forget of every claim when there is no name")
	}
	// It reads passClaimGone, to say nothing once this tab has learned that
	// nothing is left to collect since its drop went
	// (TestPassNoticeGoesWithWhatItSaid), and moves no counter.
	for _, kept := range []string{"storageSet(", "localStorage", "PASS_FORGET_STORAGE_KEY", "passVisit", "passForgets",
		"passCodeForgets", "passNoticeForgets", "passClaimGone =", "dropPassNonce(", "waitOnPassForget(", "heedPassForget("} {
		if strings.Contains(order, kept) {
			t.Errorf("forgetPassOrder does %q: the order's name is sent once and kept nowhere, and the drop stops nothing else", kept)
		}
	}
	if strings.Count(order, "passClaimGone") != 1 || strings.Count(order, "const at = Date.now();") != 1 {
		t.Error("forgetPassOrder does more with passClaimGone than compare it with when its drop went")
	}
	if strings.Count(code, "forgetPassOrder(") != 2 || strings.Count(code, ".order") != 1 {
		t.Error("a checkout's order is read, or forgotten by name, other than where a checkout answered after a forget is dropped")
	}
	// What it says when educhan cannot be reached names the forget pressed in
	// the other tab, as that tab's mark has it, and how to drop the claim once
	// back online with no more than that forget did: Stop collecting it here
	// again after a Stop, which leaves the poster key, the name, a badge and a
	// Pass held here; Stop collecting it here after Remove the Pass, which leaves
	// no Pass to remove; Forget this browser again after Forget.
	kept := "\nconst PASS_ORDER_KEPT = {\n  stop:\n    \"" +
		"educhan couldn't be reached to drop the checkout that was still opening when Stop collecting it here was pressed, so this browser may still be collecting it. Open the Pass page when you're back online and press Stop collecting it here again." + "\",\n  remove:\n    \"" +
		"educhan couldn't be reached to drop the checkout that was still opening when the Pass was removed, so this browser may still be collecting it. Open the Pass page when you're back online and press Stop collecting it here." + "\",\n  browser:\n    \"" +
		"educhan couldn't be reached to drop the checkout that was still opening when this browser was forgotten, so the browser may still be collecting it. Press Forget this browser on the Verify page again when you're back online." + "\",\n};\n"
	if !strings.Contains(code, kept) || !strings.Contains(order, "const notice = PASS_ORDER_KEPT[pressed] || PASS_ORDER_KEPT.browser;") ||
		!strings.HasPrefix(order, "function forgetPassOrder(order, pressed) {") || strings.Count(code, "PASS_ORDER_KEPT") != 3 {
		t.Error("a checkout's claim that could not be dropped by its order's name is no longer said in words that name the forget " +
			"pressed, and how to drop it once back online with no more than that")
	}
	if !strings.Contains(code, "\nlet passForgetHeardLast = \"\";\n") || strings.Count(code, "passForgetHeardLast") != 3 ||
		!strings.Contains(withoutComments(jsFunction(t, script, "stopForPassForget")),
			"const pressed = mark.slice(0, mark.indexOf(\":\"));\n  const code = pressed !== \"stop\";\n  passForgetHeardLast = pressed;\n") {
		t.Error("forgetPassOrder is no longer told which forget was pressed in the other tab, as its mark has it")
	}
	// What offers Remove the Pass, Forget this browser and Stop collecting it
	// here says so, as a shared computer's reader needs to hear it.
	for name, want := range map[string][2]string{
		"Forget this browser": {"forgetBrowserHTML", "It also stops this browser collecting a Pass bought here that hasn't arrived yet, " +
			"even one whose checkout was still opening;"},
		"the held Pass": {"passHeldHTML", "Removing it also stops this browser collecting anything bought here that hasn't " +
			"arrived yet, even a checkout still opening."},
		"Stop collecting it here": {"passStopCollectingHTML", "so whoever uses this browser next can't. That stops a checkout " +
			"still opening in this browser too."},
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
// The exceptions are two claim notices, said wherever the reader is, since
// they ask for a step: that a checkout's claim could not be dropped by its
// order's name (forgetPassOrder), and that Remove the Pass could not reach
// educhan to drop this browser's claims. Stop collecting it here's claim
// notice, that it could not reach educhan, is said only on the Pass page.
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
		"Remove the Pass":      handler(`if (event.target.closest('[data-action="forget-pass"]')) {`),
		"Stop collecting":      handler(`const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');`),
		"Copy the code":        handler(`if (event.target.closest('[data-action="copy-pass"]')) {`),
		"Check now":            handler(`if (event.target.closest('[data-action="collect-pass"]')) {`),
		"another tab's change": withoutComments(jsFunction(t, script, "followPassStorage")),
	}
	waits := regexp.MustCompile(`\bawait |\.then\(|\.catch\(`)
	draws := regexp.MustCompile(`\b(drawPass|redrawPass|passFocus|focusPassPanel|showNotice|showPassNotice|refocus|renderPass|announce)\(|location\.assign\(`)
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
	// Each of those two claim notices asks the reader for a step, so it is said
	// on whatever page this tab is on: the reader who pressed the forget in
	// another tab was told it was done, and the reader who pressed Remove here
	// may have moved on before educhan failed to answer. That notice is all
	// forgetPassOrder draws, and all Remove draws once its forget is answered,
	// unless this tab has learned that nothing is left to collect since its
	// forget went (TestPassNoticeGoesWithWhatItSaid). On the Pass page it
	// answers a press there, this tab's own Buy, Renew or Remove, so it is said
	// there as every other answer to a press there is, brought into view
	// (showPassNotice); on any other page it moves nothing (showNotice). It is
	// noted as a claim notice either way, said for its forget, for a later
	// forget's answer to take off, and Remove's as a held notice too, unless a
	// forget that takes held notices off came meanwhile, which leaves the
	// claim's warning alone (TestPassNoticeGoesWithWhatItSaid).
	order := withoutComments(jsFunction(t, script, "forgetPassOrder"))
	said := regexp.MustCompile(`\.catch\(\(err\) => \{\s*if \(err\.status !== 403 && passClaimGone <= at\) \{\s*` +
		`const notice = PASS_ORDER_KEPT\[pressed\] \|\| PASS_ORDER_KEPT\.browser;\s*` +
		`if \(passPageShown\(\)\) \{\s*showPassNotice\(notice\);\s*\} else \{\s*showNotice\(notice\);\s*\}\s*` +
		`noteClaimNotice\(at\);\s*\}\s*\}\);`)
	if len(draws.FindAllString(order, -1)) != 2 || strings.Count(order, "passPageShown()") != 1 || !said.MatchString(order) {
		t.Error("forgetPassOrder no longer says, on whatever page this tab is on, that a claim it could not drop is still collected, " +
			"in view on the Pass page and moving nothing elsewhere, or says or draws something else")
	}
	removed := bodies["Remove the Pass"]
	unreached := regexp.MustCompile(`dropped\.then\(\(done\) => \{\s*if \(done \|\| passClaimGone > at\) \{\s*return;\s*\}\s*` +
		`const held = forgets === passNoticeForgets;\s*` +
		`const notice = held \? PASS_CLAIM_KEPT\.remove\.said : PASS_CLAIM_KEPT\.remove\.alone;\s*` +
		`if \(passPageShown\(\)\) \{\s*showPassNotice\(notice\);\s*\} else \{\s*showNotice\(notice\);\s*\}\s*` +
		`if \(held\) \{\s*notePassHeld\(said, PASS_CLAIM_KEPT\.remove\.alone, at\);\s*\} else \{\s*` +
		`noteClaimNotice\(at\);\s*\}\s*\}\);$`)
	if !unreached.MatchString(removed) || len(draws.FindAllString(removed[strings.Index(removed, "dropped.then("):], -1)) != 2 {
		t.Error("Remove the Pass no longer says, on whatever page this tab is on, that educhan could not be reached to drop " +
			"this browser's claims, in view on the Pass page and moving nothing elsewhere, or says or draws something else then")
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
	// left the page or the Pass held changed, is one left behind, as if they
	// had left Stripe without paying: the forms come back over it, and it is
	// not drawn as a payment Stripe has yet to confirm. One answered after a
	// forget is set aside in the same way, and has its claim dropped as well
	// (pinned in TestPassAnswerAboutAPassThatLeftIsDropped).
	left := strings.Index(submit, "if (res && kind !== \"activate\") {\n    passStartedAgain = true;\n  }")
	if guarded := strings.Index(submit, "if (calledOff || dropped || passForgotten(forgets) || !passStillHeld(held)) {"); left < 0 || guarded < left ||
		!strings.Contains(submit, "passStartedAgain = false;\n    window.location.assign(res.url);") {
		t.Error("a checkout answered after the reader left the Pass page, or after a forget, is drawn as one to collect")
	}
	// Check now moves focus only if its own drawing happened, which renderPass
	// says; the claim's answer is shown through showClaimed, which asks first.
	if !strings.Contains(bodies["Check now"], "renderPass().then((drawn) => {\n      if (drawn) {\n        passFocus(") ||
		!strings.Contains(bodies["renderPass"], "collectPass(visit, asked);\n  }\n  return true;\n}") {
		t.Error("Check now moves focus once its drawing was overtaken, or the reader left the page")
	}
}

// A notice raised by a press on the Pass page is brought into view when it is
// out of view: on a phone the forms sit far below the notice, at the top of
// the page, and a refusal said there unseen looks like nothing happened. It
// goes straight there, not smoothly, only as far as it must (block
// "nearest"), and only once the press has given focus where it gives it, which
// it leaves there. Every notice the Pass page's presses raise goes this way,
// Check now's included: what the first claim of the drawing it asks for comes
// to answers it. What the page says on its own, later claims' news and the
// run-out notice said again for the page drawn after its press included, and
// every other page's notices stay where showNotice puts them, which scrolls
// nothing and moves focus only from the notice it replaces to the new one
// (TestNoticeReplacedKeepsFocusOnIt).
func TestPassNoticesFromAPressAreBroughtIntoView(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	shown := withoutComments(jsFunction(t, script, "showPassNotice"))
	if shown != "function showPassNotice(message) {\n  showNotice(message);\n  setTimeout(() => {\n"+
		"    const box = el.notice.getBoundingClientRect();\n"+
		"    const header = document.querySelector(\".site-header\").getBoundingClientRect();\n"+
		"    if (!el.notice.hidden && (box.top < header.bottom || box.bottom > window.innerHeight)) {\n"+
		"      el.notice.scrollIntoView({ block: \"nearest\" });\n    }\n  });\n}" {
		t.Errorf("showPassNotice no longer brings a notice out of view into it, and only then, straight and as far as it must:\n%s", shown)
	}
	if notice := withoutComments(jsFunction(t, script, "showNotice")); strings.Contains(notice, "scroll") ||
		strings.Count(notice, ".focus(") != 1 || !strings.Contains(notice, ".focus({ preventScroll: true });") {
		t.Error("showNotice moves the page, or moves focus other than from a notice it replaces, for every page's notices")
	}
	if strings.Count(code, "el.notice.scrollIntoView(") != 1 || strings.Contains(code, "smooth") {
		t.Error("the page scrolls to a notice other than in showPassNotice, or scrolls smoothly")
	}
	css, err := fs.ReadFile(webFS(), "app.css")
	if err != nil {
		t.Fatal(err)
	}
	rule := string(css)[strings.Index(string(css), "\n.notice {\n"):]
	rule = rule[:strings.Index(rule, "\n}\n")]
	if !strings.Contains(rule, "\n  scroll-margin-top: 160px;") || strings.Contains(string(css), "smooth") {
		t.Error("a notice brought into view can stop under the sticky header, which is 135px tall on a phone, or scrolls smoothly")
	}
	block := func(opener string) string {
		start := strings.Index(code, opener)
		if start < 0 {
			t.Fatalf("app.js has no %q; this test reads it", opener)
		}
		body := code[start:]
		return body[:strings.Index(body, "\n    return;\n  }")]
	}
	presses := map[string]string{
		"redeemPassReissue": withoutComments(jsFunction(t, script, "redeemPassReissue")),
		"checkHeldPass":     withoutComments(jsFunction(t, script, "checkHeldPass")),
		"submitPass":        withoutComments(jsFunction(t, script, "submitPass")),
		"Remove the Pass":   block(`if (event.target.closest('[data-action="forget-pass"]')) {`),
		"Stop collecting":   block(`const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');`),
		"Copy the code":     block(`if (event.target.closest('[data-action="copy-pass"]')) {`),
	}
	// Remove's claim notice, said once its forget could not reach educhan, is
	// said in view on the Pass page and moves nothing on any other page the
	// reader has gone to (TestPassAnswersDrawOnlyOnThePassPage).
	elsewhere := "} else {\n        showNotice(notice);\n      }"
	if strings.Count(presses["Remove the Pass"], elsewhere) != 1 {
		t.Error("Remove the Pass no longer says its claim notice, off the Pass page, where showNotice puts it")
	}
	presses["Remove the Pass"] = strings.Replace(presses["Remove the Pass"], elsewhere, "}", 1)
	said := 0
	for name, body := range presses {
		if strings.Contains(body, "showNotice(") || !strings.Contains(body, "showPassNotice(") {
			t.Errorf("%s says a notice that may land out of view", name)
		}
		said += strings.Count(body, "showPassNotice(")
	}
	// Check now marks the drawing it asks for (passAsked), which renderPass
	// takes before anything else can, and hands to its collection; only the
	// first claim's answer, the press's, is said in view (showClaimed), and
	// later polls' news is not.
	checkNow := block(`if (event.target.closest('[data-action="collect-pass"]')) {`)
	render := withoutComments(jsFunction(t, script, "renderPass"))
	collect := withoutComments(jsFunction(t, script, "collectPass"))
	claimed := withoutComments(jsFunction(t, script, "showClaimed"))
	if !strings.Contains(checkNow, "event.preventDefault();\n    passAsked = true;\n    renderPass().then(") ||
		!strings.Contains(render, "{\n  const visit = ++passVisit;\n  const asked = passAsked;\n  passAsked = false;\n") ||
		strings.Count(code, "passAsked = ") != 3 || // and its declaration
		!strings.Contains(render, "collectPass(visit, asked);") ||
		!strings.HasPrefix(collect, "async function collectPass(visit, asked = false) {") ||
		strings.Count(collect, ", asked);") != 2 ||
		!strings.Contains(collect, "\n    }\n    asked = false;\n    await new Promise((resolve) => setTimeout(resolve, wait));") ||
		!strings.Contains(claimed, "  if (notice && asked) {\n    showPassNotice(notice);\n  } else if (notice) {\n    showNotice(notice);\n  }\n"+
			"  if (notice) {\n    notePassHeld();\n  }\n}") {
		t.Error("what Check now's first claim comes to is not said in view, or a later poll's news is")
	}
	said += strings.Count(claimed, "showPassNotice(")
	// The notice that the Pass held has run out answers the press that found
	// it so, in view; said again once the page asked for after it has landed,
	// it is the page's own news and moves nothing, since the reader may have
	// scrolled away since the press.
	runOut := withoutComments(jsFunction(t, script, "showPassRunOut"))
	if strings.Count(runOut, "showPassNotice(said);") != 1 || strings.Count(runOut, "showNotice(now);") != 1 ||
		strings.Count(runOut, "Notice(") != 3 || strings.Index(runOut, "showPassNotice(said);") > strings.Index(runOut, "refreshFailedPass(") ||
		strings.Index(runOut, "showNotice(now);") < strings.Index(runOut, ".then(") {
		t.Error("the run-out notice is not said in view as the press's answer, or its saying again moves the page")
	}
	said += strings.Count(runOut, "showPassNotice(")
	// The notice a checkout's tab gives when it could not drop that
	// checkout's claim (forgetPassOrder) answers its own Buy or Renew when it
	// is on the Pass page, and is said in view there, and only there; on any
	// other page it moves nothing (pinned in TestPassAnswersDrawOnlyOnThePassPage).
	order := withoutComments(jsFunction(t, script, "forgetPassOrder"))
	if !strings.Contains(order, "if (passPageShown()) {\n        showPassNotice(notice);\n      } else {\n        showNotice(notice);\n      }") {
		t.Error("forgetPassOrder's notice, raised on the Pass page by this tab's own Buy or Renew, may land out of view")
	}
	said += strings.Count(order, "showPassNotice(")
	// Nowhere else: not what the Pass page says on its own (back from Stripe),
	// and not another page's.
	if n := strings.Count(code, "showPassNotice("); n != said+1 {
		t.Errorf("showPassNotice( appears %d times; want its definition and the Pass page's presses' %d", n, said)
	}
	if strings.Contains(withoutComments(jsFunction(t, script, "renderPass")), "showPassNotice(") {
		t.Error("renderPass moves the page to a notice no press raised")
	}
}

// A notice put up in place of another while focus is on the old one's dismiss
// button, a later answer replacing an earlier one, say, would take that focus
// to the top of the document with the old button. showNotice gives it to the
// new dismiss button instead, without scrolling the page, so a keyboard stays
// where it was. A notice put up while focus is anywhere else takes none, on
// every page. The reader's own dismissal clears the notice as it did before
// the Pass, and focus goes where the browser leaves it.
func TestNoticeReplacedKeepsFocusOnIt(t *testing.T) {
	script := appScript(t)
	if got := withoutComments(jsFunction(t, script, "showNotice")); got != "function showNotice(message) {\n"+
		"  const focused = el.notice.contains(document.activeElement);\n"+
		"  el.notice.innerHTML =\n"+
		"    '<span class=\"notice-text\"></span>' +\n"+
		"    '<button class=\"notice-dismiss\" type=\"button\" data-action=\"dismiss-notice\" aria-label=\"Dismiss\">&times;</button>';\n"+
		"  el.notice.querySelector(\".notice-text\").textContent = message;\n"+
		"  el.notice.hidden = false;\n"+
		"  if (focused) {\n    el.notice.querySelector(\".notice-dismiss\").focus({ preventScroll: true });\n  }\n}" {
		t.Errorf("showNotice no longer keeps focus that was on the notice it replaces on the new one, without scrolling, "+
			"or takes focus from elsewhere:\n%s", got)
	}
	// A dropped file's refusal is put up in place of the notice before it,
	// which is taken down only when nothing replaced it: taken down first,
	// focus resting on its dismiss button would fall to the top of the page.
	drop := words(withoutComments(jsBlock(t, script, `document.addEventListener("drop", (event) => {`, "});")))
	if !strings.HasSuffix(drop, `field.classList.remove("dragging"); const before = el.notice.querySelector(".notice-text"); `+
		`setFile(field, event.dataTransfer.files[0]); if (before && noticeShows(before)) { clearNotice(); } });`) ||
		strings.Count(drop, "clearNotice()") != 1 {
		t.Errorf("a dropped file no longer has its refusal put up in place of the notice before it, "+
			"or takes that notice down first:\n%s", drop)
	}
}

// Back from Stripe and Start again do what the Pass page's comments say. The
// address is tidied to /pass first, so a reload or a shared link says nothing
// again and sets nothing again. Back with ?cancelled offers the forms over the
// claim still collected (passStartedAgain) and back with ?paid takes them away,
// whatever was pressed before. Start again leaves the collecting going
// underneath, and a Stop that ended the collecting ends Start again with it.
func TestPassBackFromStripeAndStartAgain(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	render := withoutComments(jsFunction(t, script, "renderPass"))
	back := "  if (back) {\n    window.history.replaceState(null, \"\", \"/pass\");\n    renderedPath = \"/pass\";\n" +
		"    passStartedAgain = back === \"cancelled\";\n  }\n"
	if at := strings.Index(render, back); at < 0 || at > strings.Index(render, "await ") || strings.Count(render, "passStartedAgain") != 1 {
		t.Error("back from Stripe no longer tidies the address before anything else, or no longer sets Start again from the address alone")
	}
	if !strings.Contains(render, "  if (back === \"cancelled\") {\n    showNotice(\"Checkout cancelled. Nothing was charged.\");\n  }") {
		t.Error("back from Stripe's cancel no longer says the checkout was cancelled and nothing was charged")
	}
	stop := code[strings.Index(code, `const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');`):]
	stop = stop[:strings.Index(stop, "\n    return;\n  }")]
	if reset := strings.Index(stop, "      if (done) {\n        passStartedAgain = false;\n      }\n"); reset < 0 ||
		reset > strings.Index(stop, "if (!passPageShown()) {") {
		t.Error("a Stop that ended the collecting leaves Start again on, so later drawings still offer the forms over it")
	}
	restart := code[strings.Index(code, `if (event.target.closest('[data-action="restart-pass"]')) {`):]
	restart = restart[:strings.Index(restart, "\n    return;\n  }")]
	if restart != "if (event.target.closest('[data-action=\"restart-pass\"]')) {\n    event.preventDefault();\n"+
		"    passStartedAgain = true;\n    drawPass(lastPassInfo);\n    passFocus(\"form[data-pass] input\");" {
		t.Errorf("Start again no longer just offers the forms back, with collecting going on underneath:\n%s", restart)
	}
	if n := strings.Count(code, "passStartedAgain = "); n != 6 {
		t.Errorf("passStartedAgain is set %d times; want its declaration, back from Stripe, a checkout answered, "+
			"one followed to Stripe, a Stop that finished and Start again", n)
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
		!strings.Contains(store, "if (!kept || kept.code !== code) {\n    return false;\n  }") ||
		!strings.HasSuffix(store, "\n  return true;\n}") || strings.Count(store, "return ") != 2 {
		t.Error("storePass no longer reads back what it stored")
	}
	for _, name := range []string{"submitPass", "checkHeldPass"} {
		body := withoutComments(jsFunction(t, script, name))
		if !strings.Contains(body, "kept = storePass(res.pass, res.expires_at);") || !strings.Contains(body, ": PASS_NOT_KEPT") {
			t.Errorf("%s says the Pass is here without checking it was kept", name)
		}
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "keepClaimed")), "const kept = storePass(res.pass, res.expires_at);") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "passIssuedHTML")), "const here = Boolean(held && held.code === issued.code);") {
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
		held := strings.Index(block, "if (held) {\n      parts.push(passHeldHTML({ enabled: false }, held, shown, box));\n    }")
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
		if !strings.Contains(refresh, "if (!lastPassInfo.failed) {\n    return Promise.resolve(false);\n  }\n"+
			"  return renderPass(...selectors).then((drawn) => {") || !strings.HasSuffix(refresh, "\n    return drawn;\n  });\n}") {
			t.Error("refreshFailedPass no longer asks for the Pass page again after an answer on one that could not load, " +
				"drawing it as redrawPass does")
		}
		// Something other than the reader sets that drawing off, so it draws as
		// redrawPass does (renderPass, given selectors): focus on a control it
		// draws again, Copy the code or Use this code here say, stays there
		// without scrolling the page, rather than falling to the first of the
		// selectors.
		if !strings.Contains(withoutComments(jsFunction(t, script, "renderPass")),
			"if (selectors.length) {\n    redrawPass(info, ...selectors);\n  } else {\n    drawPass(info);\n  }") {
			t.Error("renderPass, given selectors, no longer draws as redrawPass does, so a refreshed page drops the focus on a control it draws again")
		}
		// The new drawing moves focus only when it dropped it to the top of the
		// document: focus the reader took elsewhere meanwhile, to the header or
		// the notice, stays where they put it.
		if !strings.Contains(refresh, "if (drawn && (!document.activeElement || document.activeElement === document.body)) {\n"+
			"      passFocus(...selectors);\n    }") {
			t.Error("refreshFailedPass pulls focus back into the Pass page from wherever the reader moved it")
		}
		// A Pass found run out asks through showPassRunOut, which says its
		// notice again for the drawing that comes
		// (TestPassRunOutIsSaidForThePageBesideIt).
		for name, want := range map[string]int{"submitPass": 4, "redeemPassReissue": 1, "checkHeldPass": 3} {
			body := withoutComments(jsFunction(t, script, name))
			if n := strings.Count(body, "refreshFailedPass(") + strings.Count(body, "showPassRunOut("); n != want {
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
		// empty in its declaration, on leaving the page, by these two,
		// pressed here or in another tab (stopForPassForget, for a "code:"
		// mark), and once a Pass is held again (the next subtest).
		code := withoutComments(script)
		remove := code[strings.Index(code, `if (event.target.closest('[data-action="forget-pass"]')) {`):]
		remove = remove[:strings.Index(remove, "\n  }\n")]
		stop := withoutComments(jsFunction(t, script, "stopForPassForget"))
		if !strings.Contains(withoutComments(jsFunction(t, script, "forgetBrowser")), "passLost = \"\";") ||
			!strings.Contains(remove, "passLost = \"\";") || strings.Count(code, "passLost = \"\";") != 8 ||
			!regexp.MustCompile(`if \(code\) \{[^}]*passLost = "";`).MatchString(stop) {
			t.Error("Forget this browser or Remove the Pass, here or in another tab, leaves what the page said about a Pass that has gone")
		}
	})
	t.Run("what was said about a Pass gone goes once one is held again", func(t *testing.T) {
		// Beside a Pass held, the note would say the Pass here has gone. It
		// goes with every Pass this tab keeps (storePass: a code typed in, run
		// out or not, a claim's or a reissue code's, a check's date), and with
		// one another tab stores, heard as it is stored or found when the
		// back-forward cache brings the page back, but not with one this
		// browser would not keep, nor while none is held.
		store := withoutComments(jsFunction(t, script, "storePass"))
		if !strings.Contains(store, "const kept = storedPass();\n  if (!kept || kept.code !== code) {\n    return false;\n  }\n"+
			"  passLost = \"\";\n  keepShownCode(code);\n  return true;\n}") || strings.Count(store, "passLost") != 1 {
			t.Error("a Pass this tab keeps leaves beside it what the page said about a Pass that has gone, " +
				"or one it could not keep takes that away")
		}
		follow := withoutComments(jsFunction(t, script, "followPassStorage"))
		if !strings.Contains(follow, "if (now) {\n    passLost = \"\";\n    keepShownCode(now.code);\n  }") || strings.Count(follow, "passLost") != 1 {
			t.Error("a Pass another tab stores leaves beside it what this tab's Pass page said about a Pass that has gone, " +
				"or a removal there takes that away")
		}
		// A page the back-forward cache kept hears a storage event sent
		// meanwhile only once it is shown, if at all, so it looks for itself
		// before it draws the Pass page again.
		show := withoutComments(jsBlock(t, script, `window.addEventListener("pageshow", (event) => {`, "});"))
		if !strings.Contains(show, "if (event.persisted && state.route.view === \"pass\") {\n"+
			"    if (storedPass()) {\n      passLost = \"\";\n    }\n    render();\n  }") || strings.Count(show, "passLost") != 1 ||
			strings.Count(show, "storedPass(") != 1 {
			t.Error("a Pass page brought back from the back-forward cache draws, beside a Pass another tab stored meanwhile, " +
				"what it said about a Pass that has gone")
		}
	})
	t.Run("what was said about a Pass gone offers nothing", func(t *testing.T) {
		// The note stays while the page draws a purchase being collected, a
		// code the browser could not keep or sales paused, none of which has
		// Buy beside it, so it says why the Pass went and points to nothing:
		// the Buy form, when it is drawn, says the rest.
		notes := regexp.MustCompile(`\bpassLost = (.+);`).FindAllStringSubmatch(withoutComments(script), -1)
		said := 0
		for _, note := range notes {
			if note[1] == `""` {
				continue
			}
			said++
			if !strings.Contains(note[1], "${PASS_GONE_REASONS}") || regexp.MustCompile(`(?i)\b(buy|below)\b`).MatchString(note[1]) {
				t.Errorf("what the page says about a Pass gone points to something it may not draw beside it: %s", note[1])
			}
		}
		if said != 4 {
			t.Errorf("the page says a Pass has gone in %d places; want a check, a renewal's check, Renew and a post's answer", said)
		}
	})
	t.Run("Renew found no longer valid points to no Buy form", func(t *testing.T) {
		// The server refuses Renew for a Pass it no longer knows with "buy a
		// new one", but the page that follows may draw no Buy form: a
		// purchase being collected, or a code the browser could not keep,
		// holds it back. So Renew's notice is in the page's own words, saying
		// why the Pass went, as what it says of the Pass gone does, before any
		// refusal is said in the server's words; and no notice the page words
		// itself, on the Pass page or for a post, points to buying.
		submit := withoutComments(jsFunction(t, script, "submitPass"))
		renew := strings.Index(submit, "if (kind === \"renew\" && refusal === \"invalid\") {\n    drawPass(lastPassInfo);")
		if renew < 0 || !strings.Contains(submit[renew:], "passFocus(\"form[data-pass] input\");\n"+
			"    showPassNotice(`This Pass is no longer valid, so it has been removed from this browser. ${PASS_GONE_REASONS}`);\n"+
			"    notePassHeld();\n    return;\n  }") ||
			strings.Count(submit, "passErrorText(") != 1 || strings.Index(submit, "passErrorText(") < renew {
			t.Error("Renew answered that the Pass is no longer valid says so in the server's words, which say to buy a new one")
		}
		said := regexp.MustCompile(`(?:showPassNotice|showNotice)\((.*?)\);|passNotice = (.*?);`)
		for name, text := range map[string]string{
			"the Pass page":   words(withoutComments(passSection(t, script))),
			"a post's answer": words(withoutComments(submitHandler(t, script))),
		} {
			found := 0
			for _, notice := range said.FindAllStringSubmatch(text, -1) {
				found++
				if regexp.MustCompile(`(?i)\bbuy\b`).MatchString(notice[1] + notice[2]) {
					t.Errorf("%s says, in its own words, to buy a Pass, where no Buy form may be drawn: %s", name, notice[0])
				}
			}
			if found == 0 {
				t.Errorf("no notice found in %s; this test reads them", name)
			}
		}
	})
	t.Run("a panel drawn already filled is focused", func(t *testing.T) {
		if !strings.Contains(withoutComments(jsFunction(t, script, "showClaimed")), `focusPassPanel("pass-issued");`) ||
			!strings.Contains(jsFunction(t, script, "passIssuedHTML"), `id="pass-issued" tabindex="-1"`) {
			t.Error("a code just handed over is drawn without moving focus to it")
		}
		// A code handed over while the reader was elsewhere (a claim or reissue
		// answer that arrived after they left) is focused when they come back,
		// once: the drawing that first shows it marks it shown, so no later
		// drawing or poll moves focus to it again.
		render := withoutComments(jsFunction(t, script, "renderPass"))
		fresh := strings.Index(render, "const fresh = Boolean(passJustIssued && !passJustIssued.shown);")
		drawn := strings.Index(render, "drawPass(info);")
		focused := strings.Index(render, "if (fresh) {\n    focusPassPanel(\"pass-issued\");\n  }")
		if fresh < 0 || drawn < fresh || focused < drawn {
			t.Error("a code handed over while the reader was away is drawn without moving focus to it when they come back")
		}
		if !strings.Contains(withoutComments(jsFunction(t, script, "passViewHTML")), "parts.push(passIssuedHTML(issued, held, box));\n    issued.shown = true;\n") {
			t.Error("a code just handed over is not marked shown once drawn, so every drawing and poll moves focus back to it")
		}
	})
	t.Run("a poll that changes nothing draws nothing", func(t *testing.T) {
		// Every 202 pending would otherwise draw the page again, hiding a code
		// shown with Show the code and unticking the 18+ box on Renew while a
		// renewal's checkout is open. Only the note of a failed attempt goes.
		claimed := withoutComments(jsFunction(t, script, "showClaimed"))
		same := strings.Index(claimed, "if (!news && !unseen && claim === (lastPassInfo.claim === true)) {\n"+
			"    const note = document.getElementById(\"pass-collect-note\");\n    if (note) {\n      note.textContent = \"\";\n    }\n    return;\n  }")
		if unseen := strings.Index(claimed, "const unseen = Boolean(passJustIssued && !passJustIssued.shown);"); same < 0 || unseen > same ||
			strings.Index(claimed, "redrawPass(") < same {
			t.Error("a claim's answer that changes nothing draws the Pass page again")
		}
	})
	t.Run("the page says where the reader is", func(t *testing.T) {
		render := withoutComments(jsFunction(t, script, "renderPass"))
		if said := strings.Index(render, "announce(\"educhan Pass\");"); said < 0 || said < strings.Index(render, "drawPass(info);") ||
			said > strings.Index(render, "if (back === \"cancelled\") {") {
			t.Error("the Pass page is drawn without its title, or without saying so to a screen reader")
		}
	})
	t.Run("back from Stripe, nothing is said to be missing while a lost answer is asked again", func(t *testing.T) {
		// A nonce kept from a claim whose answer was lost is asked for again
		// (collectPass) even with nothing left under the claim, and may bring
		// the code, so "nothing waiting" is not said meanwhile.
		render := withoutComments(jsFunction(t, script, "renderPass"))
		if !strings.Contains(render, "} else if (back === \"paid\" && info.enabled && !info.claim && !passNoncePending(PASS_NONCE_STORAGE_KEY)) {\n"+
			"    showNotice(\n      \"There's nothing waiting to be collected in this browser.") {
			t.Error("back from Stripe, the Pass page says nothing is waiting while it asks again for a claim whose answer was lost")
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
	t.Run("the price says whether tax is in it, in the server's words", func(t *testing.T) {
		// GET /api/pass says "included" for a Price with the tax in it and
		// "added" for one Stripe adds it to at checkout, where it can come to
		// nothing, and says nothing of tax without Stripe Tax.
		intro := words(withoutComments(jsFunction(t, script, "passIntroHTML")))
		if !strings.Contains(intro, "const priced = info.tax === \"included\" ? `${price} for ${term}, tax included` "+
			": info.tax === \"added\" ? `${price} plus any tax for ${term}` : `${price} for ${term}`;") {
			t.Error("the price no longer says whether tax is in it, or says so without the server's word")
		}
		// Without a price from the server (Stripe could not be asked, or
		// answered with a Price the Pass can't be sold at, or sales are
		// paused) no cost is drawn at all, though priced always has words.
		if !strings.Contains(intro, "const cost = price ? `<p class=\"verify-note\"><strong>${esc(priced)}</strong>, paid once. "+
			"It doesn't renew by itself.</p>` : \"\";") {
			t.Error("the cost line is not the price in the server's words, or is drawn without a price")
		}
		for behavior, word := range passTaxWords {
			if !strings.Contains(intro, `info.tax === "`+word+`"`) {
				t.Errorf("the page does not know the server's word %q for a Price whose tax behavior is %s", word, behavior)
			}
		}
		if strings.Contains(script, "automatic_tax") {
			t.Error("app.js still reads automatic_tax, which the server no longer sends")
		}
	})
	t.Run("a refusal no other branch knows is said in the server's words, and its button given back", func(t *testing.T) {
		// Checkout's 503 for a Price the Pass can't be sold at, and its 502
		// while Stripe can't be asked, reach the reader only this way, so
		// passErrorText says something else only for a server that did not
		// answer and a cooldown's wait, and nothing is left pressed. It is
		// said in view, however far down the page the button was
		// (showPassNotice).
		submit := words(withoutComments(jsFunction(t, script, "submitPass")))
		if !strings.Contains(submit, "showPassNotice(refusedForAge ? PASS_AGE_REFUSAL : passErrorText(failed)); "+
			"button.disabled = false; button.textContent = label; refocus(button);") {
			t.Error("a refused checkout is not said, or leaves its button pressed")
		}
		text := withoutComments(jsFunction(t, script, "passErrorText"))
		if strings.Count(text, "return ") != 3 || !strings.HasSuffix(words(text), "const said = err.message.trim(); "+
			"const text = said.charAt(0).toUpperCase() + said.slice(1); return /[.!?]$/.test(text) ? text : `${text}.`; }") {
			t.Error("passErrorText says something other than the server's own words for some refusals")
		}
		if strings.Contains(withoutComments(script), passPriceUnsold) {
			t.Error("the page has its own copy of the server's refusal of a Price the Pass can't be sold at")
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
// out of storage, but only the Pass, and what goes with it, is followed in the
// browser's other tabs (followPassStorage, followPassForget, and
// keepPassCheckoutMark, which puts back only the mark of a checkout the tab has
// on its way): a tab already open keeps a key it made (pagePosterKey), the
// badge (state.verification) and the name in a form it drew until it is
// reloaded, and a post from it writes the key and the name back to storage, for
// every page loaded after it. The Verify page and the FAQ's shared-computer
// advice say so, and to close those tabs first. A tab that followed the rest as
// well would make that advice say less than the page does, so this fails then
// too, for the wording to change with it.
func TestForgetThisBrowserSaysWhatOtherTabsKeep(t *testing.T) {
	script := appScript(t)
	listeners := regexp.MustCompile(`addEventListener\("storage", (\w+)\)`).FindAllStringSubmatch(withoutComments(script), -1)
	if len(listeners) != 3 || listeners[0][1] != "followPassStorage" || listeners[1][1] != "followPassForget" ||
		listeners[2][1] != "keepPassCheckoutMark" {
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
	if !strings.Contains(view, "const issued = passJustIssued;") || !strings.Contains(view, "parts.push(passIssuedHTML(issued, held, box));") {
		t.Error("passViewHTML no longer draws the code just handed over")
	}
	if strings.Contains(view, "function passViewHTML(info, ") {
		t.Error("passViewHTML takes the code just handed over from its caller again, so a redraw loses it")
	}
	if n := strings.Count(withoutComments(script), "passJustIssued = null;"); n != 6 {
		t.Errorf("a code just handed over is dropped in %d places; want its declaration, leaving the Pass page or the document "+
			"once it is drawn, removing the Pass, forgetting the browser, another tab's Remove or Forget, "+
			"and educhan finding it no longer valid, here or in another tab, with no code it replaced left beside it", n)
	}
	// It goes once the reader leaves the Pass page, kept or not, once the page
	// has drawn it: one this browser could not keep is nowhere else, and its
	// panel says it won't be shown again once they leave, so the next person
	// at a shared computer must not find it by opening the Pass page. One the
	// page has not drawn, because its answer came while the reader was
	// elsewhere or they left before the drawing came, has been seen by
	// nobody, and stays to be drawn when they come back, kept or not: the
	// server hands a code over once, a reissue code is spent, and beside a
	// kept one may be the code of the Pass it took the place of, which is
	// nowhere else.
	render := withoutComments(jsFunction(t, script, "render"))
	if !strings.Contains(render, "if (state.route.view === \"pass\" && route.view !== \"pass\") {\n"+
		"    passLost = \"\";\n    forgetTypedCode();\n    forgetShownCode();\n  }") {
		t.Error("render no longer drops a code just handed over that the page has drawn once the reader leaves the Pass page, and only then")
	}
	// Shown is set as the page draws the code's panel, and only there.
	view = withoutComments(jsFunction(t, script, "passViewHTML"))
	if !strings.Contains(view, "if (issued) {\n    parts.push(passIssuedHTML(issued, held, box));\n    issued.shown = true;\n  }") ||
		strings.Count(withoutComments(script), ".shown = true") != 1 ||
		!strings.Contains(withoutComments(jsFunction(t, script, "keepClaimed")), "      reissued,\n      shown: false,\n    };") {
		t.Error("a code just handed over is not marked shown as the Pass page draws it, and only then")
	}
	// Nor when they leave the document, for another site, and come Back to a
	// page the back-forward cache kept: that page is shown as it was left
	// until it is drawn again, so the code's panel goes with it, kept or not.
	// One the page has not drawn yet is drawn there. A panel on the page goes
	// even when its code has gone already, dropped by another tab's Remove or
	// Forget that found no Pass to remove, whose drawing waits on the forget,
	// which would otherwise leave it in the page the back-forward cache keeps.
	shown := withoutComments(jsFunction(t, script, "forgetShownCode"))
	if shown != "function forgetShownCode() {\n  if (passJustIssued && passJustIssued.shown) {\n    passJustIssued = null;\n  }\n"+
		"  const panel = document.getElementById(\"pass-issued\");\n  if (panel) {\n    panel.remove();\n  }\n}" {
		t.Errorf("forgetShownCode no longer drops a code just handed over once drawn, kept or not, and only once drawn, "+
			"and takes the panel of one off the page whether or not its code has gone already:\n%s", shown)
	}
	hide := withoutComments(jsBlock(t, script, `window.addEventListener("pagehide", () => {`, "});"))
	if !strings.Contains(hide, "\n  forgetShownCode();\n") || strings.Count(withoutComments(script), "forgetShownCode()") != 3 {
		t.Error("leaving the document no longer drops a code just handed over that the page has drawn, " +
			"so a page brought back from the back-forward cache shows it again")
	}
	issued := withoutComments(jsFunction(t, script, "passIssuedHTML"))
	if !strings.Contains(words(issued), "educhan keeps no copy of it: this browser can show it again only until its data is cleared.") {
		t.Error("the panel of a code just handed over no longer says the browser can show it again")
	}
	if !strings.Contains(words(issued), "educhan keeps no copy of it, and this browser couldn't keep it either, so it won't be shown again once you leave this page.") {
		t.Error("the panel of a code this browser could not keep no longer says it goes once the reader leaves the page")
	}
}

// The panel of a code just handed over says what this browser holds as it is
// drawn, not what it held when the code came. A code it kept and no longer
// holds, another code having taken its place, typed in here over a Pass that
// ran out or stored by another tab, or its data cleared, stays on screen to be
// written down, but the panel no longer says the browser has it or will send
// it with posts, and says it won't be shown again once the reader leaves the
// page, as it won't; a code held that has run out is neither ready nor sent
// with posts until it is renewed, whether it came so or a post's answer marked
// it so since; and a code shown, not kept, holds Buy back as a Pass held does,
// while the code of a Pass the browser no longer holds does not, so Buy is
// offered beside it once no other Pass is held. A code educhan says is no
// longer valid, as the Pass held here, is worth nothing to write down, so it
// goes, and the panel keeps the other codes beside it, the code of a different
// Pass it took the place of and those carried from an earlier panel
// (TestPassCodesShownEarlierAreCarried), which may still work and be nowhere
// else, each said to be in this browser again once it is.
func TestPassIssuedPanelSaysWhatThisBrowserHolds(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	issued := withoutComments(jsFunction(t, script, "passIssuedHTML"))
	if !strings.Contains(issued, "function passIssuedHTML(issued, held, box) {\n  const here = Boolean(held && held.code === issued.code);") {
		t.Error("the panel of a code just handed over is no longer worded from the Pass this browser holds as it is drawn")
	}
	view := withoutComments(jsFunction(t, script, "passViewHTML"))
	if held, drawn := strings.Index(view, "const held = storedPass();"), strings.Index(view, "parts.push(passIssuedHTML(issued, held, box));"); held < 0 || drawn < held {
		t.Error("passViewHTML no longer gives the panel of a code just handed over the Pass stored here")
	}
	// "Ready" and "will send it with your posts" are said only when no case
	// below replaces them: the code is the one held, and it has not run out.
	if strings.Count(issued, "Your Pass is ready.") != 1 || !strings.Contains(issued, "let status = \"Your Pass is ready.\";") ||
		strings.Count(issued, "will send it with your posts") != 1 ||
		!strings.Contains(issued, "let said = \"This browser has it, and will send it with your posts.\";") ||
		strings.Count(issued, "this browser can show it again") != 1 ||
		len(regexp.MustCompile(`\bsaid =`).FindAllString(issued, -1)) != 7 {
		t.Error("the panel of a code just handed over says it is ready, sent with posts or shown again in a case that is not so")
	}
	if !strings.Contains(issued, "if (here && held.expired) {\n    status = \"Your Pass has run out.\";\n"+
		"    said = \"This browser has its code, but won't send it with your posts until the Pass is renewed.\";\n  }") {
		t.Error("the panel of a code just handed over for a Pass that has run out says it is ready, or sent with posts")
	}
	start, end := strings.Index(issued, "} else if (!here && issued.kept) {"), strings.Index(issued, "} else if (!here && held && !held.expired && ranOut) {")
	if start < 0 || end < start {
		t.Fatal("passIssuedHTML no longer words a code this browser kept and no longer holds apart from one it could not keep")
	}
	gone := issued[start:end]
	if !strings.Contains(gone, "status = \"This browser no longer has this code.\";") ||
		!strings.Contains(words(gone), `said = held ? "Another code has taken its place here, so it isn't sent with your posts." : "It isn't sent with your posts.";`) ||
		!strings.Contains(gone, "write = \"Write this code down or save it somewhere safe now if you still need it.\";") ||
		!strings.Contains(words(gone), `again = "educhan keeps no copy of it, and this browser no longer has it, so it won't be shown again once you leave this page.";`) ||
		!strings.Contains(words(gone), `need = ranOut ? "Its Pass has run out: you need it to renew that Pass, here or on another device." `+
			`: "You need it to use its Pass again, here or on another device.";`) {
		t.Error("the panel of a code this browser kept and no longer holds says it has it, or not that it won't be shown again")
	}
	// The code it took the place of is said to be taken now while the code is
	// held here, and to have been taken once it is not.
	if !strings.Contains(issued, "\n  const took = here ? \"takes\" : \"took\";\n") || strings.Count(issued, "${took}") != 2 {
		t.Error("the panel of a code just handed over no longer says whether it takes or took the place of the code beside it")
	}
	// The code held here is shown by the held Pass's panel, so it offers Show
	// the code for every code but the one this panel shows, kept as it came or
	// typed back in since.
	if !strings.Contains(view, "\n  const shown = Boolean(held && issued && issued.code === held.code);\n") ||
		strings.Count(view, "passHeldHTML(") != 2 || strings.Count(view, ", held, shown, box));") != 2 {
		t.Error("the held Pass's panel offers Show the code for the code a panel above it shows, or none for one it does not")
	}
	// Buy is held back by a Pass held here, or a code shown that the browser
	// could not keep, the panel's own or one carried from an earlier panel, not
	// by the code of one it no longer holds, nor one found no longer valid.
	if !strings.Contains(view, "\n  const unkept = Boolean(issued && ((issued.code && !issued.kept) || issued.earlier.some((old) => !old.kept)));\n"+
		"  const has = Boolean(held || unkept);\n") {
		t.Error("the Pass page holds back Buy for a code just handed over that this browser no longer holds, " +
			"or offers it beside a code carried that the browser could not keep")
	}
	// Found no longer valid as the Pass held here, by Check it again, the check
	// after a reissue code or a renewal, Renew or a post's answer, here or in
	// another tab (TestPassRemovedInAnotherTabLeavesThisOne), the code goes,
	// and the code it took the place of stays, if there is one, and so does
	// each code carried from an earlier panel, unless it is the code found no
	// longer valid, typed back in say, which goes too: a code worth nothing is
	// never left on the panel to be written down, and no other code goes with
	// it. With no code left, the panel goes.
	forget := withoutComments(jsFunction(t, script, "forgetInvalidCode"))
	if forget != "function forgetInvalidCode(code) {\n  if (!passJustIssued) {\n    return;\n  }\n"+
		"  if (passJustIssued.code === code) {\n    passJustIssued.code = \"\";\n  }\n"+
		"  if (passJustIssued.replaced === code) {\n    passJustIssued.replaced = \"\";\n  }\n"+
		"  const carried = passJustIssued.earlier.findIndex((earlier) => earlier.code === code);\n"+
		"  if (carried >= 0) {\n    passJustIssued.earlier.splice(carried, 1);\n  }\n"+
		"  if (!passJustIssued.code && !passJustIssued.replaced && !passJustIssued.earlier.length) {\n    passJustIssued = null;\n  }\n}" {
		t.Errorf("forgetInvalidCode no longer takes every code found no longer valid off its panel, "+
			"keeping every code beside it that is not:\n%s", forget)
	}
	for name, want := range map[string]string{
		"recheckPass":   "if (reason === \"invalid\") {\n    forgetPass();\n    forgetInvalidCode(held.code);",
		"checkHeldPass": "} else if (reason === \"invalid\") {\n    forgetPass();\n    forgetInvalidCode(held.code);",
		"submitPass":    "} else if (kind === \"renew\" && refusal === \"invalid\") {\n    forgetPass();\n    forgetInvalidCode(held.code);",
	} {
		if !strings.Contains(withoutComments(jsFunction(t, script, name)), want) {
			t.Errorf("%s removes a Pass educhan says is no longer valid and leaves a code just handed over for it on its panel", name)
		}
	}
	if !strings.Contains(withoutComments(submitHandler(t, script)), "if (sent && created.pass === \"invalid\") {\n      forgetPass();\n      forgetInvalidCode(sent.code);") {
		t.Error("a post answered that its Pass is no longer valid leaves a code just handed over for it to be drawn on the next visit")
	}
	if n := len(regexp.MustCompile(`\bforgetInvalidCode\(`).FindAllString(code, -1)); n != 6 {
		t.Errorf("forgetInvalidCode( appears %d times; want its definition, the four answers that say a Pass held here is "+
			"no longer valid, and another tab's removal of the Pass (followPassStorage)", n)
	}
	// With its code gone, the panel says so, and shows only the codes beside
	// it, the code it took the place of and those carried, with their words,
	// and no Copy the code, which would find no code to copy.
	start = strings.Index(issued, "if (!issued.code) {")
	if start < 0 {
		t.Fatal("passIssuedHTML no longer draws a panel whose code was found no longer valid apart")
	}
	left := issued[start:]
	left = left[:strings.Index(left, "\n  }\n")]
	if !strings.Contains(left, "${replaced}") || strings.Contains(left, "class=\"pass-code\"") || strings.Contains(left, "copy-pass") ||
		!strings.Contains(left, "\n      <p><strong>The new code handed over here is no longer valid, so this browser no longer has it.</strong></p>\n") ||
		!strings.Contains(words(left), "Write that code down now if you still need it.") ||
		!strings.Contains(words(left), "so it won't be shown again once you leave this page.") {
		t.Error("the panel of a code found no longer valid shows that code, or does not say it is gone, " +
			"or leaves out the code it took the place of")
	}
	// The code it took the place of is worded for what this browser holds as
	// it is drawn too, whether the panel's own code is there or gone: typed
	// back in here, or stored by another tab, it is said to be in this browser
	// again, in the words a code carried says it with (PASS_CODE_BACK_HTML),
	// whose held Pass's panel can show it again, and neither to be written down
	// to go on using it nor to be gone from the browser.
	back := strings.Index(issued, "\n  const back = Boolean(held && held.code === issued.replaced);\n"+
		"  const use = back ? \"\" : \": write its code down to go on using it\";\n"+
		"  const heldAgain = back ? ` ${PASS_CODE_BACK_HTML}` : \"\";\n")
	if back < 0 || back > strings.Index(issued, "if (issued.replaced && issued.reissued) {") ||
		!strings.Contains(words(issued), "<p>It ${took} the place of the code this browser had, "+
			"<span class=\"pass-code-old\">${esc(issued.replaced)}</span>. If that code was for another Pass, that Pass isn't "+
			"affected${use}. If it was for the Pass this reissue code was for, it no longer works.${heldAgain}</p>`;") ||
		!strings.Contains(words(issued), "<p>It ${took} the place of the Pass this browser had, whose code is "+
			"<span class=\"pass-code-old\">${esc(issued.replaced)}</span>. That Pass isn't affected${use}.${heldAgain}</p>`;") ||
		strings.Count(issued, "write its code down") != 1 || strings.Count(issued, "${heldAgain}") != 2 {
		t.Error("the code a code handed over here took the place of, typed back in or stored by another tab, is not said " +
			"to be in this browser again, or is still to be written down to go on using it")
	}
	if got := words(regexp.MustCompile("(?m)^const PASS_CODE_BACK_HTML = `([^`]*)`;$").FindString(script)); got !=
		"const PASS_CODE_BACK_HTML = `<strong>This browser has that code again.</strong> educhan keeps no copy of it, and "+
			"this browser can show it below only until its data is cleared, so write it down if you still need it.`;" {
		t.Errorf("a code back in this browser is said to be so in other words: %q", got)
	}
	if !strings.Contains(left, "const keep =\n      issued.replaced && !back\n        ? `") ||
		!strings.Contains(words(left), "? `<p><strong>Write that code down now if you still need it.</strong> educhan keeps no "+
			"copy of it, and this browser no longer has it, so it won't be shown again once you leave this page.</p>` : \"\";") ||
		strings.Contains(left, "This browser has that code again") ||
		!strings.Contains(left, "\n      ${replaced}\n      ${keep}\n      ${earlier}\n    </div>`;") {
		t.Error("the panel of a code found no longer valid says the code it took the place of is gone from this browser, " +
			"even when the browser holds it again, or leaves out the codes carried from an earlier panel")
	}
}

// A second code handed over on the same visit, by a claim or a reissue code,
// takes the place of the first's panel, which may show codes the server will
// not show again: the first code, kept or not, and the code of the Pass it took
// the place of. Each is carried onto the new panel, with words of its own, so
// none is lost before the reader leaves the page, Remove the Pass or Forget
// this browser takes it, or educhan finds it no longer valid
// (TestPassIssuedPanelSaysWhatThisBrowserHolds), even the code held as it
// comes, when the new one could not be kept, since another tab can yet store
// another in its place. The new code and the code it takes the place of, which
// the new panel shows, are not carried, and no code is carried twice, so none
// is drawn twice and no id is either.
func TestPassCodesShownEarlierAreCarried(t *testing.T) {
	script := appScript(t)
	keep := withoutComments(jsFunction(t, script, "keepClaimed"))
	if !strings.Contains(keep, "    const replaced = kept && held ? held.code : \"\";\n    passJustIssued = {\n"+
		"      code: res.pass,\n      expires: res.expires_at,\n      kept,\n      replaced,\n"+
		"      earlier: passCodesCarried(passJustIssued, res.pass, replaced),\n      reissued,\n") {
		t.Error("a code handed over on the visit of an earlier one no longer carries the codes the earlier panel showed")
	}
	// The held code is left to the new panel only when that panel shows it as
	// the code the new one took the place of: beside a new code this browser
	// could not keep, it is carried, since another tab can yet store another
	// in its place.
	carry := withoutComments(jsFunction(t, script, "passCodesCarried"))
	if carry != "function passCodesCarried(old, code, replaced) {\n  const carried = [];\n  if (!old) {\n    return carried;\n  }\n"+
		"  const shown = [\n    { code: old.code, kept: old.kept, was: \"issued\" },\n"+
		"    { code: old.replaced, kept: true, was: old.reissued ? \"reissued\" : \"replaced\" },\n    ...old.earlier,\n  ];\n"+
		"  for (const earlier of shown) {\n    const drawn = earlier.code === code || earlier.code === replaced;\n"+
		"    if (earlier.code && !drawn && !carried.some((other) => other.code === earlier.code)) {\n      carried.push(earlier);\n    }\n  }\n"+
		"  return carried;\n}" {
		t.Errorf("passCodesCarried no longer carries every code an earlier panel showed but the new code, the code it "+
			"took the place of and a code carried already:\n%s", carry)
	}
	// Each is drawn on the new panel, whether its own code is there or found
	// no longer valid, and only there.
	issued := withoutComments(jsFunction(t, script, "passIssuedHTML"))
	if !strings.Contains(issued, "\n  const earlier = issued.earlier.map((old) => passEarlierHTML(old, held)).join(\"\");\n") ||
		strings.Count(issued, "${earlier}") != 2 || !strings.Contains(issued, "\n      ${replaced}\n      ${earlier}\n      <div class=\"actions\">") ||
		strings.Count(withoutComments(script), "passEarlierHTML(") != 2 {
		t.Error("the panel of a code just handed over no longer draws the codes carried from an earlier panel")
	}
	// Worded for what it was there, and for whether this browser holds it as
	// it is drawn: not held, it won't be shown again once the reader leaves,
	// and a code it took the place of is to be written down to go on using
	// it; held, typed back in say, it is said to be in this browser again,
	// whose held Pass's panel can show it.
	earlier := words(withoutComments(jsFunction(t, script, "passEarlierHTML")))
	for _, want := range []string{
		"function passEarlierHTML(old, held) { const code = `<span class=\"pass-code-old\">${esc(old.code)}</span>`; " +
			"const here = Boolean(held && held.code === old.code); if (old.was === \"issued\") {",
		"const keep = here ? `<strong>This browser has it now.</strong> educhan keeps no copy of it, and this browser can " +
			"show it below only until its data is cleared, so write it down if you still need it.` " +
			": `<strong>Write it down now if you still need it.</strong> educhan keeps no copy of it, and this browser " +
			"doesn't have it, so it won't be shown again once you leave this page.`; " +
			"return ` <p>Earlier on this page, the code ${code} was handed over here. ${keep}</p>`; }",
		"const use = here ? \"\" : \": write its code down to go on using it\"; " +
			"const was = old.was === \"reissued\" ? `Earlier on this page, a reissue code's new code took the place of the code " +
			"this browser had, ${code}. If that code was for another Pass, that Pass isn't affected${use}. " +
			"If it was for the Pass that reissue code was for, it no longer works.` " +
			": `Earlier on this page, a code handed over here took the place of the Pass this browser had, whose code is " +
			"${code}. That Pass isn't affected${use}.`;",
		"const keep = here ? ` ${PASS_CODE_BACK_HTML}` : \"\"; return ` <p>${was}${keep}</p>`; }",
	} {
		if !strings.Contains(earlier, want) {
			t.Errorf("a code carried from an earlier panel is no longer worded for what it was there and whether this "+
				"browser holds it, wanting:\n%s", want)
		}
	}
	if strings.Contains(earlier, "id=") || strings.Contains(earlier, "copy-pass") {
		t.Error("a code carried from an earlier panel gives an id or a Copy the code of its own, which a second carried code would repeat")
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
	if !strings.Contains(withoutComments(jsFunction(t, script, "keepClaimed")), `const replaced = kept && held ? held.code : "";`) ||
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
		"PASS_NONCE_STORAGE_KEY":              {"renderPass", "collectPass", "forgetPassClaim", "stopForPassForget"},
		"PASS_REISSUE_NONCE_STORAGE_KEY":      {"redeemPassReissue", "forgetPassClaim", "stopForPassForget"},
		"PASS_REISSUE_UNANSWERED_STORAGE_KEY": {"redeemPassReissue", "forgetPassClaim", "stopForPassForget"},
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
	for _, name := range []string{"keepClaimed", "forgetInvalidCode", "keepShownCode"} {
		justReads += strings.Count(withoutComments(jsFunction(t, script, name)), "passJustIssued.code")
	}
	if n := strings.Count(code, "passJustIssued.code"); n != justReads {
		t.Errorf("passJustIssued.code is read %d times, %d of them in keepClaimed, forgetInvalidCode and keepShownCode; "+
			"want none elsewhere", n, justReads)
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
		"if (emptyBoxes) {\n    passCodeForgets += 1;\n    passNoticeForgets += 1;\n    dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);\n" +
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
	// waits for every other forget on its way too. Just before it comes off,
	// the mark says what the answer was, so the browser's other tabs read it
	// from its removal, and an answer that dropped the claim takes off a claim
	// notice here said for a forget that went before this one
	// (heedPassClaimGone).
	if !strings.Contains(forget, "waitOnPassForget(\n    mark,\n    emptyBoxes,\n    request.then((done) => {\n"+
		"      storageSet(mark, `${pressed}:${at}:${done ? PASS_FORGET_DROPPED : PASS_FORGET_KEPT}`);\n      try {\n"+
		"        localStorage.removeItem(mark);\n      } catch {\n\n      }\n      if (done) {\n        heedPassClaimGone(at);\n      }\n    })\n  );") ||
		!strings.Contains(forget, "return passForgetting.then(() => request);\n}") ||
		strings.Count(forget, "storageSet(mark, ") != 2 || strings.Count(forget, "heedPassClaimGone(") != 1 {
		t.Error("forgetPassClaim no longer leaves the request it sent for the page to wait on, or no longer says in its mark, " +
			"just before taking it off, what the answer was, or no longer takes off a claim notice said for an earlier forget " +
			"once its own answer drops the claim")
	}
	if !strings.Contains(code, "\nconst PASS_FORGET_DROPPED = \"dropped\";\nconst PASS_FORGET_KEPT = \"kept\";\n") ||
		strings.Count(code, "PASS_FORGET_DROPPED") != 3 || strings.Count(code, "PASS_FORGET_KEPT") != 2 {
		t.Error("a forget's answer is written into its mark, or read from it, other than as forgetPassClaim writes it and " +
			"heedPassForgetAnswer reads it")
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
	// Remove the Pass, Forget this browser and Stop collecting it here read
	// passForgets no more: each notes when it was pressed (at), and puts up no
	// claim notice once this tab has learned that nothing is left to collect
	// since then (passClaimGone), as forgetPassOrder does; Remove and Forget
	// read passNoticeForgets beside the record their notice is noted beside,
	// once, and compare it once, to put up no held notice once a forget that
	// takes one off has come meanwhile, as the post's notice does, and
	// collectPass's and redeemPassReissue's once the Pass held is checked
	// (TestPassNoticeGoesWithWhatItSaid).
	pressed := map[string]int{}
	for opener, want := range map[string][2]int{
		`if (event.target.closest('[data-action="forget-pass"]')) {`:                 {2, 1},
		`if (event.target.closest('[data-action="forget-browser"]')) {`:              {2, 1},
		`const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');`: {0, 1},
	} {
		block := code[strings.Index(code, opener):]
		block = block[:strings.Index(block, "\n    return;\n  }")]
		notices := len(regexp.MustCompile(`\bpassNoticeForgets\b`).FindAllString(block, -1))
		gone := len(regexp.MustCompile(`\bpassClaimGone\b`).FindAllString(block, -1))
		if strings.Contains(block, "passForgets") || strings.Count(block, "const at = Date.now();") != 1 ||
			notices != want[0] || gone != want[1] || (want[0] > 0 && !strings.Contains(block, "const forgets = passNoticeForgets;")) {
			t.Errorf("%s reads passForgets, or does more with passNoticeForgets (%d uses) or passClaimGone (%d uses) than "+
				"read and compare them for its notices", opener, notices, gone)
		}
		pressed["passNoticeForgets"] += notices
		pressed["passClaimGone"] += gone
	}
	pressed["passNoticeForgets"] += len(regexp.MustCompile(`\bpassNoticeForgets\b`).FindAllString(withoutComments(submitHandler(t, script)), -1))
	for counter, users := range map[string][]string{
		"passForgets":        {"forgetPassClaim", "collectPass", "stopForPassForget", "redeemPassReissue", "submitPass", "passForgetsSettled"},
		"passCodeForgets":    {"forgetPassClaim", "stopForPassForget", "passForgotten", "recheckPass", "redeemPassReissue", "checkHeldPass", "submitPass", "passForgetsSettled"},
		"passCodeForgetting": {"settlePassForgetting", "passForgotten"},
		"passNoticeForgets":  {"forgetPassClaim", "dropForgottenPassNotice", "collectPass", "redeemPassReissue"},
		"passClaimGone":      {"heedPassClaimGone", "forgetPassOrder"},
	} {
		uses, named := 1+pressed[counter], strings.Join(users, ", ")
		if pressed[counter] > 0 {
			named += ", Remove the Pass, Forget this browser, Stop collecting it here, the post's notice"
		}
		for _, name := range users {
			uses += len(regexp.MustCompile(`\b`+counter+`\b`).FindAllString(withoutComments(jsFunction(t, script, name)), -1))
		}
		if n := len(regexp.MustCompile(`\b`+counter+`\b`).FindAllString(code, -1)); n != uses {
			t.Errorf("%s appears %d times; want its declaration and its uses in %s, %d in all", counter, n, named, uses)
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
	if browser := withoutComments(jsFunction(t, script, "forgetBrowser")); !strings.Contains(browser, "const dropped = forgetPassClaim(\"browser\", at);") ||
		!strings.HasSuffix(browser, "\n  return dropped;\n}") {
		t.Error("Forget this browser no longer drops a purchase still being collected, here and in the browser's other tabs")
	}
	remove := code[strings.Index(code, `if (event.target.closest('[data-action="forget-pass"]')) {`):]
	remove = remove[:strings.Index(remove, "\n  }\n")]
	if !strings.Contains(remove, "const dropped = forgetPassClaim(\"remove\", at);") {
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
	dropped := strings.Index(stop, "const dropped = forgetPassClaim(\"stop\", at);")
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
	followStop := withoutComments(jsFunction(t, script, "stopForPassForget"))
	if !strings.Contains(code, collectButtons) || strings.Index(draw, offWhileStopping) < strings.Index(draw, "el.main.innerHTML = passViewHTML(info);") ||
		!strings.Contains(followStop, "for (const button of el.main.querySelectorAll(PASS_COLLECT_BUTTONS)) {\n    button.disabled = true;\n") ||
		strings.Index(followStop, "button.disabled = true;") < strings.Index(followStop, "const waited = heedPassForget(key, mark);") ||
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
	// What it says is a claim notice, said for the press, for a later
	// forget's answer to take off and a Pass stored in another tab to leave,
	// and is not said once this tab has learned that nothing is left to
	// collect since the press (TestPassNoticeGoesWithWhatItSaid).
	failed := regexp.MustCompile(`if \(!done && visit === passVisit\) \{\s*collectPass\(visit\);\s*\}`)
	said := regexp.MustCompile(`if \(!done\) \{\s*refocus\(stopClaim\);\s*if \(passClaimGone <= at\) \{\s*` +
		`showPassNotice\("Couldn't reach educhan, so this browser is still collecting it\.[^"]*"\);\s*noteClaimNotice\(at\);\s*\}\s*return;\s*\}`)
	if !failed.MatchString(stop) || !said.MatchString(stop) {
		t.Error("a Stop collecting it here that could not reach educhan no longer goes on collecting, as it says, or no " +
			"longer says so as a claim notice, or says so once this tab has learned that nothing is left to collect")
	}
	// Done, the page is drawn again at once as having nothing to collect, so
	// the collecting panel, Check now and Stop go with what they collected.
	done := regexp.MustCompile(`refocus\(stopClaim\);\s*if \(passClaimGone <= at\) \{\s*showPassNotice\([^)]*\);\s*` +
		`noteClaimNotice\(at\);\s*\}\s*return;\s*\}\s*` +
		`drawPass\(\{ \.\.\.lastPassInfo, claim: false \}\);\s*passFocus\("form\[data-pass\] input", "#pass-held"\);\s*` +
		`showPassNotice\(\s*"This browser has stopped collecting it\.`)
	if !done.MatchString(stop) {
		t.Error("a Stop collecting it here that worked no longer draws the page again with nothing left to collect")
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
// and renewing it hands over no code, so the server refuses the retry. The
// page then checks the held code, as after a renewal it collects, and says a
// date that moved on in words that claim nothing about where the time came
// from; a reissue code refused then is not said to need replacing. A check
// turned away by the per-address cooldown a campus shares is asked once more,
// after the wait the server names.
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
	if !strings.Contains(collect, `const found = again ? await passAfterLostAnswer() : "";`) {
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
	retried := `if (failed.message === PASS_REISSUE_SPENT && again) { showPassNotice( "This reissue code didn't work this time. ` +
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
	// clipboard, by anything copied or cut in this tab since, and when
	// another tab takes the code copied out of storage
	// (TestPassRemovedInAnotherTabLeavesThisOne).
	if n := len(regexp.MustCompile(`\bpassCodeCopied = `).FindAllString(code, -1)); n != 5 ||
		!strings.Contains(code, "\nlet passCodeCopied = \"\";\n") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "followPassStorage")),
			"if ((event.key === null || was === passCodeCopied) && (!now || now.code !== passCodeCopied)) {\n    passCodeCopied = \"\";\n  }") ||
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
	emptied := strings.Index(remove, "emptyCopiedHeldCode();")
	if emptied < 0 || strings.Contains(remove, "await ") || emptied > strings.Index(remove, "const dropped = forgetPassClaim(\"remove\", at);") ||
		emptied > strings.Index(remove, ".then(") || strings.Count(remove, "emptyCopied") != 1 {
		t.Error("Remove the Pass no longer empties the clipboard of a code copied here while the click allows it")
	}
	// Remove takes one code from this browser, the Pass it holds, and empties
	// the clipboard of that code alone: a copy of another code from a panel,
	// one handed over that the browser couldn't store, say, or a code it no
	// longer holds, stays there, so the steps that enter it after the Remove
	// can be followed. Forget this browser empties any copy (forgetBrowser,
	// below), as it takes every code this browser has.
	held := withoutComments(jsFunction(t, script, "emptyCopiedHeldCode"))
	if !strings.Contains(held, "function emptyCopiedHeldCode() {\n  const held = storedPass();\n"+
		"  if (held && held.code === passCodeCopied) {\n    emptyCopiedPassCode();\n  }\n}") {
		t.Error("Remove the Pass empties the clipboard of a code other than the Pass it removes, or no longer of that Pass's")
	}
	browser := withoutComments(jsFunction(t, script, "forgetBrowser"))
	emptied = strings.Index(browser, "emptyCopiedPassCode();")
	forgetClick := code[strings.Index(code, `if (event.target.closest('[data-action="forget-browser"]')) {`):]
	forgetClick = forgetClick[:strings.Index(forgetClick, "\n  }\n")]
	if !strings.Contains(script, "\nfunction forgetBrowser(at) {") || emptied < 0 ||
		emptied > strings.Index(browser, "const dropped = forgetPassClaim(\"browser\", at);") ||
		strings.Contains(browser, "await ") || strings.Contains(browser, ".then(") ||
		!regexp.MustCompile(`^if \(event\.target\.closest\('\[data-action="forget-browser"\]'\)\) \{\s*event\.preventDefault\(\);\s*const at = Date\.now\(\);\s*const dropped = forgetBrowser\(at\);`).MatchString(forgetClick) {
		t.Error("Forget this browser no longer empties the clipboard of a code copied here while the click allows it")
	}
	if n := strings.Count(code, "emptyCopiedPassCode()"); n != 3 {
		t.Errorf("emptyCopiedPassCode() appears %d times; want its definition, Remove the Pass's emptyCopiedHeldCode and Forget this browser", n)
	}
	// The promise is as narrow as the flag, and says what it costs: the
	// button named, a code it copied since educhan was last opened in this
	// tab, with nothing copied in the tab since, and not the code of a Pass
	// another tab has removed or replaced since, which is the Pass held where
	// only Remove is spoken of, and any Pass where Forget empties whichever
	// code was copied, nor once this site's stored data was cleared from
	// outside the page, which the tab hears of as a storage event with no key
	// and then counts no copy as its own (followPassStorage); whatever is on
	// the clipboard by then is emptied, a copy the page cannot see took
	// anything included, and a code copied by hand is not.
	for name, page := range map[string]struct{ text, unless string }{
		"the held Pass": {words(jsFunction(t, script, "passHeldHTML")),
			"unless another tab removed or replaced the Pass meanwhile, or this site's stored data was cleared " +
				"from outside the page."},
		"Forget this browser": {words(jsFunction(t, script, "forgetBrowserHTML")),
			"unless it copied the code of a Pass that another tab removed or replaced meanwhile, or this site's " +
				"stored data was cleared from outside the page."},
		"the FAQ": {words(faqAnswer(t, script, "faq-pass")),
			"unless you copied the code of a Pass that another tab removed or replaced meanwhile, or this site's " +
				"stored data was cleared from outside the page."},
	} {
		for _, want := range []string{"Copy the code", "since you last opened educhan in", "copied nothing in",
			"whatever is on it by then", page.unless, "even after a copy in",
			"that educhan can't see took anything, such as one from an email box in some browsers",
			"copied any other way, by hand included, stays"} {
			if !strings.Contains(page.text, want) {
				t.Errorf("%s no longer says which copies of the code are emptied from the clipboard (%q)", name, want)
			}
		}
	}
	// No page says a code copied with the button stays until something else
	// is copied: Forget this browser empties whichever code it copied.
	for name, text := range map[string]string{
		"the held Pass":       words(jsFunction(t, script, "passHeldHTML")),
		"Forget this browser": words(jsFunction(t, script, "forgetBrowserHTML")),
		"the FAQ":             words(faqAnswer(t, script, "faq-pass")),
	} {
		for _, sentence := range strings.Split(text, ". ") {
			if strings.Contains(sentence, "until something else is copied") && strings.Contains(sentence, "Copy the code") {
				t.Errorf("%s says a code copied with Copy the code stays until something else is copied: %q", name, sentence)
			}
		}
	}
	// And where Remove is spoken of, that it empties a copy of the Pass it
	// removes and leaves another code copied with the button, with an example.
	for text, wants := range map[string][]string{
		words(jsFunction(t, script, "passHeldHTML")): {"copied this Pass's code",
			"Removing this Pass also leaves another code copied with Copy the code there",
			"a new one this browser couldn't store", "since it doesn't take that code"},
		words(faqAnswer(t, script, "faq-pass")): {"when the code you copied was that Pass's",
			"nother code copied with Copy the code", "a new one the browser couldn't store", "stays when you remove the Pass"},
	} {
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("a page no longer says Remove empties a copy of the Pass it removes and no other code (%q)", want)
			}
		}
	}
}

// Remove the Pass or Forget this browser in one tab must not leave a code
// just handed over on another tab's screen. The storage event tells every
// other tab of the browser: the forget's mark first, which drops every such
// code (TestPassForgottenInAnotherTabStopsThisOne), then the Pass's removal,
// which draws the Pass page again if it is open. Any other removal there is
// educhan's word that the code is no longer valid, and takes off only that
// code, as in the tab that heard it.
func TestPassRemovedInAnotherTabLeavesThisOne(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if !strings.Contains(code, "\nwindow.addEventListener(\"storage\", followPassStorage);\n") {
		t.Error("a tab no longer follows the Pass another tab removed or replaced")
	}
	// Another tab's removal of the Pass is followed as this tab follows its
	// own finding that the Pass is no longer valid: the code removed, read
	// from the event's old value, goes from the panel, be it the code handed
	// over or a code it took the place of, typed back in, and any other code
	// stays: a code it took the place of, still good, maybe, and nowhere else,
	// and a kept code another had already taken the place of, drawn as no
	// longer here (passIssuedHTML). A removal with no code to read, or storage
	// cleared (a key of null), takes nothing off. Another code written there
	// leaves a code just handed over be, kept or not: one it could not keep was
	// never the Pass it holds, and may be a spent reissue code's only copy.
	// Each stays until the reader leaves the page. A removal with a code to
	// read also takes the notice off the Pass page, whatever it says, and off
	// any other page a held notice, as a Remove or Forget mark does, since it
	// may be one whose mark the browser would not store; but not a claim
	// notice, since a removal drops no claim (TestPassNoticeGoesWithWhatItSaid).
	follow := withoutComments(jsFunction(t, script, "followPassStorage"))
	if !strings.Contains(follow, "const now = storedPass();\n  const was = passCodeIn(event.oldValue);\n"+
		"  if (event.key === PASS_STORAGE_KEY && event.newValue === null && was) {\n    forgetInvalidCode(was);\n"+
		"    dropForgottenPassNotice();\n  }\n"+
		"  dropStalePassNotice();\n  showPassLink();\n  if (now) {") ||
		strings.Contains(follow, "passJustIssued") || strings.Count(follow, "forgetInvalidCode(") != 1 ||
		!strings.Contains(follow, `if (passPageShown() && el.main.querySelector("[data-pass-page]")) {`+
			"\n    redrawPass(lastPassInfo, \"#pass-held\", \"form[data-pass] input\");") {
		t.Error("another tab's removal of the Pass takes more off this tab's screen than the code it removed, " +
			"or leaves that code there, or another tab storing a different code takes a code just handed over off")
	}
	// So Remove the Pass and Forget this browser set their mark before they
	// take the Pass, or anything else of theirs, from storage (only the nonces
	// and this tab's own checkout marks, which other tabs leave be, go first,
	// in forgetPassClaim): another tab hears the mark first, and drops every
	// code just handed over, kept or not, with any code it took the place of,
	// before the Pass's removal, which alone it would follow as a code found no
	// longer valid. The clipboard is still emptied in the click
	// (TestPassCopiedHereIsEmptiedFromTheClipboard).
	remove := code[strings.Index(code, `if (event.target.closest('[data-action="forget-pass"]')) {`):]
	remove = remove[:strings.Index(remove, "\n  }\n")]
	marked := strings.Index(remove, "const dropped = forgetPassClaim(\"remove\", at);")
	if marked < 0 || strings.Count(remove, "forgetPass()") != 1 || strings.Index(remove, "forgetPass()") < marked ||
		strings.Contains(remove[:marked], "removeItem(") || strings.Contains(remove[:marked], "storageSet(") {
		t.Error("Remove the Pass takes the Pass from storage before its mark is there, so another tab hears the removal first")
	}
	browser := withoutComments(jsFunction(t, script, "forgetBrowser"))
	marked = strings.Index(browser, "const dropped = forgetPassClaim(\"browser\", at);")
	before := browser[:max(marked, 0)]
	if marked < 0 || strings.Contains(before, "forgetCredential()") || strings.Contains(before, "forgetPass()") ||
		strings.Contains(before, "removeItem(") || strings.Contains(before, "storageSet(") ||
		strings.Index(browser, "forgetCredential();") < marked || strings.Index(browser, "forgetPass();") < marked {
		t.Error("Forget this browser takes the Pass, or anything else, from storage before its mark is there, " +
			"so another tab hears the removal first")
	}
	// A code this tab copied stops counting as its to empty from the
	// clipboard once another tab takes that code itself out of storage,
	// removed or replaced by another (the event's old value held it, and the
	// browser no longer holds it), or storage is cleared and the browser no
	// longer holds it, whether or not a code just handed over is on screen.
	// Another tab storing the same code again, renewed or with a new date, or
	// changing a Pass whose code is not the one copied (a code copied from the
	// panel of one the browser no longer holds, say), leaves it counted, so
	// Remove and Forget still empty the clipboard, as the page promises.
	if !strings.Contains(follow, "if ((event.key === null || was === passCodeCopied) && (!now || now.code !== passCodeCopied)) {\n"+
		"    passCodeCopied = \"\";\n  }") {
		t.Error("a code this tab copied still counts as its to empty after another tab removed or replaced it, " +
			"or stops counting when another tab renews or re-dates the Pass held, or changes one that is not the code copied")
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
// a Pass on its way, empties its code box and drops a code just handed over,
// kept or not, so neither a reissue code typed there nor a code shown there
// is left for the next person at the computer, even when the Pass's own
// removal drew that tab again first, or the browser could keep no Pass to
// remove. Stop leaves a reissue code alone, here and in other tabs: it is
// about a checkout, and a reissue code it dropped would be spent with nothing
// to show for it.
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
	// when it was pressed, and is taken off once the request is answered, just
	// after it says what the answer was (TestPassClaimsCarryANonceAndCanBeDropped):
	// marks never take each other's place, so every tab hears each one go.
	forget := withoutComments(jsFunction(t, script, "forgetPassClaim"))
	marked := strings.Index(forget, "storageSet(mark, `${pressed}:${at}`);")
	asked := strings.Index(forget, `await api("/api/pass/claim"`)
	if !strings.Contains(forget, "const mark = `${PASS_FORGET_STORAGE_KEY}:${newPassNonce()}`;") || marked < 0 || marked > asked ||
		!strings.Contains(forget, "request.then((done) => {\n      storageSet(mark, ") ||
		strings.Count(forget, "removeItem(") != 1 {
		t.Error("forgetPassClaim no longer tells the browser's other tabs, before it asks the server, that it is forgetting")
	}
	if !strings.Contains(code, "\nfunction forgetPassClaim(pressed, at) {") || !strings.Contains(forget, "const emptyBoxes = pressed !== \"stop\";") ||
		strings.Count(code, "forgetPassClaim(\"remove\", at)") != 1 || strings.Count(code, "forgetPassClaim(\"browser\", at)") != 1 ||
		strings.Count(code, "forgetPassClaim(\"stop\", at)") != 1 || strings.Count(code, "forgetPassClaim(") != 4 {
		t.Error("only Remove the Pass and Forget this browser have other tabs empty their code boxes")
	}
	follow := withoutComments(jsFunction(t, script, "followPassForget"))
	gate := strings.Index(follow, "const key = event.key;\n  if (!key || !key.startsWith(`${PASS_FORGET_STORAGE_KEY}:`)) {\n    return;\n  }")
	read := strings.Index(follow, "const mark = event.newValue;")
	if gate != strings.Index(follow, "{")+4 || read < gate || strings.Count(follow, "event.") != 3 {
		t.Error("followPassForget reads a storage event other than a forget mark's")
	}
	// A mark that goes ends the wait on it, and what it held as it went says
	// what its forget came to (heedPassForgetAnswer), while one this tab
	// already waits on (found when it was loaded, or brought back), and the
	// answer written into a mark just before it goes, are not stopped for.
	// Every other one is stopped for in one place, whether this tab hears of it
	// by its storage event or finds it there when it is loaded or brought back
	// from the back-forward cache (heedPassForgetsUnderWay): a page brought
	// back holds what it held when it was left, a collection's nonce, a reissue
	// code's or the last holder's news, and the storage event for a mark it
	// found, which comes after the pageshow event, finds it already waited on,
	// so it is not stopped for a second time, and only stopping for it there
	// drops what the forget drops.
	stop := strings.Index(follow, "if (!mark) {\n    const heard = passForgetHeard.get(key);\n    if (heard) {\n      heard();\n    }\n"+
		"    heedPassForgetAnswer(event.oldValue || \"\");\n    return;\n  }\n"+
		"  if (passForgetWaits.has(key) || passMarkRead(mark).answer) {\n    return;\n  }\n  stopForPassForget(key, mark).then(() => {")
	if stop < 0 || stop < read {
		t.Error("followPassForget no longer stops this tab for a forget it hears of, once and only once")
	}
	stopping := withoutComments(jsFunction(t, script, "stopForPassForget"))
	steps := []string{"const pressed = mark.slice(0, mark.indexOf(\":\"));", "const code = pressed !== \"stop\";", "passForgetHeardLast = pressed;",
		"passVisit += 1;", "passForgets += 1;",
		"dropPassNonce(PASS_NONCE_STORAGE_KEY);",
		"if (code) {\n    passCodeForgets += 1;\n    dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);\n" +
			"    dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);\n    forgetTypedCode();\n    passLost = \"\";\n    passJustIssued = null;\n" +
			"    dropForgottenPassNotice();\n  }",
		"const waited = heedPassForget(key, mark);",
		"for (const button of el.main.querySelectorAll(PASS_COLLECT_BUTTONS)) {\n    button.disabled = true;", "return waited;\n}"}
	for at, step := 0, 0; step < len(steps); step++ {
		next := strings.Index(stopping[at:], steps[step])
		if next < 0 {
			t.Errorf("stopForPassForget no longer does %q, in its order, when another tab forgets", steps[step])
			break
		}
		at += next
	}
	under := withoutComments(jsFunction(t, script, "heedPassForgetsUnderWay"))
	if !strings.Contains(under, "} else if (passForgetWaits.has(key)) {\n      continue;\n    } else if (passForgetLeft(mark) > 0) {\n"+
		"      stopForPassForget(key, mark);\n      continue;\n    }") ||
		strings.Count(code, "stopForPassForget(") != 3 || strings.Count(code, "heedPassForget(") != 2 {
		t.Error("a tab brought back from the back-forward cache while a forget is on its way waits on it without dropping what it drops")
	}
	// Stop ("stop:") leaves a reissue code alone, here and in other tabs: its
	// nonce, the code typed, and an answer on its way, which redeemPassReissue
	// drops only for Remove and Forget (passCodeForgets, not passForgets), and
	// which waits on Stop without taking it for one of them (passCodeForgetting
	// is made only of the forgets that take the Pass).
	for name, steps := range map[string][]string{
		"forgetPassClaim": {"passCodeForgets += 1;", "dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);",
			"dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);"},
		"stopForPassForget": {"passCodeForgets += 1;", "dropPassNonce(PASS_REISSUE_NONCE_STORAGE_KEY);",
			"dropPassNonce(PASS_REISSUE_UNANSWERED_STORAGE_KEY);", "forgetTypedCode();", "passLost = \"\";", "passJustIssued = null;"},
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
		!strings.Contains(withoutComments(jsFunction(t, script, "heedPassForget")), `return waitOnPassForget(key, !mark.startsWith("stop:"), over);`) {
		t.Error("a Stop's mark is waited on as one that takes the Pass, and drops a reissue answer")
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "redeemPassReissue")), "const forgets = passCodeForgets;") {
		t.Error("redeemPassReissue drops its answer for Stop collecting it here, leaving a spent reissue code and a dead Pass")
	}
	// It waits for the mark to go, or until a while after it was set, for a
	// tab closed before its answer, and draws the Pass page again from the
	// server only then: if the reader is on it, past the age screen, even when
	// the mark arrived while it was still loading and drew nothing (a skeleton
	// is no Pass page yet). The drawing keeps a keyboard where it was, as
	// redrawPass does: focus on the page just before it, on a button given back
	// meanwhile say, goes to what took its place, and focus on the body or
	// outside the page stays there, so a reader who comes back to this tab does
	// not find it moved, or a phone's page scrolled to a button. Check now or
	// Stop, turned off while the forget goes, gives focus it had to its panel,
	// which is on the page, rather than dropping it to the body.
	heed := withoutComments(jsFunction(t, script, "heedPassForget"))
	left := withoutComments(jsFunction(t, script, "passForgetLeft"))
	if !strings.Contains(heed, "passForgetHeard.set(key, resolve);\n    setTimeout(resolve, passForgetLeft(mark));") ||
		!strings.Contains(heed, "}).then(() => {\n    passForgetHeard.delete(key);\n  });") ||
		left != "function passForgetLeft(mark) {\n  const { at, answer } = passMarkRead(mark);\n"+
			"  const left = at + PASS_FORGET_WAIT_MS - Date.now();\n"+
			"  return !answer && left > 0 ? Math.min(left, PASS_FORGET_WAIT_MS) : 0;\n}" ||
		withoutComments(jsFunction(t, script, "passMarkRead")) != "function passMarkRead(mark) {\n"+
			"  const [pressed, at, answer = \"\"] = mark.split(\":\");\n  return { pressed, at: Number(at), answer };\n}" {
		t.Error("a tab no longer waits for another tab's mark to go, or no longer for at most PASS_FORGET_WAIT_MS after it was set, " +
			"or waits on one that says what its forget's answer was")
	}
	render := withoutComments(jsFunction(t, script, "renderPass"))
	if !regexp.MustCompile(`stopForPassForget\(key, mark\)\.then\(\(\) => \{\s*if \(passPageShown\(\) && ageStatus\(\) === "ok"\) \{\s*`+
		regexp.QuoteMeta(`renderPass('form[data-pass] button[type="submit"]', "#pass-held", "form[data-pass] input");`)+`\s*\}\s*\}\);`).MatchString(follow) ||
		strings.Contains(follow, "data-pass-page") || strings.Contains(follow, "activeElement") || strings.Contains(follow, "passFocus(") ||
		!strings.Contains(render, "async function renderPass(...selectors) {") ||
		!strings.Contains(render, "if (selectors.length) {\n    redrawPass(info, ...selectors);\n  } else {\n    drawPass(info);\n  }") {
		t.Error("followPassForget no longer draws the Pass page from the server once the other tab's request is answered, " +
			"or moves focus that was not on the page")
	}
	if !strings.Contains(stopping, "const focused = document.activeElement;\n  for (const button of el.main.querySelectorAll(PASS_COLLECT_BUTTONS)) {\n"+
		"    button.disabled = true;\n    if (button === focused) {\n      (button.closest('[tabindex=\"-1\"]') || el.main).focus({ preventScroll: true });\n    }\n  }") {
		t.Error("Check now or Stop, turned off for another tab's forget, drops the focus it had off the page")
	}
	// A tab opened while a forget is on its way hears no storage event for it,
	// nor does one brought back from the back-forward cache for a mark set or
	// taken off while it was away, so each looks for the marks there before
	// anything is drawn: one it does not know of is waited on, one whose
	// mark has gone stops being waited on, and one set longer ago than a tab
	// waits, left by a tab closed before its answer, is taken off. One found
	// saying what its forget's answer was, in the moment before its tab takes
	// it off, is over too: what it says is heeded (heedPassForgetAnswer), and
	// it is taken off.
	if !strings.Contains(under, "if (key && key.startsWith(`${PASS_FORGET_STORAGE_KEY}:`)) {\n        marks.set(key, localStorage.getItem(key) || \"\");") ||
		!strings.Contains(under, "for (const [key, heard] of passForgetHeard) {\n    if (!marks.has(key) || passMarkRead(marks.get(key)).answer) {\n      heard();") ||
		!strings.Contains(under, "for (const [key, mark] of marks) {\n    if (passMarkRead(mark).answer) {\n      heedPassForgetAnswer(mark);\n"+
			"    } else if (passForgetWaits.has(key)) {\n      continue;\n    } else if (passForgetLeft(mark) > 0) {\n"+
			"      stopForPassForget(key, mark);\n      continue;\n    }\n    try {\n      localStorage.removeItem(key);") {
		t.Error("a tab opened while a forget is on its way no longer waits on it, or no longer takes off a mark left behind " +
			"or one that says what its forget's answer was, or no longer heeds that answer")
	}
	if withoutComments(jsFunction(t, script, "heedPassForgetAnswer")) != "function heedPassForgetAnswer(mark) {\n"+
		"  const { at, answer } = passMarkRead(mark);\n  if (answer === PASS_FORGET_DROPPED) {\n    heedPassClaimGone(at);\n  }\n}" ||
		strings.Count(code, "heedPassForgetAnswer(") != 3 {
		t.Error("another tab's forget answered with the claim dropped no longer takes off a claim notice here said for an " +
			"earlier forget, as of when that forget was pressed, or something else does that from a mark")
	}
	if !strings.Contains(code, "\nwindow.addEventListener(\"storage\", followPassForget);\nwindow.addEventListener(\"storage\", keepPassCheckoutMark);\nheedPassForgetsUnderWay();\n") ||
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
// waited, and left in place, gets focus back, and a drawing something else
// set off keeps focus on the control that had it.
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
	// What arrives after a button was pressed (Check now's claim, or another
	// tab's forget, say) draws the page again through redrawPass, which keeps
	// focus that was on the page on the same control when the drawing draws
	// it again, without scrolling the page, since it is where it was, and
	// otherwise moves it to what took the control's place. The same control
	// is found by its id, else by its action within its own panel (a Copy the
	// code beside the held Pass's code is not one beside a code just handed
	// over), else by its form's kind and its part there.
	redraw := withoutComments(jsFunction(t, script, "redrawPass"))
	if redraw != "function redrawPass(info, ...selectors) {\n"+
		"  const focused = el.main.contains(document.activeElement) ? document.activeElement : null;\n"+
		"  const twin = focused ? passTwin(focused) : \"\";\n  drawPass(info);\n"+
		"  if (focused && !el.main.contains(document.activeElement)) {\n    const same = twin && el.main.querySelector(twin);\n"+
		"    if (same) {\n      same.focus({ preventScroll: true });\n    } else {\n      passFocus(...selectors);\n    }\n  }\n}" {
		t.Errorf("redrawPass no longer keeps focus that was on the page on the same control, or else on what took its place:\n%s", redraw)
	}
	twin := withoutComments(jsFunction(t, script, "passTwin"))
	if twin != "function passTwin(control) {\n  if (control.id) {\n    return `#${CSS.escape(control.id)}`;\n  }\n"+
		"  const panel = control.closest(\".verify-panel[id]\");\n  const within = panel ? `#${CSS.escape(panel.id)} ` : \"\";\n"+
		"  if (control.dataset.action) {\n    return `${within}[data-action=\"${control.dataset.action}\"]`;\n  }\n"+
		"  const form = control.closest(\"form[data-pass]\");\n  if (form && control.matches('button[type=\"submit\"], input')) {\n"+
		"    const part = control.matches(\"input\") ? \"input\" : 'button[type=\"submit\"]';\n"+
		"    return `form[data-pass=\"${form.dataset.pass}\"] ${part}`;\n  }\n  return \"\";\n}" {
		t.Errorf("passTwin no longer finds a control drawn again by its id, its action in its own panel, or its form's kind and its part there:\n%s", twin)
	}
	// A drawing set off by something else (a claim's answer, another tab's
	// change to the Pass) leaves a reader typing in the code box where they
	// were, caret and all, and redrawPass does not move them on.
	draw := withoutComments(jsFunction(t, script, "drawPass"))
	if !strings.Contains(draw, "const typing = box && document.activeElement === box ? [box.selectionStart, box.selectionEnd] : null;") ||
		!strings.Contains(draw, "if (typing) {\n      again.focus();\n      again.setSelectionRange(typing[0], typing[1]);") ||
		!strings.Contains(redraw, "if (focused && !el.main.contains(document.activeElement)) {\n    const same = twin && el.main.querySelector(twin);") {
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
	// A button the page was drawn again over while it waited, by another
	// tab's forget say, is gone: focus goes to the same button drawn again
	// (passTwin), else the first of the Pass forms' controls, else the first
	// panel, and at the least the page (passFocus), never the top of the
	// document.
	if refocused := withoutComments(jsFunction(t, script, "refocus")); refocused != "function refocus(button) {\n"+
		"  if (document.activeElement && document.activeElement !== document.body) {\n    return;\n  }\n"+
		"  if (button.isConnected) {\n    button.focus();\n    return;\n  }\n"+
		"  passFocus(passTwin(button), \"form[data-pass] input, form[data-pass] button\", '.verify-panel[tabindex=\"-1\"]');\n}" {
		t.Errorf("refocus no longer gives focus back to a button that lost it while disabled, and only then, "+
			"or to the same button drawn again when the page was drawn again meanwhile:\n%s", refocused)
	}
	passFocused := withoutComments(jsFunction(t, script, "passFocus"))
	if !strings.HasSuffix(passFocused, "\n  }\n  el.main.focus();\n}") {
		t.Error("passFocus, finding none of what it was given, no longer focuses the page, so focus can stay at the top of the document")
	}
	// passTwin has no selector for a control it cannot tell again, and
	// querySelector would throw on the empty one.
	if !strings.Contains(passFocused, "const target = selector && el.main.querySelector(selector);") {
		t.Error("passFocus no longer passes over an empty selector")
	}
	if n := strings.Count(code, "passTwin("); n != 3 {
		t.Errorf("passTwin is named %d times; want its declaration, redrawPass and refocus", n)
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
	probe, checkout := strings.Index(submit, "if (!passCanBeKept()) {\n      showPassNotice(PASS_NO_STORAGE);\n      return;"), strings.Index(submit, `"/api/pass/checkout"`)
	if probe < 0 || checkout < probe {
		t.Error("submitPass no longer refuses a checkout in a browser that would keep nothing")
	}
	can := withoutComments(jsFunction(t, script, "passCanBeKept"))
	if !strings.Contains(can, "if (!navigator.cookieEnabled) {") || !strings.Contains(can, `const kept = storageGet(STORAGE_PROBE_KEY) === "1";`) {
		t.Error("passCanBeKept no longer checks both cookies and storage")
	}
	if got := jsString(t, script, "PASS_NO_STORAGE"); got != "This browser couldn't store what educhan needs, so it couldn't collect or keep a Pass. Allow this site to store data first." {
		t.Errorf("PASS_NO_STORAGE says %q", got)
	}
}

// A code typed in after it ran out is kept, marked run out with the date the
// server has for it, so the page offers to renew it.
func TestPassTypedInAfterItRanOutCanBeRenewed(t *testing.T) {
	submit := withoutComments(jsFunction(t, appScript(t), "submitPass"))
	if !strings.Contains(submit, `const expired = kind === "activate" && refusal === "expired" && failed.payload.code;`) ||
		!strings.Contains(submit, "} else if (expired) {\n    forgetTypedCode();\n    kept = storePass(failed.payload.code, failed.payload.expires_at, true);") {
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
	// A Pass found run out while sales are paused is not offered a renewal
	// the page no longer has: the words say sales are paused instead
	// (TestPassRunOutIsSaidForThePageBesideIt holds the rest).
	if got := withoutComments(jsFunction(t, script, "passRunOutText")); !strings.HasPrefix(got, "function passRunOutText() {\n"+
		"  if (lastPassInfo.paused) {\n    return `This Pass has run out. ${PASS_PAUSED}`;\n  }\n") {
		t.Errorf("passRunOutText offers to renew a Pass while sales are paused:\n%s", got)
	}
}

// What the page says of a Pass found run out, by Check it again or a code typed
// in, points only to what it draws beside it: Renew while the Pass is on sale,
// that sales are paused where Renew would be, and, on a page drawn without the
// server's answer, which has no Renew, that renewing needs the page to load.
// Such a page is asked for again (refreshFailedPass), and once that drawing has
// landed, or failed, the notice, while it is still on screen, is said again for
// it if its words have changed, without moving the page, so never pointing to a
// Renew the page does not draw and never twice in the same words, and is taken
// off if the Pass it is about is no longer stored as it was, removed, replaced
// or renewed meanwhile, say.
func TestPassRunOutIsSaidForThePageBesideIt(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if got := withoutComments(jsFunction(t, script, "passRunOutText")); got != "function passRunOutText() {\n"+
		"  if (lastPassInfo.paused) {\n    return `This Pass has run out. ${PASS_PAUSED}`;\n  }\n"+
		"  if (lastPassInfo.enabled) {\n    return \"This Pass has run out. You can renew it below.\";\n  }\n"+
		"  return lastPassInfo.failed\n"+
		"    ? \"This Pass has run out. You can renew it here once this page loads, which it couldn't just now.\"\n"+
		"    : \"This Pass has run out.\";\n}" {
		t.Errorf("passRunOutText points to a Renew the page does not draw, or no longer to one it does:\n%s", got)
	}
	// The held Pass's panel draws Renew exactly while the Pass is on sale,
	// which a page drawn without the server's answer does not say it is.
	held := withoutComments(jsFunction(t, script, "passHeldHTML"))
	if !strings.Contains(held, "if (info.paused) {\n    renew = `<p class=\"muted\">${esc(PASS_PAUSED)}</p>`;\n"+
		"  } else if (info.enabled) {\n    renew = `") ||
		!strings.Contains(withoutComments(jsFunction(t, script, "passViewHTML")), "parts.push(passHeldHTML({ enabled: false }, held, shown, box));") {
		t.Error("the held Pass's panel no longer draws Renew exactly while the Pass is on sale, as passRunOutText says")
	}
	// The follow-up acts on its own notice alone, told by the node its words
	// are in, so a later press's notice in the same words, a second run-out
	// code typed in say, is left as it is; and it is taken off, as at any
	// drawing, once the Pass it was said beside is no longer stored as it was
	// (TestPassNoticeGoesWithWhatItSaid). Said again, it is noted beside the
	// record taken as the run-out was stored, not after the wait.
	run := withoutComments(jsFunction(t, script, "showPassRunOut"))
	if run != "function showPassRunOut(...selectors) {\n  const said = passRunOutText();\n"+
		"  const record = passHeldRecord();\n  showPassNotice(said);\n  const text = notePassHeld(record);\n"+
		"  refreshFailedPass(...selectors).then(() => {\n    if (!passPageShown()) {\n      return;\n    }\n"+
		"    dropStalePassNotice();\n    const now = passRunOutText();\n"+
		"    if (now !== said && noticeShows(text)) {\n      showNotice(now);\n      notePassHeld(record);\n    }\n  });\n}" {
		t.Errorf("showPassRunOut no longer says the run-out notice again for the page drawn after it, once and where it "+
			"is, only while its own notice is on screen, or no longer takes it off once the Pass it is about is not held "+
			"as it was:\n%s", run)
	}
	if got := withoutComments(jsFunction(t, script, "noticeShows")); got != "function noticeShows(text) {\n"+
		"  return !el.notice.hidden && el.notice.contains(text);\n}" {
		t.Errorf("noticeShows no longer tells whether a notice's own words are still on screen:\n%s", got)
	}
	if strings.Contains(code, "textContent === said") || strings.Contains(code, "function noticeSays(") {
		t.Error("a notice is told apart from a later one by its words, which a later press can say again")
	}
	// Both answers that find a Pass run out say so this way, once the page they
	// point into is drawn, and nothing else says it.
	check := withoutComments(jsFunction(t, script, "checkHeldPass"))
	submit := withoutComments(jsFunction(t, script, "submitPass"))
	if !strings.Contains(check, "} else if (expired) {\n    drawPass(lastPassInfo);\n"+
		"    passFocus('[data-action=\"check-pass\"]', \"#pass-held\");\n"+
		"    showPassRunOut('[data-action=\"check-pass\"]', \"#pass-held\");\n  } else {") ||
		!strings.Contains(submit, "if (expired) {\n    drawPass(lastPassInfo);\n    passFocus(\"#pass-held\");\n    if (kept) {\n"+
			"      showPassRunOut(\"#pass-held\");\n    } else {\n      showPassNotice(PASS_RUN_OUT_NOT_KEPT);\n      notePassHeld();\n"+
			"      refreshFailedPass(\"#pass-held\");\n    }\n    return;\n  }") ||
		strings.Count(code, "passRunOutText(") != 3 || strings.Count(code, "showPassRunOut(") != 3 {
		t.Error("a Pass found run out is said to be so in words that do not follow the page drawn after them")
	}
	// That drawing is waited on: refreshFailedPass resolves once it has landed,
	// focus given, or come to nothing, and at once when there is none.
	refresh := withoutComments(jsFunction(t, script, "refreshFailedPass"))
	if refresh != "function refreshFailedPass(...selectors) {\n  if (!lastPassInfo.failed) {\n    return Promise.resolve(false);\n  }\n"+
		"  return renderPass(...selectors).then((drawn) => {\n"+
		"    if (drawn && (!document.activeElement || document.activeElement === document.body)) {\n"+
		"      passFocus(...selectors);\n    }\n    return drawn;\n  });\n}" {
		t.Errorf("refreshFailedPass no longer resolves once its drawing has landed, focus given:\n%s", refresh)
	}
}

// A notice about the Pass this browser holds is true of it for as long as it
// stays, on whatever page it is shown. Each held notice, one that says what the
// browser holds (a Pass taken on, good until a date, renewed or run out, one
// the browser could not store, or none), is noted beside the Pass stored as it
// was when the press or the answer set what it says, by the node its words are
// in. It is said on the Pass page, beside a post whose answer says its Pass has
// run out or is no longer valid, or on the Verify page the press of Forget this
// browser drew, except Remove's and Forget this browser's when educhan could
// not be reached, which are claim notices too and are said on whatever page.
// One put up after an await, the post's once its thread is drawn, Forget this
// browser's and Remove's once their forget is answered, takes that record
// before the await, and every one is checked as it is noted, so a change
// another tab made meanwhile, a renewal say, takes it off at once. Each put up
// after an await, those and a renewal's or a lost answer's said once the Pass
// held has been checked, reads before the await what has taken held notices off
// so far, and is not put up once Remove or Forget this browser, here or in
// another tab, or another tab's removal of the Pass, has come meanwhile, since
// there was no notice yet for them to take off. It goes at the next drawing of
// the Pass page, whatever set it off, and at another tab's change to the Pass,
// on whatever page this tab is on, once the browser no longer holds that as
// stored. That is decided by the stored record, never by a clock, so a Pass the
// server said has run out is still the one the notice spoke of on a computer
// whose clock is slow, and the server's date added to the same run-out mark
// changes nothing. A claim notice says a claim of this browser may still be
// collected and what to press: forgetPassOrder's, and Remove's, Forget this
// browser's and Stop collecting it here's when educhan could not be reached.
// Each is noted with when its own forget went, and, besides what takes any
// notice off (the reader dismissing it, moving to another page or a later
// notice put in its place, say), only an answer showing the claim gone takes it
// off, on whatever page this tab is on: a forget pressed after its own went,
// here or in another tab, answered with the claim dropped (read from that
// forget's mark as it goes), or this tab's own collection or GET /api/pass,
// sent after it, finding nothing left. None is put up once this tab has learned
// that already: forgetPassOrder and Stop collecting it here then say nothing,
// Remove the Pass nothing beyond what its press said, and Forget this browser,
// over the Verify page its press drew, only what it took, or nothing once a
// Remove or Forget pressed since, here or in another tab, or another tab's
// removal of the Pass, has come; a forget pressed since and not yet answered
// leaves it to be put up. A forget pressed and not yet answered, one answered
// with the claim kept, one whose tab closed first, and a Pass stored, changed
// or removed in another tab leave it; Remove's and Forget's, which also say the
// browser holds no Pass, give way to the claim's warning alone whenever a held
// notice would go. Remove and Forget this browser in another tab take every
// other notice off the Pass page here, and a held notice off any other page, by
// the mark or, if the browser would not store that, by the removal itself, and
// a Stop leaves them be (TestPassForgottenInAnotherTabStopsThisOne,
// TestPassRemovedInAnotherTabLeavesThisOne). A late follow-up acts on its own
// notice alone (TestPassRunOutIsSaidForThePageBesideIt).
func TestPassNoticeGoesWithWhatItSaid(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	// A run-out mark stands for the Pass whatever date is stored beside it, so
	// Check it again in another tab, giving the same mark the server's date,
	// leaves a post's notice that the Pass has run out.
	if got := withoutComments(jsFunction(t, script, "passHeldRecord")); got != "function passHeldRecord() {\n"+
		"  const held = storedPass();\n  if (!held) {\n    return \"\";\n  }\n"+
		"  return held.marked ? `${held.code} run out` : `${held.code} ${held.expires.getTime()}`;\n}" {
		t.Errorf("passHeldRecord no longer gives the Pass as it is stored, code and expiry or the run-out mark alone, "+
			"without reading the clock:\n%s", got)
	}
	if got := withoutComments(jsFunction(t, script, "notePassHeld")); got != "function notePassHeld(said = passHeldRecord(), alone = \"\", at = 0) {\n"+
		"  const text = el.notice.querySelector(\".notice-text\");\n  passNoticesHeld.set(text, said);\n"+
		"  if (alone) {\n    passClaimNotices.set(text, { alone, at });\n  }\n  dropStalePassNotice();\n"+
		"  return text;\n}" || !strings.Contains(code, "\nconst passNoticesHeld = new WeakMap();\n") {
		t.Errorf("notePassHeld no longer notes the notice just put up, by its node, beside the record it is given, "+
			"as a claim notice too, said for its forget, when given the claim's warning alone, and checks it at once:\n%s", got)
	}
	// A claim notice is noted with the claim's warning alone and when the
	// forget it warns about went: its own by forgetPassOrder, Stop, and Remove
	// and Forget once a forget has taken their held part off meanwhile; and,
	// for the same forget, the warning alone put in the place of Remove's or
	// Forget's once its held part goes.
	if got := withoutComments(jsFunction(t, script, "noteClaimNotice")); got != "function noteClaimNotice(at) {\n"+
		"  const text = el.notice.querySelector(\".notice-text\");\n  passClaimNotices.set(text, { alone: text.textContent, at });\n}" ||
		!strings.Contains(code, "\nconst passClaimNotices = new WeakMap();\n") || strings.Count(code, "passClaimNotices.") != 5 ||
		strings.Count(code, "noteClaimNotice(at)") != 5 || strings.Count(code, "noteClaimNotice(claim.at)") != 2 ||
		strings.Count(code, "noteClaimNotice(") != 7 || strings.Count(withoutComments(jsFunction(t, script, "forgetPassOrder")), "noteClaimNotice(at);") != 1 {
		t.Errorf("a claim notice is no longer noted by its node with its own words as the claim's warning and when its forget "+
			"went, by forgetPassOrder, Stop collecting it here and Remove's and Forget's in their place, or is noted somewhere "+
			"else:\n%s", got)
	}
	if got := withoutComments(jsFunction(t, script, "passNoticeHeld")); got != "function passNoticeHeld() {\n"+
		"  const text = el.notice.querySelector(\".notice-text\");\n"+
		"  return text && noticeShows(text) ? passNoticesHeld.get(text) : undefined;\n}" {
		t.Errorf("passNoticeHeld no longer gives what the notice on screen said this browser holds, and nothing for "+
			"another notice:\n%s", got)
	}
	// A held notice found no longer true goes, unless it is a claim notice too:
	// then the claim's warning alone takes its place, through showNotice, which
	// keeps focus that was on it (TestNoticeReplacedKeepsFocusOnIt), noted as a
	// claim notice only, for the same forget, so a Pass stored in another tab
	// leaves it.
	if got := withoutComments(jsFunction(t, script, "dropStalePassNotice")); got != "function dropStalePassNotice() {\n"+
		"  const said = passNoticeHeld();\n"+
		"  if (said === undefined || said === passHeldRecord()) {\n    return;\n  }\n"+
		"  const claim = passClaimNotices.get(el.notice.querySelector(\".notice-text\"));\n"+
		"  if (claim) {\n    showNotice(claim.alone);\n    noteClaimNotice(claim.at);\n  } else {\n    takePassNoticeOff();\n  }\n}" {
		t.Errorf("dropStalePassNotice no longer takes off a held notice about a Pass this browser no longer holds as it was "+
			"stored, or puts the claim's warning alone in place of one that is a claim notice too, or touches another notice:\n%s", got)
	}
	// What Remove and Forget this browser say when educhan could not be
	// reached: what they took, then the claim's warning, which alone takes the
	// notice's place once the rest is taken off; each says what to press once
	// back online, on whatever page it is read.
	for _, want := range []string{
		"\nconst PASS_CLAIM_KEPT = {\n  remove: {\n    said:\n      \"This browser no longer has a Pass, but educhan couldn't be reached to stop it " +
			"collecting anything bought here that hasn't arrived yet. Use Forget this browser on the Verify page when you're back online.\",\n" +
			"    alone:\n      \"educhan couldn't be reached to stop this browser collecting anything bought here that hasn't arrived yet. " +
			"Use Forget this browser on the Verify page when you're back online.\",\n  },\n",
		"\n  browser: {\n    said:\n      \"This browser no longer holds your post key, your name, a badge or a Pass. educhan couldn't be " +
			"reached to stop it collecting a Pass bought here that hasn't arrived yet, if there is one, so press Forget this browser on the " +
			"Verify page again when you're back online.\",\n    alone:\n      \"educhan couldn't be reached to stop this browser collecting " +
			"a Pass bought here that hasn't arrived yet, if there is one, so press Forget this browser on the Verify page again when " +
			"you're back online.\",\n  },\n};\n",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("Remove's or Forget this browser's notice when educhan could not be reached, or the claim's warning alone, "+
				"no longer says %q", want)
		}
	}
	if strings.Count(code, "PASS_CLAIM_KEPT.") != 6 {
		t.Error("the notices Remove and Forget this browser give when educhan could not be reached are said somewhere else")
	}
	// Every drawing of the Pass page asks first, before it takes lastPassInfo
	// or the page's code box: redrawPass, renderPass and refreshFailedPass
	// draw through drawPass, as does the drawing another tab's change sets off.
	// That change asks on whatever page this tab is on (followPassStorage,
	// pinned in TestPassRemovedInAnotherTabLeavesThisOne), so a post's notice
	// goes once another tab renews or replaces the Pass it spoke of; noting a
	// held notice asks at once (notePassHeld, above); and showPassRunOut asks
	// again once its own drawing has landed.
	if draw := withoutComments(jsFunction(t, script, "drawPass")); !strings.HasPrefix(draw, "function drawPass(info) {\n  dropStalePassNotice();\n") ||
		strings.Count(code, "dropStalePassNotice()") != 5 ||
		strings.Count(withoutComments(jsFunction(t, script, "followPassStorage")), "dropStalePassNotice();") != 1 {
		t.Error("a drawing of the Pass page, or another tab's change to the Pass, no longer takes off a notice about a " +
			"Pass this browser no longer holds as it was stored")
	}
	// Each held notice is noted once it is put up. One put up in the same turn
	// as what it says, here and in showClaimed and showPassRunOut
	// (TestPassNoticesFromAPressAreBroughtIntoView,
	// TestPassRunOutIsSaidForThePageBesideIt), is noted beside what the browser
	// holds then; one put up after an await is noted beside the record taken
	// before it (said, below).
	for name, notice := range map[string][2]string{
		"Check it again":                 {"showPassNotice(kept ? `Your Pass is good until ${passUntilText(res.expires_at)}.` : PASS_NOT_KEPT);", "notePassHeld();"},
		"a code taken on":                {"showPassNotice(kept ? `This browser now has your Pass, good until ${passUntilText(res.expires_at)}.` : PASS_NOT_KEPT);", "notePassHeld();"},
		"a run-out code not stored":      {"showPassNotice(PASS_RUN_OUT_NOT_KEPT);", "notePassHeld();"},
		"a good Pass held":               {`showPassNotice("This browser has a Pass already. To use another one here, write this one's code down, then remove it.");`, "notePassHeld();"},
		"a renewal's Pass gone":          {`showPassNotice("The Pass you meant to renew is no longer in this browser. Enter its code below to renew it here.");`, "notePassHeld();"},
		"a renewal refused":              {"showPassNotice(`This Pass is no longer valid, so it has been removed from this browser. ${PASS_GONE_REASONS}`);", "notePassHeld();"},
		"a reissue code's answer":        {"if (notice) {\n      showPassNotice(notice);", "notePassHeld();"},
		"Remove the Pass":                {`showPassNotice("This browser no longer has a Pass.");`, "notePassHeld(said);"},
		"Remove, unreached":              {"showNotice(notice);\n      }\n      if (held) {", "notePassHeld(said, PASS_CLAIM_KEPT.remove.alone, at);"},
		"a post's Pass":                  {"if (passNotice && forgets === passNoticeForgets) {\n      showNotice(passNotice);", "notePassHeld(said);"},
		"Forget this browser, unreached": {"showNotice(held ? PASS_CLAIM_KEPT.browser.said : PASS_CLAIM_KEPT.browser.alone);\n        if (held) {", "notePassHeld(said, PASS_CLAIM_KEPT.browser.alone, at);"},
		"Forget this browser":            {": \"This browser no longer holds your post key, your name, a badge or a Pass.\"\n        );", "notePassHeld(said);"},
	} {
		at := strings.Index(code, notice[0])
		if at < 0 || strings.Count(code, notice[0]) != 1 || !strings.HasPrefix(strings.TrimSpace(code[at+len(notice[0]):]), notice[1]) {
			t.Errorf("the notice for %s is no longer noted as said beside the Pass this browser holds (%s)", name, notice[1])
		}
	}
	if n := strings.Count(code, "notePassHeld("); n != 16 {
		t.Errorf("notePassHeld( appears %d times; want its definition, the twelve notices above, showClaimed's and "+
			"showPassRunOut's two", n)
	}
	// A notice put up after an await is noted beside the record taken straight
	// after what it says was set, before the await, so another tab's renewal,
	// or a Pass stored there, while the thread loads, the Verify page is drawn
	// or the forget is answered counts against it: a post's, once its answer
	// has marked the Pass run out or removed it and before the thread is
	// fetched; Remove's, once the Pass is removed; Forget this browser's, once
	// the browser is forgotten. Remove's first notice, put up at once, is noted
	// beside the same record. Each reads, beside that record, what has taken
	// held notices off so far (passNoticeForgets), which this tab's own Remove
	// and Forget have already moved by then, and after the await puts up its
	// held notice only if nothing has moved it since: the post's says nothing
	// then, and Remove's and Forget's say the claim's warning alone, if that.
	submit := withoutComments(submitHandler(t, script))
	marked := strings.Index(submit, "markPassExpired(sent.code);")
	taken := strings.Index(submit, "const said = passHeldRecord();\n    const forgets = passNoticeForgets;\n"+
		"    await navigate(`${threadPath(created.board, created.thread_no)}#p${created.no}`);\n"+
		"    if (passNotice && forgets === passNoticeForgets) {\n      showNotice(passNotice);\n      notePassHeld(said);\n    }")
	if marked < 0 || taken < marked || strings.Count(submit, "notePassHeld(") != 1 || strings.Count(submit, "passHeldRecord()") != 1 {
		t.Error("a post's notice that its Pass has run out or is no longer valid is no longer noted, once the thread is drawn, " +
			"beside the Pass this browser held as the answer was acted on, or is put up after Remove or Forget this browser, " +
			"or another tab's removal of the Pass, came while the thread loaded")
	}
	handler := func(opener string) string {
		block := code[strings.Index(code, opener):]
		return block[:strings.Index(block, "\n    return;\n  }")]
	}
	remove := handler(`if (event.target.closest('[data-action="forget-pass"]')) {`)
	if !strings.Contains(remove, "forgetPass();\n    const said = passHeldRecord();\n    const forgets = passNoticeForgets;\n    drawPass(") ||
		strings.Count(remove, "passHeldRecord()") != 1 || strings.Index(remove, "const said = ") > strings.Index(remove, "dropped.then(") ||
		!strings.Contains(remove, "const held = forgets === passNoticeForgets;\n"+
			"      const notice = held ? PASS_CLAIM_KEPT.remove.said : PASS_CLAIM_KEPT.remove.alone;\n") {
		t.Error("Remove the Pass's notices are no longer noted beside what the browser held once the Pass was removed, " +
			"or its claim notice says the browser holds no Pass after a forget that takes held notices off came meanwhile")
	}
	forget := handler(`if (event.target.closest('[data-action="forget-browser"]')) {`)
	if !strings.Contains(forget, "const at = Date.now();\n    const dropped = forgetBrowser(at);\n    const said = passHeldRecord();\n"+
		"    const forgets = passNoticeForgets;\n") ||
		strings.Count(forget, "passHeldRecord()") != 1 || strings.Index(forget, "const said = ") > strings.Index(forget, "renderVerify()") {
		t.Error("Forget this browser's notice is no longer noted beside what the browser held once it was forgotten")
	}
	// That the forget is done, or what it took alone, is said only over the
	// Verify page the press drew: its Forget block, taken once that drawing
	// lands with the reader still under the Verify address, still on the page,
	// and the address still the Verify page's, once the forget is answered
	// too, and only if no forget that takes held notices off came meanwhile.
	// That educhan could not be reached is a claim notice, said on whatever
	// page the reader is on, the claim's warning alone after such a forget.
	if !strings.Contains(forget, "const drawn = renderVerify().then(() =>\n"+
		"      state.route.view === \"verify\" ? document.getElementById(\"forget-browser\") : null\n    );\n"+
		"    Promise.all([drawn, dropped]).then(([forgotten, done]) => {\n      const held = forgets === passNoticeForgets;\n"+
		"      if (!done && passClaimGone <= at) {\n"+
		"        showNotice(held ? PASS_CLAIM_KEPT.browser.said : PASS_CLAIM_KEPT.browser.alone);\n"+
		"        if (held) {\n          notePassHeld(said, PASS_CLAIM_KEPT.browser.alone, at);\n        } else {\n"+
		"          noteClaimNotice(at);\n        }\n"+
		"      } else if (held && forgotten && forgotten.isConnected && state.route.view === \"verify\") {\n        showNotice(\n"+
		"          done\n            ? \"This browser no longer holds your post key, your name, a badge, a Pass or a Pass bought here "+
		"and still being collected.\"\n            : \"This browser no longer holds your post key, your name, a badge or a Pass.\"\n"+
		"        );\n        notePassHeld(said);\n      }\n    });") ||
		strings.Count(forget, "showNotice(") != 2 || strings.Count(forget, "renderVerify()") != 1 {
		t.Error("Forget this browser says the forget is done over a page the reader went to, or a Verify page drawn since, " +
			"or after a forget that takes held notices off came meanwhile, or no longer says on whatever page that educhan " +
			"could not be reached")
	}
	// Like any notice, a Pass notice goes when this tab draws a page afresh:
	// render clears it before it draws the page the reader moves to, and the
	// pageshow listener has render draw afresh only the Pass page brought back
	// from the back-forward cache, so a tab brought back on any other page
	// keeps its notice, a claim notice's warning included, as the docs say
	// wherever they say a claim notice stays.
	render := withoutComments(jsFunction(t, script, "render"))
	if strings.Count(render, "clearNotice();") != 1 || !strings.Contains(render, "  renderNav(route);\n  clearNotice();\n") ||
		strings.Index(render, "clearNotice();") > strings.Index(render, "const age = ageStatus();") {
		t.Error("render no longer takes the notice off before it draws the page the reader moves to")
	}
	show := withoutComments(jsBlock(t, script, `window.addEventListener("pageshow", (event) => {`, "});"))
	if strings.Count(show, "render()") != 1 || strings.Contains(show, "Notice") ||
		!strings.Contains(show, "  if (event.persisted && state.route.view === \"pass\") {\n    if (storedPass()) {\n"+
			"      passLost = \"\";\n    }\n    render();\n  }\n") {
		t.Error("a tab brought back from the back-forward cache keeps its notice on the Pass page, which it draws afresh, " +
			"or loses it on another page")
	}
	// No claim notice is put up once this tab has learned that nothing is
	// left to collect as of a time after its own forget went (passClaimGone,
	// compared with at, when that forget went): forgetPassOrder and Stop
	// collecting it here then say nothing, Remove the Pass nothing beyond what
	// its press said, and Forget this browser, over the Verify page its press
	// drew, only what it took, or nothing once a Remove or Forget pressed
	// since, here or in another tab, or another tab's removal of the Pass, has
	// moved passNoticeForgets (above). A forget pressed since and not yet
	// answered no longer stops it.
	for name, gate := range map[string][2]string{
		"forgetPassOrder": {withoutComments(jsFunction(t, script, "forgetPassOrder")),
			`const at = Date\.now\(\);\s*api\("/api/pass/claim", [^\n]*\.catch\(\(err\) => \{\s*` +
				`if \(err\.status !== 403 && passClaimGone <= at\) \{`},
		"Remove the Pass": {remove, `const at = Date\.now\(\);\s*const dropped = forgetPassClaim\("remove", at\);\s*forgetPass\(\);[\s\S]*` +
			`dropped\.then\(\(done\) => \{\s*if \(done \|\| passClaimGone > at\) \{\s*return;\s*\}`},
		"Forget this browser": {forget, `const at = Date\.now\(\);\s*const dropped = forgetBrowser\(at\);[\s\S]*` +
			`if \(!done && passClaimGone <= at\) \{`},
		"Stop collecting it here": {handler(`const stopClaim = event.target.closest('[data-action="stop-pass-claim"]');`),
			`const at = Date\.now\(\);\s*const dropped = forgetPassClaim\("stop", at\);\s*const visit = passVisit;[\s\S]*` +
				`if \(!done\) \{\s*refocus\(stopClaim\);\s*if \(passClaimGone <= at\) \{\s*showPassNotice\(`},
	} {
		if !regexp.MustCompile(gate[1]).MatchString(gate[0]) {
			t.Errorf("%s puts its claim notice up although this tab has learned that nothing is left to collect since its "+
				"forget went, or no longer once a forget pressed since is still unanswered", name)
		}
	}
	// Only an answer showing the claim gone takes a claim notice off, on
	// whatever page, whoever said it: the latest time as of which this tab
	// has learned that is kept, and a claim notice said for a forget that went
	// before it goes, Remove's and Forget's whole; one said for that forget or
	// a later one stays. Its own forget's answer (forgetPassClaim), another
	// tab's read from its mark (heedPassForgetAnswer), and a claim of this
	// tab's collection or GET /api/pass finding nothing left, as of when it was
	// sent, tell it; nothing else does.
	if got := withoutComments(jsFunction(t, script, "heedPassClaimGone")); got != "function heedPassClaimGone(at) {\n"+
		"  if (at > passClaimGone) {\n    passClaimGone = at;\n  }\n"+
		"  const text = el.notice.querySelector(\".notice-text\");\n"+
		"  const claim = text && noticeShows(text) ? passClaimNotices.get(text) : undefined;\n"+
		"  if (claim && claim.at < at) {\n    takePassNoticeOff();\n  }\n}" ||
		!strings.Contains(code, "\nlet passClaimGone = 0;\n") || strings.Count(code, "passClaimGone = ") != 2 {
		t.Errorf("a claim notice no longer goes once this tab learns that nothing is left to collect as of a time after its "+
			"forget went, or goes otherwise, or passClaimGone moves back or elsewhere:\n%s", got)
	}
	collect := withoutComments(jsFunction(t, script, "collectPass"))
	sent := strings.Index(collect, "const sent = Date.now();\n    try {\n      res = await api(\"/api/pass/claim\", {")
	answered := strings.Index(collect, "if (res.claim !== true) {\n        dropPassNonce(PASS_NONCE_STORAGE_KEY);\n        heedPassClaimGone(sent);\n      }")
	refused := strings.Index(collect, "dropPassNonce(PASS_NONCE_STORAGE_KEY);\n      heedPassClaimGone(sent);\n"+
		"      const notices = passNoticeForgets;\n      const found = again ? await passAfterLostAnswer() : \"\";")
	if sent < 0 || answered < sent || refused < answered || strings.Index(collect, "showClaimed(") < answered ||
		strings.Index(collect, "if (forgets !== passForgets) {\n      return;\n    }") > answered {
		t.Error("this tab's collection no longer takes off a claim notice said for an earlier forget once a claim it sent " +
			"after it finds nothing left, before what the answer brings is drawn")
	}
	// A renewal's notice, and what the check of the Pass held after a lost
	// answer finds, wait on that check (confirmRenewal, passAfterLostAnswer),
	// in this tab's collection and in a reissue code's answer. Each reads what
	// has taken held notices off so far before the check, and says nothing if
	// that has moved by the time it is answered, another tab's removal of the
	// Pass with no mark heard first, say, since there was no notice yet for it
	// to take off.
	redeemed := withoutComments(jsFunction(t, script, "redeemPassReissue"))
	for name, gate := range map[string][3]string{
		"a renewal collected": {collect, "const notices = passNoticeForgets;\n      const notice = news === \"renewed\" ? await confirmRenewal(res) : \"\";\n" +
			"      if (forgets !== passForgets) {\n        return;\n      }\n      if (visit === passVisit || news) {\n" +
			"        showClaimed(res.claim === true, news, notices === passNoticeForgets ? notice : \"\", asked);\n      }",
			"showClaimed(res.claim === true"},
		"the Pass checked after a claim's lost answer": {collect, "const notices = passNoticeForgets;\n" +
			"      const found = again ? await passAfterLostAnswer() : \"\";\n      if (forgets !== passForgets) {\n        return;\n      }\n" +
			"      const notice = notices === passNoticeForgets ? found : \"\";\n      if (visit === passVisit || notice) {\n" +
			"        showClaimed(false, notice ? \"checked\" : \"\", notice, asked);\n      }", "showClaimed(false"},
		"a reissue code's renewal or lost answer": {redeemed, "let notice = \"\";\n  const notices = passNoticeForgets;\n" +
			"  if (news === \"renewed\") {\n    notice = await confirmRenewal(res, true);\n  } else if (", "showPassNotice(notice);"},
	} {
		at := strings.Index(gate[0], gate[1])
		if at < 0 || strings.Count(gate[0], gate[2]) != 1 {
			t.Errorf("%s is no longer said only if nothing that takes a held notice off came while the Pass held was checked", name)
		}
	}
	cleared := regexp.MustCompile(`if \(passForgotten\(forgets\)\) \{\s*return;\s*\}\s*` +
		`if \(notices !== passNoticeForgets\) \{\s*notice = "";\s*\}`).FindStringIndex(redeemed)
	if checked, said := strings.Index(redeemed, "notice = await passAfterLostAnswer("), strings.Index(redeemed, "showPassNotice(notice);"); checked < 0 ||
		cleared == nil || cleared[0] < checked || said < cleared[1] ||
		strings.Count(redeemed, "passNoticeForgets") != 2 || strings.Count(collect, "passNoticeForgets") != 4 {
		t.Error("a reissue code's renewal, or what the check after its lost answer finds, is said although something that " +
			"takes a held notice off came while the Pass held was checked")
	}
	if got := withoutComments(jsFunction(t, script, "loadPassInfo")); got != "async function loadPassInfo() {\n  const sent = Date.now();\n"+
		"  try {\n    const info = await api(\"/api/pass\");\n    if (!(info && info.enabled && info.claim)) {\n      heedPassClaimGone(sent);\n    }\n"+
		"    return info && info.enabled ? info : { enabled: false };\n  } catch (err) {\n    return { enabled: false, failed: err };\n  }\n}" {
		t.Errorf("GET /api/pass no longer takes off a claim notice said for a forget that went before it was sent once it "+
			"answers that nothing is left to collect, or does so when it was not answered:\n%s", got)
	}
	if strings.Count(code, "heedPassClaimGone(") != 6 {
		t.Error("something other than a forget's answer, here or from another tab's mark, a claim of this tab's collection " +
			"or GET /api/pass takes off a claim notice, or one of them no longer does")
	}
	// Remove and Forget this browser in another tab, by its mark, and another
	// tab's removal of the Pass with a code to read, count as what takes a held
	// notice off (passNoticeForgets), for one still to be put up after an
	// await, and take the notice on screen off: on the Pass page whatever it
	// says, and on any other page a held notice; but not a claim notice, which
	// stays, Remove's and Forget's giving way to the claim's warning alone,
	// noted for the same forget, through showNotice, which keeps focus on it.
	// A Stop's mark leaves every notice. Nothing else in those two places
	// takes a notice off.
	if got := withoutComments(jsFunction(t, script, "dropForgottenPassNotice")); got != "function dropForgottenPassNotice() {\n"+
		"  passNoticeForgets += 1;\n  const text = el.notice.querySelector(\".notice-text\");\n"+
		"  if (!text || !noticeShows(text)) {\n    return;\n  }\n  const claim = passClaimNotices.get(text);\n"+
		"  if (!claim) {\n    if (passPageShown() || passNoticesHeld.has(text)) {\n      takePassNoticeOff();\n    }\n"+
		"  } else if (passNoticesHeld.has(text)) {\n    showNotice(claim.alone);\n    noteClaimNotice(claim.at);\n  }\n}" {
		t.Errorf("another tab's Remove, Forget or removal of the Pass no longer counts against a held notice still to be put "+
			"up, or no longer takes another notice off the Pass page or a held one off any page, or takes a claim notice off "+
			"or leaves Remove's or Forget's whole:\n%s", got)
	}
	for name, call := range map[string]string{
		"stopForPassForget": "    passJustIssued = null;\n    dropForgottenPassNotice();\n  }\n",
		"followPassStorage": "    forgetInvalidCode(was);\n    dropForgottenPassNotice();\n  }\n",
	} {
		body := withoutComments(jsFunction(t, script, name))
		if strings.Count(body, call) != 1 || strings.Contains(body, "takePassNoticeOff()") || strings.Contains(body, "clearNotice()") {
			t.Errorf("%s no longer takes a notice off by the rules for another tab's Remove or Forget, or takes one off another way", name)
		}
	}
	if strings.Count(code, "dropForgottenPassNotice()") != 3 || strings.Contains(code, "passNoticeForgotten") {
		t.Error("something else takes a notice off by dropForgottenPassNotice, which only another tab's Remove, Forget or " +
			"removal of the Pass does")
	}
	// Taken off by something other than the reader, the notice does not take
	// focus on its dismiss button to the top of the document with it: focus
	// goes to the page just below, without scrolling it, where a drawing that
	// follows keeps it (redrawPass, through passTwin's id).
	if got := withoutComments(jsFunction(t, script, "takePassNoticeOff")); got != "function takePassNoticeOff() {\n"+
		"  const focused = el.notice.contains(document.activeElement);\n  clearNotice();\n"+
		"  if (focused) {\n    el.main.focus({ preventScroll: true });\n  }\n}" ||
		strings.Count(code, "takePassNoticeOff()") != 4 {
		t.Errorf("a notice taken off by something other than the reader drops focus on it to the top of the document, "+
			"or scrolls the page:\n%s", got)
	}
}

// A code handed over whose Pass's expiry has passed by the server's clock, as
// it came (a reissue code's, for a Pass in its 30 days of grace, say) or since,
// is never called ready, kept or not, whatever this computer's clock says: it
// has run out and needs renewing (TestPassRunOutIsJudgedByTheServersClock).
// Beside a good Pass held, which the code box will not put another code in
// place of, no step says to give that Pass up for it: it is to be written down
// and renewed in a browser that holds no good Pass. A code typed in that has
// run out, and that the browser cannot store, is said to have run out as well
// as not stored.
func TestPassRunOutCodeHandedOverIsNeverReady(t *testing.T) {
	script := appScript(t)
	issued := withoutComments(jsFunction(t, script, "passIssuedHTML"))
	ranOut := strings.Index(issued, "\n  const ranOut = !here && new Date(issued.expires) <= serverNow();\n")
	if ranOut < 0 || ranOut > strings.Index(issued, "let status = ") {
		t.Error("the panel of a code just handed over no longer tells whether its Pass has run out")
	}
	// Every case for a code this browser does not hold that has run out comes
	// before the one that would leave it ready.
	cases := []string{"if (here && held.expired) {", "} else if (!here && issued.kept) {",
		"} else if (!here && held && !held.expired && ranOut) {", "} else if (!here && held && !held.expired) {",
		"} else if (!here && ranOut) {", "} else if (!here) {", "\n  return `"}
	at := make([]int, len(cases))
	for i, c := range cases {
		at[i] = strings.Index(issued, c)
		if at[i] < 0 || (i > 0 && at[i] < at[i-1]) {
			t.Fatalf("passIssuedHTML no longer has %q in its order; this test reads it", c)
		}
	}
	again := "again = \"educhan keeps no copy of it, and this browser couldn't keep it either, so it won't be shown again " +
		"once you leave this page.\"; need = \"You need it to renew the Pass, here or on another device.\";"
	if got := words(issued[at[2]:at[3]]); got != "} else if (!here && held && !held.expired && ranOut) { "+
		"status = \"This Pass has run out.\"; "+
		"said = \"This browser couldn't store it in place of the code it has, which is still good, so it isn't sent with your posts.\"; "+
		"if (box) { said += \" It can be renewed in a browser that holds no good Pass, by entering this code there.\"; } "+again {
		t.Errorf("a code that has run out, beside a good Pass held, is called ready, or told to take that Pass's place:\n%s", got)
	}
	if got := words(issued[at[4]:at[5]]); got != "} else if (!here && ranOut) { "+
		"status = \"This Pass has run out.\"; said = \"This browser couldn't store it, so it can't be renewed here.\"; "+
		"if (box) { said += \" Allow this site to store data, then enter the code below to renew it.\"; } "+again {
		t.Errorf("a code that has run out, with nothing good held, is called ready, or not told how to renew it:\n%s", got)
	}
	if got := jsString(t, script, "PASS_RUN_OUT_NOT_KEPT"); got != "This Pass has run out, and this browser couldn't store it, "+
		"so it can't be renewed here. Allow this site to store data, then enter the code again to renew it." {
		t.Errorf("PASS_RUN_OUT_NOT_KEPT says %q", got)
	}
	if strings.Count(withoutComments(script), "PASS_RUN_OUT_NOT_KEPT") != 2 {
		t.Error("PASS_RUN_OUT_NOT_KEPT is said somewhere other than for a code typed in that has run out " +
			"(TestPassRunOutIsSaidForThePageBesideIt)")
	}
}

// Words on the Pass page that point to the code box below, the held Pass's
// "enter a newer code below" and those of a code this browser could not keep,
// are said only where it is drawn: on every page but one from a server without
// the Pass, which takes no code, and so offers no Check it again either. A code
// the browser could not keep says what keeps it here in steps the code box
// takes: with nothing held, or a Pass held that has run out, which a code typed
// in replaces, to let the site store data and enter it; beside a good Pass
// held, which the box will not put another code in place of, to write down both
// codes, and any other code on the panel still needed, remove that Pass, which
// takes the panel away, and then enter it. None says the browser stores
// nothing, which it may not know (storage full, say). Such a code counts as
// kept once this browser holds it, typed in here or stored by another tab, so
// it is worded as one the browser no longer has once another takes its place or
// the data is cleared, and Buy is no longer held back for it
// (TestPassIssuedPanelSaysWhatThisBrowserHolds).
func TestPassCodeBoxIsPointedToOnlyWhereItIsDrawn(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	view := withoutComments(jsFunction(t, script, "passViewHTML"))
	box := strings.Index(view, "\n  const box = Boolean(info.enabled || info.failed);\n")
	failed := strings.Index(view, "if (info.failed) {")
	off := strings.Index(view, "if (!info.enabled) {\n    parts.push(`<p class=\"empty\">The Pass is not on sale on this server.</p>`);\n"+
		"    return parts.join(\"\");\n  }")
	fields := regexp.MustCompile(`parts\.push\(passCodeFieldHTML\(held\)\);`).FindAllStringIndex(view, -1)
	if box < 0 || box > strings.Index(view, "parts.push(passIssuedHTML(issued, held, box));") || failed < box || off < failed ||
		len(fields) != 2 || fields[0][0] < failed || fields[0][0] > off || fields[1][0] < off ||
		strings.Count(view, ", box));") != 3 {
		t.Error("passViewHTML no longer tells the panels whether the code box is drawn below them")
	}
	held := withoutComments(jsFunction(t, script, "passHeldHTML"))
	if !strings.HasPrefix(held, "function passHeldHTML(info, held, codeOnScreen, box) {") ||
		!strings.Contains(words(held), "const check = held.expired && box ? ` <p class=\"muted\">Renewed it somewhere else? "+
			"Check it again, or enter a newer code below.</p> <div class=\"actions\"> "+
			"<button class=\"btn subtle\" type=\"button\" data-action=\"check-pass\">Check it again</button> </div>` : \"\";") {
		t.Error("the held Pass's panel offers Check it again, or a newer code below, on a page without the code box")
	}
	issued := withoutComments(jsFunction(t, script, "passIssuedHTML"))
	goodOut := strings.Index(issued, "} else if (!here && held && !held.expired && ranOut) {")
	good := strings.Index(issued, "} else if (!here && held && !held.expired) {")
	noneOut := strings.Index(issued, "} else if (!here && ranOut) {")
	none := strings.Index(issued, "} else if (!here) {")
	end := strings.Index(issued, "\n  return `\n    <div class=\"verify-panel pass-issued\"")
	if goodOut < 0 || good < goodOut || noneOut < good || none < noneOut || end < none {
		t.Fatal("passIssuedHTML no longer words a code the browser could not keep for what it holds; this test reads it")
	}
	// A code that has run out (TestPassRunOutCodeHandedOverIsNeverReady) has
	// steps of its own, and none of them gives up a good Pass held for it.
	if got := words(issued[goodOut:good]); strings.Contains(got, "remove") || !strings.Contains(got, "if (box) {") {
		t.Errorf("a code that has run out, beside a good Pass held, says to remove that Pass, or points past the code box:\n%s", got)
	}
	if got := words(issued[noneOut:none]); !strings.Contains(got, `if (box) { said += " Allow this site to store data, then enter the code below to renew it."; }`) {
		t.Errorf("a code that has run out, with nothing good held, no longer says what keeps it here to renew:\n%s", got)
	}
	if got := words(issued[good:noneOut]); got != "} else if (!here && held && !held.expired) { "+
		"said = \"This browser couldn't store it in place of the code it has, so it isn't sent with your posts.\"; "+
		"if (box) { said += \" To use it here, write down this code, the one this browser has and any other code on this "+
		"panel you still need, remove that Pass, which takes this panel off the page, then enter this code below.\"; } "+
		"again = \"educhan keeps no copy of it, and this browser couldn't keep it either, so it won't be shown again once "+
		"you leave this page.\"; need = \"You need it to use the Pass, here or on another device.\";" {
		t.Errorf("a code the browser could not keep beside a good Pass held offers steps the code box refuses, or none:\n%s", got)
	}
	if got := words(issued[none:end]); got != "} else if (!here) { "+
		"said = \"This browser couldn't store it, so it can't send it with your posts.\"; "+
		"if (box) { said += \" Allow this site to store data, then enter the code below.\"; } "+
		"again = \"educhan keeps no copy of it, and this browser couldn't keep it either, so it won't be shown again once "+
		"you leave this page.\"; need = \"You need it to use the Pass, here or on another device.\"; }" {
		t.Errorf("a code the browser could not keep, with nothing good held, no longer says what keeps it here:\n%s", got)
	}
	// The steps are the code box's own: a good Pass held refuses another code
	// (with the same test as the panel's), Remove takes the panel away, and a
	// Pass that has run out is replaced.
	submit := withoutComments(jsFunction(t, script, "submitPass"))
	remove := code[strings.Index(code, `if (event.target.closest('[data-action="forget-pass"]')) {`):]
	remove = remove[:strings.Index(remove, "\n  }\n")]
	if !strings.Contains(submit, "if (held && !held.expired) {\n      showPassNotice(\"This browser has a Pass already. "+
		"To use another one here, write this one's code down, then remove it.\");\n      notePassHeld();\n      return;\n    }") ||
		!strings.Contains(remove, "passJustIssued = null;") {
		t.Error("the steps a code the browser could not keep gives are no longer the ones the code box and Remove take")
	}
	if strings.Contains(code, "store anything") || strings.Contains(code, "keep anything") {
		t.Error("the Pass page says this browser stores nothing, which it cannot tell from storage that is full")
	}
	if got := jsString(t, script, "PASS_NOT_KEPT"); got !=
		"This browser couldn't store the Pass, so it can't send it with your posts. Allow this site to store data, then enter the code again." {
		t.Errorf("PASS_NOT_KEPT says %q", got)
	}
	// Held since, by storePass here or another tab's storing it
	// (followPassStorage, both pinned in TestPassPageDetails), a code on the
	// panel counts as kept, its own or one carried, and nothing else sets kept.
	keep := withoutComments(jsFunction(t, script, "keepShownCode"))
	if keep != "function keepShownCode(code) {\n  if (!passJustIssued) {\n    return;\n  }\n"+
		"  if (passJustIssued.code === code) {\n    passJustIssued.kept = true;\n  }\n"+
		"  for (const earlier of passJustIssued.earlier) {\n    if (earlier.code === code) {\n      earlier.kept = true;\n    }\n  }\n}" ||
		strings.Count(code, "keepShownCode(") != 3 || strings.Count(code, ".kept = ") != 2 {
		t.Errorf("a code on the panel this browser has held since it came is not counted as kept:\n%s", keep)
	}
}

// A press on the Pass page turns its button off before it waits on anything,
// so pressing again while the first is on its way sends nothing more: not a
// second checkout, a reissue code spent twice, or the Pass held checked twice.
// Nothing else stops a second press, since the page's submit and Check it
// again handlers call these straight away.
func TestPassPressesTurnTheirButtonOffBeforeTheyWait(t *testing.T) {
	script := appScript(t)
	submit := withoutComments(jsFunction(t, script, "submitPass"))
	reissue := strings.Index(submit, "if (isReissueCode(typed)) {")
	if reissue < 0 {
		t.Fatal("submitPass no longer hands a reissue code over in its own branch; this test reads around it")
	}
	reissueEnd := strings.Index(submit[reissue:], "\n    }\n") + reissue
	for name, body := range map[string]string{
		"a reissue code":                    submit[reissue:reissueEnd],
		"Use this code here, Buy and Renew": submit[reissueEnd:],
		"Check it again":                    withoutComments(jsFunction(t, script, "checkHeldPass")),
	} {
		wait, off := strings.Index(body, "await "), strings.Index(body, "button.disabled = true;")
		if wait < 0 || off < 0 || off > wait {
			t.Errorf("%s waits before turning its button off, so a second press sends again", name)
		}
	}
}

// The Pass's price is Stripe's amount, a whole number of the currency's
// smallest unit, and the page divides it by ten to the power Stripe gives that
// unit, which the server sends beside it (stripeExponent), not by the
// browser's own count of the currency's decimals: Stripe keeps ISK and UGX in
// hundredths, where the browser has them whole, so 300000 is 3,000 ISK, never
// 300,000. Intl then writes a whole amount with the currency's usual digits,
// and one with a part of a unit with Stripe's decimals, which the usual digits
// could round away: the browser has ALL whole too, and 1050 is ALL 10.50,
// never 11.
func TestPassPriceIsWrittenWithStripesDecimals(t *testing.T) {
	script := appScript(t)
	text := withoutComments(jsFunction(t, script, "passPriceText"))
	divided := regexp.MustCompile(`\n    return format\.format\(price\.amount / 10 \*\* price\.(\w+)\);\n`).FindStringSubmatch(text)
	if divided == nil || !strings.Contains(text, "    !Number.isSafeInteger(price."+divided[1]+") ||\n    price."+divided[1]+" < 0\n  ) {\n    return \"\";") {
		t.Fatalf("passPriceText no longer divides Stripe's amount by ten to the exponent the server sends, or uses one it has not checked:\n%s", text)
	}
	code := withoutComments(script)
	if strings.Contains(code, "resolvedOptions") {
		t.Error("the page takes a currency's decimals from the browser's own table, which is not Stripe's")
	}
	// The only digits the page ever gives Intl are Stripe's, and only for an
	// amount with a part of a unit.
	field := "price." + divided[1]
	if !strings.Contains(text, "\n  const options = { style: \"currency\", currency: price.currency.toUpperCase() };\n"+
		"  if (price.amount % 10 ** "+field+" !== 0) {\n    options.minimumFractionDigits = "+field+";\n"+
		"    options.maximumFractionDigits = "+field+";\n  }\n  try {\n    const format = new Intl.NumberFormat(undefined, options);\n") ||
		strings.Count(code, "FractionDigits") != 2 {
		t.Errorf("passPriceText no longer writes a part of a unit with Stripe's decimals, or gives Intl other digits:\n%s", text)
	}
	// What the page hands Intl, read from the price as GET /api/pass sends it,
	// is what Stripe charges in the Price's own currency, and a part of a unit
	// is written with as many decimals as Stripe gives it.
	for _, c := range []struct {
		currency string
		amount   int64
		charged  float64
		digits   int // -1 for the currency's usual digits
	}{
		{"usd", 2000, 20, -1},
		{"usd", 2050, 20.5, 2},
		{"jpy", 2000, 2000, -1},
		{"isk", 300000, 3000, -1},
		{"ugx", 7500000, 75000, -1},
		{"bhd", 20000, 20, -1},
		{"bhd", 20005, 20.005, 3},
		{"all", 1050, 10.5, 2},
		{"rsd", 9999, 99.99, 2},
	} {
		sent, err := json.Marshal(passPrice{Amount: c.amount, Currency: c.currency, Exponent: stripeExponent(c.currency)})
		if err != nil {
			t.Fatal(err)
		}
		var price map[string]any
		if err := json.Unmarshal(sent, &price); err != nil {
			t.Fatal(err)
		}
		exponent, ok := price[divided[1]].(float64)
		if !ok {
			t.Fatalf("the page reads the price's %q, which GET /api/pass does not send: %s", divided[1], sent)
		}
		if shown := float64(c.amount) / math.Pow(10, exponent); shown != c.charged {
			t.Errorf("a Pass priced at %d %s in Stripe is written as %v %s; Stripe charges %v", c.amount, c.currency, shown, c.currency, c.charged)
		}
		digits := -1
		if c.amount%int64(math.Pow(10, exponent)) != 0 {
			digits = int(exponent)
		}
		if digits != c.digits {
			t.Errorf("a Pass priced at %d %s in Stripe is written with %d decimals (-1 for the usual); want %d", c.amount, c.currency, digits, c.digits)
		}
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

// The header's Pass link follows the Pass this browser holds as soon as that
// changes, not at the next drawing of the page: every write of the Pass goes
// through storePass, markPassExpired or forgetPass, and storePass and
// forgetPass each ask showPassLink once they have written, as does a change
// another tab makes (followPassStorage), so on a server without the Pass the
// link goes with the Pass, in this tab and every other, and comes back when
// one is stored. markPassExpired only marks a Pass held, which keeps the link
// as it was. Focus on the link as it goes moves to the page just below.
func TestHeaderPassLinkFollowsThePassHeld(t *testing.T) {
	script := appScript(t)
	code := withoutComments(script)
	if store := withoutComments(jsFunction(t, script, "storePass")); !strings.Contains(store,
		"  storageSet(PASS_STORAGE_KEY, JSON.stringify(expired ? { code, expires, expired } : { code, expires }));\n"+
			"  showPassLink();\n  const kept = storedPass();\n") {
		t.Error("storing a Pass no longer shows the header's Pass link at once")
	}
	if got := withoutComments(jsFunction(t, script, "forgetPass")); got != "function forgetPass() {\n  try {\n"+
		"    localStorage.removeItem(PASS_STORAGE_KEY);\n  } catch {\n\n    return;\n  }\n  showPassLink();\n}" {
		t.Errorf("removing the Pass no longer hides the header's Pass link at once on a server without the Pass:\n%s", got)
	}
	if !strings.Contains(withoutComments(jsFunction(t, script, "followPassStorage")), "  dropStalePassNotice();\n  showPassLink();\n") {
		t.Error("another tab's change to the Pass no longer shows or hides the header's Pass link here")
	}
	// Focus on the link as it goes would fall to the top of the document with
	// it, so it goes to the page just below, without scrolling the page.
	if got := withoutComments(jsFunction(t, script, "showPassLink")); got != "function showPassLink() {\n"+
		"  if (!el.passLink) {\n    return;\n  }\n  const focused = document.activeElement === el.passLink;\n"+
		"  el.passLink.hidden = !(state.site.pass || storedPass());\n"+
		"  if (focused && el.passLink.hidden) {\n    el.main.focus({ preventScroll: true });\n  }\n}" {
		t.Errorf("the header's Pass link, hidden with focus on it, drops focus to the top of the document or scrolls "+
			"the page, or no longer follows the Pass held:\n%s", got)
	}
	if n := strings.Count(code, "showPassLink()"); n != 5 {
		t.Errorf("showPassLink() appears %d times; want its definition, renderNav, storePass, forgetPass and followPassStorage", n)
	}
	if strings.Count(code, "PASS_STORAGE_KEY, ") != 2 || strings.Count(code, "localStorage.removeItem(PASS_STORAGE_KEY)") != 1 {
		t.Error("the Pass is written somewhere other than storePass, markPassExpired and forgetPass, which the header's link would not follow")
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
			// Stripe's events carry the payer's address too, and Stripe Tax
			// asks for one, so neither page gives a list that leaves it out.
			"Stripe keeps your payment details, such as your card, email, name and billing address, " +
				"as any shop's payment company does, and educhan stores none of them",
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
	// its time (passReissueNoOrigin), and not at all for a Pass more than 30
	// days past its expiry, which the server no longer keeps
	// (passExpiredGrace), so every promise of a reissue code for a lost code
	// says it may no longer be possible then, in these words.
	held := words(jsFunction(t, script, "passHeldHTML"))
	for name, want := range map[string]string{
		"the held Pass": "you'll be sent a reissue code to enter on this page, which gives your Pass a new code; " +
			"if the payment that bought it was refunded or disputed, or the Pass ran out more than 30 days ago, " +
			"that may no longer be possible.",
		"the FAQ's Pass entry": "If the payment that bought the Pass was refunded or disputed, or the Pass ran out " +
			"more than 30 days ago, that may no longer be possible.",
	} {
		text := map[string]string{"the held Pass": held, "the FAQ's Pass entry": faq}[name]
		if !strings.Contains(text, "you'll be sent a reissue code") || !strings.Contains(text, want) {
			t.Errorf("%s promises a reissue code for a lost code without saying a refunded purchase, or a Pass run out "+
				"more than 30 days ago, can end it", name)
		}
	}
	if !strings.Contains(intro, `const term = info.enabled ? passTermText(info.days) : "a set time";`) {
		t.Error("the Pass page says how long a Pass lasts without the server's terms")
	}
	if got := jsString(t, script, "PASS_GONE_REASONS"); got != "It may have run out more than 30 days ago, or been refunded or replaced." {
		t.Errorf("PASS_GONE_REASONS says %q", got)
	}
	// The server counts a Pass's term (EDUCHAN_PASS_DAYS), what is left of it
	// after it runs out (passExpiredGrace) and how long a refund is remembered
	// (passRevocationTTL) in days, which a calendar month or year can be a day
	// or more off, so the page gives each length in days, as the server does:
	// "365 days" rather than "a year", and "30 days" rather than "a month".
	if got := withoutComments(jsFunction(t, script, "passTermText")); got != "function passTermText(days) {\n"+
		"  return plural(days, \"day\", \"days\");\n}" {
		t.Errorf("the Pass page gives a Pass's term as something other than the days the server counts:\n%s", got)
	}
	calendar := regexp.MustCompile(`(?i)\b(?:a|one|each|per|every) (?:month|year)\b|\b(?:months|years)\b|\bmonthly\b|\byearly\b|\bannual`)
	for name, text := range map[string]string{
		"the Pass page":        words(withoutComments(passSection(t, script))),
		"the FAQ's Pass entry": faq,
	} {
		for _, found := range calendar.FindAllString(text, -1) {
			t.Errorf("%s gives a length as %q, where the server counts days", name, found)
		}
	}
	if n := strings.Count(withoutComments(script), "${PASS_GONE_REASONS}"); n != 6 {
		t.Errorf("the reasons a Pass may have gone are given in %d places; want a check, a renewal's check, Renew "+
			"and its notice, a post's answer and its notice", n)
	}
}
