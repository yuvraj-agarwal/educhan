package app

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The filter's unit tests. Each table says what a rule must catch and, as
// importantly, what it must leave alone: a student site is full of numbers,
// dates, maths and app names that are not contact details.

func bodyVerdict(body string) contentVerdict {
	return checkContent(contentInput{Body: body}, false)
}

// withCheckDigit completes a number with its Verhoeff check digit.
func withCheckDigit(t *testing.T, base string) string {
	t.Helper()
	for d := '0'; d <= '9'; d++ {
		if verhoeffValid(base + string(d)) {
			return base + string(d)
		}
	}
	t.Fatalf("no Verhoeff check digit completes %s", base)
	return ""
}

func TestVerhoeffMatchesTheReferenceExample(t *testing.T) {
	// The worked example from the scheme's description: 236 takes check
	// digit 3.
	if !verhoeffValid("2363") {
		t.Fatal("2363 should be Verhoeff-valid")
	}
	for _, bad := range []string{"2360", "2361", "2362", "2364", "2365", "2366", "2367", "2368", "2369", "2633"} {
		if verhoeffValid(bad) {
			t.Fatalf("%s should not be Verhoeff-valid", bad)
		}
	}
	// Every single-digit error in a valid number is caught.
	valid := withCheckDigit(t, "23456789012")
	for i := range valid {
		for d := byte('0'); d <= '9'; d++ {
			if d == valid[i] {
				continue
			}
			changed := valid[:i] + string(d) + valid[i+1:]
			if verhoeffValid(changed) {
				t.Fatalf("changing digit %d of %s to %c went unnoticed", i, valid, d)
			}
		}
	}
}

func TestFilterBlocks(t *testing.T) {
	aadhaar := withCheckDigit(t, "23456789012")
	spacedAadhaar := aadhaar[:4] + " " + aadhaar[4:8] + " " + aadhaar[8:]
	dashedAadhaar := aadhaar[:4] + "-" + aadhaar[4:8] + "-" + aadhaar[8:]
	// Keycap digits: each digit, an emoji presentation selector, and the
	// enclosing keycap. The match ends at the last digit.
	keycaps, circled := "", ""
	for _, d := range "9876543210" {
		keycaps += string(d) + "\ufe0f\u20e3"
		circled += string(d) + "\u20dd"
	}

	cases := []struct {
		name, body, rule, match string
	}{
		// Phone numbers.
		{"indian mobile", "call 9876543210 after 6", ruleContact, "9876543210"},
		{"indian mobile with country code", "my number is +91 98765 43210 ok", ruleContact, "+91 98765 43210"},
		{"indian mobile with trunk zero", "ring 09876543210", ruleContact, "09876543210"},
		{"indian mobile in fives", "98765-43210", ruleContact, "98765-43210"},
		{"indian mobile in threes", "number: 987 654 3210", ruleContact, "987 654 3210"},
		{"spaced digits", "9 8 7 6 5 4 3 2 1 0 is me", ruleContact, "9 8 7 6 5 4 3 2 1 0"},
		{"full-width digits", "９８７６５４３２１０", ruleContact, "９８７６５４３２１０"},
		{"devanagari digits", "मेरा नंबर ९८७६५४३२१० है", ruleContact, "९८७६५४३२१०"},
		{"zero-width characters", "98765​43210", ruleContact, "98765​43210"},
		{"us number", "text (415) 867-5309 tonight", ruleContact, "(415) 867-5309"},
		{"us number dashed", "415-867-5309", ruleContact, "415-867-5309"},
		{"us international", "+1 415 867 5309", ruleContact, "+1 415 867 5309"},
		{"uk mobile", "07911 123456", ruleContact, "07911 123456"},
		// Grouped so that the Indian reading leaves a group of one after the
		// trunk zero, which only the UK rule reads.
		{"uk mobile in threes", "07 911 123 456", ruleContact, "07 911 123 456"},
		{"uk international", "+44 7911 123456", ruleContact, "+44 7911 123456"},
		{"other international", "whatsapp +49 151 23456789", ruleContact, "+49 151 23456789"},
		{"australian mobile", "text 0412 345 678", ruleContact, "0412 345 678"},
		{"across a line break", "call me 98765\n43210", ruleContact, "98765\n43210"},
		{"across a blank line", "98765\n\n43210", ruleContact, "98765\n\n43210"},
		{"over three lines", "9876\n543\n210", ruleContact, "9876\n543\n210"},
		{"across a carriage return", "98765\r\n43210", ruleContact, "98765\r\n43210"},
		{"keycap digits", keycaps, ruleContact, strings.TrimSuffix(keycaps, "\ufe0f\u20e3")},
		{"circled digits", circled, ruleContact, strings.TrimSuffix(circled, "\u20dd")},
		{"underscored", "98765_43210", ruleContact, "98765_43210"},
		{"comma between halves", "98765,43210", ruleContact, "98765,43210"},
		{"letter O for zero", "my number is 98765 4321O", ruleContact, "98765 4321O"},
		{"letter l for one", "98765 432l0", ruleContact, "98765 432l0"},
		{"pairs with dashes", "my number is 98-76-54-32-10", ruleContact, "98-76-54-32-10"},

		// Markup tags read as the reader sees them: a spoiler's run on, a code
		// block's set apart.
		{"number split by a spoiler", "98765[spoiler]43210[/spoiler]", ruleContact, "98765[spoiler]43210"},
		{"number in two spoilers", "[spoiler]98765[/spoiler] [spoiler]43210[/spoiler]", ruleContact,
			"98765[/spoiler] [spoiler]43210"},
		{"number after a word in a spoiler", "call me[spoiler]9876543210[/spoiler]", ruleContact, "9876543210"},
		{"app split by a spoiler", "add me on sn[spoiler]ap[/spoiler]", ruleContact, "add me on sn[spoiler]ap"},
		{"handle split by a spoiler", "my snap is cool[spoiler]kid_12[/spoiler]", ruleContact,
			"my snap is cool[spoiler]kid_12"},
		{"email split by a spoiler", "coolkid12@g[spoiler]mail.com[/spoiler]", ruleContact,
			"coolkid12@g[spoiler]mail.com"},
		{"link split by a spoiler", "disc[spoiler]ord.gg/abc123[/spoiler]", ruleContact, "disc[spoiler]ord.gg/abc123"},
		{"app in a code block", "add me on [code]snap[/code]", ruleContact, "add me on [code]snap"},
		{"handle in a code block", "my snap is [code]coolkid_12[/code]", ruleContact, "my snap is [code]coolkid_12"},

		// Email addresses.
		{"email", "mail priya.sharma@gmail.com for notes", ruleContact, "priya.sharma@gmail.com"},
		{"email spaced", "priya @ gmail . com", ruleContact, "priya @ gmail . com"},
		{"email spelled out", "priya.sharma at gmail dot com", ruleContact, "priya.sharma at gmail dot com"},
		{"email bracketed", "priya [at] outlook [dot] com", ruleContact, "priya [at] outlook [dot] com"},
		{"email at an institution", "priya at iitb dot ac dot in", ruleContact, "priya at iitb dot ac dot in"},
		{"email in code", "[code]ADMIN = \"rahul.k@yahoo.co.in\"[/code]", ruleContact, "rahul.k@yahoo.co.in"},
		{"email bracketed dot", "coolkid12@gmail[.]com", ruleContact, "coolkid12@gmail[.]com"},
		{"email bracketed at", "coolkid12[@]gmail.com", ruleContact, "coolkid12[@]gmail.com"},
		{"email with no tld", "coolkid12@gmail", ruleContact, "coolkid12@gmail"},
		{"email with at and dot spelled", "coolkid@gmail dot com", ruleContact, "coolkid@gmail dot com"},
		{"email spaced at and spelled dot", "mail coolkid12 @ gmail dot com", ruleContact, "coolkid12 @ gmail dot com"},
		{"email digit in tld", "coolkid12@gmail.c0m", ruleContact, "coolkid12@gmail.c0m"},

		// Links to profiles and invites.
		{"discord invite", "join discord.gg/abc123", ruleContact, "discord.gg/abc123"},
		{"discord invite spelled", "discord dot gg/abc123", ruleContact, "discord dot gg/abc123"},
		{"instagram profile", "https://www.instagram.com/some_user", ruleContact, "https://www.instagram.com/some_user"},
		{"telegram", "t.me/someone", ruleContact, "t.me/someone"},
		{"whatsapp link", "wa.me/919876543210", ruleContact, "wa.me/919876543210"},
		{"snapchat add", "snapchat.com/add/cool_guy", ruleContact, "snapchat.com/add/cool_guy"},
		{"bracketed dot in a link", "discord(.)gg/abc123", ruleContact, "discord(.)gg/abc123"},
		{"spaced short host", "t . me/coolkid", ruleContact, "t . me/coolkid"},
		{"host spelled letter by letter", "d i s c o r d . g g / abc123", ruleContact, "d i s c o r d . g g / abc123"},
		{"whatsapp short link", "https://wa.link/abc123", ruleContact, "https://wa.link/abc123"},
		{"whatsapp web", "https://web.whatsapp.com/send?phone=919876543210", ruleContact,
			"https://web.whatsapp.com/send"},

		// Asking to talk somewhere else, and handles.
		{"add me on snap", "add me on snap", ruleContact, "add me on snap"},
		{"add me on snap with handle", "add me on snap @ my_name", ruleContact, "add me on snap @ my_name"},
		{"spaced app name", "add me on s n a p", ruleContact, "add me on s n a p"},
		{"dotted app name", "dm me on d.i.s.c.o.r.d", ruleContact, "dm me on d.i.s.c.o.r.d"},
		{"leetspeak", "add me on sn4pch4t", ruleContact, "add me on sn4pch4t"},
		{"leetspeak discord", "hmu on d1sc0rd", ruleContact, "hmu on d1sc0rd"},
		{"cyrillic lookalikes", "add me on ѕnар", ruleContact, "add me on ѕnар"},
		{"nickname", "hmu on insta", ruleContact, "hmu on insta"},
		{"whats your snap", "what's your snap?", ruleContact, "what's your snap"},
		{"drop your insta", "drop ur insta", ruleContact, "drop ur insta"},
		{"whats your number", "whats ur number", ruleContact, "whats ur number"},
		{"join my discord", "join my discord for jee prep", ruleContact, "join my discord"},
		{"lets move", "lets talk on telegram", ruleContact, "lets talk on telegram"},
		{"insta colon handle", "insta: x_y_z", ruleContact, "insta: x_y_z"},
		{"snap at handle", "snap @cool.guy", ruleContact, "snap @cool.guy"},
		{"my snap is", "my snap is ana_23", ruleContact, "my snap is ana_23"},
		{"whatsapp me", "whatsapp me", ruleContact, "whatsapp me"},
		{"steam", "add me on steam", ruleContact, "add me on steam"},
		{"dotted app and handle", "s.n.a.p: coolkid_12", ruleContact, "s.n.a.p: coolkid_12"},
		{"leetspeak app and handle", "sn4pch4t: coolkid_12", ruleContact, "sn4pch4t: coolkid_12"},
		{"discord discriminator", "discord: coolkid#1234", ruleContact, "discord: coolkid#1234"},
		{"app id without a colon", "insta id coolkid_12", ruleContact, "insta id coolkid_12"},
		{"snap me handle", "snap me coolkid_12", ruleContact, "snap me coolkid_12"},

		// ID numbers.
		{"aadhaar", "aadhaar " + aadhaar, ruleIDNumber, aadhaar},
		{"aadhaar spaced", spacedAadhaar + " is mine", ruleIDNumber, spacedAadhaar},
		{"aadhaar dashed", dashedAadhaar, ruleIDNumber, dashedAadhaar},
		{"ssn", "ssn 123-45-6789", ruleIDNumber, "123-45-6789"},
		{"national insurance", "NI number AB 12 34 56 C", ruleIDNumber, "AB 12 34 56 C"},
		{"national insurance unspaced", "AB123456C", ruleIDNumber, "AB123456C"},

		// Every branch of the number rules, each with the shape that reaches it.
		{"uk landline in london", "ring 020 7123 4567", ruleContact, "020 7123 4567"},
		{"uk landline elsewhere", "0161 234 5678 is the office", ruleContact, "0161 234 5678"},
		{"international with 00", "0049 151 23456789", ruleContact, "0049 151 23456789"},
		{"indian with 0091", "0091 98765 43210", ruleContact, "0091 98765 43210"},
		{"us number with its 1", "1 415 867 5309", ruleContact, "1 415 867 5309"},
		{"us number with a cue", "call 415 867 5309", ruleContact, "415 867 5309"},
		{"us number dotted", "415.867.5309", ruleContact, "415.867.5309"},
		{"us number with minus signs", "415−867−5309", ruleContact, "415−867−5309"},
		{"en dash", "98765–43210", ruleContact, "98765–43210"},
		{"em dash", "98765—43210", ruleContact, "98765—43210"},
		{"short groups beside a long one", "98765 432 10", ruleContact, "98765 432 10"},
		{"threes and pairs", "987 654 32 10", ruleContact, "987 654 32 10"},
		{"dotted pairs", "98.76.54.32.10", ruleContact, "98.76.54.32.10"},
		// Markup and emoticons beside a number are not maths.
		{"a heart after a number", "text me 9876543210 <3", ruleContact, "9876543210"},
		{"greentext", ">9876543210", ruleContact, "9876543210"},
		{"quote-link style", ">>9876543210", ruleContact, "9876543210"},
		{"a smiley after a number", "call 9876543210 =)", ruleContact, "9876543210"},
		{"a caret smiley", "9876543210 ^^", ruleContact, "9876543210"},
		{"a sarcasm tag", "call me 9876543210 /s", ruleContact, "9876543210"},
		// A label is a whole word straight before the number.
		{"whatsapp no", "whatsapp no 9876543210", ruleContact, "9876543210"},
		{"a name ending in a label", "ask frank 9876543210", ruleContact, "9876543210"},
		{"a word containing a label", "scroll down 9876543210", ruleContact, "9876543210"},
		{"doing", "what are you doing 9876543210", ruleContact, "9876543210"},
		{"neutral", "neutral 9876543210", ruleContact, "9876543210"},
		// An arrow or a phone cue before an operator is a label, not a sum.
		{"an arrow", "my number -> 9876543210", ruleContact, "9876543210"},
		{"a fat arrow", "whatsapp => 9876543210", ruleContact, "9876543210"},
		{"an arrow after a name", "ravi -> 9876543210", ruleContact, "9876543210"},
		{"a fat arrow after a name", "ravi => 9876543210", ruleContact, "9876543210"},
		{"a cue before an equals sign", "ph no = 9876543210", ruleContact, "9876543210"},
		{"a dash after a word", "call me - 9876543210", ruleContact, "9876543210"},

		// Fancy text reads as the letters and digits it shows.
		{"dingbat digits", "❾❽❼❻❺❹❸❷❶⓿", ruleContact, "❾❽❼❻❺❹❸❷❶⓿"},
		{"circled dingbat digits", "➈➇➆➅➄➃➂➁➀🄋", ruleContact, "➈➇➆➅➄➃➂➁➀🄋"},
		{"double circled digits", "⓽⓼⓻⓺⓹⓸⓷⓶⓵⓿", ruleContact, "⓽⓼⓻⓺⓹⓸⓷⓶⓵⓿"},
		{"small capitals", "add me on ꜱɴᴀᴘ", ruleContact, "add me on ꜱɴᴀᴘ"},
		{"negative circled letters", "add me on 🅢🅝🅐🅟", ruleContact, "add me on 🅢🅝🅐🅟"},
		{"negative squared letters", "add me on 🆂🅽🅰🅿", ruleContact, "add me on 🆂🅽🅰🅿"},
		{"parenthesised letters", "add me on ⒮⒩⒜⒫", ruleContact, "add me on ⒮⒩⒜⒫"},
		{"cherokee lookalikes", "add me on Ꮪnap", ruleContact, "add me on Ꮪnap"},
		{"regional indicators", "add me on 🇸🇳🇦🇵", ruleContact, "add me on 🇸🇳🇦🇵"},

		// Links to every kind of profile the link rule knows.
		{"discord invite path", "discord.com/invite/abc123", ruleContact, "discord.com/invite/abc123"},
		{"discord user", "discord.com/users/123456789", ruleContact, "discord.com/users/123456789"},
		{"tiktok profile", "tiktok.com/@coolkid", ruleContact, "tiktok.com/@coolkid"},
		{"twitter profile", "twitter.com/coolkid", ruleContact, "twitter.com/coolkid"},
		{"x profile", "x.com/coolkid", ruleContact, "x.com/coolkid"},
		{"reddit user", "reddit.com/u/coolkid", ruleContact, "reddit.com/u/coolkid"},
		{"facebook profile", "facebook.com/coolkid", ruleContact, "facebook.com/coolkid"},
		{"messenger", "messenger.com/t/coolkid", ruleContact, "messenger.com/t/coolkid"},

		// Email addresses written out.
		{"bracketed at, spelled dot", "priya (at) somecollege dot com", ruleContact, "priya (at) somecollege dot com"},
		{"name-like local part", "priya_k at somecollege dot com", ruleContact, "priya_k at somecollege dot com"},
		{"provider spelled", "priya at gmail dot com", ruleContact, "priya at gmail dot com"},
		{"at the rate", "coolkid12 at the rate gmail dot com", ruleContact, "coolkid12 at the rate gmail dot com"},
		{"attherate", "coolkid12attherategmail.com", ruleContact, "coolkid12attherategmail.com"},
		{"spaced at before a provider", "coolkid12 @ gmail", ruleContact, "coolkid12 @ gmail"},
		{"at a provider with a handle", "coolkid12 at gmail", ruleContact, "coolkid12 at gmail"},
		{"double at", "coolkid12@@gmail.com", ruleContact, "coolkid12@@gmail.com"},
		{"d0t", "coolkid12 at gmail d0t com", ruleContact, "coolkid12 at gmail d0t com"},
		{"underscored dot", "coolkid12@gmail_dot_com", ruleContact, "coolkid12@gmail_dot_com"},
		{"my gmail is", "my gmail is coolkid12", ruleContact, "my gmail is coolkid12"},
		{"gmail colon", "gmail: coolkid12", ruleContact, "gmail: coolkid12"},

		// Asking, in more words and more apps.
		{"curly apostrophe", "what’s your snap", ruleContact, "what’s your snap"},
		{"opening curly apostrophe", "what‘s ur number", ruleContact, "what‘s ur number"},
		{"yubo", "add me on yubo", ruleContact, "add me on yubo"},
		{"wizz", "dm me on wizz", ruleContact, "dm me on wizz"},
		{"whatsapp shortened", "wtsp me", ruleContact, "wtsp me"},
		{"whatsapp misspelt", "add me on whtsapp", ruleContact, "add me on whtsapp"},
		{"slashed app name", "add me on s/n/a/p", ruleContact, "add me on s/n/a/p"},
		{"are you on", "are you on snap?", ruleContact, "are you on snap"},
		{"drop your at", "drop ur @", ruleContact, "drop ur @"},
		{"what's your at", "what's your @ there", ruleContact, "what's your @"},
		{"handle before the app", "@coolkid on snap", ruleContact, "@coolkid on snap"},
		{"handle is my app", "coolkid_12 is my snap", ruleContact, "coolkid_12 is my snap"},
		{"arrow", "snap -> coolkid_12", ruleContact, "snap -> coolkid_12"},
		{"fat arrow", "insta => coolkid_12", ruleContact, "insta => coolkid_12"},
		{"unicode arrow", "snap → coolkid_12", ruleContact, "snap → coolkid_12"},
		{"discriminator without a colon", "discord coolkid#1234", ruleContact, "discord coolkid#1234"},
		{"app and handle", "snap coolkid_12", ruleContact, "snap coolkid_12"},
		{"sc colon", "sc: coolkid_12", ruleContact, "sc: coolkid_12"},
		{"app arrow handle", "snapchat -> coolkid_12", ruleContact, "snapchat -> coolkid_12"},
		{"handle on my app", "add coolkid_12 on my insta", ruleContact, "coolkid_12 on my insta"},
		{"handle and app in brackets", "coolkid_12 (snap)", ruleContact, "coolkid_12 (snap"},
		{"doubled letter in the app", "add me on snapp", ruleContact, "add me on snapp"},
		{"ur snap?", "ur snap?", ruleContact, "ur snap?"},
		{"stranger-chat app", "add me on omegle", ruleContact, "add me on omegle"},
		{"ambiguous app after a talk verb", "hmu on dc", ruleContact, "hmu on dc"},
		{"steam after add me", "add me on steam pls", ruleContact, "add me on steam"},
		{"find me on insta", "find me on insta", ruleContact, "find me on insta"},
		{"can we move to snap", "can we move to snap", ruleContact, "can we move to snap"},
		{"my insta is a name", "my insta is priya", ruleContact, "my insta is priya"},
		{"my email is", "my email is coolkid12", ruleContact, "my email is coolkid12"},
		{"spaced at with a dotted name", "priya.k at gmail.com", ruleContact, "priya.k at gmail.com"},
		{"at a provider's site", "priya at gmail.com", ruleContact, "priya at gmail.com"},
		// The look-alikes of what TestFilterLeavesOrdinaryPostsAlone lets
		// through, still caught.
		{"bare ask for a number", "what's your no.", ruleContact, "what's your no"},
		{"number ask with one sec", "text me your number one sec", ruleContact, "text me your number"},
		{"number ask with a digit and sec", "whats ur number 1 sec", ruleContact, "whats ur number"},
		{"dotted mobile after a cue", "call 98.123.145.67", ruleContact, "98.123.145.67"},
		{"dotted mobile in fives", "98.76.54.32.10", ruleContact, "98.76.54.32.10"},
		{"mixed separators", "98.76-54.32.10", ruleContact, "98.76-54.32.10"},
		{"decimal-range shape after a cue", "call 98.765-432.10", ruleContact, "98.765-432.10"},
		{"phone after call me for", "call me for 9876543210", ruleContact, "9876543210"},
		{"handle with digits on snap", "coolkid12 on snap", ruleContact, "coolkid12 on snap"},
		{"handle with digits on insta", "rahul99 on insta", ruleContact, "rahul99 on insta"},
		{"name-like at a site", "coolkid12 at somesite.com", ruleContact, "coolkid12 at somesite.com"},
		{"code-shaped name at a provider", "ab12 at gmail.com", ruleContact, "ab12 at gmail.com"},
		{"code-shaped name at a university", "ana23 at iitb.ac.in", ruleContact, "ana23 at iitb.ac.in"},
		{"tg on its own", "what's your tg?", ruleContact, "what's your tg"},
		{"check out my app", "check out my insta", ruleContact, "check out my insta"},

		// Some Hinglish.
		{"insta pe add karo", "insta pe add karo", ruleContact, "insta pe add karo"},
		{"apna number do", "apna number do na", ruleContact, "apna number do"},
		{"apna number bhejo", "apna number bhejo", ruleContact, "apna number bhejo"},
		{"tera number de", "tera number de", ruleContact, "tera number de"},
		{"pe baat karte", "whatsapp pe baat karte hain", ruleContact, "whatsapp pe baat karte"},
		{"handle with hai", "mera insta coolkid_12 hai", ruleContact, "mera insta coolkid_12 hai"},
		{"copula before the handle", "insta id hai coolkid_12", ruleContact, "insta id hai coolkid_12"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := bodyVerdict(c.body)
			if v.Outcome != contentBlock || v.Rule != c.rule || v.Field != fieldBody || v.Match != c.match {
				t.Fatalf("%q: got %+v, want a %s block of %q", c.body, v, c.rule, c.match)
			}
			if v.Message == "" || strings.Contains(v.Message, c.match) {
				t.Fatalf("%q: the message should say what to remove without repeating it: %q", c.body, v.Message)
			}
		})
	}
}

func TestFilterLeavesOrdinaryPostsAlone(t *testing.T) {
	aadhaar := withCheckDigit(t, "23456789012")
	wrong := aadhaar[:11] + string('0'+(aadhaar[11]-'0'+1)%10)

	posts := []string{
		// Maths and science.
		"[math]9876543210 \\times 2 = 19753086420[/math]",
		"[eqn]x = 9876543210[/eqn]",
		"Is 9999999967 prime?",
		"x = 9876543210 / 2",
		"9876543210 = 2 × 3^2 × 5 × 17^2 × 379721",
		"the sum is 9876543210",
		"6.022e23 particles in a mole",
		"c = 299792458 m/s",
		"299792458",
		"p = 0.9876543210",
		// A calculator's answer: one point, nothing round it.
		"the angle is 63.43494882 degrees",
		"my calculator says 70.71067812",
		"I got 70.71067812 for part (b)",
		"sqrt(5000) is 70.71067812 right?",
		"probability is 66.66666667 percent",
		"efficiency comes to 86.60254038",
		"1/0.0123 is 81.30081301",
		"ans 816.4965809",
		"the result was 98765.43210",
		"got 98.76543210 percentile",
		"coordinates 28.61393890, 77.20902120",
		"the answer is 9,876,543,210",
		"9.876.543.210 in European notation",
		"C6H12O6 + 6O2 -> 6CO2 + 6H2O",
		"2+919876543210",
		// Dates, times, years, scores, prices, references.
		"exam on 2026-10-13, results 13/10/2026",
		"13.10.2026 10.30 in hall B",
		"lab is 10:30-11:45 on monday",
		"I was there 2019-2023",
		"2019 2020 2021",
		"2019-2020-2021",
		"we won 3-2",
		"it costs ₹1,49,999 or $1,299.99",
		"ISBN 978-0-306-40615-7",
		"978-0-306-40615-7",
		"81-7525-766-0",
		"order 7834567890 never arrived",
		"JEE app no 240310123456 status pending?",
		"iCall: 9152987821, or +91 99996 66555",
		"Trevor Project 1-866-488-7386",
		"988 or 1-800-273-8255",
		"+1 800 273 8255",
		"Childhelp 1-800-422-4453, DV hotline 1-800-799-7233",
		"seat number 7012345678 in hall B",
		"marks: 87 91 76 88 92 65 74",
		"roll 23: 87 91 76 88 92",
		"1 2 3 4 5 6 7 8 9 10",
		"population 1428627663",
		"call 1800 123 4567 for the board office",
		"(555) 123-4567",
		"(415) 555-0123",
		"07700 900123",
		"+44 7700 900123",
		"020 7946 0123",
		"QQ 12 34 56 C",
		"ab123456c is the commit",
		"000-12-3456",
		wrong,
		aadhaar[:4] + " " + aadhaar[4:8] + " " + wrong[8:],
		// App names, said, not offered.
		"is Discord down?",
		"I deleted Snapchat",
		"my discord is lagging so bad",
		"my insta is private",
		"my snap is gone lol",
		"my tele is broken",
		"telegram channels for jee notes?",
		"find me in line at the canteen",
		"my mom will call me on my phone",
		"my phone number changed so I can't get OTPs",
		"look at youtube dot com",
		"instagram.com/p/Cxyz123",
		"https://www.reddit.com/r/JEENEETards/",
		// Code.
		"[code]user = User(email=\"test@example.com\")[/code]",
		"[code]git clone git@github.com:owner/repo.git[/code]",
		"[code]ssh deploy@host.sh[/code]",
		"[code]const d = {tg: 1}[/code]",
		"[code]my_snap = snap(x)[/code]",
		"[code]arr = [9876543210, 1234567890][/code]",
		"[spoiler]the twist[/spoiler] [code]x = 1[/code]",
		// Ordinary talk.
		"this exam is killing me",
		"I could kill for a coffee",
		"I'm going to kill this exam tomorrow",
		"we're gonna kill it at the concert tonight",
		"I'll shoot you an email tomorrow",
		"I'll shoot you guys a message at lunch",
		"I'm going to shoot the school play tomorrow",
		"I will bomb the class presentation tomorrow",
		"don't come to school tomorrow, it's a holiday",
		"don't go to school tomorrow trust me it's cancelled",
		"don't go to class tomorrow, trust me, the teacher is absent",
		"We're going to shoot in the school auditorium tomorrow for our film project",
		"we will shoot at the library tomorrow for the yearbook",
		"I'm shooting everyone's portraits tomorrow for the yearbook",
		"we're gonna kill them at the match tomorrow",
		"i'll kill you at chess tomorrow",
		"Headline: \"Boy, 15, said 'I will shoot up the school', court hears\"",
		"there was a school shooting today in Texas",
		"how many snakes are deadly?",
		"rope yourself into organising the fest, it's fun",
		"We're shooting at the gym tonight for basketball practice",
		"please don't kys, things get better",
		"don't you dare kill yourself",
		"never ever drink bleach",
		"Why did the Columbine school shooting happen in 1999?",
		"News: a 15-year-old was arrested after threatening to shoot up his school in Texas today.",
		"don't cut yourself on the broken glass in the lab",
		"the bus stop is 5 kms away",
		"how old are you guys? I'm 15",
		"dm me if you want the notes",
		"where are you from? I'm from Pune",
		"kal exam hai, notes bhej do yaar",
		"मुझे कल परीक्षा देनी है, बहुत डर लग रहा है",
		"yaar maths ka paper bahut tough tha, 2 baje tak padhai karo",
		"a b c d e f",
		"",

		// Encouragement that is not.
		"you should end it with her, she's toxic",
		"you need to cut yourself some slack",
		"cut yourself some slack, you did your best",
		"you should kill your self-doubt before the exam",
		"kill your self doubt",
		"how do i kill my self-doubt",
		"you'll kill yourself studying like that",
		"no one will care if you skip the party",
		"nobody will miss you if you skip one class lol",
		"you're going to die in minecraft tomorrow lol",
		"go die to the boss again lol",
		"if your friends told you to jump off a bridge, would you?",
		"a diver must jump off the cliff at 4 m/s, find the time",
		"in minecraft I jump off the cliff into the water",
		"the stunt double had to jump off a building",
		"i'd rather drink bleach than do this homework",
		"my dog tried to drink bleach once",
		"if you drink bleach you will die, so don't",
		"careful, you'll cut yourself",
		"careful, you could cut yourself on that",
		"how deep should i cut the wood for the shelf",
		"cut deeper into the topic in your essay",
		"i just die every time i see this meme",
		// Questions about danger that name no medicine and no one.
		"how many snakes would kill you",
		"how much water would kill you",
		"how many volts would it take to kill you",
		"the lethal dose of caffeine is about 10 g, so coffee is safe",
		"LD50 is the lethal dose for half the test animals",
		"how do people die in their sleep?",
		"how did people commit suicide in ancient rome",
		"best way to end it with my girlfriend?",
		"fastest way to die in minecraft hardcore",
		// Crisis words in ordinary sentences.
		"I don't want to live in the hostel next year",
		"no point living in the hostel",
		"I'm done with life sciences",
		"done with living in delhi",
		"i don't want to live with my sister",
		"i cut myself off from social media",
		"i cut myself a slice of cake",
		"i cut myself some slack",
		"i want to die of embarrassment",
		"i'm killing myself laughing",
		"this is the hill i want to die on",
		"how many kms is it to delhi",
		"a few kms away",
		"convert miles to kms",
		"kms per litre",
		"aws kms key rotation",
		"windows kms activation",
		"KMS College fest",
		"kitne kms hai",
		"How do I encrypt S3 objects with KMS?",
		"I burned myself on the Bunsen burner",
		"i cut myself shaving",
		"I need to cut myself some slack",
		"i want to cut myself a piece of cake",
		"I don't want to wake up at 5 for coaching",
		"no reason to live in the hostel",
		"no point in going on with Physics",
		"I don't want to live with my parents during college",
		"my parents don't want to live here",
		"i want to disappear from instagram for a while",
		"i'm thinking about dying my hair blue",
		"i'm thinking about suicide prevention for my essay",
		"i want to end it with my boyfriend",
		"i'm going to end it with him before the trip",
		"i'm dying of boredom",
		"cut yourself off from Instagram for a week",
		"end your self-doubt",
		"Hang your self-portrait on the wall",
		"you need to die to learn the boss patterns",
		"what happens chemically if you drink bleach",
		"do lemmings really jump off a cliff?",
		"How much caffeine is lethal?",
		"How much radiation is lethal?",
		"how did people die in the trenches in ww1",
		// Threat words that are not threats.
		"there will be a shooting star tonight",
		"there'll be a shooting practice at the range tomorrow",
		"there's gonna be a bloodbath in the maths exam tomorrow",
		"I'm going to bomb the school viva tomorrow",
		"I will bomb the class maths test tomorrow",
		"I'll shoot you a quick message tomorrow",
		"I'll shoot you the notes tonight",
		"I'll shoot you guys the slides at lunch",
		"we're gonna attack them at the match tomorrow",
		"we will kill them at the battle of the bands tomorrow",
		"we're shooting at the library tomorrow for photography club",
		"we have a school shooting drill tomorrow",
		"essay on school shooting prevention due tomorrow",
		"school shooting debate tomorrow in class",
		"if you eat that you will die tomorrow lol",
		"you're all going to die someday, so enjoy school",
		"don't come to school tomorrow, you'll see the new timetable on the board",
		"don't go to school tomorrow if you want to be safe from the rain",
		"you're dead to me after school today",
		"bringing a knife for the cake tomorrow",
		"you're so mature for your age",
		// Questions that are not about where someone is.
		"which area do you think is best? dm me for notes",
		"what area are you interested in? message me",
		"which college do you think is best? dm me",
		"Which college do you suggest for ECE? DM me",
		"What's your area of interest for research? hmu",
		"there'll be a bloodbath in tomorrow's paper",
		"I'm going to bomb the college interview tomorrow",
		"I'll shoot you the pdf tonight",
		"we're gonna attack them in tomorrow's match",
		"discussing the Uvalde school shooting in class tomorrow",
		"You will die if you eat that canteen food",
		"Everyone is gonna die someday",
		"I'm going to shoot Monday's lab report tomorrow",
		"i'm going to kill physics tomorrow",
		"there is a bomb in the game at level 3",
		// Spelled-out addresses that are not.
		"I study at iitb.ac.in",
		"python 3.12 docs at docs.python.org/3/",
		"sign in at gmail.com",
		"find it at khanacademy.org",
		// App names that are something else.
		"let's go to dc this weekend",
		"can we move to wa next year",
		"find me in dc next week",
		"let's move on ig",
		"lets talk over this imo",
		"my insta is underrated",
		"my snap is horrible lately",
		"my discord is mostly for gaming",
		"SNP: rs4680 is linked to anxiety",
		"discord: 3h of grinding",
		"tg: v2.0 is out",
		"insta - 2nd post today",
		"snap: 9am",
		"insta: 1.5k followers",
		"[code]ig = ig_client(token)[/code]",
		"insta: amazing",
		"snap: whatever",
		"what's your snap score?",
		"what's ur snap streak with her",
		"check out my roblox game",
		"whatsapp memes are the best",
		"I want to go to IMO this year",
		"we want to move to SC",
		"let's go to Epic Universe",
		"I want to continue in signal processing",
		"my insta is lowkey cringe now",
		"my reddit is shadowbanned",
		"my snap is pretty dead",
		"My SNP is rs4680",
		"my email is down again",
		"[code]reddit = praw.Reddit(client_id)[/code]",
		"[code]insta = instaloader.Instaloader()[/code]",
		"[code]ig = 1.5e-3[/code]",
		"Instagram - 3h 20m",
		"Discord: v2.0 of our bot",
		"insta: 2h",
		"what's your reddit karma",
		"what's your roblox avatar",
		"share your TikTok screen time",
		"My family uses WhatsApp, me and my friends use Insta",
		"@everyone on discord, the server moves tonight",
		"2h on insta again",
		"lol @ gmail being down",
		"Download Python 3.12 at python.org",
		"Problem 4 at leetcode.com",
		"Lectures are available at ocw.mit.edu",
		"Check your result at results.du.ac.in",
		"log in at outlook.com",
		"interest at the rate of 5 percent",
		// Numbers that are data.
		"salary 60000-80000 per month",
		"415 867 5309",
		"data: 9 8 7 6 5 4 3 2 1 0",
		"the file is 9876543210 bytes",
		"9876543210 rupees is the budget",
		"+9876543210 rupees profit",
		"9876543210 + 1",
		"1 + 9876543210",
		"x >= 9876543210",
		"what is 9876543210 factorial",
		"the answer is 9876543210",
		"2^33 is 8589934592",
		"13! is 6227020800",
		"8589934592 bytes",
		"World population hit 8045311447",
		"9999999999 + 1",
		"[code]print(7777777777)[/code]",
		"+350000000 USD",
		"$9876543210",
		"₹ 9876543210",
		"Batch sizes 256 512 1024",
		"Find the mode of 7 9 6 8 7 9 6 8 7 9",
		"mock scores 65-70-78-84-91",
		"fee 75000-90000 a year",
		"expecting 60000-80000 per month",
		"x = 9876543210 * y",
		"9876543210 % 7",
		"bob@mail.example.com",
		"7000000000",
		"2023 2024 2025",
		"666-12-3456",
		"912-12-3456",
		"123-00-4567",
		"123-45-0000",
		"078-05-1120",
		"0161 496 0123",
		"0044 7700 900123",
		"+44 7911 1234567",
		"AO 12 34 56 C",
		"GB 12 34 56 C",

		// A program's output.
		"Output: 9876543210",
		"output: 8589934592",
		"Output:\n8589934592",
		"Sample Output:\n7894561230",
		"my program prints 9876543210 instead of the right answer",
		"it returns 9876543210 for n=40",
		"expected 9876543210 got 9876543211",
		"expected: 8589934592\ngot: 8589934593",
		"result: 9876543210",
		"the result is 8589934592",
		"[code]print(2**33)\n# 8589934592[/code]",
		"[code]print(2**33)  # 8589934592[/code]",
		"stdout: 9876543210",
		"why does my code print 8589934592",
		// Addresses, ranges and labelled references.
		"example.com used to resolve to 93.184.216.34",
		"googlebot crawls from 66.249.66.123",
		"[code]ping 93.184.216.34[/code]",
		"wavelengths 656.28-686.72 nm",
		"the index ranged 654.32 - 678.90 over the week",
		"96.123-97.456 percentile",
		"ISBN-10: 8177091875",
		"ISBN-10 : 8177091875",
		"ISBN10: 8177091875",
		"Check the book, ISBN-10: 8177091875, in the library",
		"PNR status for 8177091875",
		"PNR status: 8177091875",
		"PNR of my train is 8177091875",
		"order id for 7834567890",
		"roll number of mine is 7834567890",
		// A ranking or a count, not a phone number.
		"what's your number one tip for the JEE?",
		"what's your number 1 tip for the JEE?",
		"whats ur no. 1 priority this sem",
		"what's your number of attempts left?",
		"give me your number one book for organic chem",
		// Screen time, account ages, tools and codes beside an app.
		"insta 2h30m",
		"instagram 1h45m today",
		"2h30m on insta again",
		"my reddit is 4yrs old",
		"snap 10yrs",
		"python3 on discord",
		"ps5 on discord",
		"gpt4 on reddit",
		"cs50 on discord",
		"jee2025 on reddit",
		"html5 in my insta",
		"Q3 at leetcode.com",
		"I'm an SDE2 at amazon.com",
		"Div2 at codeforces.com",
		"ABC300 at atcoder.jp",
		"Q3 at leetcode dot com",
		"check out my reddit post about the physics olympiad",
		"what's your reddit post about?",
		"check out my tiktok video",
		"what's your TG EAPCET rank?",
		"share your TG EAPCET rank",
		// Support and warnings around self-harm words.
		"a bad rank doesn't end your life",
		"don't let one exam end your life",
		"one bad result does not end your life",
		"failing jee isn't going to end your life",
		"a low score will not end your life, there are other paths",
		"never let a rank end your life",
		"Be careful with the scalpel in the dissection, you could cut yourself",
		"Be careful in the lab, you could cut yourself on broken glass",
		"wear gloves or you'll cut yourself",
		"you should die your hair blue",
		"go die your hair",
		"that's not a hill you should die on",
		"you should die on this hill",
		"in minecraft you need to die to get back to spawn",
		"in elden ring you should die a few times to learn the boss",
		"you need to die once in the tutorial to unlock it",
		// Burnout, breakups and going on to something.
		"i'm burning myself out studying for jee",
		"I've been burning myself out with all these assignments",
		"i burned myself out last semester",
		"I don't want to burn myself out before the exam",
		"im thinking about ending things with my boyfriend",
		"thinking of ending things with my girlfriend",
		"there's no point in going on to a phd if you hate research",
		"i dont want to wake up to another 8am lecture",
		// Exams, subjects, teams and occasions after "kill".
		"I'm going to kill JEE tomorrow",
		"I'm going to kill NEET tomorrow",
		"I'm going to kill Boards tomorrow",
		"I'm going to kill Calculus tomorrow",
		"We're going to kill Barcelona tomorrow",
		"We're going to kill CSK tonight",
		"I'm going to kill Rahul's wedding tomorrow",
		// Hyperbole about dying.
		"you're gonna die when you see the results tomorrow",
		"you're going to die when you hear what happened in class today",
		"you'll die when you see the results tomorrow",
		"you're gonna die of boredom in assembly tomorrow",
		"you're gonna die laughing at school tomorrow",
		"you will die of embarrassment at school tomorrow",
		// "There will be a ..." that is not an attack.
		"There will be a bloodbath in the stock market tomorrow after the results.",
		"Sensex crashed. There will be a bloodbath on Dalal Street tomorrow",
		"There will be a massacre at the polls on Tuesday, the opposition is finished",
		"Weather update: there will be a bomb cyclone this weekend, stay indoors",
		"Reminder: there will be a bomb drill at school tomorrow, don't panic when the alarm rings",
		"there will be a school shooting drill tomorrow",
		"there will be a shooting drill at school tomorrow",
		"There will be attack on titan marathon tonight in the hostel common room",
		// "asl" as slang or the language, and idioms beside an intro question.
		"bored asl, anyone wanna meet up at the library to study?",
		"this exam was hard asl. we should meet up and revise together",
		"i'm learning ASL, anyone want to meet up and practise signing?",
		"ASL club is meeting up on friday, come over to room 12",
		"taking an ASL class this sem, anyone wanna meet and practise?",
		"as I said, dm me",
		"Which college do you go to? Call me crazy but I think IIT is overrated.",
		"Where are you from? I grew up somewhere else and moved here for JEE coaching.",
		"Which college are you in? Between you and me, the mess food is awful.",
		"What year are you in? It's no big deal, but it's a secret that 2nd year is the hardest.",
		"Which school do you go to? Are you alone in thinking the CBSE syllabus is too long?",
		"Which college are you in? Campus pics?",
		"What grade are you in? Our little secret to passing chem is the NCERT examples.",
		"Where do you study? I think we should meet the deadline by friday.",
		"kaunsa college best hai? seniors se baat karo",
		"kaunsa college best hai CSE ke liye? fest pe milte hain",
		"kaunsa school accha hai Delhi mein? milte hain PTM pe",
		"kaunsa college best hai? HC Verma ki books library mein milte hain",
	}
	for _, body := range posts {
		if v := bodyVerdict(body); v.Outcome != contentPublish {
			t.Errorf("%q: got %+v, want it published", body, v)
		}
	}
}

func TestFilterSupport(t *testing.T) {
	for _, body := range []string{
		"i want to die",
		"I really want to die",
		"honestly i just wanna kms",
		"kms",
		"i'm going to kill myself",
		"I've been cutting myself again",
		"i feel so suicidal",
		"i don't want to be alive anymore",
		"everyone would be better off without me",
		"i want to d i e",
		"i w4nt t0 d1e",
		// What the rules and the FAQ name: wanting to hurt yourself.
		"I want to hurt myself",
		"i want to hurt myself so bad",
		"I want to cut myself",
		"i really want to hurt myself tonight",
		"i feel like hurting myself",
		"i want to self harm",
		"I hate myself and want to self harm",
		"i keep thinking about hurting myself",
		"thinking about ending it",
		"I can't do this anymore, I'm done with life",
		"i just want to disappear",
		"i want to jump off a bridge",
		"i want to commit suicide",
		"i want to take my own life",
		"i want to kill my self",
		"i want 2 die",
		"i wana die",
		"i keep thinking about suicide",
		"i've been thinking about dying",
		"i feel like dying",
		"dont wanna live anymore",
		"i'm going to end it tonight",
		"i'm going to end it",
		"i'm cutting myself again",
		"i am starving myself",
		"i don't want to live in this world anymore",
		"i don't want to live because everything hurts",
		"kms",
		"i want to cut myself with a razor",
		"mujhe marna hai",
		"mar jana hai mujhe",
		"suicide karna chahta hoon",
		"i cut myself last night",
		"i want to disappear from this world",
		"i'm going to end it all",
		"kms lol",
		"KMS",
		"i want to kms",
		"i don't want to live like this",
		"i really want to die",
		"no reason to live anymore",
		// Look-alikes of the burnout, breakup and "going on to" posts that
		// TestFilterLeavesOrdinaryPostsAlone lets through.
		"thinking about ending things",
		"thinking about ending things with pills",
		"thinking about ending things tonight",
		"i've been burning myself again",
		"i don't want to wake up to another day",
		"no reason to go on anymore",
		// "live to see" can be crisis language whatever follows.
		"i don't want to live to see tomorrow",
	} {
		v := bodyVerdict(body)
		if v.Outcome != contentSupport || v.Message != supportMessage || v.Rule != "" || v.Match != "" {
			t.Errorf("%q: got %+v, want support", body, v)
		}
		// Seen once, the post goes on: crisis language is never itself a
		// reason to keep a post back.
		if v := checkContent(contentInput{Body: body}, true); v.Outcome != contentPublish {
			t.Errorf("%q with support_seen: got %+v, want it published", body, v)
		}
	}
}

func TestFilterHolds(t *testing.T) {
	cases := []struct{ body, reason string }{
		// Self-harm encouragement.
		{"kys", holdSelfHarm},
		{"just k y s already", holdSelfHarm},
		{"go die", holdSelfHarm},
		{"you should kill yourself", holdSelfHarm},
		{"cut yourself", holdSelfHarm},
		{"neck yourself", holdSelfHarm},
		{"rope yourself", holdSelfHarm},
		{"end yourself", holdSelfHarm},
		{"jump off a bridge", holdSelfHarm},
		{"thinspo pls", holdSelfHarm},
		{"meanspo thread", holdSelfHarm},
		{"looking for an ana coach", holdSelfHarm},
		// A negation further back than the words themselves is about
		// something else, and "kys" is never advice.
		{"i don't care, kys", holdSelfHarm},
		{"don't care + didn't ask, kys", holdSelfHarm},
		{"never mind, kys", holdSelfHarm},
		{"careful what you say. kys", holdSelfHarm},
		{"dont reply. just kill yourself", holdSelfHarm},
		{"you won't pass anyway, kill yourself", holdSelfHarm},
		// Self-harm method.
		{"how many pills would it take to die", holdSelfHarm},
		{"painless way to die?", holdSelfHarm},
		{"what's a lethal dose of paracetamol", holdSelfHarm},
		{"lethal dose of paracetamol?", holdSelfHarm},
		{"how many sleeping pills are lethal", holdSelfHarm},
		{"how to tie a noose", holdSelfHarm},
		{"how many pills would kill me", holdSelfHarm},
		{"how many paracetamol would be fatal", holdSelfHarm},
		// Threats with a place or a time.
		{"I'm going to shoot up the school tomorrow", holdThreat},
		{"i will bring a gun to school", holdThreat},
		{"we're gonna bomb the school", holdThreat},
		{"I'll kill you tomorrow", holdThreat},
		{"im gonna stab my teachers at lunch", holdThreat},
		{"there will be a shooting at school on monday", holdThreat},
		{"don't come to school tomorrow. you've been warned", holdThreat},
		{"1'm g0nna sh00t up the sch00l", holdThreat},
		{"I'm bringing a gun to school tomorrow", holdThreat},
		{"i'm bringing my dad's gun to school on monday", holdThreat},
		{"im shooting up the school tomorrow", holdThreat},
		{"gonna shoot up the school tmrw", holdThreat},
		{"shooting up the school tomorrow, don't come", holdThreat},
		{"tomorrow I shoot up the school", holdThreat},
		{"school shooting tomorrow at 10. don't come", holdThreat},
		{"i'll be shooting everyone at school tomorrow", holdThreat},
		{"i'm gonna stab my maths teacher tomorrow", holdThreat},
		// Grooming: an age, school or location question with a private cue.
		{"how old are you? dm me", holdGrooming},
		{"asl? we should talk privately", holdGrooming},
		{"what school do you go to? send me a pic", holdGrooming},
		{"where do you live? are you home alone", holdGrooming},
		{"u 14? can we talk privately", holdGrooming},
		{"age? let's talk in private", holdGrooming},

		// More encouragement.
		{"kysss", holdSelfHarm},
		{"k!ll yourself", holdSelfHarm},
		{"you don't deserve to live", holdSelfHarm},
		{"end your life", holdSelfHarm},
		{"just die", holdSelfHarm},
		{"lol just die already", holdSelfHarm},
		{"drink bleach", holdSelfHarm},
		{"go drink bleach", holdSelfHarm},
		{"why don't you drink bleach", holdSelfHarm},
		{"honestly just jump off a bridge", holdSelfHarm},
		{"no one will care if you die", holdSelfHarm},
		{"nobody would miss you.", holdSelfHarm},
		{"cut deeper next time", holdSelfHarm},
		{"tu mar ja", holdSelfHarm},
		{"mar ja", holdSelfHarm},
		{"suicide kar le", holdSelfHarm},
		{"careful what you say. kys", holdSelfHarm},
		{"you're not careful enough, kys", holdSelfHarm},
		{"careful, thinspo inside", holdSelfHarm},
		{"nobody would miss you lol", holdSelfHarm},
		{"nobody would miss you if you were gone", holdSelfHarm},
		{"you should cut deeper", holdSelfHarm},
		{"cut yourself, nobody cares", holdSelfHarm},
		// More threats.
		{"i'm coming to school with a gun tomorrow", holdThreat},
		{"I'm bringing a gun tomorrow", holdThreat},
		{"there's going to be a bomb at school tomorrow", holdThreat},
		{"there is a bomb in the school", holdThreat},
		{"i'm going to set the school on fire tomorrow", holdThreat},
		{"i will burn the school down tomorrow", holdThreat},
		{"you're dead tomorrow after school", holdThreat},
		{"i'm gonna pull a columbine", holdThreat},
		{"school shooting next monday", holdThreat},
		{"i'll stab you in the washroom", holdThreat},
		{"i'm gonna shooot up the school tomorrow", holdThreat},
		{"I'm going to shoot Rahul tomorrow", holdThreat},
		{"sabko goli maar dunga kal school mein", holdThreat},
		{"I'm going to shoot up the school during the exam tomorrow", holdThreat},
		{"i'm going to kill my teacher after the game tomorrow", holdThreat},
		{"i'm going to stab my teacher during the test tomorrow", holdThreat},
		{"\"I will shoot up the school tomorrow\" and the police can't stop me", holdThreat},
		{"I'm going to shoot everyone at school tomorrow for real, not a video", holdThreat},
		{"I'm going to shoot my classmates tomorrow for my channel", holdThreat},
		{"if you come to school tomorrow you will die", holdThreat},
		{"i'm going to murder you tomorrow, final warning", holdThreat},
		{"i will kill you tomorrow, game over", holdThreat},
		{"I'm going to shoot up the school fest tomorrow", holdThreat},
		{"there's a bomb in the library", holdThreat},
		{"i'm bringing my dad's rifle on friday", holdThreat},
		{"school shooting this friday", holdThreat},
		// More grooming.
		{"which area do you live in? dm me", holdGrooming},
		{"what college are you from? message me", holdGrooming},
		{"how old? dm me", holdGrooming},
		{"which school? send me a pic", holdGrooming},
		{"pic? how old are you", holdGrooming},
		{"keep this between us, how old are you?", holdGrooming},
		{"how old are you? send me your location", holdGrooming},
		{"you're so mature for your age, dm me", holdGrooming},
		{"kitne saal ke ho? dm karo", holdGrooming},
		{"kahan rehte ho? insta pe aao", holdGrooming},
		{"how old are you? keep this between us", holdGrooming},
		{"which school? dm me", holdGrooming},
		{"how old are you? pic?", holdGrooming},
		{"which class are you in? share your location", holdGrooming},
		// Look-alikes of the idioms and hyperbole that
		// TestFilterLeavesOrdinaryPostsAlone lets through.
		{"go end your life", holdSelfHarm},
		{"it doesn't matter, end your life", holdSelfHarm},
		{"why don't you end your life", holdSelfHarm},
		{"go die your pathetic loser", holdSelfHarm},
		{"you should die your hair is ugly", holdSelfHarm},
		{"minecraft noob you should die", holdSelfHarm},
		{"i hate minecraft, go die", holdSelfHarm},
		{"I will kill Rahul tomorrow", holdThreat},
		{"I'm going to kill RAHUL tomorrow", holdThreat},
		{"I'm going to stab Priya at school", holdThreat},
		{"you'll die at school tomorrow", holdThreat},
		{"you're gonna die when you see me at school tomorrow", holdThreat},
		{"you're gonna die when you see what I brought to school tomorrow", holdThreat},
		{"there will be a bomb at school tomorrow", holdThreat},
		{"there will be a bloodbath at school tomorrow", holdThreat},
		{"asl pls dm me", holdGrooming},
		{"a/s/l? dm me", holdGrooming},
		{"how old are you? call me tonight", holdGrooming},
		{"how old are you? call me the moment you're home", holdGrooming},
		{"how old are you? let's meet this weekend", holdGrooming},
		{"how old are you? our little secret", holdGrooming},
		{"how old are you? our little secret is safe with me", holdGrooming},
		{"how old are you? between you and me", holdGrooming},
		{"where do you live? somewhere private?", holdGrooming},
		{"kaunse college mein padhte ho? milte hain", holdGrooming},
		{"kahan rehte ho? mujhse baat karo", holdGrooming},
	}
	// The method questions show the support message first, because the
	// person asking may be asking for themselves. Every other hold comes
	// straight away: encouragement aimed at someone else is not answered
	// with the support message, which is not for the person sending it.
	supportFirst := map[string]bool{
		"how many pills would it take to die": true,
		"painless way to die?":                true,
		"what's a lethal dose of paracetamol": true,
		"lethal dose of paracetamol?":         true,
		"how many sleeping pills are lethal":  true,
		"how to tie a noose":                  true,
		"how many pills would kill me":        true,
		"how many paracetamol would be fatal": true,
	}
	for _, c := range cases {
		// support_seen, so that a post that would show the support message
		// first is seen past it to the hold.
		v := checkContent(contentInput{Body: c.body}, true)
		if v.Outcome != contentHold || v.Hold != c.reason || v.Message != "" || v.Match != "" {
			t.Errorf("%q: got %+v, want held for %s", c.body, v, c.reason)
		}
		unseen := bodyVerdict(c.body)
		switch {
		case supportFirst[c.body]:
			if unseen.Outcome != contentSupport {
				t.Errorf("%q without support_seen: got %+v, want the support message first", c.body, unseen)
			}
		case unseen != v:
			t.Errorf("%q without support_seen: got %+v, want held straight away as %+v", c.body, unseen, v)
		}
	}
	// Intent with no place or time is not held.
	for _, body := range []string{"I'm going to kill you", "i will hurt him"} {
		if v := checkContent(contentInput{Body: body}, true); v.Outcome != contentPublish {
			t.Errorf("%q: got %+v, want it published", body, v)
		}
	}
}

// TestFilterAsksAboutAMethodBeforeHoldingIt pins that a question about how to
// hurt or kill oneself is answered with the support message first, because
// the person asking may be asking for themselves, and is held only once the
// poster has seen it. Encouragement aimed at someone else goes straight to
// the hold: the support message is not for the person sending it.
func TestFilterAsksAboutAMethodBeforeHoldingIt(t *testing.T) {
	for _, body := range []string{
		"how many pills would kill me",
		"how many paracetamol would be fatal",
		"painless way to die?",
		"what's a lethal dose of paracetamol",
		"how to tie a noose",
		"will 20 pills kill me",
		"how deep should i cut",
		"how deep should i cut my wrist",
		"how many would it take to kill myself",
		"lethal dose of paracetamol?",
		"careful, how many pills would kill me",
		"what dose of insulin would kill me",
	} {
		if v := bodyVerdict(body); v.Outcome != contentSupport || v.Message != supportMessage {
			t.Errorf("%q: got %+v, want support first", body, v)
		}
		if v := checkContent(contentInput{Body: body}, true); v.Outcome != contentHold || v.Hold != holdSelfHarm {
			t.Errorf("%q with support_seen: got %+v, want it held", body, v)
		}
	}
	for _, body := range []string{"kys", "go die", "neck yourself", "thinspo pls"} {
		if v := bodyVerdict(body); v.Outcome != contentHold || v.Hold != holdSelfHarm {
			t.Errorf("%q: got %+v, want it held without the support message", body, v)
		}
	}
}

// TestFilterPairsAThreatAcrossTheSubjectAndTheBody pins that the subject and
// the body are read as one text for a threat: the subject is the thread's
// title, shown right above the body.
func TestFilterPairsAThreatAcrossTheSubjectAndTheBody(t *testing.T) {
	for _, in := range []contentInput{
		{Subject: "Don't come to school tomorrow", Body: "You've been warned."},
		{Subject: "School tomorrow", Body: "I'm going to shoot everyone"},
		{Subject: "tomorrow", Body: "I will kill everyone"},
		{Subject: "I will kill everyone", Body: "at school tomorrow"},
		{Subject: "Don't come to school tomorrow. You've been warned."},
	} {
		if v := checkContent(in, true); v.Outcome != contentHold || v.Hold != holdThreat {
			t.Errorf("%+v: got %+v, want it held as a threat", in, v)
		}
	}
	// Each half alone is ordinary.
	for _, in := range []contentInput{
		{Subject: "School tomorrow", Body: "Is anyone else going early?"},
		{Subject: "I will kill everyone", Body: "at chess, obviously"},
	} {
		if v := checkContent(in, true); v.Outcome != contentPublish {
			t.Errorf("%+v: got %+v, want it published", in, v)
		}
	}
}

func TestFilterPrecedence(t *testing.T) {
	// A block comes first, so the poster fixes what they can.
	v := bodyVerdict("i want to die, my snap is ana_23, kys")
	if v.Outcome != contentBlock || v.Rule != ruleContact {
		t.Fatalf("block should come before support and holds: %+v", v)
	}
	// Then support, unless seen.
	v = bodyVerdict("i want to die. go die")
	if v.Outcome != contentSupport {
		t.Fatalf("support should come before holds: %+v", v)
	}
	v = checkContent(contentInput{Body: "i want to die. go die"}, true)
	if v.Outcome != contentHold || v.Hold != holdSelfHarm {
		t.Fatalf("a seen support screen should go on to the holds: %+v", v)
	}
	// support_seen never lets a block through.
	v = checkContent(contentInput{Body: "9876543210"}, true)
	if v.Outcome != contentBlock {
		t.Fatalf("support_seen should not skip a block: %+v", v)
	}
	// A threat is held as a threat even when it is also something else.
	v = bodyVerdict("kys, I'm going to shoot up the school tomorrow")
	if v.Outcome != contentHold || v.Hold != holdThreat {
		t.Fatalf("a threat should be held as a threat: %+v", v)
	}
	// Of two blocks, the first in the text is the one named.
	v = bodyVerdict("add me on snap or call 9876543210")
	if v.Match != "add me on snap" {
		t.Fatalf("the earliest block should be named: %+v", v)
	}
}

func TestFilterReadsTheSubjectAndTheShownName(t *testing.T) {
	v := checkContent(contentInput{Subject: "call 9876543210", Body: "hello"}, false)
	if v.Outcome != contentBlock || v.Field != fieldSubject || v.Match != "9876543210" ||
		!strings.Contains(v.Message, "the subject") {
		t.Fatalf("subject: %+v", v)
	}
	v = checkContent(contentInput{Name: "snap: cool_guy99", Body: "hello"}, false)
	if v.Outcome != contentBlock || v.Field != fieldName || !strings.Contains(v.Message, "the Name field") {
		t.Fatalf("name: %+v", v)
	}
	// Form order: the subject is named before the body.
	v = checkContent(contentInput{Subject: "a@gmail.com", Body: "9876543210"}, false)
	if v.Field != fieldSubject {
		t.Fatalf("the subject should be reported first: %+v", v)
	}
	// Holds read every field.
	v = checkContent(contentInput{Subject: "how old are you", Body: "dm me"}, false)
	if v.Outcome != contentHold || v.Hold != holdGrooming {
		t.Fatalf("a cue split across fields: %+v", v)
	}
	// The tripcode secret is never read: displayedName drops it.
	if got := displayedName("ana#9876543210"); got != "ana" {
		t.Fatalf("displayedName kept the secret: %q", got)
	}
	if got := displayedName("  ana  "); got != "ana" {
		t.Fatalf("displayedName should trim like the stored name: %q", got)
	}
}

func TestFilterQuotesMatchesAsWrittenAndShort(t *testing.T) {
	long := "add me on snap @ " + strings.Repeat("x_", 60)
	v := bodyVerdict(long)
	if v.Outcome != contentBlock || utf8.RuneCountInString(v.Match) != maxMatchLen || !strings.HasPrefix(v.Match, "add me on snap") {
		t.Fatalf("long match: %+v", v)
	}
	// A cut quote says so, so the page knows the source goes on past it.
	if !strings.HasSuffix(v.Match, "…") || !strings.HasPrefix(long, strings.TrimSuffix(v.Match, "…")) {
		t.Fatalf("a cut match should end in an ellipsis after the start of the source: %q", v.Match)
	}
	// One that fits is quoted whole, with nothing added.
	if v := bodyVerdict("add me on snap @ x_y"); v.Match != "add me on snap @ x_y" {
		t.Fatalf("short match: %+v", v)
	}
	v = bodyVerdict("  ＡＢ１２３４５６Ｃ  ")
	if v.Outcome == contentBlock && v.Match != "ＡＢ１２３４５６Ｃ" {
		t.Fatalf("a match should be quoted as written: %q", v.Match)
	}
}

// TestFilterPatternsSurviveCanon pins what canon relies on: the only lower-
// case l in any pattern source is a literal letter, so turning it into an i
// changes the words matched and never the syntax.
func TestFilterPatternsSurviveCanon(t *testing.T) {
	rules := map[string][]rule{
		"askRules": askRules, "supportRules": supportRules, "selfHarmRules": selfHarmRules,
		"selfHarmMethodRules": selfHarmMethodRules,
		"threatIntentRules":   threatIntentRules, "threatPlaceOrTimeRules": threatPlaceOrTimeRules,
		"threatWarningRules": threatWarningRules, "threatWarningCues": threatWarningCues,
		"schoolShootingRules": schoolShootingRules, "futureTimeRules": futureTimeRules,
		"ageQuestionRules": ageQuestionRules, "privateCueRules": privateCueRules,
		"myAppRule": {myAppRule}, "appColonHandle": {appColonHandle}, "appAtHandle": {appAtHandle},
		"appLabelHandle": {appLabelHandle}, "spacedInviteRule": {spacedInviteRule},
		"atAskRule": {atAskRule}, "myMailRule": {myMailRule}, "mailColonHandle": {mailColonHandle},
		"hindiHandle": {hindiHandle}, "handleBeforeApp": {handleBeforeApp}, "namedTargetRules": namedTargetRules,
	}
	posix := regexp.MustCompile(`\[:[a-z]+:\]|\\p\{[^}]*l|\(\?[a-z]*l`)
	for name, rs := range rules {
		for i, r := range rs {
			if r.loose == nil {
				t.Fatalf("%s[%d] has no loose form", name, i)
			}
			if posix.MatchString(r.plain.String()) {
				t.Fatalf("%s[%d] uses syntax with a lower-case l in it", name, i)
			}
			if strings.ContainsRune(r.loose.String(), 'l') {
				t.Fatalf("%s[%d]: canon left an l in the loose form", name, i)
			}
			if r.loose.NumSubexp() != r.plain.NumSubexp() {
				t.Fatalf("%s[%d]: canon changed the groups", name, i)
			}
		}
	}
}

// TestFilterCostIsBounded runs the filter over bodies built to be slow — as
// long as a post can be, made of what the rules look for — at an eighth of the
// length and at full length. Every pattern is a Go regexp, which runs in
// linear time, and the scanners each make one pass, so eight times the text
// should cost about eight times as much; anything quadratic would cost sixty-
// four. The bound sits between the two, with slack for timer noise, so it
// holds on a slow runner and under the race detector alike.
func TestFilterCostIsBounded(t *testing.T) {
	bodies := map[string]func(n int) string{
		"digits":       func(n int) string { return strings.Repeat("9 ", n/2) },
		"digit groups": func(n int) string { return strings.Repeat("98765 ", n/6) },
		"spaced":       func(n int) string { return strings.Repeat("s n a p ", n/8) },
		"leet":         func(n int) string { return strings.Repeat("1 ", n/2) },
		"at signs":     func(n int) string { return strings.Repeat("a@", n/2) },
		"dots":         func(n int) string { return strings.Repeat("a.", n/2) },
		"words":        func(n int) string { return strings.Repeat("i will kill you at school ", n/26) },
		"how many":     func(n int) string { return strings.Repeat("how many ", n/9) },
		"my":           func(n int) string { return strings.Repeat("my snap ", n/8) },
		"math":         func(n int) string { return strings.Repeat("[math]1[/math]", n/14) },
		"wide":         func(n int) string { return strings.Repeat("９", n) },
		"invisible":    func(n int) string { return strings.Repeat("\u200b", n) },
		"keycaps":      func(n int) string { return strings.Repeat("9\ufe0f\u20e3", n/3) },
		"line breaks":  func(n int) string { return strings.Repeat("98765\n", n/6) },
		"spoilers":     func(n int) string { return strings.Repeat("9[spoiler]8[/spoiler]", n/21) },
		"expanding":    func(n int) string { return strings.Repeat("\ufdfa1", n/2) },
		"subject pairs": func(n int) string {
			return strings.Repeat("shooting up the school. ", n/24)
		},
	}
	// The best of three runs, so a moment the scheduler gave to another
	// package's tests is not read as the filter's cost; the bound below is
	// unchanged.
	cost := func(body string) time.Duration {
		in := contentInput{Subject: truncateRunes(body, maxSubjectLen), Name: truncateRunes(body, maxNameLen), Body: body}
		best := time.Duration(1<<63 - 1)
		for range 3 {
			start := time.Now()
			checkContent(in, true)
			best = min(best, time.Since(start))
		}
		return best
	}
	for name, build := range bodies {
		small, full := cost(build(maxBodyLen/8)), cost(build(maxBodyLen))
		if limit := 24*small + 50*time.Millisecond; full > limit {
			t.Errorf("%s: %v at full length against %v at an eighth; more than linear", name, full, small)
		}
		if full > 10*time.Second {
			t.Errorf("%s: %v for one post", name, full)
		}
	}
}

// TestFilterReadsNoCharacterAsAPhrase holds the cap on what one character may
// fold to: the compatibility forms a digit or a letter has still read as what
// they show, and a character that folds to a whole phrase reads as itself, so a
// post cannot make the plain view many times longer than what was sent.
func TestFilterReadsNoCharacterAsAPhrase(t *testing.T) {
	for src, want := range map[string]string{
		"⑽":  "10",
		"㎉":  "kcal",
		"ﬃ":  "ffi",
		"９":  "9",
		"ﷺ":  "ﷺ",
		"ﷺ1": "ﷺ1",
	} {
		if got := plainView(src, nil).text; got != want {
			t.Errorf("%q reads as %q, want %q", src, got, want)
		}
	}
}

func BenchmarkCheckContent(b *testing.B) {
	body := strings.Repeat("Is the answer 42? I will check my notes tomorrow at school. ", maxBodyLen/62)
	for i := 0; i < b.N; i++ {
		checkContent(contentInput{Subject: "doubt", Name: "anon", Body: body}, false)
	}
}
