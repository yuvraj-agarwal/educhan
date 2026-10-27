package app

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
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
	// A wrong check digit: only the label before it says what it is.
	mistyped := aadhaar[:11] + string('0'+(aadhaar[11]-'0'+1)%10)
	spacedMistyped := mistyped[:4] + " " + mistyped[4:8] + " " + mistyped[8:]
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
		{"us number", "text (495) 867-5309 tonight", ruleContact, "(495) 867-5309"},
		{"us number dashed", "495-867-5309", ruleContact, "495-867-5309"},
		{"us international", "+1 495 867 5309", ruleContact, "+1 495 867 5309"},
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
		{"handle split by a spoiler", "my snap is madeup[spoiler]kid_12[/spoiler]", ruleContact,
			"my snap is madeup[spoiler]kid_12"},
		{"email split by a spoiler", "madeupkid12@g[spoiler]mail.com[/spoiler]", ruleContact,
			"madeupkid12@g[spoiler]mail.com"},
		{"link split by a spoiler", "disc[spoiler]ord.gg/abc123[/spoiler]", ruleContact, "disc[spoiler]ord.gg/abc123"},
		{"app in a code block", "add me on [code]snap[/code]", ruleContact, "add me on [code]snap"},
		{"handle in a code block", "my snap is [code]madeupkid_12[/code]", ruleContact, "my snap is [code]madeupkid_12"},
		{"handle's digits in a spoiler", "ig: madeupkid[spoiler]2009[/spoiler]", ruleContact, "ig: madeupkid[spoiler]2009"},
		{"leet app split by a spoiler", "whats ur sn[spoiler]4[/spoiler]p", ruleContact,
			"whats ur sn[spoiler]4[/spoiler]p"},
		{"app's first letter in a spoiler", "add me on 1[spoiler]nsta[/spoiler]", ruleContact,
			"add me on 1[spoiler]nsta"},
		{"number before a word in a spoiler", "[spoiler]9876543210[/spoiler]call me", ruleContact, "9876543210"},
		{"number after an invisible character", "call me\u200b9876543210", ruleContact, "9876543210"},

		// Email addresses.
		{"email", "mail madeup.priya@gmail.com for notes", ruleContact, "madeup.priya@gmail.com"},
		{"email spaced", "madeup.priya @ gmail . com", ruleContact, "madeup.priya @ gmail . com"},
		{"email spelled out", "madeup.priya at gmail dot com", ruleContact, "madeup.priya at gmail dot com"},
		{"email bracketed", "madeuppriya [at] outlook [dot] com", ruleContact, "madeuppriya [at] outlook [dot] com"},
		{"email at an institution", "madeuppriya at iitb dot ac dot in", ruleContact, "madeuppriya at iitb dot ac dot in"},
		{"email in code", "[code]ADMIN = \"madeup.rahul@yahoo.co.in\"[/code]", ruleContact, "madeup.rahul@yahoo.co.in"},
		{"email bracketed dot", "madeupkid12@gmail[.]com", ruleContact, "madeupkid12@gmail[.]com"},
		{"email bracketed at", "madeupkid12[@]gmail.com", ruleContact, "madeupkid12[@]gmail.com"},
		{"email with no tld", "madeupkid12@gmail", ruleContact, "madeupkid12@gmail"},
		{"email with at and dot spelled", "madeupkid@gmail dot com", ruleContact, "madeupkid@gmail dot com"},
		{"email spaced at and spelled dot", "mail madeupkid12 @ gmail dot com", ruleContact, "madeupkid12 @ gmail dot com"},
		{"email digit in tld", "madeupkid12@gmail.c0m", ruleContact, "madeupkid12@gmail.c0m"},

		// Links to profiles and invites.
		{"discord invite", "join discord.gg/abc123", ruleContact, "discord.gg/abc123"},
		{"discord invite spelled", "discord dot gg/abc123", ruleContact, "discord dot gg/abc123"},
		{"instagram profile", "https://www.instagram.com/madeup_user", ruleContact, "https://www.instagram.com/madeup_user"},
		{"telegram", "t.me/madeup_someone", ruleContact, "t.me/madeup_someone"},
		{"whatsapp link", "wa.me/919876543210", ruleContact, "wa.me/919876543210"},
		{"snapchat add", "snapchat.com/add/madeup_guy", ruleContact, "snapchat.com/add/madeup_guy"},
		{"bracketed dot in a link", "discord(.)gg/abc123", ruleContact, "discord(.)gg/abc123"},
		{"spaced short host", "t . me/madeupkid", ruleContact, "t . me/madeupkid"},
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
		{"insta colon handle", "insta: madeup_x_y_z", ruleContact, "insta: madeup_x_y_z"},
		{"snap at handle", "snap @madeup.guy", ruleContact, "snap @madeup.guy"},
		{"my snap is", "my snap is madeup_ana23", ruleContact, "my snap is madeup_ana23"},
		{"whatsapp me", "whatsapp me", ruleContact, "whatsapp me"},
		{"steam", "add me on steam", ruleContact, "add me on steam"},
		{"dotted app and handle", "s.n.a.p: madeupkid_12", ruleContact, "s.n.a.p: madeupkid_12"},
		{"leetspeak app and handle", "sn4pch4t: madeupkid_12", ruleContact, "sn4pch4t: madeupkid_12"},
		{"discord discriminator", "discord: madeupkid#1234", ruleContact, "discord: madeupkid#1234"},
		{"app id without a colon", "insta id madeupkid_12", ruleContact, "insta id madeupkid_12"},
		{"snap me handle", "snap me madeupkid_12", ruleContact, "snap me madeupkid_12"},

		// ID numbers.
		{"aadhaar", "aadhaar " + aadhaar, ruleIDNumber, aadhaar},
		{"aadhaar spaced", spacedAadhaar + " is mine", ruleIDNumber, spacedAadhaar},
		{"aadhaar dashed", dashedAadhaar, ruleIDNumber, dashedAadhaar},
		{"ssn", "ssn 123-45-6789", ruleIDNumber, "123-45-6789"},
		{"national insurance", "NI number AB 12 34 56 C", ruleIDNumber, "AB 12 34 56 C"},
		{"national insurance unspaced", "AB123456C", ruleIDNumber, "AB123456C"},
		// Full-width and styled capitals read as plain ones.
		{"national insurance full width", "NI ＡＢ １２ ３４ ５６ Ｃ", ruleIDNumber, "ＡＢ １２ ３４ ５６ Ｃ"},
		{"national insurance bold", "𝐀𝐁123456𝐂", ruleIDNumber, "𝐀𝐁123456𝐂"},

		// Every branch of the number rules, each with the shape that reaches it.
		{"uk landline in london", "ring 020 7123 4567", ruleContact, "020 7123 4567"},
		{"uk landline elsewhere", "0161 234 5678 is the office", ruleContact, "0161 234 5678"},
		{"international with 00", "0049 151 23456789", ruleContact, "0049 151 23456789"},
		{"indian with 0091", "0091 98765 43210", ruleContact, "0091 98765 43210"},
		{"us number with its 1", "1 495 867 5309", ruleContact, "1 495 867 5309"},
		{"us number with a cue", "call 495 867 5309", ruleContact, "495 867 5309"},
		{"us number dotted", "495.867.5309", ruleContact, "495.867.5309"},
		{"us number with minus signs", "495−867−5309", ruleContact, "495−867−5309"},
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
		{"tiktok profile", "tiktok.com/@madeupkid", ruleContact, "tiktok.com/@madeupkid"},
		{"twitter profile", "twitter.com/madeupkid", ruleContact, "twitter.com/madeupkid"},
		{"x profile", "x.com/madeupkid", ruleContact, "x.com/madeupkid"},
		{"reddit user", "reddit.com/u/madeupkid", ruleContact, "reddit.com/u/madeupkid"},
		{"facebook profile", "facebook.com/madeupkid", ruleContact, "facebook.com/madeupkid"},
		{"messenger", "messenger.com/t/madeupkid", ruleContact, "messenger.com/t/madeupkid"},

		// Email addresses written out.
		{"bracketed at, spelled dot", "madeuppriya (at) somecollege dot com", ruleContact, "madeuppriya (at) somecollege dot com"},
		{"name-like local part", "madeuppriya_k at somecollege dot com", ruleContact, "madeuppriya_k at somecollege dot com"},
		{"provider spelled", "madeuppriya at gmail dot com", ruleContact, "madeuppriya at gmail dot com"},
		{"at the rate", "madeupkid12 at the rate gmail dot com", ruleContact, "madeupkid12 at the rate gmail dot com"},
		{"attherate", "madeupkid12attherategmail.com", ruleContact, "madeupkid12attherategmail.com"},
		{"spaced at before a provider", "madeupkid12 @ gmail", ruleContact, "madeupkid12 @ gmail"},
		{"at a provider with a handle", "madeupkid12 at gmail", ruleContact, "madeupkid12 at gmail"},
		{"double at", "madeupkid12@@gmail.com", ruleContact, "madeupkid12@@gmail.com"},
		{"d0t", "madeupkid12 at gmail d0t com", ruleContact, "madeupkid12 at gmail d0t com"},
		{"underscored dot", "madeupkid12@gmail_dot_com", ruleContact, "madeupkid12@gmail_dot_com"},
		{"my gmail is", "my gmail is madeupkid12", ruleContact, "my gmail is madeupkid12"},
		{"gmail colon", "gmail: madeupkid12", ruleContact, "gmail: madeupkid12"},

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
		{"handle before the app", "@madeupkid on snap", ruleContact, "@madeupkid on snap"},
		{"handle is my app", "madeupkid_12 is my snap", ruleContact, "madeupkid_12 is my snap"},
		{"arrow", "snap -> madeupkid_12", ruleContact, "snap -> madeupkid_12"},
		{"fat arrow", "insta => madeupkid_12", ruleContact, "insta => madeupkid_12"},
		{"unicode arrow", "snap → madeupkid_12", ruleContact, "snap → madeupkid_12"},
		{"discriminator without a colon", "discord madeupkid#1234", ruleContact, "discord madeupkid#1234"},
		{"app and handle", "snap madeupkid_12", ruleContact, "snap madeupkid_12"},
		{"sc colon", "sc: madeupkid_12", ruleContact, "sc: madeupkid_12"},
		{"app arrow handle", "snapchat -> madeupkid_12", ruleContact, "snapchat -> madeupkid_12"},
		{"handle on my app", "add madeupkid_12 on my insta", ruleContact, "madeupkid_12 on my insta"},
		{"handle and app in brackets", "madeupkid_12 (snap)", ruleContact, "madeupkid_12 (snap"},
		{"doubled letter in the app", "add me on snapp", ruleContact, "add me on snapp"},
		{"ur snap?", "ur snap?", ruleContact, "ur snap?"},
		{"stranger-chat app", "add me on omegle", ruleContact, "add me on omegle"},
		{"ambiguous app after a talk verb", "hmu on dc", ruleContact, "hmu on dc"},
		{"steam after add me", "add me on steam pls", ruleContact, "add me on steam"},
		{"find me on insta", "find me on insta", ruleContact, "find me on insta"},
		{"can we move to snap", "can we move to snap", ruleContact, "can we move to snap"},
		{"my insta is a name", "my insta is madeuppriya", ruleContact, "my insta is madeuppriya"},
		{"my email is", "my email is madeupkid12", ruleContact, "my email is madeupkid12"},
		{"spaced at with a dotted name", "madeuppriya.k at gmail.com", ruleContact, "madeuppriya.k at gmail.com"},
		{"at a provider's site", "madeuppriya at gmail.com", ruleContact, "madeuppriya at gmail.com"},
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
		{"handle with digits on snap", "madeupkid12 on snap", ruleContact, "madeupkid12 on snap"},
		{"handle with digits on insta", "madeuprahul99 on insta", ruleContact, "madeuprahul99 on insta"},
		{"name-like at a site", "madeupkid12 at somesite.com", ruleContact, "madeupkid12 at somesite.com"},
		{"code-shaped name at a provider", "ab12 at gmail.com", ruleContact, "ab12 at gmail.com"},
		{"code-shaped name at a university", "ana23 at madeup.ac.in", ruleContact, "ana23 at madeup.ac.in"},
		{"degree-shaped name at a provider", "B.Tech at gmail.com", ruleContact, "B.Tech at gmail.com"},
		{"address at a country's university", "email me at ali@madeup-uni.edu.tr", ruleContact, "ali@madeup-uni.edu.tr"},
		{"address at an academic subdomain", "x.y@mail.madeup.ac.il", ruleContact, "x.y@mail.madeup.ac.il"},
		{"mail provider and a dash", "email - madeuppriya.k12", ruleContact, "email - madeuppriya.k12"},
		{"mail provider and an arrow", "gmail -> madeuppriya.k12", ruleContact, "gmail -> madeuppriya.k12"},
		{"mail label and a dash", "email id - madeuppriya.k12", ruleContact, "email id - madeuppriya.k12"},
		{"my mail provider and a dash", "my gmail - madeuppriya.k12", ruleContact, "my gmail - madeuppriya.k12"},
		{"you on an app", "u on snap?", ruleContact, "u on snap?"},
		{"you on insta", "you on insta?", ruleContact, "you on insta?"},
		{"gimme your app", "gimme ur snap", ruleContact, "gimme ur snap"},
		{"lemme get your app", "lemme get ur insta", ruleContact, "lemme get ur insta"},
		{"hmu and an app", "hmu snap", ruleContact, "hmu snap"},
		{"app, a chevron and a handle", "insta > madeuppriya.k12", ruleContact, "insta > madeuppriya.k12"},
		{"app, two chevrons and a handle", "ig >> madeuppriya_k12", ruleContact, "ig >> madeuppriya_k12"},
		{"app, a bar and a handle", "insta | madeuppriya_k12", ruleContact, "insta | madeuppriya_k12"},
		{"the gram", "add me on the gram", ruleContact, "add me on the gram"},
		{"discord tag and a discriminator", "discord tag madeuppriya#1234", ruleContact, "discord tag madeuppriya#1234"},
		{"label before a tool-shaped handle", "insta id madeup3", ruleContact, "insta id madeup3"},
		{"tera snap kya hai", "tera snap kya hai", ruleContact, "tera snap kya hai"},
		{"teri insta id kya h", "teri insta id kya h", ruleContact, "teri insta id kya h"},
		{"tera number kya hai", "tera number kya hai", ruleContact, "tera number kya hai"},
		{"insta id do", "insta id do", ruleContact, "insta id do"},
		{"apni insta id bhejo", "apni insta id bhejo", ruleContact, "apni insta id bhejo"},
		{"tg on its own", "what's your tg?", ruleContact, "what's your tg"},
		{"check out my app", "check out my insta", ruleContact, "check out my insta"},
		{"got without expected", "u got 9876543210 saved?", ruleContact, "9876543210"},
		{"signal username link", "add me signal.me/#eu/AbCdEfGhIjKlMnOp", ruleContact, "signal.me/#eu/AbCdEfGhIjKlMnOp"},
		{"signal group link", "join https://signal.group/#CjQKIGabcdef", ruleContact,
			"https://signal.group/#CjQKIGabcdef"},
		{"signal number link", "signal.me/#p/+919876543210", ruleContact, "signal.me/#p/+919876543210"},
		{"slash spelled out", "discord dot gg slash abcdef", ruleContact, "discord dot gg slash abcdef"},
		{"forward slash spelled out", "t.me forward slash madeuppriya_k12", ruleContact, "t.me forward slash madeuppriya_k12"},
		{"slash spelled in a path", "instagram.com slash madeuppriya_k12", ruleContact, "instagram.com slash madeuppriya_k12"},
		{"bracketed slash", "discord.gg(/)abcdef", ruleContact, "discord.gg(/)abcdef"},
		{"spelled slash after a spaced host", "d i s c o r d . g g slash abcdef", ruleContact,
			"d i s c o r d . g g slash abcdef"},

		// A cue to call or message straight before a number, whatever is
		// round it, and an app's own account.
		{"cue before a maths word", "call me 9876543210 sum", ruleContact, "9876543210"},
		{"cue before an equals sign", "call me = 9876543210", ruleContact, "9876543210"},
		{"app before an equals sign", "wa=9876543210", ruleContact, "9876543210"},
		{"app before a slash", "wa/9876543210", ruleContact, "9876543210"},
		{"cue before a maths question", "text me 9876543210 if you can do the integers question", ruleContact,
			"9876543210"},
		{"whatsapp account", "my whatsapp acc 9876543210", ruleContact, "9876543210"},
		{"telegram account", "telegram account: 9876543210", ruleContact, "9876543210"},
		{"payment app account", "gpay account no 9876543210", ruleContact, "9876543210"},
		{"google pay account", "google pay acc 9876543210", ruleContact, "9876543210"},
		{"digits dotted", "9.8.7.6.5.4.3.2.1.0", ruleContact, "9.8.7.6.5.4.3.2.1.0"},
		{"digits dashed", "9-8-7-6-5-4-3-2-1-0", ruleContact, "9-8-7-6-5-4-3-2-1-0"},

		// Some Hinglish.
		{"insta pe add karo", "insta pe add karo", ruleContact, "insta pe add karo"},
		{"apna number do", "apna number do na", ruleContact, "apna number do"},
		{"apna number bhejo", "apna number bhejo", ruleContact, "apna number bhejo"},
		{"tera number de", "tera number de", ruleContact, "tera number de"},
		{"pe baat karte", "whatsapp pe baat karte hain", ruleContact, "whatsapp pe baat karte"},
		{"handle with hai", "mera insta madeupkid_12 hai", ruleContact, "mera insta madeupkid_12 hai"},
		{"copula before the handle", "insta id hai madeupkid_12", ruleContact, "insta id hai madeupkid_12"},

		// Read since the posts in testdata/filter_corpus were measured.
		// Numbers: a "+" inside the bracket, any grouping after a request to
		// call, an ID number its label names, and digits read out in words.
		{"plus inside the bracket", "(+91) 98765 43210", ruleContact, "(+91) 98765 43210"},
		{"pairs after call me", "call me 98 76 54 32 10", ruleContact, "98 76 54 32 10"},
		{"a pair a line after text me", "text me\n98\n76\n54\n32\n10", ruleContact, "98\n76\n54\n32\n10"},
		{"aadhaar its label names", "aadhaar no " + spacedMistyped + " is correct", ruleIDNumber, spacedMistyped},
		{"aadhaar its label names, in one block", "my aadhar " + mistyped, ruleIDNumber, mistyped},
		{"ssn with spaces after its label", "ssn 418 62 7351 on file", ruleIDNumber, "418 62 7351"},
		{"social security number with spaces", "social security number 418 62 7351", ruleIDNumber, "418 62 7351"},
		{"digits in words", "text me nine eight seven six five four three two two one", ruleContact,
			"nine eight seven six five four three two two one"},
		{"words and digits mixed", "9 eight 3 one 0 four 2 2 7 seven", ruleContact, "9 eight 3 one 0 four 2 2 7 seven"},
		{"double and oh", "oh seven nine one one, double two three, four five six", ruleContact,
			"oh seven nine one one, double two three, four five six"},
		{"hinglish digit words", "nau aath saat chhe paanch chaar teen do do ek", ruleContact,
			"nau aath saat chhe paanch chaar teen do do ek"},
		{"us number in words after call me", "call me on two one two, eight six seven, five three zero nine",
			ruleContact, "two one two, eight six seven, five three zero nine"},
		{"a stray word before the words", "pls do nine eight seven six five four three two two one", ruleContact,
			"nine eight seven six five four three two two one"},
		// A letter in brackets inside an app's name.
		{"letter in brackets", "i[n]sta: madeup_moon_17", ruleContact, "i[n]sta: madeup_moon_17"},
		{"letter in round brackets", "sn(a)p: madeup_moon_17", ruleContact, "sn(a)p: madeup_moon_17"},
		// Asking to move, or to swap accounts.
		{"move this to an app", "let's move this to discord", ruleContact, "let's move this to discord"},
		{"take this to an app", "can we take this to telegram?", ruleContact, "can we take this to telegram"},
		{"exchange snaps", "exchange snaps?", ruleContact, "exchange snaps"},
		{"swap insta", "swap insta?", ruleContact, "swap insta"},
		{"leave your number", "leave your number in the comments", ruleContact, "leave your number"},
		{"number bhejo", "number bhejo", ruleContact, "number bhejo"},
		// Handles: an "@" a few words after the app, a Discord tag after
		// "disc", a label after an app that is also a word, the ghost.
		{"@ a few words after the app", "telegram pe aao: @madeup_organicchem", ruleContact,
			"telegram pe aao: @madeup_organicchem"},
		{"discord tag after disc", "disc: madeupfox#1234", ruleContact, "disc: madeupfox#1234"},
		{"line id", "line id: madeup_fox27", ruleContact, "line id: madeup_fox27"},
		{"steam username", "steam username madeup_fox27", ruleContact, "steam username madeup_fox27"},
		{"snapchat ghost", "👻: madeup.fox_27", ruleContact, "👻: madeup.fox_27"},
		{"my signal is a handle", "my signal is madeup.quietfox.27", ruleContact, "my signal is madeup.quietfox.27"},
		// Digits spaced one at a time with a dash or a point among them,
		// read in the groups the marks make.
		{"spaced digits with a dash group", "call karna 8 4 3 2 - 1 0 9 7 6 5", ruleContact, "8 4 3 2 - 1 0 9 7 6 5"},
		{"spaced US number with dashes", "my cell: 9 1 7 - 4 8 2 - 0 1 6 8", ruleContact, "9 1 7 - 4 8 2 - 0 1 6 8"},
		{"spaced SSN with dashes", "ssn: 6 1 4 - 3 9 - 2 8 5 7 (spaced so it doesnt autofill lol)", ruleIDNumber,
			"6 1 4 - 3 9 - 2 8 5 7"},
		// A letter for a digit with no digit beside it.
		{"lone letters between spaced digits", "num: 8 l 2 O 3 3 4 5 6 1", ruleContact, "8 l 2 O 3 3 4 5 6 1"},
		{"letters in a group of digits", "my mobile: 07911 4OO 133 (o's are zeros obvs)", ruleContact, "07911 4OO 133"},
		// Backwards, in pieces, and in code.
		{"reversed after saying so", "My number in reverse so bots can't read it: 0 1 2 6 5 4 8 7 9 9", ruleContact,
			"0 1 2 6 5 4 8 7 9 9"},
		{"pieces joined by then", "my num starts with 99 then 30 then 412 then 875", ruleContact,
			"99 then 30 then 412 then 875"},
		{"pieces split to get past the filter", "split it so the mods dont catch: 98 / 1 / 67 / 45 / 231", ruleContact,
			"98 / 1 / 67 / 45 / 231"},
		{"strings joined in code", "[code]PHONE = '+91' + '98450' + '17723'[/code] use this to reach me", ruleContact,
			"'+91' + '98450' + '17723'"},
		{"a piece for every digit", "call me 9 / 8 / 4 / 5 / 0 / 1 / 7 / 7 / 2 / 3", ruleContact,
			"9 / 8 / 4 / 5 / 0 / 1 / 7 / 7 / 2 / 3"},
		{"a piece for every digit, joined by words", "my number: 9 then 8 then 4 then 5 then 0 then 1 then 7 then 7 then 2 then 3",
			ruleContact, "9 then 8 then 4 then 5 then 0 then 1 then 7 then 7 then 2 then 3"},
		{"two pieces straight after a call", "call me on 98450/12345", ruleContact, "98450/12345"},
		{"two pieces joined by a word", "my number is 98450 then 12345", ruleContact, "98450 then 12345"},
		{"pieces after a cue, a list word further back", "marks are posted. call me on 98/45/01/77/23", ruleContact,
			"98/45/01/77/23"},
		// One digit set apart among longer groups.
		{"Indian mobile with a digit apart", "98450 1 2345", ruleContact, "98450 1 2345"},
		{"UK mobile with a digit apart", "07911 1 23456", ruleContact, "07911 1 23456"},
		{"Australian mobile with a digit apart", "0412 3 45678", ruleContact, "0412 3 45678"},
		{"pairs and a digit apart after a call", "call me 98 45 0 12 345", ruleContact, "98 45 0 12 345"},
		// National shapes.
		{"country code after a word for a list", "WAEC past questions sharing, +234 803 123 4567 on whatsapp",
			ruleContact, "+234 803 123 4567"},
		{"local number with its area code apart", "Text me @ 482-0196 (area code 494) for the chem lab write up",
			ruleContact, "482-0196"},
		{"bare US digits after a line's name", "Google Voice: 2954820174", ruleContact, "2954820174"},
		{"Australian landline", "(07) 3221 3317 is my home number if mob is off", ruleContact, "(07) 3221 3317"},
		{"poster's own office line", "my office (03) 9027 0100", ruleContact, "(03) 9027 0100"},
		{"office line asked for the poster on", "call me at uni (08) 8313 4455", ruleContact, "(08) 8313 4455"},
		{"a mobile is not an office's", "admissions 0412 345 678", ruleContact, "0412 345 678"},
		// Ofcom's drama numbers are 496 0000-0999 in some area codes only.
		{"UK landline past the drama range", "call me on 0161 496 1234", ruleContact, "0161 496 1234"},
		{"UK landline in an area code with no drama range", "call me on 0170 496 0123", ruleContact, "0170 496 0123"},
		// Addresses spelled out a character at a time, or in two halves.
		{"spaced-out address", "email (remove spaces): m a d e u p . 0 9 @ g m a i l . c o m", ruleContact,
			"m a d e u p . 0 9 @ g m a i l . c o m"},
		{"address in two halves", "[at] gmail [dot] com — prefix is madeup.quietstorm2007, you figure it out", ruleContact,
			"[at] gmail [dot] com — prefix is madeup.quietstorm2007"},
		// Asks read from the app outwards: a verb of moving or talking with
		// its object and preposition, a question about the reader's account,
		// messages there, Hinglish either way round, a group set up there.
		{"come to an app", "come to tg, the board is too slow for doubt solving", ruleContact, "come to tg"},
		{"switch to an app that is also a word", "can we switch to signal? i'll explain the situation there",
			ruleContact, "switch to signal"},
		{"follow back on an app", "follow me back on insta pls", ruleContact, "follow me back on insta"},
		{"got an account", "u got insta?", ruleContact, "u got insta"},
		{"have an app that is also a word, off the board", "do you have signal? would rather not do this on a public board",
			ruleContact, "do you have signal"},
		{"messages there", "slide into my dms on insta if u wanna rant about kota", ruleContact, "into my dms on insta"},
		{"Hinglish verb before the app", "dm kar insta pe, sab bata dunga", ruleContact, "dm kar insta pe"},
		{"Hinglish verb after the app", "insta pe aaja baat karte", ruleContact, "insta pe aaja"},
		{"Hinglish join", "telegram pe join karo, link maang lo comment me", ruleContact, "telegram pe join"},
		{"Hinglish ask for an address", "apna gmail id bata, drive link share kar dunga", ruleContact, "apna gmail id bata"},
		{"Hinglish ask for numbers", "WhatsApp number do sab, group bana rahe hain", ruleContact, "WhatsApp number do"},
		{"a call set up on an app", "can someone make a discord vc for the mock tonight?", ruleContact,
			"make a discord vc"},
		{"text instead of posting", "anyone want to text instead of posting here? easier", ruleContact,
			"instead of posting"},
		{"dm the poster instead of posting", "dm me instead of posting here", ruleContact, "instead of posting"},
		{"talk to the poster instead of posting", "talk to me instead of commenting here", ruleContact,
			"instead of commenting"},
		{"this talk taken off the board", "let's take this convo off here", ruleContact, "off here"},
		{"talk there after a link", "instagram.com/p/C7xYz123/ — this is my post, comment there so we can talk",
			ruleContact, "comment there"},
		// Handles a few words after the app.
		{"come to an app, in Hinglish, with a handle", "snap pe aaja: madeup.0p", ruleContact,
			"snap pe aaja: madeup.0p"},
		{"handle after the ghost", "my 👻 is madeup.sprinkles add me", ruleContact, "👻 is madeup.sprinkles"},
		{"handle on another app", "threema id MADEUP42, add me if you care about privacy", ruleContact,
			"threema id MADEUP42"},
		{"handle spelled out", "telegram: madeup underscore jee", ruleContact, "telegram: madeup underscore jee"},
		{"quoted handle after search", "telegram id same as here, search 'madeupkotafactory'", ruleContact,
			"telegram id same as here, search 'madeupkotafactory'"},
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
		"is the number 9999999967 prime?",
		// Lists of one-digit-and-a-decimal values and of ranks.
		"sem-wise SGPA 8.1 8.4 8.7 9.0 9.2",
		"done exercises 6.1 6.2 6.3 7.1 7.2 today",
		"ranks in the last three mocks: 712 845 1203",
		"my mock ranks were 712 845 1203",
		"chapters 1.1 1.2 2.1 2.2 3.1 done",
		"paytm order 9876543210",
		"reddit.com slash r slash jee",
		"instagram.com/#about",
		"the slash command in discord",
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
		"0491 570 156",
		"+61 491 570 158",
		"Tele-MANAS 1-800-891-4416, free and in Hindi too",
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
		"[code]ssh deploy@madeup.sh[/code]",
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
		// Hyperbole about exams and results, feelings and kindness.
		"you're going to die in the chemistry exam tomorrow",
		"you're gonna die at the exam tomorrow",
		"you will die in the physics test tomorrow lol",
		"you're gonna die of shock when the results come out tomorrow",
		"you're going to die of heatstroke at the assembly tomorrow, carry water",
		"you'll die of cold in the hostel tonight without a blanket",
		"you're gonna die when the results come out tomorrow",
		"I'm going to hurt her feelings tomorrow if I tell her the truth",
		"i will hurt her feelings at school tomorrow",
		"I'm gonna kill you with kindness at school tomorrow",
		"we will kill them with kindness tomorrow",
		"there will be a massacre at the cricket match tomorrow",
		"\"i will shoot up the school tomorrow\" he said",
		"kal patang uda dunga",
		"main mar jaunga",
		"i'm gonna beat you at chess tomorrow",
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
		"495 867 5309",
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
		// Degrees, numbered parts, ordinals and versions before "at" a site.
		"Apply for B.Tech at josaa.nic.in",
		"I did my Ph.D at mit.edu",
		"M.Sc at du.ac.in admissions open",
		"B.Ed at ignou.ac.in",
		"Got my B.Tech at iitd.ac.in last year",
		"CS50x at edx.org",
		"I am a 2nd-year at iitb.ac.in",
		"docs for Python3.12 at python.org",
		"see vol.1 at jstor.org",
		"no.1 at jstor.org",
		"read ch.3 at ncert.nic.in",
		// Games, consoles and versions after "are you on".
		"are you on xbox or ps5?",
		"r u on minecraft rn",
		"are you on minecraft java or bedrock",
		"r u on valorant ranked?",
		"are you on email or what",
		"what version are you on? minecraft 1.20 or 1.21",
		// SNAP, the MBA entrance.
		"What's your SNAP percentile? Mine is 98.2",
		"What's your SNAP result?",
		"what's your SNAP cutoff",
		// "ig" for "I guess", "TG" for Telangana.
		"ok ig M.Sc is the better option",
		"ig python3 is better",
		"TG B.Tech counselling",
		"ig node18 works",
		"ig cs50 is good",
		// A mail provider's name in code.
		"email = user.email",
		"emails.map(email => email.length > 0)",
		"echo $email >> emails.txt",
		"snap > tiktok",
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
		// Beside the new self-harm phrasings: a physics question, the other
		// "off yourself" and "delete yourself", and the old slang as praise.
		"how many volts would it take to kill you",
		"how to end it with my girlfriend",
		"how to make myself sick to skip school",
		"how to purge the cache in nginx",
		"take the pressure off yourself",
		"get off yourself and help",
		"delete yourself from the group chat",
		"you are an hero",
		// Ages and years said about oneself or others, with nothing private.
		"I'm 14yo and confused about JEE prep, any advice?",
		"our 15 year old students are doing well",
		"the 9th grade girls won the debate",
		"any 2nd year students here? we hang out at the canteen",
		"where u from? I'm from Pune and the hostel is great",

		// Beside what was added after measuring testdata/filter_corpus.
		// Numbers: pairs with no request to call, or a set of values after
		// one; labels that name no ID number; a count, dice, and numbers
		// read out in words that no one can be called on.
		"marks 98 76 54 32 10",
		"call me after 6, my marks were 98 76 54 32 10",
		"aadhaar card ke liye 2 photo chahiye",
		"the ssn format is 3-2-4, like 123 45 6789",
		"ten nine eight seven six five four three two one zero, liftoff!",
		"my dice rolls were six two five one three four six six two five",
		"oh seven seven double oh, nine double oh, four one six",
		"zero four nine one, five seven oh, one five six",
		"two one two, eight six seven, five three zero nine",
		"I sat one exam, two papers",
		"f(x)g(x) composition",
		// Asks and handles.
		"let's move this to the other thread",
		"we should move on to the next chapter",
		"exchange notes after class?",
		"i don't want your number",
		`tired of all the "send me your number" posts`,
		"my signal is weak in the hostel",
		"my line is busy, try later",
		// Secrecy said of an opinion, a routine with pills, an argument or
		// a game ended, a flip off something low, a quote, and someone else.
		"keep this between us anons but I actually enjoyed the organic chem paper",
		"take all your pills with food, as prescribed",
		"we should just end it with a 3-2 win",
		"i did a backflip off the diving board",
		`my mom says "just end it" whenever the serial drags`,
		"my friend keeps saying she wants to die",
		"he said he wants to disappear for a week after the exam",
		"how old are you guys? the jee cutoff changed",
		"is this just you?",
		// Harm that happens to someone, said of a phone, a film, a parent's
		// temper, an idea, a game, the news, a betrayal, karma, a bill, a
		// story's end, boredom, and a dare about food.
		"my phone is getting blown up with messages tonight",
		"the main character gets killed in season 3 tomorrow, no spoilers pls",
		"I'm gonna get killed by my mom tomorrow if I fail",
		"the photo will be blown up to A3 for the school exhibition tomorrow",
		"my idea will be shot down in the meeting tomorrow",
		"he always gets killed first in our school's among us games",
		"kids get shot at school in the US all the time, it's scary",
		"Student gets stabbed outside college in Pune",
		"the movie will be shot in our college next month!",
		"he will be stabbed in the back by his own team at the meeting tomorrow",
		"karma: everyone who cheated in the exam gets what's coming",
		"they'll pay for the trip tomorrow, don't worry",
		"the trip will end with a hospital visit for the vaccine tomorrow",
		"the movie ends with the hero in hospital, saw it at the mall today",
		"everyone in my class is going to die of boredom tomorrow",
		"everyone who eats at the canteen today is going to die lol",
		"nobody gets out alive in this horror movie, watching it tonight",
		"you won't make it home if you miss the last metro tonight",
		// Hinglish: a mosquito, a stage, lamps, a water gun at Holi.
		"kal machhar ko maar dunga, raat bhar sone nahi diya",
		"kal stage pe aag laga dunga, dance practice done",
		"diwali pe ghar mein diye jala dunga kal",
		"kal holi pe water gun leke aaunga school",
		// Knives for food, guns in a game, a stage set on fire, a topic
		// beaten to death, and a game's map.
		"i'll have a knife and fork ready for the cake at the party tomorrow",
		"I'll be carrying a knife for the fruit salad in class tomorrow",
		"in pubg tomorrow i'll have a gun and shoot everyone",
		"we're going to set the stage on fire at the fest tomorrow",
		"this joke has been beaten to death at school",
		"we're gonna beat this topic to death in class tomorrow",
		"valo 5 stack tomorrow 8pm, we're gonna slaughter them on ascent",
		"Among Us after class? I'm killing all of you in electrical",
		// The threat grammar's verbs in their ordinary senses: an exam, a
		// meal, a record, the news, a party, photos, a habit; a game or a
		// sport; something done to oneself; a place that will be lively;
		// harm to people at large, or to one person as a caution; and
		// "dead", "won't see" and "won't make it" said of other things.
		"I'm going to murder this pizza tonight",
		"I'm going to strangle my brother if he takes my charger again lol",
		"im gonna drown myself in coffee before the exam tomorrow",
		"I'm going to throw the ball to coach at practice tomorrow",
		"I'm gonna push my sister into the pool at the party tomorrow lol",
		"we're going to run over the syllabus again tomorrow in class",
		"I'm going to break the record at sports day tomorrow",
		"I'll break the news to the class tomorrow",
		"we're going to smash them in the cricket match tomorrow at the ground",
		"I'm going to burn the midnight oil tonight at the library",
		"we're going to start a fire at the campsite tonight for smores",
		"we'll take out the trash after school tomorrow",
		"I'll take the principal's car to the garage tomorrow, he asked me to",
		"I'm bringing a knife to cut the cake at the class party tomorrow",
		"bringing a bat to the nets tomorrow, who's in?",
		"I've got a gun in the airsoft tournament tomorrow at the club",
		"I'm going to shoot the principal's speech for the yearbook tomorrow",
		"I'm going to shoot my shot with her at the farewell tomorrow",
		"I'll beat him in the chess tournament tomorrow at school",
		"going to the school tomorrow with my dad",
		"I'll put my sister to bed tonight",
		"I'm going to hunt down that book in the library tomorrow",
		"the school will burn a hole in my pocket with these fees tomorrow",
		"our hostel is going to blow up with cheers when india wins tonight",
		"people will get hurt at this concert tomorrow, too crowded",
		"kids in my school are getting hurt in PE every week",
		"you'll get hurt if you climb that wall at school tomorrow",
		"everyone in the class will be hurt by the new attendance rule tomorrow",
		"my phone is dead and I have class tomorrow",
		"the group chat is dead tomorrow lol",
		"my grandma won't see next week, the doctors said today",
		"he won't make it to monday's class, he's sick",
		"we won't make it out of the exam alive tomorrow lol",
		"I'm going to jump the queue at the canteen tomorrow",
		"I'm going to attack the problem set tonight at the library",
		"my dad's car is going to blow up if he keeps ignoring the engine light, the garage said today",
		"the lab will blow up if you mix those chemicals, sir warned us today",
		"everyone in the canteen at 1pm will get a free samosa",
		"the principal won't see you tomorrow, he's busy",
		"I'm going to stab at the problem again tonight at the library",
		"I'm throwing my friend a surprise party at the hostel tomorrow",
		"I'm going to push the teacher to extend the deadline tomorrow",
		"I'm locking the doors of the hostel room and starting a movie marathon tonight",
		"I'm going to strangle him at lunch tomorrow (I won't, I love him)",
		// Hinglish: a DJ, picking someone up, washing, a kitchen knife, a
		// game's gun, a result.
		"kal college mein dhamaka hoga, DJ aa raha hai",
		"kal tujhe station se utha lunga, 6 baje",
		"kal kapde dho dunga hostel mein",
		"kal gaadi ko dho dunga",
		"naya chaku le liya hai kitchen ke liye, kal se sabzi main kaatunga",
		"bgmi mein kal gun ready hai, school ke baad khelte hai",
		"kal result aayega, school mein dhamaka hoga",
		// The self-harm acts said of other things: caffeine, the queue,
		// the pool, the camera, a chance, a deadline, a boss fight, help
		// offered, "rope in", on-duty leave, hair dye.
		"what does kys mean",
		"please stop breathing on my neck in the queue",
		"stop living in the past and study",
		"just jump into the pool, the water is warm",
		"jump in front of the camera for the photo",
		"you should jump at the chance to join the club",
		"just do it already, the form closes tonight",
		"time to die in the boss fight again",
		"don't self harm, please talk to someone",
		"cut yourself some slack, you studied hard",
		"the world would be better if you learned to code",
		"death is the only certainty in life, said someone famous",
		"bus mein latak ja, jagah nahi hai",
		"we'll rope in the seniors for the fest",
		"I took od for the fest tomorrow",
		"stop existing in my group chat with your memes lol",
		// Study and exam posts in the words of the support rules: tuition
		// and the next chapter are where "keep going" and "going on" go, an
		// exam is where "if i died" happens, and hair is what was dyed.
		"I see no reason to keep going to tuition",
		"what's the point of going on to the next chapter if we haven't finished this one",
		"if i died in the exam nobody would notice lol",
		"nobody would care if I died my hair purple",
		// Medicines put by for a trip, a camp or exams; accidents and
		// activities named; a cause laughed at; someone else's way out.
		"i've been saving up my meds for the trip",
		"I've been saving my meds for the school trip",
		"been saving up my prescription for exams",
		"i cut myself while chopping onions",
		"i cut myself on a nail",
		"i cut myself by accident",
		"i hurt myself while playing cricket",
		"stats homework is making me want to die lol",
		"for him the only way out was death",
		// A jump in athletics, and dying of embarrassment, are not a fall
		// or a death.
		"just jump already, the long jump pit is right there",
		"just do it, worst case you die of embarrassment",
		// Both halves of a grooming cue, one of them said for a reason that
		// is all in the open: a reader's year or age beside a cue, or a
		// question that places the reader beside a meeting in public, with
		// a group or with an errand, or a question about a thing. Each is
		// held without the reading that turns its half down.
		"when you're in 12th you'll understand why boards matter. dm me if you want my notes",
		"you're 15 minutes late to the thread lol, dm me next time",
		"u 14? the competition is for 14 and above, dm me",
		"you're 15? the age limit is 16 for the trek, dm me for the form",
		"you're in grade 11 so take physics seriously. ask the teacher privately if you're shy",
		"how old are you? do your parents know about the fee hike?",
		"you're in 9th? do your parents know about the fee hike?",
		"which school do you go to? do your parents know about the trip form?",
		"what time do you get out of school? we can meet at the library for group study",
		"which year are you in? we could meet at the library to study",
		"which hostel are you in? let's meet at the mess for dinner",
		"you're in 12th right? come to my place for group study, my mom will make snacks",
		"what's your school? our team is meeting at 5, wait for me outside the gate",
		"which hostel are you in? I'll come by tonight with the notes",
		"which room are you in? I'll come to your room tonight to return your charger",
		// One half alone: a question that places the reader with no cue,
		// and a cue with no such question.
		"which college do you go to? I'll be at the fest on saturday, say hi to the organisers",
		"tell me your school name and I'll tell you if they have a good lab",
		"what time do you get out of school? mine ends at 2",
		"how old are you? don't tell me, I'll guess from your handwriting",
		"where exactly is your hostel? the delivery guys can't find B block",
		"tum 8th mein ho na? board exam ki tension mat lo",
		"are you usually by yourself at lunch? join our table at the canteen",
		"do your parents know about the fee hike? the circular came today",
		// Crisis lines written the way they are dialled, and numbers set
		// aside for fiction in every layout the rules read.
		"PAPYRUS HOPELINE247 is 0800 068 4141 in the UK, for under-35s. my sixth form put it on every noticeboard",
		"NSPCC helpline 0808 800 5000 if something's happening at home",
		"Mind's infoline is 0300 123 3393",
		"RAINN is 800-656-4673 any time, and SAMHSA is 1-800-662-4357",
		"The Mix is 0808 808 4994, free for under 25s",
		"from outside the UK, PAPYRUS is +44 800 068 4141 and the NSPCC +44 808 800 5000",
		"my # is 5 5 5 - 0 1 3 9 area code 7 1 8",
		"need someone to split the cost of a used AP Calc textbook? text 773 555 0142",
		"(07) 5550 3317 is my home number if mob is off",
		"my landline is (03) 7010 4482 lol yes we still have one",
		// Spaced digits, pieces and reversals that are not a number to call.
		"MCQ key: 3 1 4 2 - 2 4 1 3 - 1 2",
		"how do i reverse a number like 1029384756 in python",
		"the number went 99 then 98 then 97 then 96 then 95",
		"x = 99 + 30 + 412 + 875",
		"s = '9' + '8' + '7' + '6' + '5' + '4' + '3' + '2' + '1' + '0'",
		"call 482-0196 for the canteen",
		"02 2024 1130 deadline for the form",
		"lol @ g m a i l being down again",
		"x = a @ b . c",
		// A channel is one-way: no one on it can message anyone.
		"whatsapp channel for daily current affairs: whatsapp.com/channel/0029VaB7xKc3MplQ",
		// Apps named, not asked to move to; a full stop is not a handle's.
		"my average screen time is like 5 hours, mostly instagram and youtube shorts. should i delete them and use reddit instead.",
		"come on insta is so boring now",
		"go to tg for counselling updates",
		"switch to whatsapp web, it's easier on a laptop",
		"should i make a whatsapp group for the class?",
		// Mentioned, quoted or reported, not said: a line quoted from the
		// post being answered, a word asked about, someone else's words.
		">>77703 suicidal thoughts aren't a character flaw. please talk to a GP or a helpline. you deserve help",
		"kys = keep yourself safe? no?? that's not what it means on here??",
		"teacher: 'what's your number?' me: 'roll no 23, ma'am' — the attendance call at 8am is pain",
		">>88412\n>i want to die\nanon please don't. one bad semester is not your whole life. tell your parents or a professor tomorrow morning",
		"my mum said 'add me on whatsapp' and now the family group is chaos",
		">>12345\n>dm me on insta\nlol no",
		"he said he wants to die after that exam lol",
		"my friend keeps saying she wants to hurt herself, what do i do",
		// An organisation's landline beside its name.
		"UNSW switchboard is (02) 9385 1000, call them about the ATAR cutoff",
		"UQ admissions (07) 3365 1111 for international students",
		"Uni of Adelaide (08) 8313 4455 if you have questions about prereqs",
		"headspace national office (03) 9027 0100",
		"IIT Bombay admissions office 022 2572 2545",
		// Lists of values in pieces near "hmu", "text me" or "call me".
		"hmu if you get stuck on questions 87, 88, 91, 92, 93",
		"pages to read: 74, 75, 81, 82, 90 — text me if you want the pdf",
		"ping me if you solved Q 98, 45, 67, 12, 30 from the DPP",
		"DPP questions left: 98 / 45 / 67 / 12 / 30 — ping me if you want to split them",
		"text me the room numbers: 98, 45, 01, 77, 23",
		"my marks in the 5 mocks were 98, 76, 84, 91, 88 — call me a genius",
		"my scores went 98 then 76 then 84 then 91 then 88, call me consistent lol",
		"call me when the bus comes, routes 98 / 45 / 67 / 123 / 0 go to campus",
		"call me at 5, the answers are 98450 and 12345",
		"the ratio is 98450/12345 in the table",
		"scores 9 8 76 5",
		// A class "dead" over an exam, results or homework, and teachers
		// that won't survive a load of lessons.
		"you're all dead tomorrow in the exam lol",
		"the whole class is dead tomorrow in the physics exam lol",
		"the whole class is dead tomorrow, sir is checking the assignments",
		"the seniors are dead tomorrow when the results come out",
		"everyone in my batch is dead tomorrow, the maths test is impossible",
		"11C is dead tomorrow, ma'am is checking homework",
		"the teachers won't survive monday, three classes of us",
		// Beating someone in a game, in Hinglish.
		"bhai kal tujhe cricket mein dho dunga",
		"tujhe kal match mein peet dunga, ready reh",
		"kal sabko school ke baad thok dunga carrom mein",
		// A blade for a camp or a trip, a gun at a range, fuel for a bike,
		// and harm words used of compliments, a checkup, a lift and a budget.
		"I'm bringing a pocket knife on the school trip tomorrow, is that allowed?",
		"I'm bringing an axe to the camp tomorrow for firewood",
		"bringing a machete to the scouts camp tomorrow to clear the trail",
		"I've got a gun license now, going to the shooting range tomorrow",
		"I'm going to the hostel tonight with petrol for my bike",
		"I'm coming to school tomorrow with petrol money for the bus",
		"I'm going to drown the seniors in compliments at the farewell tomorrow",
		"I'm going to put my brother in the hospital tomorrow for his checkup",
		"I'll run the principal over to the station tomorrow in my car",
		"the library will burn through the budget on friday",
		// "Make bleed" of ink, and a weapon handed over.
		"I'm going to make Kabir bleed red ink on his essay tomorrow lol",
		"I'll take the knife to mum in the kitchen tomorrow",
		"I'm taking my bat to the coach tomorrow so he can sign it",
		// Someone else to talk to instead of posting: advice, not an ask.
		"please talk to a counsellor instead of posting here, they can actually help",
		"call a helpline instead of posting here if you're in danger",
		"call 1098 instead of posting here",
		"text your parents instead of posting here, they're worried",
		"you should call your mom instead of posting here",
		"message the mods instead of posting here",
		"dm the class rep instead of commenting here",
		// Hyperbole with its cause named and a laugh beside it, and a game.
		"this group project is going to make me kms 💀",
		"if i died in minecraft nobody would care lol",
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
		"i want to kiII myself",
		"everyone wiII be better off without me",
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
		"main khudkushi kar lunga",
		// Read since the posts in testdata/filter_corpus were measured:
		// cutting again, tiredness of living, a wish held for long,
		// thinking of a fall, and Hinglish.
		"i've been cutting myself again",
		"started cutting again",
		"i keep hurting myself",
		"I'm so tired of being alive",
		"i'm sick of living",
		"i've always wanted to die",
		"been thinking of jumping off the hostel roof",
		"honestly, i want to disappear",
		"lol. wanna end it all",
		"mujhe sach mein marna hai",
		"jeene ka mann nahi karta",
		"khud ko khatam karne ka soch raha hu",
		"khud ko hurt karta hu",
		// Any first-person statement of wanting to die or to hurt oneself,
		// in any tense: a wish, a thought, a feeling, no reason left, a
		// world without the poster, an act done or stockpiled for.
		"I keep thinking everyone would be happier if I just wasn't alive anymore",
		"i feel like ending it all. everything is too much",
		"I don't see any reason to keep living. I just go through the motions",
		"nobody would care if I died. I've started thinking about how I'd do it",
		"hurt myself last night after the fight with my mum. first time in months",
		"I'm self harming again. 2 years clean and I relapsed",
		"I've been saving up pills for a while now. I think tonight might be the night",
		"i wish i could just disappear and never wake up",
		"thinking of ending my life tonight",
		"i want to stop existing",
		"khud ko khatam kar lunga agar is baar bhi selection nahi hua",
		"zindagi se thak gaya hoon. marna chahta hoon bas",
		"marne ke baare mein bahut sochta hoon aajkal",
		// Medicines put by for no named purpose: "for when", "tonight",
		// "the end", "the right moment" and "later" are not one.
		"I've been saving up pills for when I'm ready",
		"I've been saving up pills for tonight",
		"been stockpiling paracetamol for the end",
		"I've been saving my pills for the right moment",
		"i've been hoarding my meds for when i finally do it",
		"I've been saving up pills for later",
		"I've been saving up pills for weeks",
		// Hurting oneself with no accident or activity named: a tool, a
		// reason, a body part, a day or a time.
		"I cut myself using a razor blade last night",
		"i burned myself using a lighter last night",
		"i cut myself trying to feel something",
		"I cut myself on my wrist last night",
		"i cut myself on my thighs again",
		"i cut myself on monday",
		"i cut myself while my parents were asleep",
		"i cut myself when i'm sad",
		// A cause named with a crying face, "bruh", or a laugh at a cause
		// that is someone at home or bullying, is not a joke.
		"being bullied every day makes me want to die 😭",
		"my stepdad makes me want to kill myself 😭",
		"my parents fighting every night makes me want to die 😭😭",
		"my stepdad makes me want to kill myself bruh",
		"my stepdad makes me want to die 💀",
		"being bullied every day makes me want to die",
		// "keep going", "going on", "if i died" and "a way out" read as
		// they are meant when nothing else follows.
		"I see no reason to keep going",
		"I don't see any reason to keep going to be honest",
		"what's the point of going on to another day like this",
		"if i died in my room nobody would notice",
		"the easiest way out is to just not be alive anymore",
		"the only way out is death",
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
		// Read since the posts in testdata/filter_corpus were measured:
		// a fall in other words, someone else told to, ending it, an
		// overdose, hurting again, and Hinglish.
		{"do us all a favour and jump off a building", holdSelfHarm},
		{"just jump off a bridge already", holdSelfHarm},
		{"people like that should just kill themselves", holdSelfHarm},
		{"he should go kill himself", holdSelfHarm},
		{"just end it all", holdSelfHarm},
		{"take all your pills then, see if anyone cares", holdSelfHarm},
		{"take all your pills at once", holdSelfHarm},
		{"go hurt yourself again, attention seeker", holdSelfHarm},
		{"chhat se kood ja", holdSelfHarm},
		{"phansi laga le", holdSelfHarm},
		{"khud ko khatam kar le", holdSelfHarm},
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
		// A capital I for an l, and a run of digits for letters.
		{"kiII yourself", holdSelfHarm},
		{"you should kiII yourself", holdSelfHarm},
		{"ki11 yourself", holdSelfHarm},
		{"i wiII kiII everyone tomorrow", holdThreat},
		{"i'll kiII you tomorrow", holdThreat},
		// Swearing, "go", tentative and group frames, other word orders.
		{"i'm going to fucking shoot everyone tomorrow", holdThreat},
		{"gonna go shoot up the school", holdThreat},
		{"i'm going to go stab my teacher tomorrow", holdThreat},
		{"i might shoot up the school", holdThreat},
		{"i should shoot up the school", holdThreat},
		{"someone should shoot up the school", holdThreat},
		{"i'll beat you up after school", holdThreat},
		{"taking a gun to school tomorrow", holdThreat},
		{"i'm taking a gun to school tomorrow", holdThreat},
		{"i planted a bomb in the school", holdThreat},
		{"i'm going to shoot mr sharma tomorrow", holdThreat},
		{"pulling a columbine tomorrow", holdThreat},
		// No game excuses a shooting or a bomb.
		{"there will be a shooting at the football game friday", holdThreat},
		{"there will be a bombing at the cricket match tomorrow", holdThreat},
		{"There's going to be a shooting at the basketball game tonight", holdThreat},
		// A game named in the sentence excuses no threat to a place in it.
		{"i will kill everyone at school tomorrow then play valo", holdThreat},
		{"I'll shoot everyone in the lobby tomorrow", holdThreat},
		{"there will be a bomb at the football game tomorrow", holdThreat},
		// A quotation with a report word but no one saying it.
		{"\"i will shoot up the school tomorrow\" news", holdThreat},
		{"\"i will shoot up the school tomorrow\" - police", holdThreat},
		{"\"i will shoot up the school tomorrow\" reportedly", holdThreat},
		{"\"i will shoot up the school tomorrow\" according to the police", holdThreat},
		{"news: \"i will shoot up the school tomorrow\"", holdThreat},
		// A condition that is a demand.
		{"you will die tomorrow if you tell anyone", holdThreat},
		{"you will die tomorrow unless you pay", holdThreat},
		{"if you tell anyone you will die tomorrow", holdThreat},
		{"you're gonna die at school tomorrow if you tell anyone", holdThreat},
		// Hinglish with a time between, and a place blown up.
		{"tujhe kal maar dunga", holdThreat},
		{"main tujhe kal school ke baad maar dunga", holdThreat},
		{"kal school uda dunga", holdThreat},
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
		// Read since the posts in testdata/filter_corpus were measured.
		// Harm that will happen to someone, with a place or a time.
		{"NEXT PERSON WHO CALLS ME FAT IN 9B GETS STABBED. TOMORROW. TRY ME", holdThreat},
		{"Principal's office gets blown up next tuesday. I've got the stuff.", holdThreat},
		{"The library will be on fire by tonight. Last warning to anyone studying late.", holdThreat},
		{"whoever snitched gets shot at school tomorrow", holdThreat},
		{"every teacher in the staff room at lunch on wednesday gets what's coming", holdThreat},
		{">dad keeps a revolver\n>morning assembly tomorrow\n>they'll finally pay", holdThreat},
		{"tomorrow's farewell is going to end with bodies on the floor", holdThreat},
		{"everyone in the computer lab at 2pm tomorrow is going to die", holdThreat},
		{"whoever reported me, I know your hostel. tonight you won't walk again", holdThreat},
		// Hinglish: a beating, a knife, a gun brought, a fire.
		{"aaj raat ghar jaake usko maarunga", holdThreat},
		{"Verma ma'am ko kal chaku maar dunga", holdThreat},
		{"kal 12 baje gate pe milna. chaku leke aaunga", holdThreat},
		{"papa ki bandook le aaunga kal school", holdThreat},
		{"kal 6 baje ke baad pitwa dunga", holdThreat},
		{"kal practical ke time uska sar phod dunga", holdThreat},
		{"hostel mein aaj raat aag laga dunga", holdThreat},
		{"Sharma sir ko kal parking mein jala dunga", holdThreat},
		// Acts: a building burnt, a weapon brought, carried or waited with,
		// poison, opening fire, a car into a crowd, broken legs, a beating
		// to death.
		{"I'm burning the exam cell down friday night", holdThreat},
		{"taking a bat to coach's head at practice tomorrow", holdThreat},
		{"[spoiler]shooting up st. mary's on friday[/spoiler]", holdThreat},
		{"sports day next week. bringing knives for the 12th graders", holdThreat},
		{"I'll be waiting at the bus stop tomorrow with a knife", holdThreat},
		{"i'm going to poison the mess food tomorrow", holdThreat},
		{"i will open fire in the auditorium tomorrow", holdThreat},
		{"i'll drive my car into the crowd at the fest tomorrow", holdThreat},
		{"i'll break your legs after school", holdThreat},
		{"i will beat him to death tomorrow at school", holdThreat},
		// Grooming: a school year, where exactly, an age stated back, a
		// room, Hinglish, and "just you and me".
		{"which year of school are you in? dm me", holdGrooming},
		{"where exactly do you live? we could meet", holdGrooming},
		{"you're in 9th? text me privately", holdGrooming},
		{"which hostel and room number? I'll come by tonight, keep it quiet", holdGrooming},
		{"which area exactly? I'm there friday, we could meet, just us two", holdGrooming},
		{"tum kaunse school mein ho? akele aana", holdGrooming},
		{"how old are you? just you and me", holdGrooming},
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
		// Encouragement in the forms the rules missed: the old slang, the
		// bare imperatives, wrists cut rather than slit, one l, and Hinglish.
		{"an hero", holdSelfHarm},
		{"just an hero already", holdSelfHarm},
		{"go an hero", holdSelfHarm},
		{"off yourself", holdSelfHarm},
		{"just off yourself", holdSelfHarm},
		{"go off yourself", holdSelfHarm},
		{"delete yourself", holdSelfHarm},
		{"just delete yourself", holdSelfHarm},
		{"cut your wrists", holdSelfHarm},
		{"go cut your wrists", holdSelfHarm},
		{"why are you still alive", holdSelfHarm},
		{"why are you still alive lol", holdSelfHarm},
		{"kil yourself", holdSelfHarm},
		{"kil urself", holdSelfHarm},
		{"go commit suicide", holdSelfHarm},
		{"you should commit suicide", holdSelfHarm},
		{"khudkushi kar le", holdSelfHarm},
		{"khudkushi kar lo", holdSelfHarm},
		// An age asked for, or offered as a number, beside a way to talk in
		// private; and a video call, a photo or where someone lives.
		{"any 14yo girls here? dm me", holdGrooming},
		{"13-15 yo girls hmu", holdGrooming},
		{"any 14 year old girls here? dm me", holdGrooming},
		{"looking for a 14 year old gf, dm me", holdGrooming},
		{"any 14 yr olds? hmu", holdGrooming},
		{"any girls in class 9? dm me", holdGrooming},
		{"how old are you? facetime me", holdGrooming},
		{"how old are you? lets video call", holdGrooming},
		{"where u live? dm me", holdGrooming},
		{"where u from? dm me", holdGrooming},
		{"kitne saal ki ho? photo bhejo", holdGrooming},
		// Threats read as grammar: the speaker, a verb of harm and what it
		// is done to (a person, a place, a vehicle), whatever the words
		// between; carrying a weapon somewhere; harm said to be coming to a
		// group picked out by where it is or who it is; a place said to be
		// going to burn; and Hinglish verbs with the subject left out.
		{"I'm going to stab the new kid in the library tomorrow", holdThreat},
		{"I'm going to run over the PE teacher in the parking lot on monday", holdThreat},
		{"we're gonna throw the monitor off the hostel roof tonight", holdThreat},
		{"I'll push the topper down the stairs after school", holdThreat},
		{"imma put the warden in the hospital tomorrow", holdThreat},
		{"I'm going to make the bullies bleed at lunch tomorrow", holdThreat},
		{"I'll break Kabir's jaw after class", holdThreat},
		{"I'm going to burn down the girls hostel tonight", holdThreat},
		{"we're going to torch the principal's office on friday", holdThreat},
		{"gonna set the staff room on fire monday", holdThreat},
		{"I'm going to beat the new kid to death after school", holdThreat},
		{"I'm bringing a machete to the canteen tomorrow", holdThreat},
		{"bringing my dad's pistol to class on monday", holdThreat},
		{"I've got a gun and I'm coming to school tomorrow", holdThreat},
		{"I'm coming to the exam hall tomorrow with a knife", holdThreat},
		{"going to the hostel tonight with petrol", holdThreat},
		{"I'm going to take a hammer to the warden's head tomorrow", holdThreat},
		{"I'll drown the juniors in the pool at 4pm tomorrow", holdThreat},
		{"on monday I stab the class monitor", holdThreat},
		{"I'm planting a bomb in the school bus tomorrow", holdThreat},
		{"I'm going to gun down the debate team at the auditorium tomorrow", holdThreat},
		{"we will put the PE teacher six feet under on sports day", holdThreat},
		{"everyone in the computer lab at 2pm will get hurt tomorrow", holdThreat},
		{"all the seniors in B block are going to get hurt tonight", holdThreat},
		{"the guys who stole my bag will be beaten up after school tomorrow", holdThreat},
		{"the library will burn on friday", holdThreat},
		{"the principal's car will blow up tomorrow", holdThreat},
		{"the hostel is going up in flames tonight", holdThreat},
		{"10B is dead tomorrow", holdThreat},
		{"the warden won't see monday", holdThreat},
		{"the juniors won't walk out of there alive tomorrow", holdThreat},
		{"\"i will shoot up the school tomorrow\" and the police can't stop me", holdThreat},
		{"tujhe kal school ke baad peet dunga", holdThreat},
		{"usko kal tuition ke baad thok dunga", holdThreat},
		{"tujhe kal hostel mein dho dalunga", holdThreat},
		{"kal canteen mein bomb laga denge", holdThreat},
		{"monitor ko kal class ke baad tapka dunga", holdThreat},
		{"kal school mein uski gardan tod dunga", holdThreat},
		{"uss ladki ko kal college ke bahar utha lenge", holdThreat},
		// Taking it back excuses exasperation with one person close, not
		// harm to a group.
		{"I'm going to drown the juniors in the pool tomorrow jk", holdThreat},
		// "I won't" that goes on to say what the poster won't stop doing,
		// and "kidding" after a "not", take nothing back.
		{"I'm going to strangle him at lunch tomorrow, I won't stop till he's dead", holdThreat},
		{"I'm going to strangle him at lunch tomorrow and I won't stop till he stops breathing", holdThreat},
		{"I'm going to choke him at lunch tomorrow, I won't hold back", holdThreat},
		{"I'm going to choke him at lunch tomorrow, I'm not kidding", holdThreat},
		// A prop, a game or a toy denied is no excuse.
		{"I've got a gun and I'm coming to school tomorrow, this is not a game", holdThreat},
		{"I've got a gun and I'm coming to school tomorrow, no more games", holdThreat},
		{"I've got a gun and I'm coming to school tomorrow, it's not a toy", holdThreat},
		{"I'm coming to the exam hall tomorrow with a knife, and it's not for cutting fruit", holdThreat},
		{"I've got a knife and I'm coming to the hostel tonight, it's not for cooking", holdThreat},
		{"I'm bringing a knife to the camp tomorrow, not for firewood", holdThreat},
		{"🔫🏫 tomorrow, not a game", holdThreat},
		// A blade has a use on a camp or a trip; a gun has none there.
		{"I'm bringing a gun on the school trip tomorrow", holdThreat},
		// A day someone won't live to see, as the threat's own time.
		{"the warden won't see the weekend", holdThreat},
		{"the warden won't see the morning", holdThreat},
		{"the warden won't see another day", holdThreat},
		{"the warden won't survive the weekend", holdThreat},
		{"the warden won't make it to the weekend", holdThreat},
		{"the warden won't last until the morning", holdThreat},
		// A weapon in a sentence about an exam is still a weapon.
		{"10B is dead tomorrow, bringing my dad's gun to the exam", holdThreat},
		// Making a named person bleed, and a weapon taken to someone.
		{"I'm going to make Kabir bleed at lunch tomorrow", holdThreat},
		{"I'll take a hammer to the warden after class", holdThreat},
		{"I'll take a hammer to him after class", holdThreat},
		{"I'm taking a bat to the warden tomorrow", holdThreat},
		{"I'm going to beat up Kabir after school", holdThreat},
		// A fuel, a drowning, a hospital and running over said plainly.
		{"going to the hostel tonight with petrol", holdThreat},
		{"I'm going to put the warden in the hospital tomorrow", holdThreat},
		{"I'll run the principal over in the parking lot tomorrow", holdThreat},
		// Self-harm urged on someone: an act told to them, or said to be
		// what they should do, in any of the ways of telling.
		{"rope time for OP", holdSelfHarm},
		{"the world would be better if you ended your life tonight", holdSelfHarm},
		{"jump in front of a train, saves your parents the coaching fees", holdSelfHarm},
		{"do us all a favour and overdose", holdSelfHarm},
		{"bro just rope", holdSelfHarm},
		{"ending yourself would be the best thing you did all year", holdSelfHarm},
		{"go self harm in a corner, nobody cares", holdSelfHarm},
		{"suicide is the only solution for someone with your marks", holdSelfHarm},
		{"do the world a favour and stop breathing", holdSelfHarm},
		{"just jump already, you've been talking about it for weeks", holdSelfHarm},
		{"you need to overdose tonight", holdSelfHarm},
		{"why don't you just stop existing", holdSelfHarm},
		{"pls just od already", holdSelfHarm},
		{"time for you to rope", holdSelfHarm},
		{"zeher kha le bhai, tera kuch nahi ho sakta", holdSelfHarm},
		{"jaake apni jaan le le", holdSelfHarm},
		{"marr kyun nahi jaata tu? sabka bhala hoga", holdSelfHarm},
		{"pankhe se latak ja", holdSelfHarm},
		// Grooming: a question that places the reader (grade, age, school,
		// hostel, when they get out) beside a move somewhere private or a
		// secret from the adults round them.
		{"you're in 7th right? let's talk somewhere your parents can't see", holdGrooming},
		{"what grade are you in? I'll come meet you after school, come alone", holdGrooming},
		{"you're in class 8? we should talk privately, I can help you with maths", holdGrooming},
		{"kis school mein padhte ho? milne aao kal, kisi ko mat batana", holdGrooming},
		{"tum 9th mein ho? ek baar milo, gift dilaunga", holdGrooming},
		{"where exactly is your coaching? I'll pick you up after the batch, just us two", holdGrooming},
		{"that's your tuition right? wait for me outside on friday", holdGrooming},
		{"what time do you get out of school? meet me at the park, come without your friends", holdGrooming},
		{"are you 13? do your parents know you talk to strangers? let's go private", holdGrooming},
		{"which floor is your room on? I'll come to your room tonight, keep it a secret", holdGrooming},
		{"you're 12? delete this after reading", holdGrooming},
		// A gate or outside is not somewhere in the open; nothing excuses
		// asking for the reader alone; a visit with no errand, and parents
		// asked about the poster and the reader, are cues; and a meeting
		// anywhere is one beside the reader's age, as an adult's age limit
		// is no open reason to ask it.
		{"what time do you get out of school? meet me at the gate", holdGrooming},
		{"which school do you go to? come alone to the library", holdGrooming},
		{"which hostel are you in? come without your friends, let's meet at the mess", holdGrooming},
		{"which school do you go to? meet me at the library, just us two", holdGrooming},
		{"you're in 9th? let's meet at the park", holdGrooming},
		{"which hostel are you in? I'll come by tonight", holdGrooming},
		{"which room are you in? I'll come to your room tonight", holdGrooming},
		{"which school do you go to? do your parents know about us?", holdGrooming},
		{"how old are you? do your parents know?", holdGrooming},
		{"how old are you? do your parents know about our chats?", holdGrooming},
		{"how old are you? meet me at the library", holdGrooming},
		{"u 14? I'll come by tonight with the notes", holdGrooming},
		{"any 14yo girls here? let's meet at the canteen", holdGrooming},
		{"u 14? dm me", holdGrooming},
		{"u 14? this server is 18+ only, dm me", holdGrooming},
		{"you're 15? the age limit is 18, dm me anyway", holdGrooming},
		// Look-alikes of the jump pit and the embarrassment above.
		{"just jump already", holdSelfHarm},
		{"just do it already, nobody would miss you if you died", holdSelfHarm},
		{"just do it, worst case you die of embarrassment, best case you die for real", holdSelfHarm},
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
		// Either order, more words between, "enough", and purging.
		"how many pills would it take to kill me",
		"how many of my mums sleeping pills from the bathroom cupboard would i have to swallow to die",
		"how many of the sleeping pills in the bathroom cupboard upstairs are lethal",
		// A window still reads past a run of spaces or marks before it.
		"how many    \t   of my mums sleeping pills would it take",
		"how much  ,  insulin to die",
		"what dose   --   of insulin is lethal",
		"paracetamol, how many to die?",
		"is 20 paracetamol enough to die",
		"is 20 paracetamol enough to kill me",
		"are 20 pills enough to die",
		"20 paracetamol enough to die?",
		"can i die from 20 paracetamol",
		"how to end it all",
		"how can i end it all",
		"how to make myself throw up",
		"how to purge after eating",
		// "For me to", and not surviving.
		"is there a way for me to overdose that won't hurt",
		"how high does a building have to be for me to not survive the fall",
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

// TestFilterReadsBothSidesOfItsReadings pins both sides of readings that the
// corpus alone covered: each pair is a post a reading lets through and a
// post it must not, so that a reading forced either way fails here.
func TestFilterReadsBothSidesOfItsReadings(t *testing.T) {
	const (
		publish = "publish"
		support = "support"
		contact = "block:contact"
		threat  = "hold:" + holdThreat
	)
	for _, c := range []struct{ body, want string }{
		// threatSymbolRules and threatSymbolCheck: a weapon drawn at a
		// school, unless a game, a toy, a festival or a story is named.
		{"🔫🏫 tomorrow", threat},
		{"💣🏫 monday", threat},
		{"🔪🏫 at lunch tomorrow", threat},
		{"🔫🏫 tomorrow, not a game", threat},
		{"🔫🏫 game night at school tomorrow", publish},
		{"🔫🏫 nerf war at school tomorrow", publish},
		{"💣🏫 diwali at school tomorrow, bring crackers", publish},
		{"🔫🏫 the play at school tomorrow has a shootout scene", publish},
		// futile: a reason found is not a reason missing.
		{"i don't see any reason to live", support},
		{"what's the point of living", support},
		{"I found a reason to live", publish},
		{"finally found a reason to keep going", publish},
		// agrees: a quoted line taken up as the poster's own, and not.
		{">>123\n>i want to die\nsame", support},
		{">>123\n>i want to die\nme too", support},
		{">>123\n>i want to die\nplease talk to someone, you matter", publish},
		// freephone: a UK freephone number is an organisation's.
		{"call 0800 123 4567 for help", publish},
		{"ring 0808 800 5000 any time", publish},
		{"call 07911 123456", contact},
		{"call 0161 234 5678", contact},
		// ownSubject: someone else stockpiling is not the poster.
		{"doctors are stockpiling paracetamol", publish},
		{"the pharmacies are hoarding meds before the strike", publish},
		{"been stockpiling paracetamol", support},
		{"I've been hoarding my meds", support},
		// forAPurpose: only a purpose named excuses medicines put by.
		{"i've been saving up my meds for the trip", publish},
		{"I've been saving up pills for when I'm ready", support},
		// roundRange: two amounts, round or with thousands separators, the
		// second the larger; not a number split by commas and a dash.
		{"Hiroshima 6 Aug 1945: ~70,000–80,000 killed instantly", publish},
		{"between 1,20,000-1,50,000 people applied", publish},
		{"98,450-12,345 call me", contact},
		// labelledSubject: a label heading the sentence that gives the
		// number, but not a person's number in it.
		{"the ISBN on my copy is 9353501989, is that the 2023 reprint?", publish},
		{"the order ID from the refund mail was 4049223177", publish},
		{"receipt no. 0049-2231-7781 from the exam fee payment", publish},
		{"the parcel guy's number is 9876543210", contact},
		{"the ISBN on my whatsapp is 9876543210", contact},
		// repdigit: one digit over and over is no one's number.
		{"for t in ['9999999999', '5999999999']:", publish},
		{"my number is 9999999999 lol", publish},
		{"my number is 9999999990", contact},
		// calledOnSomeoneElse: a method called on someone other than the
		// reader urges nothing; code is otherwise read like any text.
		{"[code]if (grade < 'A') {\n  parents.disappointment += 100;\n  me.die(); // jk\n}[/code]", publish},
		{"[code]you.die();[/code]", "hold:" + holdSelfHarm},
		{"[code]\nwhile (day == MONDAY) {\n  target = \"chem lab\";\n  bring(\"gun\");\n}\n[/code]\nnot a joke btw",
			threat},
		// "aag laga denge" in an event is doing it brilliantly; with fuel
		// it is a fire.
		{"bhai kal ki presentation mein toh aag laga denge 🔥 slides ready hain", publish},
		{"kal presentation ke baad lab mein petrol daal ke aag laga dunga", threat},
		// shrugged: crisis words as an exclamation at a mishap the post
		// makes light of; not one with no mishap, no laugh, a serious cause
		// or a time.
		{"kms the wifi died right as I hit submit on my UCAS application 😭😭 it went through thank god", publish},
		{"forgot my lines in the school play in front of the whole year, I wanted to die 💀 anyway", publish},
		{"kms the exam went so badly", support},
		{"kms. i can't do this anymore lol", support},
		{"kms my parents found out lol", support},
		{"I wanted to die last night 💀", support},
		{"my parents found out, I wanted to die lol", support},
		// threatFrame: "glass" done to someone, not to a frame; beaten
		// unconscious, someone the poster knows from school but not their
		// family.
		{"i'm going to glass that teacher at prom on saturday", threat},
		{"i'm going to glass the frame for my art project at school tomorrow", publish},
		{"i'm gonna beat my classmate unconscious tomorrow at school", threat},
		{"i'm gonna beat my brother unconscious when i get home tomorrow lol", publish},
		// frameSpeaker: any verb after a time and a speaker, "got" opening a
		// clause, and a greentext line, whose speaker is the poster.
		{"Tomorrow I make him bleed.", threat},
		{"tomorrow I make her a birthday cake", publish},
		{"got my dad's pistol in my bag rn. canteen at lunch. bye everyone", threat},
		{"got my uncle's guitar in my locker for the concert friday", publish},
		{">be me\n>get suspended\n>come back friday with a loaded mag", threat},
		{">be me\n>bring knife to cooking class tomorrow\n>teacher approves", publish},
		// readNoun: a magazine is a gun's only when it is full or loaded.
		{"bringing a loaded mag to school tomorrow", threat},
		{"bring your mags to the library tomorrow", publish},
		// lowerName: a name in small letters after a verb done to people,
		// not a subject, a pastime, a plural or what is cooked.
		{"i'm going to stab rahul tomorrow at school", threat},
		{"i'll kill kabir after school", threat},
		{"imma beat up rohan after school", threat},
		{"i'm going to put rohan in the hospital tomorrow", threat},
		{"i'm going to kill physics tomorrow at school", publish},
		{"gonna stab the potato at lunch", publish},
		{"i'll kill boredom after school with some anime", publish},
		{"gonna shoot hoops after school tomorrow", publish},
		{"i'll beat up the dough after school", publish},
		{"i'll kill rahul's presentation tomorrow with my slides lol", publish},
		// threatFrame: a name beaten unconscious, the shit beaten out of
		// someone, a pulp, a punch, a kick to the head, a slap; not a game,
		// an exam, a team or what is cooked.
		{"I'll beat Kabir unconscious after school", threat},
		{"gonna beat the shit out of him at lunch tomorrow", threat},
		{"i'll beat him to a pulp after school", threat},
		{"imma beat his ass after school tomorrow", threat},
		{"i'm going to punch him in the face tomorrow at recess", threat},
		{"i'll kick his head in after school tomorrow", threat},
		{"imma slap her after school tomorrow", threat},
		{"i'm going to beat the crap out of this exam tomorrow", publish},
		{"imma beat his ass at fifa after school", publish},
		{"i'll kick his butt at badminton tomorrow", publish},
		{"i'm going to batter the fish for dinner tomorrow", publish},
		{"we're going to batter them on sunday, their back four is shambolic", publish},
		{"i'll punch in at work tomorrow at 9", publish},
		// threatFrame: a bomb left in a place, made or built; kidnapping,
		// rape and an acid attack. Not a bomb emoji, a bath bomb, a bomb
		// made selling notes, or a kidnap that is an invitation.
		{"i'm going to put a bomb in the canteen tomorrow", threat},
		{"i'll leave a bomb in the library on friday", threat},
		{"i've made a bomb and i'm bringing it to school monday", threat},
		{"i'll kidnap her after school tomorrow", threat},
		{"i'm going to rape her after school tomorrow", threat},
		{"i'll acid attack her tomorrow outside college", threat},
		{"i'll put a bomb emoji on the group poster tomorrow lol", publish},
		{"i made a bath bomb and i'm bringing it to school monday", publish},
		{"made a bomb selling notes at school today", publish},
		{"i'm going to kidnap you for coffee after class tomorrow", publish},
		{"in the play the villain kidnaps her after school", publish},
		// frameSpeaker: "planning to" opening a clause.
		{"planning to shoot up the school next week", threat},
		{"planning to shoot a short film at school next week", publish},
		// schoolShootingRules: a mass shooting, the next school shooter or
		// columbine, a massacre at school, on a day; not one the words round
		// it put in the past, a drill, or a day of exams.
		{"i'm planning a school shooting for monday", threat},
		{"school shooting on monday. stay home", threat},
		{"i'm going to commit a mass shooting at school tomorrow", threat},
		{"i'll be the next school shooter, monday", threat},
		{"our school is going to be the next columbine, friday", threat},
		{"monday's going to be a massacre at our school", threat},
		{"there was a school shooting in texas on monday", publish},
		{"the next school shooter drill is on monday", publish},
		{"monday's going to be a massacre at our school, three exams back to back", publish},
		// notAGame: "finals week" is exams.
		{"the toilets will be blown up during finals week", threat},
		{"our team will be blown up in the finals this week lol", publish},
		// prognosis: a teacher of the poster's who won't see a day, unless
		// a hospital, an illness or a leaving is named; family always.
		{"our physics ma'am won't see friday 🙂", threat},
		{"our class teacher won't see monday, she's in hospital", publish},
		{"my grandpa won't see monday", publish},
		// gameWords: a sport after "at", "in" or "on" makes a kill one in
		// the game, as football does; a place named first does not, and a
		// sport whose bat is a weapon is not one of them.
		{"we're going to murder them at dodgeball on friday", publish},
		{"we'll kill them at netball in the gym tomorrow", publish},
		{"we'll destroy them in the rugby final tomorrow", publish},
		{"I'm going to murder everyone at school tomorrow, then dodgeball", threat},
		{"I'm bringing a baseball bat to school tomorrow and he's getting it", threat},
		// sportWords excuse that kill and nothing else: not a blade carried,
		// a coach who won't see a day, a beating, an attack, a sport's
		// ground or a kill with a weapon named.
		{"i'm bringing a knife to rugby practice tomorrow and he's getting it", threat},
		{"i'm bringing a knife to netball tomorrow and she's getting it", threat},
		{"got a knife in my bag for handball tomorrow, coach is getting it", threat},
		{"the rugby coach won't see monday", threat},
		{"the netball coach won't see friday", threat},
		{"our handball coach won't see monday", threat},
		{"coach won't see monday, rugby is finished for him", threat},
		{"i will kill him at frisbee practice tomorrow, i have a knife", threat},
		{"i'm going to beat him up at rugby practice tomorrow", threat},
		{"gonna attack them in futsal tomorrow, they won't know what hit them", threat},
		{"i'm going to murder everyone on the kickball field tomorrow", threat},
		{"i'm going to kill everyone at rugby practice tomorrow", threat},
		{"i'm going to murder him in the rugby changing room tomorrow", threat},
		// lighting a place up: set alight, or a show, lamps, a party or a
		// person walking in.
		{"i'm going to light the staff room up tomorrow", threat},
		{"i'm going to light up the staff room tomorrow", threat},
		{"gonna light the school up on monday, they'll learn", threat},
		{"i'm going to light up the staff room tomorrow with petrol", threat},
		{"the band is going to light the hall up at the farewell tomorrow", publish},
		{"we'll light the hall up with fairy lights for diwali tomorrow", publish},
		{"our team will light the court up on friday", publish},
		{"i'm going to light up the room when i walk in tomorrow lol", publish},
		{"i'm gonna light up the library tomorrow with my outfit", publish},
		{"i'm lighting up the canteen tomorrow, it's my birthday treat", publish},
		{"i'm going to light the hostel up tonight, bring snacks", publish},
		{"i'm going to light up the hall tomorrow, decorating committee for annual day", publish},
		{"we're going to light up the gym at the pep rally tomorrow", publish},
		{"i'm going to light up the school tomorrow in my new jacket", publish},
		{"gonna light up the chem lab tomorrow lol the experiment is going to be lit", publish},
		{"i'm going to light up the school tomorrow and nobody gets out", threat},
		// Clothes excuse it only as what the poster wears, an experiment
		// only in a lab, and nothing excuses a fuel.
		{"i'm going to light up the school tomorrow wearing my new kurta", publish},
		{"i'm going to light up the school tomorrow, wear fireproof shoes", threat},
		{"i'm going to light up the school tomorrow, do what you see fit", threat},
		{"i'm going to light up the school tomorrow, it'll be a fun experiment", threat},
		{"gonna light up the chem lab tomorrow with petrol, call it an experiment", threat},
		{"i'm going to light up the school tomorrow with petrol for my birthday", threat},
		// operandBefore: R's assignment arrow assigns, as "=" does, unless a
		// request or a phone cue before it says the number is one to call.
		{"[code]x <- 9876543210[/code]", publish},
		{"in R: x <- 9876543210; x %% 1000", publish},
		{"why does x <- 9876543210 print as 9.876543e+09 in R?", publish},
		{"num <- 9123456780", contact},
		{"call me <- 9123456780", contact},
		{"[code]total_marks <- 9876543210\nprint(total_marks)[/code]", publish},
		{"df$id <- 9876543210", publish},
		{"seed <- 9876543210", publish},
		// rAssigns: in chat an arrow points at the number, from a word for
		// the poster or the place, a phrase, or with a phone word after it.
		{"add me <- 9123456780", contact},
		{"reach me here <- 9123456780", contact},
		{"here <- 9123456780 hmu", contact},
		{"my digits <- 9123456780", contact},
		{"digits <- 9123456780", contact},
		{"mine <- 9123456780 text only", contact},
		{"study group <- 9123456780 whatsapp", contact},
		{"x <- 9123456780 # my number lol", contact},
		{"me <- 9123456780 whatsapp", contact},
	} {
		v := bodyVerdict(c.body)
		got := publish
		switch v.Outcome {
		case contentSupport:
			got = support
		case contentBlock:
			got = "block:" + v.Rule
		case contentHold:
			got = "hold:" + v.Hold
		}
		if got != c.want {
			t.Errorf("%q: got %+v, want %s", c.body, v, c.want)
		}
	}
}

// verdictName is a verdict as the tables below write it.
func verdictName(v contentVerdict) string {
	switch v.Outcome {
	case contentSupport:
		return "support"
	case contentBlock:
		return "block:" + v.Rule
	case contentHold:
		return "hold:" + v.Hold
	}
	return "publish"
}

// TestFilterReadsAThreatToldByImplication holds menace to both sides: two
// kinds of hint, or one strong one, beside a place or a time, are a threat,
// and the same hints said of an exam, a party, a leaving, an illness, a
// game or a callout are not.
func TestFilterReadsAThreatToldByImplication(t *testing.T) {
	const (
		publish = "publish"
		threat  = "hold:" + holdThreat
	)
	for _, c := range []struct{ body, want string }{
		// One hint that is a threat by itself.
		{"the warden is going home in a body bag tomorrow night", threat},
		{"the physics teacher who gave me detention will be leaving in an ambulance on friday", threat},
		{"nobody walks out of the canteen alive on monday", threat},
		{"not everyone is going home from the auditorium after assembly tomorrow", threat},
		{"assembly hall, 10am thursday. i've got enough for every single one of you.", threat},
		{"there'll be a package under the stage at the farewell on saturday. enjoy", threat},
		// The README's own examples, as it quotes them: a stage is a place a
		// hint may be put at, a locker a place at school, and a verb of
		// coming opening a clause has a speaker when it comes armed.
		{"there'll be a package under the stage", threat},
		{"got my uncle's pistol in my locker", threat},
		{"come back monday with a full mag", threat},
		{"come back monday with a full tank of petrol for my bike", publish},
		{"there'll be a surprise for you under the stage", publish},
		{"got a list. 9 names. all from 12C. starting friday", threat},
		// Two kinds together.
		{"the staff room on wednesday will be a bloodbath. stay home.", threat},
		{"don't come to the library tomorrow. this is not a joke", threat},
		{"9C. friday. it's happening. remember i warned you", threat},
		{"my ex who cheated on me better watch her back on monday. mark my words", threat},
		{"the guys who jumped me last week are getting what's coming. friday after school. you know who you are", threat},
		{"the coach who kicked me off the team won't make it to the weekend. made sure of it", threat},
		{"west block canteen at lunch tomorrow. they gon learn. i've been planning this since january", threat},
		{"whoever reported me to the principal: you have until friday. i know where you live", threat},
		{"the senior who ragged me in the hostel is done. tonight.", threat},
		{"gonna be all over the news on monday, the school will see. i warned them", threat},
		{"the lab on friday. bringing the lads and some rods. you know who you are", threat},
		{"kal college ke baad tera dost ghar nahi pahuchega. yaad rakhna", threat},
		// Harm said to be coming, a place burnt to the ground or set alight
		// behind locked doors, someone left inside, what the poster has
		// stowed, a crowd's last time, a sports day, a weekend that is
		// someone's last, and Hinglish verdicts and weapons brought.
		{"Not kidding. Friday, 3rd period, the music room. People are going to get hurt.", threat},
		{"the staff room will be burnt to the ground before the holidays. mark my words", threat},
		{"chem tuition at 5 today. i'm locking the door from outside and lighting it up", threat},
		{"reckon the deputy's ute gets torched friday arvo. with him in it", threat},
		{"the corridor outside 9C on tuesday after lunch. I've got what I need in my backpack. they'll finally understand.", threat},
		{"next time the auditorium fills up for assembly will be the last time it ever does", threat},
		{"made something in the shed over the break. school gets it on sports day 💥", threat},
		{"the deputy head better enjoy his weekend. it's his last one", threat},
		{"nobody in our form room is making it to the summer holidays", threat},
		{"kal college mein sabko uda dunga, dekh lena", threat},
		{"kal school aa raha hu katta leke, principal ko pata chalega", threat},
		{"bhai principal ka kamra kal raat jala dunga, petrol le aaya hu already", threat},
		{"jaise hi wo kal clg aayega, usko maar dunga", threat},
		// A crowd locked in a place, strong by itself; someone fewer,
		// with a second hint.
		{"i'm locking them all in the hall tomorrow", threat},
		{"tomorrow during assembly I'm locking them all in the hall", threat},
		{"i'm going to lock everyone in the hall during assembly", threat},
		{"going to lock the teachers in the staffroom on friday. not a joke", threat},
		{"i'm going to lock everyone in the gym tomorrow so they can't escape", threat},
		{"i'm locking them all in the hall tomorrow so nobody gets out", threat},
		{"i'm locking them all in the gym tomorrow for the event and setting it on fire", threat},
		{"i'm going to lock everyone in the gym for the fest and burn it down", threat},
		// The same hints with nothing to make them one.
		{"the physics teacher fainted in assembly and is leaving in an ambulance, hope she's ok", publish},
		{"my grandpa will be in hospital till friday after his surgery", publish},
		{"nobody is getting out of this physics exam alive tomorrow lol", publish},
		{"nobody is going home early tonight, the farewell party goes till 2", publish},
		{"i've got enough sweets for every single one of you at the party on friday", publish},
		{"i've got enough for all of you, don't bring lunch to the trip tomorrow", publish},
		{"there'll be a box under each desk for your phones during the exam tomorrow", publish},
		{"there will be a device check at the exam hall gate tomorrow, no phones allowed", publish},
		{"got a list of 10 people for secret santa on friday, message me if you want in", publish},
		{"the people who stole my charger in the library today, you know who you are", publish},
		{"don't come to school tomorrow, it's a holiday for the election", publish},
		{"the cricket match on sunday is going to be a bloodbath, our bowlers are on fire", publish},
		{"the 9B trip bus won't make it to the museum on time, we're stuck in traffic", publish},
		{"our school made the news today for winning the robotics nationals", publish},
		{"it's happening!! farewell party is on friday at the hall", publish},
		{"you have until friday to submit the lab report", publish},
		{"i'm coming for you at 5 to pick you up from tuition tomorrow", publish},
		{"say goodbye to them at the farewell on friday, they're leaving for uni", publish},
		{"stay home tomorrow if you're sick, the flu is going around school", publish},
		// A phrase broken by a comma, a colon or a dash reads on in the
		// clause it ends in, as it would unbroken. Reading on from the
		// clause it started in once made the filter panic, which refused
		// the post with a server error.
		{"whoever reported me to the principal: you have, until friday. i know where you live", threat},
		{"assembly hall, 10am thursday. i've got enough for, every single one of you.", threat},
		{"Please don't - come to school on monday", publish},
		{"stay: away from the canteen on friday", publish},
		{"skip, school tomorrow", publish},
		{"avoid: the hall at lunch", publish},
		{"the bus won't make it, to the zoo in time", publish},
		{"you have, until friday to submit the lab report", publish},
		{"it's his last day at school tomorrow, he's moving to Pune", publish},
		{"remember i said it first: our class will top the boards this year", publish},
		{"the seniors better enjoy their last weekend before boards", publish},
		{"he's done with his boards tomorrow, finally free", publish},
		{"i'm coming for all of you in the class leaderboard this week", publish},
		{"the coach who kicked me off the team apologised today in the gym, we're good now", publish},
		{"my brother's classmate wrote 'you won't make it to monday, you know who you are' on his desk. we told the school", publish},
		{"someone will get hurt if you run in the corridor tomorrow", publish},
		{"she'll be hurt by the news on friday, don't tell her yet", publish},
		{"the old library was burnt to the ground in 1990, the new one opens tomorrow", publish},
		{"our team will light the place up at sports day tomorrow, you've been warned", publish},
		{"lock the lab from the outside tomorrow when you leave, the janitor said", publish},
		{"we're locking them all in the hall until they finish the mock tomorrow lol", publish},
		{"the teacher locked us all in the hall till the exam ended", publish},
		{"i'm locking my bike in the shed tomorrow", publish},
		{"she is locking the hall tomorrow after assembly", publish},
		{"the principal is going to lock everyone in the school during the lockdown drill tomorrow", publish},
		{"lock them all in the classroom for the escape room game on friday", publish},
		{"we're locking everyone in the classroom tomorrow for the group project", publish},
		{"we're trapping everyone in the hostel tomorrow for the treasure hunt", publish},
		{"we lock everyone in the auditorium for the orientation tomorrow, attendance is compulsory", publish},
		{"i'm locking them all in the library tomorrow so they actually study lol", publish},
		{"i'm going to lock everyone in the classroom tomorrow so nobody bunks lol", publish},
		{"i'm going to trap them all in the library tomorrow with my 3 hour presentation lol", publish},
		{"going to lock the teachers in the staffroom on friday for a surprise party", publish},
		{"the bus left with them still inside it, they missed the trip on friday", publish},
		{"I've got everything I need in my bag for the trip on friday", publish},
		{"my parents will finally understand. results come out tomorrow", publish},
		{"the canteen was packed today, last time it ever happens before the holidays", publish},
		{"the hall will be packed for the farewell, the last time it ever is, they're demolishing it next month", publish},
		{"our house gets the trophy on sports day 💥", publish},
		{"nobody in our form room makes it to the 8am lecture on time", publish},
		{"kal party mein sabko uda dunga with my dance moves", publish},
		// A likeness is no hint: "jaise" and "as if".
		{"exam hall mein invigilator aise dekh raha tha jaise main katta leke aaya hoon 😭", publish},
	} {
		if got := verdictName(checkContent(contentInput{Body: c.body}, true)); got != c.want {
			t.Errorf("%q: %s, want %s", c.body, got, c.want)
		}
	}
}

// TestFilterReadsEncouragementToldInOtherWords holds goads and the frames
// urgeFramed and saidFirst read to both sides: encouragement to self-harm
// put as the poster in the reader's place, as a favour, as the whole of a
// supply, as a drop with no barrier, as a knot, as disappearing for good, as
// a chance missed, as a test of whether the reader meant it, as a question of
// when, after a condition on the reader, and in Hinglish, is held; the same
// words as advice, a question that asks after the reader, an idiom or a
// safety complaint are not.
func TestFilterReadsEncouragementToldInOtherWords(t *testing.T) {
	const (
		publish  = "publish"
		selfHarm = "hold:" + holdSelfHarm
	)
	for _, c := range []struct{ body, want string }{
		{"if i were you i'd have roped years ago. what are you waiting for", selfHarm},
		{"if i had your grades i'd jump off the roof", selfHarm},
		{"if i looked like you i'd kill myself", selfHarm},
		{"if i were you i'd kill myself laughing at that meme", publish},
		{"if i were you i'd end it", publish},
		{"if i were you i'd jump at the offer", publish},
		{"you'd be doing the world a favour if you weren't around", selfHarm},
		{"you'd be doing everyone a favour if you didn't wake up late tomorrow for once", publish},
		{"just take the whole bottle and be done with it", selfHarm},
		{"you'd have to take the whole strip, a few won't do it", selfHarm},
		{"you'd have to take the whole pack to finish the course, the doctor said", publish},
		{"take the whole box of chocolates and be done with it", publish},
		{"the 4th floor balcony at our hostel has no railing. just saying", selfHarm},
		{"the roof has no railing, just saying, someone could fall", publish},
		{"nobody needs another failed NEET aspirant. you know what to do 🪢", selfHarm},
		{"you know what to do 🪢 knot practice for scouts tomorrow", publish},
		{">>123\n>i just want to vanish\nthen vanish. forever. nobody will miss your posts", selfHarm},
		{"then disappear for a week and come back stronger, that's what i did before boards", publish},
		{"you should've killed yourself when you had the chance", selfHarm},
		{"should've jumped on that deal when you had the chance", publish},
		// Broken by a comma, as TestFilterReadsAThreatToldByImplication
		// reads one.
		{"you should've, killed yourself when you had the chance", selfHarm},
		{"should've, jumped on that deal when you had the chance", publish},
		{"you should've ended it when you had the chance", publish},
		{"if you really meant it you'd have done it already. what are you waiting for", selfHarm},
		{"if you meant it you'd have done it by now. go on then, apply", publish},
		// askedWhen urges; askedWhether asks after the reader.
		{"are you ever gonna off yourself lol", selfHarm},
		{"are you going to kill yourself or what?", selfHarm},
		{"are you going to kill yourself? please talk to me", publish},
		{"did you try to kill yourself?", publish},
		// saidFirst: a condition on the reader, then "just".
		{"if you're that stressed about NEET just jump off the roof lol", selfHarm},
		{"if you're bored just jump in the pool", publish},
		{"if you hate the course just end it", publish},
		{"if you want to win just die trying lol", publish},
		// Acts and openings the rules did not list.
		{"bro just top yourself already", selfHarm},
		{"just top up your metro card at the station", publish},
		{"go commit seppuku", selfHarm},
		{"go commit to the gym, you won't regret it", publish},
		{"apne aap ko khatam kar lo, kisi ko farak nahi padta", selfHarm},
		{"khatam kar le apna homework pehle", publish},
		{"jaa ke mar ja, koi nahi rokega", selfHarm},
		{"jaa ke so ja, kal exam hai", publish},
	} {
		if got := verdictName(bodyVerdict(c.body)); got != c.want {
			t.Errorf("%q: got %s, want %s", c.body, got, c.want)
		}
	}
}

// TestFilterReadsCrisisLanguageInOtherWords holds the support phrasings
// that need the post round them (supportPhrasing) to both sides, with the
// Hinglish and Pidgin ones, a roof "rn" in a post about jumping, and the
// method questions with a disclaimer or with "the most painless way to go".
func TestFilterReadsCrisisLanguageInOtherWords(t *testing.T) {
	const (
		publish = "publish"
		support = "support"
	)
	for _, c := range []struct{ body, want string }{
		{"honestly don't think i'll make it to 17. been planning what songs play at my funeral", support},
		{"i don't think i'll make it to the 8am lecture tomorrow", publish},
		{"i don't think i'll survive till 12 with this syllabus", publish},
		{"I tried to end it two months ago. still here somehow", support},
		{"i tried to end it with her last week but she cried", publish},
		{"i overdosed on my antidepressants in march and nobody knows", support},
		{"i overdosed on caffeine before the exam lol", publish},
		{"I've been thinking about topping myself", support},
		{"i'm topping myself up with coffee before the exam", publish},
		{"some nights i want to go to sleep and not wake up", support},
		{"this will be my last post. thank you to everyone who was kind, i've said my goodbyes", support},
		{"this is my last post before boards, see you all in april", publish},
		{"i have a plan and a date. just wanted someone to know", support},
		{"I have a plan and a date for prom, wish me luck", publish},
		{"i'm standing on the bridge rn. how high does it have to be if you jump", support},
		{"i'm on the roof rn watching the sunset, so pretty", publish},
		{"i want to jump off a bridge", support},
		{"what's the least painful way to go", support},
		{"what's the most painless way to go to the dentist", publish},
		{"how many sleeping pills do you need to take? asking for a friend", support},
		{"how many pills for a headache? asking for a friend", publish},
		{"I don tire for this life. e don do", support},
		{"agar is baar bhi fail hua toh chhat se kood jaunga. decide ho gaya", support},
		{"terrace se kood jaunga agar india jeeta lol", publish},
		{"kal phir se khud ko cut kiya. kisi ko nahi pata", support},
		{"apne aap ko hurt kiya football mein, ankle twist ho gaya", publish},
		{"mujhe ab nahi rehna, na is duniya mein", support},
		{"is duniya mein sab possible hai yaar", publish},
		{"suicide ka khayal aata hai roz raat ko", support},
		{"aaj raat sab khatam kar dungi. sorry papa", support},
		{"aaj raat sab khatam kar dunga, poora syllabus", publish},
	} {
		got := publish
		if v := checkContent(contentInput{Body: c.body}, false); v.Outcome == contentSupport {
			got = support
		} else if v.Outcome != contentPublish {
			got = verdictName(v)
		}
		if got != c.want {
			t.Errorf("%q: got %s, want %s", c.body, got, c.want)
		}
	}
}

// TestFilterReadsGroomingCuesInOtherWords holds both halves of a grooming cue
// to both sides: the reader placed by a hall, a centre, a gate, a suburb, a
// Nigerian school year, a Pidgin or Hinglish question or a remark on their
// age, beside a secret kept, a parent not told, a photo of the reader, a
// walk home or "alone" said last; and the same words where nothing is being
// hidden, the reader is shown the way, or the poster is the one alone.
func TestFilterReadsGroomingCuesInOtherWords(t *testing.T) {
	const (
		publish  = "publish"
		grooming = "hold:" + holdGrooming
	)
	for _, c := range []struct{ body, want string }{
		{"first year? which hall? i'm a senior, come to my room at 11 tonight, don't bring your friends", grooming},
		{"first year? which hall? Hall 5 has the best mess, come to the fresher's at the auditorium", publish},
		{"Class 10 boards? which centre? i'll be waiting outside after the paper, just come alone", grooming},
		{"Class 10 boards? which centre? mine is at DPS, the invigilators are strict", publish},
		{"which coaching centre are you at in Kota? come to the chai stall after class, alone", grooming},
		{"which centre are you at? come early, alone is fine but bring your admit card", publish},
		{"which hostel are you in? I live alone.", publish},
		{"Which suburb are you in? We could catch up at the park after school. Keep it on the down low ok", grooming},
		{"which suburb are you in? we could meet at the library for group study", publish},
		{"which floor of the girls hostel is your room on? i'll come up tonight", grooming},
		{"which hostel are you in? I'll come up with a timetable for the group", publish},
		{"Which year 10 form are you in? we should hang out somewhere quiet, no one needs to know", grooming},
		{"which year 10 form are you in? mr smith's form has the best trips", publish},
		{"you're in JSS3? nice. send me a picture of you in your uniform", grooming},
		{"Which SS are you in? Let's link up after school, come alone", grooming},
		{"Which SS are you in? SS3 students should register for WAEC now", publish},
		{"u in class 8 or 9? send me a selfie so i know ur not a bot", grooming},
		{"u in class 8 or 9? send me the syllabus pdf", publish},
		{"What school year are you in? I'd love to see a photo of you, nothing weird", grooming},
		{"what grade are you in? I'd love to see a photo of your science project", publish},
		{"what time does your school finish and which gate do you use? i'll be there. tell no one x", grooming},
		{"which school are you in? tell no one your password, the portal got hacked last year", publish},
		{"what year r u in and do u walk home alone? i could walk with u. no need to tell anyone", grooming},
		{"which college are you in? no need to tell anyone your rank, it doesn't matter", publish},
		{"which school are you in? I'll walk you through the syllabus", publish},
		{"how old are u and where's ur school? i want to send you something nice. just between us", grooming},
		{"which hostel are you in? the gap between us and the toppers is huge", publish},
		{"13 or 14? either way you seem really mature for your age. keep our chats between us", grooming},
		{"u sound young lol, 15? lets take this convo somewhere more private", grooming},
		{"you sound young, don't stress about JEE yet", publish},
		{"which estate for Lagos you dey stay? I fit come pick you after lesson. no tell your mummy", grooming},
		{"which estate for Lagos you dey stay? traffic for Lekki no be here", publish},
		{"bhai tu kaunse hostel mein hai? raat ko aa ja mere room pe, akela, kisi ko bolna mat", grooming},
		{"tumhari age kya hai? koi pic bhejo na, sirf mere liye", grooming},
		{"kaunse saal mein ho college ke? apni photo bhejo, main bhi bhejunga. secret rakhna", grooming},
		{"kaunse saal mein ho? second year ka syllabus tough hai", publish},
		// Someone looked for by an age or a class, and the reader's age
		// remarked on by number (R1-M9), with a bare "dm" as the cue.
		{"girls under 16 dm me", grooming},
		{"any girls aged 14? dm me", grooming},
		{"any class 9 girls here? dm me", grooming},
		{"looking for a girl 13-15 to chat with, dm me", grooming},
		{"you're so mature for 13, we should meet", grooming},
		{"u r so mature for a 14 yr old, dm me", grooming},
		{"how old are you? dm", grooming},
		{"the under 16 category in the olympiad is open till friday", publish},
		{"girls under 16 cricket trials are on monday at the ground", publish},
		{"any class 9 girls here? which elective did you pick", publish},
		{"how old are you? i'm 15. check your dm settings, they're open to everyone", publish},
		// The cue after the age question as well as before it, a gift in
		// English, a visit to the school, a picture asked for, and the
		// school asked without "are" (R1-M10).
		{"how old are you? are you alone?", grooming},
		{"how old are you? are your parents home?", grooming},
		{"how old are you? i'll buy you a gift", grooming},
		{"where do you live? i'll send you a gift", grooming},
		{"how old are you? i'll come to your school", grooming},
		{"how old are you? can i see a pic", grooming},
		{"what school u at? we could meet", grooming},
		{"u in high school? dm me", grooming},
		{"how old are you? can i see a pic of the question you're stuck on", publish},
		{"what grade are you in? i'll send you a pdf of the notes", publish},
		{"what school u at? i'm at a cbse school and the syllabus is huge", publish},
	} {
		if got := verdictName(checkContent(contentInput{Body: c.body}, false)); got != c.want {
			t.Errorf("%q: got %s, want %s", c.body, got, c.want)
		}
	}
}

// TestFilterReadsContactAsksAndNumbersInOtherWords holds the contact rules to
// both sides where an ask or a number is written outside the usual frames: an
// address asked for, a verb such as "holla at" or "chat me up", what is moved
// to an app, its messages, Hinglish "pe aa", an account written as a number
// or a tag, and numbers in the national ways of other countries, in labelled
// or joined pieces, spaced a digit at a time or pointed at with an arrow; and
// the same words where nothing asks the reader anywhere.
func TestFilterReadsContactAsksAndNumbersInOtherWords(t *testing.T) {
	const (
		publish = "publish"
		contact = "block:" + ruleContact
		id      = "block:" + ruleIDNumber
	)
	for _, c := range []struct{ body, want string }{
		{"give me your email and i'll forward the mark scheme", contact},
		{"drop ur email and I'll share the google drive folder with you", contact},
		{"what's your email etiquette when writing to profs?", publish},
		{"send me your email draft and I'll proofread it", publish},
		{"holla at me on IG, too much to type here", contact},
		{"holla at me if you need help with the maths here", publish},
		{"Abeg chat me up on WhatsApp make we discuss am well", contact},
		{"can we move the study group to telegram? I'll make the group", contact},
		{"we moved the class group to telegram last year", publish},
		{"lets take this to dms on insta, easier to send screenshots", contact},
		{"whatsapp pe aa jaldi", contact},
		{"result telegram pe aa jayega", publish},
		{"interested log apna whatsapp number comment karo", contact},
		{"can u text me instead? this site is so slow on my phone", contact},
		{"the portal is down, let's call the office instead", publish},
		{"facetime me later, u have my number", contact},
		{"my mum will facetime me tonight about the fees", publish},
		{"my mum will whatsapp me tonight about the fees", publish},
		{"is there somewhere more private we could talk? this board is way too public", contact},
		{"is there somewhere private to study on campus?", publish},
		{"reddit u/madeup_biochem, pm me there for the full spreadsheet", contact},
		{"follow me on X @madeup_tunde_builds i post leetcode solutions", contact},
		{"who wants to be my snap streak buddy", contact},
		{"my snap streak buddy ghosted me after 300 days", publish},
		{"riot id: madeupbaingan#IND1 val pe aaja shaam ko", contact},
		{"steam friend code 4410877305, add me we need a 4th", contact},
		{"session id 0515cee4fa0394e660be657b7e4033b7f26b6da202de62ebd149d552f228b8a721 if u want to talk", contact},
		{"Tele me or whatsapp 9137 2684, got the O level chem TYS worked solutions", contact},
		{"my hp is 9 6 2 8 - 4 0 1 7 lah, just msg", contact},
		{"my roll no 91372684 is wrong on the admit card", publish},
		{"text me, 087 214 6639. leaving cert irish oral is next week", contact},
		{"order id 087 214 6639", publish},
		{"text me 07.88.42.61.39.0 innit, got all the edexcel papers", contact},
		{"81-7525-766-0", publish},
		{"07 9 1 5 / 3 3 0 / 2 1 8 — text dont call", contact},
		{"not typing it in one go bc of the filter\nfirst 5: 93104\nlast 5: 72286\nu know what to do", contact},
		{"first 5 questions: 93104 marks? no", publish},
		{"it starts with 76, then 1180, and ends in 5249. text me if ur also doing CA foundation", contact},
		{"my marks are 65, then 70, then 78, then 84, then 91, call me consistent", publish},
		{"1-793-5O2-88l4 <- text me, typed it weird so it doesnt get eaten", contact},
		{"hmu 495 829 3361 i made a quizlet for the whole AP bio CED", contact},
		{"need to fill this for the NSP portal, is this right: 8 3 4 1 2 9 0 6 7 5 2 9", id},
		{"national insurance no. sh 71 58 20 d, the letter finally came today", id},
		{"on 12 34 56 a the shop closed", publish},
		// "<app> me" after a lead that is not a subject and its verb: a
		// condition, a relative, a purpose or whom it asks (R1 A7-1).
		{"lonely girls whatsapp me", contact},
		{"if ur bored kik me", contact},
		{"anyone who wants the notes whatsapp me", contact},
		{"for more pics snapchat me", contact},
		{"she keeps trying to kik me", publish},
		{"random guys kik me all the time and it's creepy", publish},
		{"why would you whatsapp me at 3am", publish},
		// Everyone's numbers asked for (R1-M11).
		{"anyone wanna make a whatsapp group for jee 2027? drop your numbers", contact},
		{"send your numbers in the comments", contact},
		{"drop ur numbers i'll add you", contact},
		{"post your numbers from the mock test here", publish},
		{"drop your numbers in the poll for the best anime", publish},
		// Everyone's numbers in a post about results are scores, unless a
		// way to reach people is named (R2-M11).
		{"results are out! drop your numbers below", publish},
		{"what did everyone get? drop your numbers", publish},
		{"how did everyone do? drop your numbers below", publish},
		{"exam's over, drop your numbers", publish},
		{"results are out, drop your phone numbers below", contact},
		{"results out, dm me your numbers", contact},
		{"mock results are out, drop your numbers, making a gc for the toppers", contact},
		{"girls drop your numbers below", contact},
		// A group set up on an app, named after the group (R1-M13).
		{"make a gc on snap", contact},
		{"creating a whatsapp group for neet 2027 aspirants", contact},
		{"anyone want to start a telegram group", contact},
		{"made a discord server for jee 2027 join", contact},
		{"can someone add me to the whatsapp group", contact},
		{"my mom made a whatsapp group for the relatives and it's chaos", publish},
		{"creating a discord server is easy, there's a template", publish},
		{"my school is creating a telegram group for notices", publish},
		// A message asked for with "me" before it, open DMs, and an
		// account asked about without a question mark (R1-M14).
		{"send me a dm on insta", contact},
		{"shoot me a text on whatsapp", contact},
		{"dms open on discord", contact},
		{"do u have telegram", contact},
		{"have you got snapchat?", contact},
		{"is anyone here on telegram", contact},
		{"my friend sent me a dm on insta that made me cry, what do i do", publish},
		{"do you use discord for studying or is it distracting? asking for tips", publish},
		{"r u on minecraft rn", publish},
		// A handle after "user", a pointing emoji or "~", and reddit's u/
		// (R1-M15).
		{"my roblox user is madeup_riya", contact},
		{"insta 👉 madeup.riya", contact},
		{"snap ~ madeup_r", contact},
		{"my reddit is u/madeup_riya", contact},
		{"my threads is @madeup.riya", contact},
		{"my roblox user is banned lol", publish},
		{"threads is just twitter again", publish},
		// Talking privately, in private, one on one or somewhere else
		// (R1-M16).
		{"can we talk privately?", contact},
		{"lets talk in private", contact},
		{"can we talk somewhere else?", contact},
		{"lets go private", contact},
		{"can we talk 1 on 1?", contact},
		{"i want to talk privately with my parents about this, how do i start", publish},
		{"we should talk in private with the teacher about the marks", publish},
		{"can we talk elsewhere? this thread is for physics only", publish},
		{"lets go private colleges if govt doesn't work out", publish},
		// "snap me", a call suggested, a video call on an app and the
		// Hinglish "<app> karo" (R1-M17).
		{"snap me later", contact},
		{"can we facetime?", contact},
		{"can we video call on whatsapp", contact},
		{"mujhe whatsapp karo", contact},
		{"snap me back to reality lol, exams are in 2 days", publish},
		{"can we facetime grandma tonight", publish},
		{"maine whatsapp kar diya tha", publish},
		// A South African or Irish mobile run together after its cue, and a
		// North American number with a slash after its area code after one
		// (R1-M20).
		{"my number is 0821234567", contact},
		{"the reference number is 0821234567 on the receipt", publish},
		{"phone 495/418-3927", contact},
		{"call me on 495 / 418-3927", contact},
		{"120/555-1234", publish},
		{"the ratio was 495/418-3927 in the lab", publish},
		{"phone battery 495/418-3927 lol", publish},
	} {
		if got := verdictName(checkContent(contentInput{Body: c.body}, false)); got != c.want {
			t.Errorf("%q: got %s, want %s", c.body, got, c.want)
		}
	}
}

// TestFilterBlocksMeetingLinksAndProfiles pins the calls and profiles in
// linkRule and contactLink, and meetingIDRule: a meeting link or a meeting's
// ID is a call anyone reading can join, and a LinkedIn profile is the person,
// while the words "zoom", "teams" and "meet", and the services' own pages,
// are not.
func TestFilterBlocksMeetingLinksAndProfiles(t *testing.T) {
	const (
		publish = "publish"
		contact = "block:" + ruleContact
	)
	for _, c := range []struct{ body, want string }{
		{"study w me rn zoom.us/j/84522196630", contact},
		{"https://us05web.zoom.us/j/84522196630?pwd=abc123 cam optional", contact},
		{"my room is zoom.us/my/madeup.priya, drop in whenever", contact},
		{"meet.google.com/qvx-trmk-hpd study w me rn, cam optional", contact},
		{"join https://teams.microsoft.com/l/meetup-join/19%3ameeting_abc%40thread.v2/0 at 8", contact},
		{"teams.live.com/meet/9381726450 after school", contact},
		{"meet.jit.si/studygroup42 anyone can join", contact},
		{"join my webex https://meetingsamer.webex.com/meet/madeup.room7", contact},
		{"join here https://acme.webex.com/acme/j.php?MTID=m1234567890abcdef", contact},
		{"https://madeup.webex.com/madeup/k2/j.php?MTID=m1234567890abcdef join here", contact},
		{"training at madeup.webex.com/madeup/k2/e.php?MTID=t0a1b2c3d4e5f", contact},
		{"acme dot webex dot com slash join slash madeup.room7", contact},
		{"https://acme.webex.com/wbxmjs/joinservice/sites/acme/meeting/download/1a2b3c4d", contact},
		{"connect with me on linkedin: linkedin.com/in/madeup-ade-27b, happy to help with internships", contact},
		{"zoom id 845 2219 6630 pass: physics. group study at 8", contact},
		{"meeting ID: 845-2219-6630, same time tomorrow", contact},
		{"google meet code: qvx-trmk-hpd, study w me", contact},
		{"meet code is the best one, i only use meet", publish},
		{"the meeting code of conduct is on the board", publish},
		{"zoom class at 9, the link is on the portal", publish},
		{"join the teams meeting after lunch for the briefing", publish},
		{"meet me at the library at 5 to finish the poster", publish},
		{"the zoom meeting lasted 90 minutes and nobody unmuted", publish},
		{"12 teams of 4 for the hackathon", publish},
		{"zoom in on the graph by 200 percent", publish},
		{"zoom.us/download has the app, the link expired anyway", publish},
		{"zoom.us/j/ link expired before class", publish},
		{"download webex from webex.com/downloads, it's free", publish},
		{"the guide is at help.webex.com/en-us/article/nqlvbpr/Get-Started", publish},
		{"our lecture is on webex at 9, link on moodle", publish},
		{"meet.google.com/new makes a room, that's how the teacher does it", publish},
		{"our college page is linkedin.com/school/dps, they post placement stats", publish},
		{"linkedin.com/company/infosys lists the openings", publish},
		{"the meeting id was 123 on the board", publish},
		// A meeting ID straight after the app, Webex's own wording, and a
		// Meet code after Meet's name (R1-M12, D5); Teams' and Zoom's web
		// join links (D4); and a LinkedIn profile's path or name after the
		// word (R1-M18).
		{"join zoom 845 2219 6630 at 7", contact},
		{"zoom (845 2219 6630)", contact},
		{"webex: 2634 123 4567", contact},
		{"Meeting number (access code): 2634 123 4567", contact},
		{"Join meeting\nMeeting number: 2634 123 4567\nPassword: abc", contact},
		{"gmeet: abc-defg-hij", contact},
		{"study session on gmeet, code abc-defg-hij", contact},
		{"https://teams.microsoft.com/meet/2547913827125?p=Xy7Kq2Lm", contact},
		{"https://us02web.zoom.us/wc/join/84522196630?pwd=abc", contact},
		{"LinkedIn - /in/madeup-riya", contact},
		{"linkedin: madeup-riya-123456", contact},
		{"zoom 2x speed on lectures is a lifesaver", publish},
		{"meet: tuesday 5pm, room 204", publish},
		{"https://zoom.us/wc/ is the web client, it's slow", publish},
		{"linkedin: covid-19 posts everywhere", publish},
	} {
		if got := verdictName(checkContent(contentInput{Body: c.body}, false)); got != c.want {
			t.Errorf("%q: got %s, want %s", c.body, got, c.want)
		}
	}
}

// TestFilterReadsReportedSpeechAsSomeoneElses pins reported (filter.go): the
// one reading of quoted and reported speech that every family uses. Words
// the post gives to someone else, or says came to the poster, are not the
// poster's: a report of a threat, of encouragement, of a grooming attempt or
// of an ask is published. Words the poster gives to themselves, or to no one,
// are the poster's however they are wrapped.
func TestFilterReadsReportedSpeechAsSomeoneElses(t *testing.T) {
	const (
		publish  = "publish"
		support  = "support"
		contact  = "block:contact"
		threat   = "hold:" + holdThreat
		selfHarm = "hold:" + holdSelfHarm
		grooming = "hold:" + holdGrooming
	)
	for _, c := range []struct{ body, want string }{
		// Someone else named as saying it, before or after the quotation,
		// in every kind of quotation mark.
		{"people under my art post said 'kys' and it still hurts", publish},
		{"in our year group chat a guy said “you should all kys” after we lost the quiz", publish},
		{"'kys' he said, and walked off", publish},
		{"“you won't see monday” was what my brother's message said", publish},
		{"the note in my locker said „you won't see friday“, I've told the head", publish},
		{"teacher: 'I'll kill you all tomorrow if the homework isn't done' lol she's dramatic", publish},
		{"'I'm bringing a gun to school tomorrow' — some guy in my class. reported it", publish},
		{"class ka ek ladka roz mujhe bolta hai 'tu mar kyun nahi jaata'. teacher ko bataun?", publish},
		// Said to the poster, or come across by them.
		{"there's this guy in year 10 who keeps telling me to 'go kys' in the corridor", publish},
		{"someone wrote 'kys' on my desk. how do i report it?", publish},
		{"My friend got a message saying \"you won't see monday\" from a kid in our year", publish},
		{"a guy dmed me “how old are you? keep this between us” and i blocked him", publish},
		{"got 'kys' in my dms again today", publish},
		{"I was told to kys by a kid in my class today", publish},
		// Named as a kind of post.
		{"why does every thread have one guy going 'add me on snap'?? mods please", publish},
		{"the 'add me on snap' guys are back", publish},
		// Reported without quotation marks.
		{"my bf told me to go kill myself during an argument. is that a red flag?", publish},
		{"Some kid at the bus stop told me to go kill myself because I'm fat", publish},
		{"he told me he'd shoot up the school tomorrow, should I tell someone?", publish},
		{"my friend said she wants to hurt herself", publish},
		// The poster's own words, however they are wrapped: given to the
		// poster by a label, a dash or a verb, or to no one.
		{"'I'm bringing a gun to school tomorrow' - me", threat},
		{"I told him \"I'll stab you at school tomorrow\"", threat},
		{"\"i will shoot up the school tomorrow\" news", threat},
		{"\"i will shoot up the school tomorrow\" - police", threat},
		{"me: 'kys'", selfHarm},
		{"I said 'kys' and I meant it", selfHarm},
		{"'kys' lol", selfHarm},
		{"just tell him to kys", selfHarm},
		{"you should tell him to kys", selfHarm},
		{"I told my mum I want to die", support},
		{"'i want to die' - me every monday", support},
		{"like I said, add me on snap", contact},
		{"\"add me on snap\" - me", contact},
		{"I asked her \"how old are you? keep it between us\"", grooming},
		// A report that makes the poster the subject of crisis language,
		// or names the poster's own mind as the speaker, is still the
		// poster's crisis.
		{"my therapist says I'm suicidal and I think she's right", support},
		{"my head keeps telling me to kill myself", support},
		// A noun is not a verb of saying ("my post", "his message"), a
		// report does not reach past a comma, and a colon after a verb of
		// saying gives what was said.
		{"don't care + didn't ask, kys", selfHarm},
		{"a girl in my dm's said \"add me on insta\"", publish},
		{"he said: kys", publish},
		// A quotation left open, as a post typed in a hurry or cut short
		// leaves one, is read as it would be closed, and a stray
		// apostrophe opens nothing. Either once made the filter panic,
		// which refused the post, a crisis post included, with a server
		// error.
		{"why does every thread have one guy going 'add me on snap", publish},
		{"he said 'add me on snap", publish},
		{"'add me on snap", contact},
		{"a 'b' c 'add me on snap", contact},
		{"'ve been thinking about killing myself all evening and I'm not joking", support},
		{"'s continue this on telegram, too many lurkers here", contact},
		// Curly single quotes are read as the straight apostrophe the view
		// folds them to, which quotationAround reads as above: a quotation
		// opens after a space, not inside "I’m".
		{"she said ‘kys’ to me in class", publish},
		{"she said ‘I’m bringing a gun to school tomorrow’ and walked off", publish},
		{"‘kys’", selfHarm},
		{"‘I’m bringing a gun to school tomorrow’ – me", threat},
	} {
		if got := verdictName(bodyVerdict(c.body)); got != c.want {
			t.Errorf("%q: got %s, want %s", c.body, got, c.want)
		}
	}
	if got := plainView("‘’ʼ′", nil).text; got != "''''" {
		t.Errorf("curly single quotes read as %q, want straight apostrophes", got)
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
		// Invisible characters fill no space between the halves.
		{Subject: "School tomorrow", Body: strings.Repeat("\u200b", 70) + "I'm going to shoot everyone"},
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

// TestFilterThreatWindowCountsWhatIsRead pads the space between a threat and
// its time with what a reader does not see, or sees as one gap: invisible
// characters, soft hyphens, combining marks and emoji. The window counts
// letters and digits, and one for each gap, so none of them moves the time
// further away. Two hundred characters of words still do.
func TestFilterThreatWindowCountsWhatIsRead(t *testing.T) {
	for name, pad := range map[string]string{
		"zero-width spaces": strings.Repeat("\u200b", 70),
		"soft hyphens":      strings.Repeat("\u00ad", 101),
		"word joiners":      strings.Repeat("\u2060", 70),
		"combining marks":   strings.Repeat("\u0301", 101),
		"emoji":             strings.Repeat("\U0001F52B", 51),
		"spaces":            strings.Repeat(" ", 150),
	} {
		for _, body := range []string{
			"I'm going to shoot everyone " + pad + " tomorrow",
			"I will kill you " + pad + "tomorrow",
			"school shooting " + pad + " tomorrow",
			"don't come to school tomorrow." + pad + " you've been warned",
		} {
			if v := checkContent(contentInput{Body: body}, true); v.Outcome != contentHold || v.Hold != holdThreat {
				t.Errorf("%s: %q: got %+v, want it held as a threat", name, body, v)
			}
		}
	}
	far := "I'm going to shoot everyone. " + strings.Repeat("blah ", 45) + "tomorrow"
	if v := checkContent(contentInput{Body: far}, true); v.Outcome != contentPublish {
		t.Errorf("a time two hundred characters of words away: got %+v, want it published", v)
	}
}

func TestFilterPrecedence(t *testing.T) {
	// A block comes first, so the poster fixes what they can.
	v := bodyVerdict("i want to die, my snap is madeup_ana23, kys")
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
	// Contact details beside a question that places the reader are the
	// private-contact half of a grooming cue: sending the post back would
	// say which half to take out, so it is held instead, support_seen or not.
	for _, body := range []string{
		"how old are you? add me on snap",
		"which school are you in? my insta is madeup_fox27",
		"kahan rehte ho? insta pe aao",
		"how old are you? call me 9876543210",
	} {
		for _, seen := range []bool{false, true} {
			if v := checkContent(contentInput{Body: body}, seen); v.Outcome != contentHold || v.Hold != holdGrooming {
				t.Fatalf("%q (support_seen %v): got %+v, want it held for grooming", body, seen, v)
			}
		}
	}
	// An ID number is not a contact cue, and is still sent back.
	aadhaar := withCheckDigit(t, "23456789012")
	if v := bodyVerdict("how old are you? my aadhaar is " + aadhaar); v.Outcome != contentBlock || v.Rule != ruleIDNumber {
		t.Fatalf("an ID number beside a grooming question should still be blocked: %+v", v)
	}
}

func TestFilterReadsTheSubjectAndTheShownName(t *testing.T) {
	v := checkContent(contentInput{Subject: "call 9876543210", Body: "hello"}, false)
	if v.Outcome != contentBlock || v.Field != fieldSubject || v.Match != "9876543210" ||
		!strings.Contains(v.Message, "the subject") {
		t.Fatalf("subject: %+v", v)
	}
	v = checkContent(contentInput{Name: "snap: madeup_guy99", Body: "hello"}, false)
	if v.Outcome != contentBlock || v.Field != fieldName || !strings.Contains(v.Message, "the Name field") {
		t.Fatalf("name: %+v", v)
	}
	// Form order: the subject is named before the body.
	v = checkContent(contentInput{Subject: "madeup@gmail.com", Body: "9876543210"}, false)
	if v.Field != fieldSubject {
		t.Fatalf("the subject should be reported first: %+v", v)
	}
	// Holds read every field.
	v = checkContent(contentInput{Subject: "how old are you", Body: "dm me"}, false)
	if v.Outcome != contentHold || v.Hold != holdGrooming {
		t.Fatalf("a cue split across fields: %+v", v)
	}
	// A handle pointed to in words is another field of the post: blocked
	// when that field is filled in, and nothing when it is empty.
	for _, c := range []struct {
		in    contentInput
		match string
	}{
		{contentInput{Name: "madeupjar88", Body: "my sc is my name here with no spaces lol"}, "sc is my name"},
		{contentInput{Subject: "orbital_panda", Body: "check my profile on insta, username is the subject of this post"},
			"insta, username is the subject"},
		{contentInput{Name: "madeup_orbitalpanda", Body: "my email is my name here + @gmail.com"}, "email is my name"},
	} {
		v := checkContent(c.in, false)
		if v.Outcome != contentBlock || v.Rule != ruleContact || v.Field != fieldBody || v.Match != c.match {
			t.Fatalf("%+v: got %+v, want a contact block of %q", c.in, v, c.match)
		}
		empty := contentInput{Body: c.in.Body}
		if v := checkContent(empty, false); v.Outcome != contentPublish {
			t.Fatalf("%+v: got %+v, want it published with the field empty", empty, v)
		}
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
	if v.Outcome != contentBlock || v.Rule != ruleIDNumber || v.Match != "ＡＢ１２３４５６Ｃ" {
		t.Fatalf("a full-width match should be blocked and quoted as written: %+v", v)
	}
}

// TestFilterSaysWhichCopyItRefused pins the occurrence a block carries: how
// many places the page's search (pointAtMatch) finds the quoted text before
// the copy the block is about, so the page selects the refused copy and not
// an earlier one the rules allowed.
func TestFilterSaysWhichCopyItRefused(t *testing.T) {
	for _, c := range []struct {
		body, match string
		occurrence  int
	}{
		{"call 98765 43210", "98765 43210", 0},
		{"[math]98765 43210[/math] is a nice number. call 98765 43210", "98765 43210", 1},
		{"order 9876543210 arrived. dm me at 9876543210", "9876543210", 1},
		{"isbn 9876543210 and call 9876543210 or 9876543210", "9876543210", 1},
		// The page lets any run of whitespace stand for another, and so does
		// the count.
		{"[code]x = 98765 43210[/code]\ncall 98765\n43210", "98765\n43210", 1},
		{"  [math]98765 43210[/math] then call  98765   43210", "98765   43210", 1},
	} {
		v := bodyVerdict(c.body)
		if v.Outcome != contentBlock || v.Match != c.match || v.Occurrence != c.occurrence {
			t.Errorf("%q: got %+v, want %q blocked as copy %d", c.body, v, c.match, c.occurrence)
		}
	}
	// The search starts one character on from the last place it found, so
	// copies that overlap each count; whitespace is JavaScript's \s, wider
	// than Go's; and a cut quote is looked for without its "…".
	for _, c := range []struct {
		src      string
		from     int
		searched string
		want     int
	}{
		{"aaaa aaa", 5, "aaa", 2},
		{"ab\u00a0cd ab cd", 7, "ab cd", 1},
		{"ab\u3000cd ab cd", 8, "ab cd", 1},
		{"x  https://t.me/madeup https://t.me/madeupabc", 23, "https://t.me/madeup", 1},
		{"no copy before", 3, "copy", 0},
		{"   ", 0, "", 0},
	} {
		if got, _ := matchOccurrence(c.src, c.from, c.searched); got != c.want {
			t.Errorf("matchOccurrence(%q, %d, %q) = %d, want %d", c.src, c.from, c.searched, got, c.want)
		}
	}
}

// TestFilterReadsFoldedQuotesAndDashes holds what the views fold to what is
// written straight: a curly apostrophe is read as "'" and every dash as "-"
// (foldRune), so isContracted, contractedIs and studyLabel read "’s" and a
// label's "–" or "—" with no branches of their own.
func TestFilterReadsFoldedQuotesAndDashes(t *testing.T) {
	for _, c := range []struct {
		want            contentOutcome
		straight, other string
	}{
		{contentBlock, "my insta's madeup_riya22", "my insta’s madeup_riya22"},
		{contentBlock, "ig handle's madeup_riya22", "ig handle’s madeup_riya22"},
		{contentPublish, "Nursing exam - why is the lethal dose of insulin so unpredictable?",
			"Nursing exam – why is the lethal dose of insulin so unpredictable?"},
		{contentPublish, "Nursing exam - why is the lethal dose of insulin so unpredictable?",
			"Nursing exam — why is the lethal dose of insulin so unpredictable?"},
		{contentPublish, "For my pharmacology viva - lethal dose of morphine in opioid-naive patients vs tolerant ones",
			"For my pharmacology viva – lethal dose of morphine in opioid-naive patients vs tolerant ones"},
		{contentHold, "pharmacology - lethal dose of paracetamol", "pharmacology — lethal dose of paracetamol"},
		{contentHold, "why bother - whats the lethal dose of paracetamol", "why bother – whats the lethal dose of paracetamol"},
	} {
		for _, body := range []string{c.straight, c.other} {
			if v := checkContent(contentInput{Body: body}, true); v.Outcome != c.want {
				t.Errorf("%q: %+v, want %v", body, v, c.want)
			}
		}
		for _, seen := range []bool{false, true} {
			a, b := checkContent(contentInput{Body: c.straight}, seen), checkContent(contentInput{Body: c.other}, seen)
			if a.Outcome != b.Outcome || a.Rule != b.Rule || a.Hold != b.Hold {
				t.Errorf("%q: %+v, but %q: %+v", c.straight, a, c.other, b)
			}
		}
	}
}

// jsSpaceRun is a run of what \s matches in the page's JavaScript (isJSSpace).
const jsSpaceRun = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`

// pageSearchCount counts copies the way the page finds them (pointAtMatch):
// the quote's words joined by any run of whitespace, searched for again one
// character on from each place found.
func pageSearchCount(src string, from int, searched string) int {
	words := strings.FieldsFunc(searched, isJSSpace)
	if len(words) == 0 {
		return 0
	}
	for i, word := range words {
		words[i] = regexp.QuoteMeta(word)
	}
	search := regexp.MustCompile(strings.Join(words, jsSpaceRun))
	start := len(src) - len(strings.TrimLeftFunc(src[from:], unicode.IsSpace))
	n := 0
	for at := 0; at < start; {
		loc := search.FindStringIndex(src[at:])
		if loc == nil || at+loc[0] >= start {
			break
		}
		n++
		_, size := utf8.DecodeRuneInString(src[at+loc[0]:])
		at += loc[0] + size
	}
	return n
}

// TestFilterCountsCopiesAsThePageSearches holds matchOccurrence, which reads
// the field once, to the count the page's own search gives, on fields made of
// a few words and whitespace of each kind, so that copies overlap, run across
// lines and stand inside longer words.
func TestFilterCountsCopiesAsThePageSearches(t *testing.T) {
	pieces := []string{"a", "a", "b", "ab", "ba", "é", " ", " ", "  ", "\n", "\u00a0", "\u3000", "\u2028"}
	rnd := rand.New(rand.NewSource(1))
	runeStarts := func(s string) []int {
		var at []int
		for i := range s {
			at = append(at, i)
		}
		return append(at, len(s))
	}
	for range 20000 {
		var b strings.Builder
		for range rnd.Intn(24) {
			b.WriteString(pieces[rnd.Intn(len(pieces))])
		}
		src := b.String()
		at := runeStarts(src)
		from := at[rnd.Intn(len(at))]
		i, j := at[rnd.Intn(len(at))], at[rnd.Intn(len(at))]
		searched := src[min(i, j):max(i, j)]
		if rnd.Intn(4) == 0 {
			searched = strings.Repeat(pieces[rnd.Intn(5)]+" ", 1+rnd.Intn(4))
		}
		got, _ := matchOccurrence(src, from, searched)
		if want := pageSearchCount(src, from, searched); got != want {
			t.Fatalf("matchOccurrence(%q, %d, %q) = %d, the page finds %d", src, from, searched, got, want)
		}
	}
}

// TestFilterCountsWhichCopyCost holds the count of copies to a pass over the
// field. A spelled-out address is quoted up to its first 79 characters, so a
// name of forty single letters quotes forty "a"s, and a run of "a"s before it
// holds a copy at nearly every character: searched for again after each one,
// as the page does, that cost 140 ms and more of one core for a post, about
// thirty times the yardstick sentence, none of it counted.
func TestFilterCountsWhichCopyCost(t *testing.T) {
	const mostPerByte = 4
	tail := "\nemail me " + strings.Repeat("a ", 44) + "a @ g m a i l . c o m"
	for _, unit := range []string{"a ", "a  ", "a\n", "a\u00a0"} {
		for _, length := range []int{maxBodyLen / 8, maxBodyLen} {
			runs := (length - utf8.RuneCountInString(tail)) / utf8.RuneCountInString(unit)
			body := strings.Repeat(unit, runs) + tail
			v, total := checkContentCost(contentInput{Body: body}, false)
			// What checkContentCost counts is the search for the block, the
			// question whether the post places the reader, and the count of
			// copies.
			f := scanField(fieldBody, body, markupReadings(body))
			found, ok := f.firstBlock()
			if !ok || v.Outcome != contentBlock {
				t.Fatalf("%q × %d: %+v, want the address blocked", unit, runs, v)
			}
			placesTheReader([]*scannedField{f})
			_, searched := quoteSource(body, found.from, found.to)
			n, work := matchOccurrence(body, found.from, searched)
			if n != v.Occurrence || n < runs-40 {
				t.Errorf("%q × %d: copy %d counted, want the block's own %d, nearly one a run", unit, runs, n, v.Occurrence)
			}
			if work > mostPerByte*len(body) {
				t.Errorf("%q × %d: %d steps to count the copies in %d bytes, more than %d a byte", unit, runs,
					work, len(body), mostPerByte)
			}
			if total != f.work()+work {
				t.Errorf("%q × %d: %d steps counted for the post, want %d searching and %d counting the copies",
					unit, runs, total, f.work(), work)
			}
		}
	}
}

// TestFilterPatternsSurviveCanon pins what canon relies on: the only lower-
// case l in any pattern source is a literal letter, so turning it into an i
// changes the words matched and never the syntax.
func TestFilterPatternsSurviveCanon(t *testing.T) {
	rules := map[string][]rule{
		"askRules": askRules, "supportRules": supportRules, "selfHarmRules": selfHarmRules,
		"selfHarmMethodRules": selfHarmMethodRules, "pillQuestionRules": pillQuestionRules,
		"threatIntentRules": threatIntentRules, "threatPlaceOrTimeRules": threatPlaceOrTimeRules,
		"threatWarningRules": threatWarningRules, "threatWarningCues": threatWarningCues,
		"schoolShootingRules": schoolShootingRules, "shootingTimeRules": shootingTimeRules,
		"ageQuestionRules": ageQuestionRules, "privateCueRules": privateCueRules,
		"myAppRule": {myAppRule}, "appColonHandle": {appColonHandle}, "appAtHandle": {appAtHandle},
		"appLabelHandle": {appLabelHandle}, "spacedInviteRule": {spacedInviteRule},
		"atAskRule": {atAskRule}, "myMailRule": {myMailRule}, "mailColonHandle": {mailColonHandle},
		"hindiHandle": {hindiHandle}, "handleBeforeApp": {handleBeforeApp}, "namedTargetRules": namedTargetRules,
		"myAmbiguousAppRule": {myAmbiguousAppRule}, "appNameRule": {appNameRule}, "offBoardRule": {offBoardRule},
		"discTagHandle": {discTagHandle}, "labelledAppHandle": {labelledAppHandle},
		"threatFrameRules": threatFrameRules, "threatComingRules": threatComingRules,
		"threatSymbolRules": threatSymbolRules, "selfHarmUrgeRules": selfHarmUrgeRules,
		"threatUnseenRules": threatUnseenRules,
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

// TestJoinActsMatchesTheActsOneByOne holds joinActs to what it stands in
// for: the acts listed one by one, longest first where one starts another,
// find the same matches in both views' spelling. Every act is tried alone
// and run on into the act after it, so that one act reading on into the
// next, where their order decides the match, is tried for each.
func TestJoinActsMatchesTheActsOneByOne(t *testing.T) {
	rnd := rand.New(rand.NewSource(20261015))
	for _, ing := range []bool{false, true} {
		var each []string
		for _, a := range threatActList(ing) {
			each = append(each, a.verb+a.rest)
		}
		slices.Sort(each)
		slices.Reverse(each)
		each = slices.Compact(each)
		oneByOne, joined := phrase(words(each...)), phrase(threatActs(ing))
		for _, view := range []func(string) string{func(s string) string { return s }, canon} {
			want, got := regexp.MustCompile(view(oneByOne)), regexp.MustCompile(view(joined))
			var samples []string
			for _, act := range each {
				re, err := syntax.Parse(view(phrase(act)), syntax.Perl)
				if err != nil {
					t.Fatal(err)
				}
				g := &searchSampler{rnd: rnd}
				g.sample(re)
				samples = append(samples, g.b.String())
			}
			for i, s := range samples {
				for _, text := range []string{s, s + " " + samples[(i+1)%len(samples)]} {
					w, g := want.FindAllStringSubmatchIndex(text, -1), got.FindAllStringSubmatchIndex(text, -1)
					if !reflect.DeepEqual(w, g) {
						t.Errorf("ing %v: %q: one by one %v, joined %v", ing, text, w, g)
					}
				}
			}
		}
	}
}

// TestFilterCostIsBounded runs the filter over bodies built to be slow — as
// long as a post can be, made of what the rules look for, and in the subject
// and name as well — and holds what it counts searching them to three bounds.
// It counts work rather than time (checkContentCost: the places the searches
// try, the characters they read, the steps their machines take, and each
// check a match is put to), so the bounds are the same on every machine, and
// under the race detector, and a timer's noise cannot fail or pass it.
//
// The first bound is per byte of the post, at full length: the most any of
// these bodies costs is about 148 steps a byte ("stacked method questions"),
// which is also the most of the 29,397 bodies built to be slow that the
// counts were last checked on, about 8% under the bound; the next most here
// are handles glued to an opening bracket, about 128 (about 198 before
// firstHandle turned them down before their checks), and a pill question run
// into such a handle, about 117. A step is not a fixed time — across those
// of these bodies that take more than a few milliseconds a millisecond is
// anything from about 18,000 steps to about 91,000, and the body that costs
// the most steps is not the slowest — so the bound holds the work linear and
// caps it, and the time is the README's to state, from timing the bodies
// ("Content rules"). The steps are the same with Go 1.22, 1.24 and 1.25,
// since the programs are Go's own compiler's output for the same patterns. A
// body that costs much more than the rest has to be looked at, and the rule
// behind it planned or rewritten, before the bound is raised. The second is
// per character, so that a post of four-byte characters is not given four
// times the steps a character an ASCII one is: the most a character is about
// 193 steps (Snapchat's ghost over and over, each read for an ask or a
// handle round it), about 20% under workPerChar. The third is that eight
// times the text costs at most ten times as much: linear work costs eight
// times, a fixed part a little less, and anything quadratic would cost
// sixty-four.
//
// What is not counted is single passes: the scanners that make the views,
// and the number scanner's own pass over the digits (the cues it looks for in
// the 32 bytes either side of a number are searches, and are counted).
// TestFilterReadsNoCharacterAsAPhrase caps how long a view can get.
func TestFilterCostIsBounded(t *testing.T) {
	const workPerByte, workPerChar = 160, 240
	rep := func(unit string) func(n int) string {
		return func(n int) string { return strings.Repeat(unit, n/utf8.RuneCountInString(unit)) }
	}
	// apart puts the pieces as far apart as the length allows, with spaces.
	apart := func(pieces ...string) func(n int) string {
		return func(n int) string {
			gap := n
			for _, p := range pieces {
				gap -= utf8.RuneCountInString(p)
			}
			return strings.Join(pieces, strings.Repeat(" ", gap/(len(pieces)-1)))
		}
	}
	// then puts tail after a line of the unit's copies, at the end.
	then := func(unit, tail string) func(n int) string {
		return func(n int) string { return rep(unit)(n-utf8.RuneCountInString(tail)-1) + "\n" + tail }
	}
	bodies := map[string]func(n int) string{
		"digits":       rep("9 "),
		"digit groups": rep("98765 "),
		"spaced":       rep("s n a p "),
		"leet":         rep("1 "),
		"at signs":     rep("a@"),
		"dots":         rep("a."),
		"words":        rep("i will kill you at school "),
		"how many":     rep("how many "),
		"my":           rep("my snap "),
		"math":         rep("[math]1[/math]"),
		"wide":         rep("９"),
		"invisible":    rep("\u200b"),
		"keycaps":      rep("9\ufe0f\u20e3"),
		"line breaks":  rep("98765\n"),
		"spoilers":     rep("9[spoiler]8[/spoiler]"),
		"expanding":    rep("\ufdfa1"),
		// One character that folds to as many as a character may, letters and
		// dots both, alone and round the signs an address is read from.
		"folds to four":      rep("\u33c2"),
		"folds round a sign": rep("\u33c2@\u33d8"),
		"two scripts":        rep("1\u044f"),
		"subject pairs":      rep("shooting up the school. "),
		// The openings and checks that cost the most, each over and over:
		// an intent with nothing after it, words run together that read as
		// one, leetspeak, a method question, an address round an at sign, a
		// quoted or named or conditional threat, the excuses checked after
		// a match, and numbers whose neighbours are read for a cue.
		"intents":        rep("i will "),
		"run together":   rep(" imaactually "),
		"handle on app":  rep("python3 on discord\n"),
		"leet threat":    rep("1'm g0nna sh00t up the sch00l "),
		"how many pills": rep("how many pills "),
		"how many to od": rep("how many to od "),
		"at round dots":  rep("@ a.aa "),
		"spelled at":     rep("attherateofa.aa "),
		"quoted":         rep("\"i will kill you\" he said "),
		"named":          rep("I will kill Rahul "),
		"conditional":    rep("u'll die if "),
		"event":          rep("We're going to kill CSK tonight "),
		"wedding":        rep("I'm going to kill Rahul's wedding tomorrow. "),
		"are you on":     rep(" you on insta "),
		"support":        rep("I really want to die. "),
		"dying hair":     rep("i'm thinking about dying my hair blue. "),
		"snap lately":    rep("my snap is horrible lately. "),
		"site":           rep("Q3 at leetcode.com. "),
		"kms":            rep("kms per litre\n"),
		"numbers":        rep("QQ 12 34 56 C "),
		"ranges":         rep("96.123-97.456 percentile\n"),
		"helpline":       rep("Trevor Project 1-866-488-7386. "),
		"scores":         rep("mock scores 65-70-78-84-91\n"),
		// A method question with words between the count and the pills, or
		// after them, each read 60 characters on from both; and with a run
		// of spaces before a window, which is read once, not once a space.
		"words before pills":   rep("how many a b c pills "),
		"words after pills":    rep("how many pills i u a "),
		"two spaces":           rep("how many  pills a b c "),
		"spaces before pills":  rep("how many" + strings.Repeat(" ", 30) + "pills i u a "),
		"spaces before to die": rep("how many" + strings.Repeat(" ", 60) + "to die "),
		"spaces before lethal": rep("what dose" + strings.Repeat(" ", 30) + "a b c lethal "),
		// Digits read out in words, a word before them that may or may not
		// be one, pairs after a request to call, an ID number's label, a
		// bracketed letter, an app with the words before a handle, and the
		// passive and Hinglish threats.
		"spelled digits":   rep("nine eight seven six five "),
		"stray digit word": rep("do nine eight seven six five four three two two one, "),
		"doubles":          rep("double "),
		"called pairs":     rep("call me 98 76 54 32 10\n"),
		"id label":         rep("aadhaar no 2345 "),
		"bracketed letter": rep("i[n]sta "),
		"app then words":   rep("telegram pe aao "),
		"passive":          rep("gets stabbed tomorrow "),
		"hinglish":         rep("usko kal maar dunga "),
		// The threat grammar's openings next to a place or a time, which is
		// when what follows them is read: a speaker and a verb, harm said
		// to be coming, a place said to burn, a day someone won't see, a
		// bomb left or made, a name in small letters, a shooting on a day,
		// and Hinglish pronouns with a time between; and an act urged, and a
		// reader's grade asked about beside a cue.
		"frame by a place":  rep("school i'm going to "),
		"coming harm":       rep("everyone in the lab will get hurt tomorrow "),
		"burning place":     rep("the hostel will burn "),
		"won't see":         rep("the warden won't see monday "),
		"bomb left":         rep("school i'll leave a bomb under the "),
		"bomb made":         rep("i made a bomb at school "),
		"small-letter name": rep("i'll stab rahul at school "),
		"shooting on a day": rep("school shooting on monday "),
		"pronoun then time": rep("kal sabko "),
		"urged":             rep("bro just rope "),
		"grade then cue":    rep("what grade are you in? come alone "),
		// A place lit up, a crowd's last time, and a teacher of the
		// poster's who won't see a day, each read for what is round it.
		"lit up":         rep("light it up "),
		"last time":      rep("will be the last time it ever does "),
		"our sir monday": rep("our sir won't see monday "),
		// The goads' openings, each read for what is round it, and the
		// nudges and finishers read once for the whole post.
		"in your shoes":  rep("if i were you i'd "),
		"favour":         rep("do us a favour if you "),
		"whole supply":   rep("take the whole bottle "),
		"no barrier":     rep("roof no barrier "),
		"knot":           rep("🪢 "),
		"disappear":      rep("then disappear "),
		"too late":       rep("should've jumped "),
		"done it by now": rep("meant done it by now "),
		"hinglish goad":  rep("apne aap ko khatam kar "),
		// The support phrasings read with the post round them.
		"tried to end it":  rep("i tried to end it "),
		"make it to":       rep("don't think i'll make it to "),
		"last post":        rep("this is my last post "),
		"on the roof":      rep("i'm on the roof "),
		"sab khatam":       rep("sab khatam kar dunga "),
		"disclaimed pills": rep("how many pills asking for a friend "),
		// The newer phrasings read with what is round them: a relapse
		// looked for in the post, a height question's sentence read for a
		// death, a calorie count read after "eat less", and "ja mar" read
		// for "gaya".
		"clean from sh":   rep("clean from sh "),
		"height question": rep("how high is the "),
		"eat less":        rep("eat less junk "),
		"ja mar gaya":     rep("ja mar gaya "),
		"end it with him": rep("you should end it with him "),
		// Grooming cues read with the sentence round them: "alone" said
		// last, a secret kept "between us", and "tell no one".
		"alone said last": rep("come here, alone. "),
		"between us":      rep("just between us "),
		"tell no one":     rep("tell no one about "),
		// Contact asks and accounts read outside the usual frames: an
		// address asked for, "instead" ending an ask, an app's messages, a
		// Session ID's hexadecimal and a friend code; and numbers in labelled
		// halves, in spaced pieces and pointed at.
		"email ask":     rep("drop ur email "),
		"instead":       rep("text me instead. "),
		"dms on app":    rep("take this to dms on insta "),
		"hex":           rep("05"),
		"friend code":   rep("friend code 1 "),
		"halves":        rep("first 5: 12 "),
		"spaced pieces": rep("1 2 / "),
		"arrow":         rep("98765 43210 <- "),
		// Calls: meeting links with nothing after them, and a meeting ID's
		// opening.
		"meeting links": rep("zoom.us/j/ meet.google.com/ teams.microsoft.com/l/ "),
		"meeting id":    rep("zoom meeting id 845 "),
		// A threat told by implication: hints of every kind, close to a
		// time and a place, and the words that open the most of them.
		"hints":         rep("he's done. you know who you are. nobody walks out of the gym alive friday. "),
		"hint openings": rep("i will be not the one "),
		// An app named over and over, each time read for an ask or a handle
		// on either side; a crisis line quoted from the reply above, said of
		// someone else, or put as a conditional; an address spelled out round
		// an at sign or with a bracketed one; and numbers split by words,
		// slashes, quotes and lookalike letters, given backwards, or after
		// their area code.
		"app alone":          rep("ig "),
		"app then a word":    rep("ig there "),
		"app emoji":          rep("👻 "),
		"quoted crisis":      rep(">>1\n>i want to die\n"),
		"said crisis":        rep("she said \"i want to die\" "),
		"for me to die":      rep("for me to die "),
		"conditional crisis": rep("if i died nobody would care "),
		"spaced address":     rep("a b @ c d "),
		"bracketed at":       rep("[at] gmail "),
		"provider label":     rep("@ gmail prefix is "),
		"joined pieces":      rep("my num 9 then "),
		"quoted pieces":      rep("'9' + "),
		"slashed pieces":     rep("98 / 1 / "),
		"lookalike digits":   rep("8 l 2 O "),
		"reversed":           rep("my number in reverse 0 1 2 3 4 5 6 7 8 1 "),
		"area code after":    rep("482-0196 area code 494 "),
		// Long runs of spaces between the places a run of letters or spaces
		// can end, each read once rather than once from every place in it:
		// what an address's name or a spelled-out address can end at, and
		// what an arrow can point back over.
		"addresses far apart": apart("@a.aa", "@a.aa", "@a.aa"),
		"spaces far apart":    apart("at", "at", "at"),
		"arrows far apart":    apart("x", "->", "->"),
		// Weapon emoji, in a run and spaced, which all stand at one reading
		// position; and a pill question next to a handle pointed at an app,
		// whose medicine never ends a word. The two weapon bodies guard the
		// count rather than reproduce what they were added for: before the
		// hints were filed by kind, pairing them was quadratic and counted as
		// nothing, which no bound on the count can see, and these passed;
		// TestFilterCountsMenacePairing holds the pairing to the count, and
		// these hold the count to the bound.
		"knives":             rep("🔪"),
		"spaced guns":        rep("🔫 "),
		"pills and a handle": rep("howmany@pills_(snap "),
		"pills run on":       rep("howmany@pills/how'many pills_(snap pills_(snap "),
		// Handles glued to an opening bracket and pointed at an app, which
		// every handle rule finds, in both views, and turns down by the
		// bracket.
		"handles to brackets": rep("howmany(snap @s_(snap @9s_(snap @9_(snap @9_(snap @9_(snap @9_(snap pills(snap "),
		"handle to a bracket": rep("@s_(ig "),
		// Method questions one after another, each read on past the next,
		// and then an app's name with a bracketed letter in it, read for an
		// ask or a handle on either side: the most, a byte, of the bodies
		// the counts were last checked on.
		"stacked method questions": rep("how many to od how many to od how many to od i[n]sta "),
		// Snapchat's ghost alone and before an app's short name, each name
		// read for an ask or a handle on either side, and each ghost four
		// bytes: the most a character of these bodies costs.
		"ghosts":        rep("👻"),
		"ghost then sc": rep("👻Sc"),
		"ghost then wa": rep("👻wa"),
		// The newer asks, each read with the words round it: an app made a
		// verb after a lead, everyone's numbers with the post round them
		// read for a group, a group set up on an app, talking privately or
		// going private, a call suggested, the Hinglish "<app> karo", an
		// account asked about without its question mark, a Meet code after
		// Meet's name, reddit's u/ and a LinkedIn profile's name; and a bare
		// "dm" after an age question.
		"app me after a lead": rep("lonely girls whatsapp me "),
		"numbers asked":       rep("drop your numbers "),
		"group on an app":     rep("make a gc on snap "),
		"privately":           rep("can we talk privately "),
		"go private":          rep("lets go private "),
		"call suggested":      rep("lets facetime "),
		"app karo":            rep("mujhe whatsapp karo "),
		"account asked":       rep("do u have telegram "),
		"meet code":           rep("gmeet abc-defg-hij "),
		"reddit u":            rep("reddit u/ab "),
		"linkedin name":       rep("linkedin: a-b-12345 "),
		"bare dm":             rep("age? dm. "),
		"slash area code":     rep("phone 495/418-3927 "),
		// Asks named or taken back, each read on for what follows it.
		"ask named":      rep("snap me is what she said "),
		"ask taken back": rep("snap me jk post it here "),
		// An address spelled out after a line of the letter its name is
		// made of, quoted forty letters long, so that nearly every letter
		// before it starts a copy of the quote, and the copy refused is
		// counted among them (matchOccurrence).
		"copies before an address": then("a ", "email me "+strings.Repeat("a ", 44)+"a @ g m a i l . c o m"),
		"copies two spaces apart":  then("a  ", "email me "+strings.Repeat("a ", 44)+"a @ g m a i l . c o m"),
	}
	// The more either verdict costs, since a post that was shown the support
	// message runs on to the holds.
	cost := func(body string) (work, size, chars int) {
		in := contentInput{Subject: truncateRunes(body, maxSubjectLen), Name: truncateRunes(body, maxNameLen), Body: body}
		for _, supportSeen := range []bool{false, true} {
			_, w := checkContentCost(in, supportSeen)
			work = max(work, w)
		}
		return work, len(in.Subject) + len(in.Name) + len(in.Body),
			utf8.RuneCountInString(in.Subject) + utf8.RuneCountInString(in.Name) + utf8.RuneCountInString(in.Body)
	}
	for name, build := range bodies {
		small, _, _ := cost(build(maxBodyLen / 8))
		full, size, chars := cost(build(maxBodyLen))
		if full > workPerByte*size {
			t.Errorf("%s: %d steps for %d bytes, %.0f a byte, more than %d", name, full, size,
				float64(full)/float64(size), workPerByte)
		}
		if full > workPerChar*chars {
			t.Errorf("%s: %d steps for %d characters, %.0f a character, more than %d", name, full, chars,
				float64(full)/float64(chars), workPerChar)
		}
		if full > 10*small {
			t.Errorf("%s: %d steps at full length against %d at an eighth; more than linear", name, full, small)
		}
	}
	// Every character of ordinary words is read at least once, so a count
	// below the length means the count has stopped counting and the bounds
	// above hold of nothing.
	if full, size, _ := cost(bodies["words"](maxBodyLen)); full < size {
		t.Errorf("%d steps counted for %d bytes of words", full, size)
	}
}

// TestFilterNumbersAreReadForAnAnswerLast holds posts of numbers that are not
// contact details to about what the numbers cost to read without asking
// whether each is a maths answer. mathsAnswer reads 64 bytes either side of a
// number for the words an answer is given in, and was asked of every number
// that stood alone, before the number was found to be of any kind: posts of
// helpline numbers, ranges and scores cost from 23 to 32 steps a byte, nearly
// twice as many as before it was added, and from 1.6 to 1.8 times the time,
// which TestFilterCostIsBounded's bound of 160 a byte cannot see. Asked last,
// of a number of a kind a contact detail is, it costs these posts nothing:
// they cost from 13 to 19 steps a byte.
func TestFilterNumbersAreReadForAnAnswerLast(t *testing.T) {
	const mostPerByte = 22
	for name, unit := range map[string]string{
		"helpline": "Trevor Project 1-866-488-7386. ",
		"ranges":   "96.123-97.456 percentile\n",
		"scores":   "mock scores 65-70-78-84-91\n",
	} {
		in := contentInput{Body: strings.Repeat(unit, maxBodyLen/len(unit))}
		for _, supportSeen := range []bool{false, true} {
			verdict, work := checkContentCost(in, supportSeen)
			if verdict.Outcome != contentPublish {
				t.Fatalf("%s: a post of them was not published: %+v", name, verdict)
			}
			if work > mostPerByte*len(in.Body) {
				t.Errorf("%s: %d steps for %d bytes, %.1f a byte, more than %d", name, work, len(in.Body),
					float64(work)/float64(len(in.Body)), mostPerByte)
			}
		}
	}
}

// TestAppFramesReuseTheirWords holds appFrame to the words it keeps from one
// app's name to the next. A post can name an app in every other word, and
// Snapchat's ghost before a short name ("👻Sc" over and over) is a name every
// two characters: reading each frame's words into new slices took about 21
// allocations a name and about twice the time, which TestFilterCostIsBounded
// cannot see, since what is read, and counted, is the same. Under the race
// detector sync.Pool drops some of what it is given back, and the searches'
// machines are made anew, about 6 a name.
func TestAppFramesReuseTheirWords(t *testing.T) {
	body := strings.Repeat("👻Sc", maxBodyLen/3)
	f := scanField("body", body, nil)
	names := 0
	appNameRule.loose.each(&f.loose, func(m []int) bool { names++; return true })
	if names != 2*(maxBodyLen/3) {
		t.Fatalf("%d app names found in %d ghosts and names, want both", names, maxBodyLen/3)
	}
	scanning := testing.AllocsPerRun(3, func() { f = scanField("body", body, nil) })
	framing := testing.AllocsPerRun(3, func() {
		f = scanField("body", body, nil)
		if _, _, _, ok := f.firstAppFrame(); ok {
			t.Fatal("a frame read in a post of ghosts and names")
		}
	}) - scanning
	if framing > 10*float64(names) {
		t.Errorf("reading the frames round %d app names allocated %.0f times, %.1f a name, more than 10",
			names, framing, framing/float64(names))
	}
}

// TestFilterCountsMenacePairing holds menace's pairing of hints to the count
// TestFilterCostIsBounded bounds. Spaced gun emoji are each a hint and all
// stand at one reading position, so each is near all the others: pairing them
// by looking at every hint near each one is quadratic, and before the hints
// were filed by kind and searched by halving it was also counted as nothing,
// so the step count could not see the 85 ms of CPU 8,000 bytes of them took.
// The count has to be the work done, not an estimate of it: a step for each
// kind a hint is filed under, and for each search a step for each halving it
// takes and one to read what it found. The guns are all of one kind, so the
// search of every other kind's list finds it empty, and the guns' own list
// is halved to its end, since every gun starts where the others do; an
// estimate that charged every search the halvings of the longest list (as
// one did) counts several times that. And eight times the hints cost at most
// sixteen times the steps (a search by halving costs a few more steps a hint
// as the hints grow; looking at every pair costs sixty-four times).
func TestFilterCountsMenacePairing(t *testing.T) {
	noPlaces := func() nearSpans { return nearSpans{} }
	pairing := func(guns int) (hints, steps int) {
		f := scanField(fieldBody, strings.Repeat("🔫 ", guns), nil)
		// The first reading builds the words and their positions, which
		// later ones reuse; the second costs what menace's own reading does.
		f.menaceHints()
		before := f.plain.work
		hints = len(f.menaceHints())
		reading := f.plain.work - before
		before = f.plain.work
		if menace([]*scannedField{f}, noPlaces) {
			t.Fatalf("%d spaced guns read as a threat with no place or time", guns)
		}
		return hints, f.plain.work - before - reading
	}
	// halvings is how many times a search halves a list of n that it ends
	// past the end of.
	halvings := func(n int) int {
		k := 0
		for lo, hi := 0, n; lo < hi; k++ {
			lo = int(uint(lo+hi)>>1) + 1
		}
		return k
	}
	smallHints, small := pairing(500)
	hints, full := pairing(4000)
	if smallHints != 500 || hints != 4000 {
		t.Fatalf("%d and %d hints from 500 and 4,000 spaced guns, want one a gun", smallHints, hints)
	}
	for _, c := range []struct{ hints, steps int }{{smallHints, small}, {hints, full}} {
		filing := c.hints * (numMenaceKinds + 1)
		searching := c.hints * (numMenaceKinds + halvings(c.hints))
		if c.steps != filing+searching {
			t.Errorf("pairing %d guns counted %d steps, want %d: %d to file them and %d to search, each "+
				"of the other kinds' lists empty and the guns' halved %d times", c.hints, c.steps,
				filing+searching, filing, searching, halvings(c.hints))
		}
	}
	if full > 16*small {
		t.Errorf("pairing 4,000 hints counted %d steps against %d for 500; more than linear", full, small)
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

// TestFilterPlansTakeEveryMethodActsPill shows support for every pill of
// pillObjects taken all at once, said as methodActs reads it ("i will take
// all my …") and as supportRuns' plans do ("i'm going to take all of my …",
// "gonna swallow all my sleeping …"), so the two lists cannot drift apart.
func TestFilterPlansTakeEveryMethodActsPill(t *testing.T) {
	for _, pill := range pillObjects {
		for _, body := range []string{
			"i will take all my " + pill + " tonight",
			"i'm going to take all my " + pill + " tonight",
			"going to take all of my " + pill + " tonight. bye",
			"im gonna swallow all my sleeping " + pill,
		} {
			if v := checkContent(contentInput{Body: normalizeBody(body)}, false); v.Outcome != contentSupport {
				t.Errorf("%q: got %s (%s %q), want support", body, corpusOutcomes[v.Outcome], v.Rule, v.Match)
			}
		}
		for _, body := range []string{
			"i'm going to take all my " + pill + " before breakfast, doctor said",
			"i'm going to take all my " + pill + " with me on the trip",
		} {
			if v := checkContent(contentInput{Body: normalizeBody(body)}, false); v.Outcome != contentPublish {
				t.Errorf("%q: got %s (%s %q), want publish", body, corpusOutcomes[v.Outcome], v.Rule, v.Match)
			}
		}
	}
}

// corpusItem is one labelled post in testdata/filter_corpus: what was posted,
// and what the filter should do with it.
type corpusItem struct {
	ID      string `json:"id"`
	Body    string `json:"body"`
	Subject string `json:"subject"`
	Name    string `json:"name"`
	// Expect is publish, block, support or hold. A hold is met by a post
	// that is shown the support message first and held once that has been
	// seen, as a method question is.
	Expect string `json:"expect"`
	// Why says what the post is, for whoever reads a failure.
	Why string `json:"why"`
	// KnownMiss marks a post the filter is known to get wrong, and Note says
	// why that is accepted rather than fixed.
	KnownMiss bool   `json:"known_miss,omitempty"`
	Note      string `json:"note,omitempty"`
}

var corpusOutcomes = map[contentOutcome]string{
	contentPublish: "publish", contentBlock: "block", contentSupport: "support", contentHold: "hold",
}

// TestFilterCorpus runs every labelled post in testdata/filter_corpus — one
// file per category, one JSON object per line, all of it made-up text —
// through the filter as the handlers do, and fails on any whose outcome is
// not its label, unless it is marked as a known miss with a note saying why
// the miss is accepted. A known miss that the filter now gets right fails too,
// so the marks stay true. It logs, per category, how many ordinary posts were
// stopped and how much of each kind of post was caught.
func TestFilterCorpus(t *testing.T) {
	dir := filepath.Join("testdata", "filter_corpus")
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no corpus files in %s: %v", dir, err)
	}
	type tally struct{ n, ok int }
	type rates struct {
		items, misses                 int
		publish, block, support, hold tally
	}
	all := rates{}
	seen := map[string]bool{}
	for _, path := range paths {
		category := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		r := rates{}
		for i, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			var it corpusItem
			dec := json.NewDecoder(strings.NewReader(line))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&it); err != nil {
				t.Fatalf("%s:%d: %v", path, i+1, err)
			}
			switch {
			case it.ID == "" || seen[it.ID]:
				t.Fatalf("%s:%d: missing or repeated id %q", path, i+1, it.ID)
			case it.KnownMiss != (it.Note != ""):
				t.Fatalf("%s: a known miss needs a note saying why, and only a known miss has one", it.ID)
			}
			seen[it.ID] = true
			in := contentInput{Subject: normalizeSubject(it.Subject), Name: displayedName(it.Name), Body: normalizeBody(it.Body)}
			v := checkContent(in, false)
			got := corpusOutcomes[v.Outcome]
			right := got == it.Expect
			if v.Outcome == contentSupport {
				after := checkContent(in, true)
				got += " then " + corpusOutcomes[after.Outcome]
				right = right || it.Expect == "hold" && after.Outcome == contentHold
			}
			var tl *tally
			switch it.Expect {
			case "publish":
				tl = &r.publish
			case "block":
				tl = &r.block
			case "support":
				tl = &r.support
			case "hold":
				tl = &r.hold
			default:
				t.Fatalf("%s: unknown label %q", it.ID, it.Expect)
			}
			tl.n++
			r.items++
			if right {
				tl.ok++
			}
			switch {
			case it.KnownMiss && right:
				t.Errorf("%s: a known miss that now comes out %s, as labelled: take known_miss and its note off", it.ID, got)
			case it.KnownMiss:
				r.misses++
			case !right:
				t.Errorf("%s (%s): want %s, got %s (%s %s)\n\t%s %s %q", it.ID, category, it.Expect, got,
					v.Rule, v.Hold, it.Why, v.Field, v.Match)
			}
		}
		logCorpusRates(t, category, r.items, r.misses, r.publish.n-r.publish.ok, r.publish.n, r.block.ok, r.block.n,
			r.support.ok, r.support.n, r.hold.ok, r.hold.n)
		all.items += r.items
		all.misses += r.misses
		for _, p := range [][2]*tally{{&all.publish, &r.publish}, {&all.block, &r.block}, {&all.support, &r.support},
			{&all.hold, &r.hold}} {
			p[0].n += p[1].n
			p[0].ok += p[1].ok
		}
	}
	logCorpusRates(t, "all", all.items, all.misses, all.publish.n-all.publish.ok, all.publish.n, all.block.ok,
		all.block.n, all.support.ok, all.support.n, all.hold.ok, all.hold.n)
}

func logCorpusRates(t *testing.T, category string, items, misses, stopped, publish, blocked, block, supported, support,
	held, hold int) {
	rate := func(a, b int) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%d/%d (%.1f%%)", a, b, 100*float64(a)/float64(b))
	}
	t.Logf("%-8s %3d items, %2d known misses: ordinary posts stopped %s, contact details blocked %s, "+
		"support shown %s, risky posts held %s", category, items, misses, rate(stopped, publish), rate(blocked, block),
		rate(supported, support), rate(held, hold))
}

// TestFilterCorpusDetailsAreMadeUp keeps anybody's real details out of the
// corpus and out of this package's tests. Every address, username and profile
// in them that is anyone's says "madeup", in the letters of the post, so an
// address spelled out a letter at a time or split by markup still does, or is
// at a domain RFC 2606 reserves, so none of them can be a real person's. A
// number cannot be marked like that, since every range set aside for drama
// passes the filter, so a number meant to be blocked is ordinary digits given
// with no name, school or handle; but a North American one can be put in an
// area code the numbering plan never assigns (N9X, 37X and 96X) and still be
// blocked, so it is, and this test holds the North American numbers to that.
// An Aadhaar number its label alone gets blocked fails its check digit, which
// no issued number does, a roll, application or student number the filter
// publishes has zeroes for its serial, and a Steam friend code is past what 32
// bits hold, and this test holds the corpus to those too.
func TestFilterCorpusDetailsAreMadeUp(t *testing.T) {
	// Blocked as a username, but a code that names no one.
	codes := map[string]string{
		"c2-7":   "a Discord invite code",
		"c2-29":  "a Session ID, random hex",
		"c2-122": "a Steam friend code, digits",
		// Game IDs: a Steam ID past what 32 bits hold, and IDs of all zeroes.
		"contact-329": "a Steam ID, digits",
		"contact-330": "a BGMI ID, zeroes",
		"contact-331": "a Free Fire UID, zeroes",
	}
	// A Steam friend code is an account's 32-bit number, so one past what 32
	// bits hold is no one's account; and one the filter does not read as a
	// phone number on its own is no one's phone ("9412877305" is an Indian
	// mobile's shape).
	steamCode := regexp.MustCompile(`(?i)\bsteam\b[^\d]{0,24}\b(\d{6,12})\b`)
	someonesSteamCode := func(code string) string {
		if n, err := strconv.ParseUint(code, 10, 64); err == nil && n <= math.MaxUint32 {
			return "fits in 32 bits, so it may be someone's account"
		}
		if verdictName(checkContent(contentInput{Body: code}, false)) != "publish" {
			return "reads as a phone number on its own, so it may be someone's phone"
		}
		return ""
	}
	madeUp := func(s string) bool {
		letters := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) {
				return unicode.ToLower(r)
			}
			return -1
		}, s)
		return strings.Contains(letters, "madeup")
	}
	email := regexp.MustCompile(`[\pL\pN._%+-]+@[\pL\pN-]+(?:\.[\pL\pN-]+)+`)
	// An address is fine at a reserved domain, as git's user on a forge, at a
	// version ("react@18.3.1") or an IP address, or as the tail of a malformed
	// one a test refuses ("ana@a@iitb.ac.in").
	fineAddress := func(text string, at []int) bool {
		addr := text[at[0]:at[1]]
		local, domain, _ := strings.Cut(addr, "@")
		return madeUp(addr) || exampleDomain(strings.ToLower(domain)) || local == "git" ||
			!strings.ContainsFunc(domain, unicode.IsLetter) || at[0] > 0 && text[at[0]-1] == '@'
	}
	handle := regexp.MustCompile(`(?:^|[^\pL\pN_.@])@([\pL\pN_][\pL\pN_.]*)`)
	profile := regexp.MustCompile(`(?i)\b(?:instagram\.com|snapchat\.com|linktr\.ee|linkedin\.com|t\.me|telegram\.me|` +
		`tiktok\.com|twitter\.com|x\.com|reddit\.com|facebook\.com|messenger\.com|zoom\.us)` +
		`\s*(?:/|\s(?:forward\s+)?slash\s)\s*(\S*)`)
	// A group's invite and a post's address are codes, not anybody's name.
	notAProfile := func(path string) bool {
		for _, p := range []string{"+", "joinchat", "p/"} {
			if strings.HasPrefix(path, p) {
				return true
			}
		}
		return false
	}

	// A North American number, in digits and laid out the North American way
	// (after a 1, with its area code in brackets, or with dashes or points),
	// or in a post meant to be blocked laid out in threes and a four at all,
	// is in an area code never assigned, in 555-0100 to 555-0199, which are
	// set aside for fiction, or toll free, a public line's.
	nanpNumber := regexp.MustCompile(`(?:^|[^\d+])(\+?1[-. ]?)?(\(\s*[2-9]\d\d\s*\)|[2-9]\d\d)([-.\s−]{1,8})` +
		`([2-9OoIl][\dOoIl]{2})([-.\s−]{1,8})([\dOoIl]{4})(?:\D|$)`)
	countryCode := regexp.MustCompile(`\+\d{1,3}[-. ]?$`)
	lookalike := strings.NewReplacer("O", "0", "o", "0", "I", "1", "l", "1")
	tollFree := setOf(`800 833 844 855 866 877 888`)
	numbers := 0
	// An Aadhaar number with its label straight before it ("my aadhaar",
	// "UID", "aadhaar no") is blocked by the label alone, so it fails its
	// check digit, which no issued number does.
	aadhaarNumber := regexp.MustCompile(`(?:^|[^\d])([2-9]\d{3}[ -]?\d{4}[ -]?\d{4})(?:[^\d]|$)`)
	aadhaars := 0
	// An exam roll, application, registration or student number the filter
	// should publish has zeroes for its serial, so it is no one's: after its
	// label, a plural one ("roll numbers") and "is", "was" or "hai" between
	// ("my roll number is 21CS0000"), and both ends of a range ("22CS1000–
	// 22CS2000").
	studentNumber := regexp.MustCompile(`(?i)\b(?:roll|application|registration|reg|enrol?ment|enrollment|student|` +
		`hall ticket|admit card)s?(?:[\s.:#-]+(?:nos?|numbers?|nums?|ids?|cards?|is|was|are|were|hai|h))*` +
		`[\s.:#-]+(\d[\dA-Z]{4,19})\b(?:[ \t]*(?:[-–—]|to|se)[ \t]*(\d[\dA-Z]{4,19})\b)?`)
	serials := 0
	labelledAadhaars := func(where, text string) {
		lower := strings.ToLower(text)
		for _, m := range aadhaarNumber.FindAllStringSubmatchIndex(lower, -1) {
			labelled := false
			for _, w := range wordsBefore(lower, m[2], 5) {
				if idFillers[w] {
					continue
				}
				labelled = aadhaarLabels[w]
				break
			}
			if !labelled {
				continue
			}
			aadhaars++
			if digits := strings.NewReplacer(" ", "", "-", "").Replace(lower[m[2]:m[3]]); verhoeffValid(digits) {
				t.Errorf("%s: the labelled Aadhaar number %q passes its check digit", where, text[m[2]:m[3]])
			}
		}
	}
	noOnesNumbers := func(where, text string, spaced bool) {
		for _, m := range nanpNumber.FindAllStringSubmatchIndex(text, -1) {
			group := func(k int) string {
				if m[2*k] < 0 {
					return ""
				}
				return text[m[2*k]:m[2*k+1]]
			}
			start := m[4]
			if m[2] >= 0 {
				start = m[2]
			}
			area := strings.Trim(group(2), "() \t\n\r")
			laidOut := group(1) != "" || strings.HasPrefix(group(2), "(") ||
				strings.ContainsAny(group(3)+group(5), "-.−") || spaced
			if countryCode.MatchString(text[:start]) || !laidOut {
				continue
			}
			numbers++
			exchange, line := lookalike.Replace(group(4)), lookalike.Replace(group(6))
			if area[1] != '9' && area[:2] != "37" && area[:2] != "96" && !tollFree[area] &&
				!(exchange == "555" && strings.HasPrefix(line, "01")) {
				t.Errorf("%s: the North American number %q is in an area code that is assigned", where,
					strings.TrimSpace(text[start:m[13]]))
			}
		}
	}

	paths, _ := filepath.Glob(filepath.Join("testdata", "filter_corpus", "*.jsonl"))
	if len(paths) == 0 {
		t.Fatal("no corpus files")
	}
	checked := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			var it corpusItem
			if err := json.Unmarshal([]byte(line), &it); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			post := it.Name + "\n" + it.Subject + "\n" + it.Body
			for _, m := range steamCode.FindAllStringSubmatch(post, -1) {
				if why := someonesSteamCode(m[1]); why != "" {
					t.Errorf("%s: the Steam account number %s %s", it.ID, m[1], why)
				}
			}
			for _, field := range []string{it.Name, it.Subject, it.Body} {
				noOnesNumbers(it.ID, field, it.Expect != "publish")
				labelledAadhaars(it.ID, field)
			}
			in := contentInput{Subject: normalizeSubject(it.Subject), Name: displayedName(it.Name),
				Body: normalizeBody(it.Body)}
			if v := checkContent(in, false); v.Outcome == contentBlock && codes[it.ID] == "" &&
				(v.Message == blockMessage(kindEmail, v.Field) || v.Message == blockMessage(kindHandle, v.Field)) {
				checked++
				if !madeUp(post) {
					t.Errorf("%s: the address or username %q is not marked made up", it.ID, v.Match)
				}
			}
			for _, text := range []string{post, it.Why, it.Note} {
				for _, at := range email.FindAllStringIndex(text, -1) {
					if !fineAddress(text, at) {
						t.Errorf("%s: the address %q is not marked made up", it.ID, text[at[0]:at[1]])
					}
				}
				for _, m := range profile.FindAllStringSubmatch(text, -1) {
					if !madeUp(m[1]) && !notAProfile(m[1]) {
						t.Errorf("%s: the profile %q is not marked made up", it.ID, m[0])
					}
				}
			}
			if it.Expect == "publish" {
				for _, m := range studentNumber.FindAllStringSubmatch(post, -1) {
					for _, number := range m[1:] {
						if number == "" {
							continue
						}
						serials++
						if !strings.HasSuffix(strings.TrimRight(number, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"),
							"000") {
							t.Errorf("%s: the published number %q has a serial that is not zeroes", it.ID, number)
						}
					}
				}
				continue // a decorator in code, "@staticmethod", is no one's
			}
			for _, m := range handle.FindAllStringSubmatch(post, -1) {
				provider, _, _ := strings.Cut(strings.ToLower(m[1]), ".")
				if !madeUp(m[1]) && !mailProviders[provider] {
					t.Errorf("%s: the username %q is not marked made up", it.ID, "@"+m[1])
				}
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d posts blocked for an address or a username, want the corpus's hundred and more", checked)
	}

	tests, _ := filepath.Glob("*_test.go")
	for _, path := range tests {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, at := range email.FindAllStringIndex(line, -1) {
				if !fineAddress(line, at) {
					t.Errorf("%s:%d: the address %q is not marked made up", path, i+1, line[at[0]:at[1]])
				}
			}
			noOnesNumbers(fmt.Sprintf("%s:%d", path, i+1), line, false)
			for _, m := range steamCode.FindAllStringSubmatch(line, -1) {
				if why := someonesSteamCode(m[1]); why != "" {
					t.Errorf("%s:%d: the Steam account number %s %s", path, i+1, m[1], why)
				}
			}
		}
	}
	// The README, AGENTS.md and the filter's own comments quote examples of
	// what it reads, and are held to the same where an example is the kind
	// the filter blocks: an address spelled out a character at a time ("m a d
	// e u p . 0 9 @ g m a i l . c o m"), closed up and read as the corpus's
	// are; ten digits run together straight after the name of a line, which
	// the filter reads as a North American number ("cell 2954820174"), in an
	// area code never assigned; a Steam friend code as the corpus's are; and
	// a quoted example the filter blocks as an address or a username, which
	// says "madeup" or stands in for the name with "x", "x_y", "name" or
	// "you" ("snap: x_y_z", "disc: name#1234", and the verification
	// examples' "you" at an institution's domain, which names no one).
	docs := []string{filepath.Join("..", "..", "README.md"), filepath.Join("..", "..", "AGENTS.md")}
	sources, _ := filepath.Glob("filter*.go")
	for _, path := range sources {
		if !strings.HasSuffix(path, "_test.go") {
			docs = append(docs, path)
		}
	}
	spelledOut := regexp.MustCompile(`(?:[\pL\pN._] ){2,}@(?: [\pL\pN._])+`)
	lineThenDigits := regexp.MustCompile(`(?i)\b(?:cell|phone|ph|mobile|mob|tel|voice|my number is)\W{0,3}([2-9]\d{9})\b`)
	quotedExample := regexp.MustCompile(`"([^"\n]{3,200})"`)
	placeholder := regexp.MustCompile(`(?i)(?:^|[^\pL\pN_.])(?:@|u/)?(?:[xyz](?:[._][xyz])*|name\d*|you)` +
		`(?:#(?:\d{4}|tag|ind1))?(?:\.bsky\.social)?(?:[^\pL\pN_.]|$)`)
	notAName := map[string]string{
		"insta id python3": "a tool's name, which the label before it makes a handle",
		"my snap is ...":   "an ellipsis where the handle goes",
	}
	// A friend code of zeroes ("friend code SW-0000-0000-0000") is no one's.
	zeroedCode := func(s string) bool {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, s)
		return len(digits) >= 8 && strings.Trim(digits, "0") == ""
	}
	docNumbers, docNames := 0, 0
	for _, path := range docs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			where := fmt.Sprintf("%s:%d", filepath.Base(path), i+1)
			for _, spelled := range spelledOut.FindAllString(line, -1) {
				closed := strings.ReplaceAll(spelled, " ", "")
				if at := email.FindStringIndex(closed); at != nil && !fineAddress(closed, at) {
					t.Errorf("%s: the spelled-out address %q is not marked made up", where, spelled)
				}
			}
			for _, m := range lineThenDigits.FindAllStringSubmatch(line, -1) {
				docNumbers++
				if area := m[1][:3]; nanp(m[1]) && area[1] != '9' && area[:2] != "37" && area[:2] != "96" &&
					!tollFree[area] {
					t.Errorf("%s: the North American number %s is in an area code that is assigned", where, m[1])
				}
			}
			for _, m := range steamCode.FindAllStringSubmatch(line, -1) {
				if why := someonesSteamCode(m[1]); why != "" {
					t.Errorf("%s: the Steam account number %s %s", where, m[1], why)
				}
			}
			if strings.HasSuffix(path, ".go") && !strings.Contains(line, "//") {
				continue // a string in the code, not an example in a comment
			}
			for _, q := range quotedExample.FindAllStringSubmatch(line, -1) {
				v := checkContent(contentInput{Body: q[1]}, false)
				if v.Outcome != contentBlock || v.Message != blockMessage(kindEmail, v.Field) &&
					v.Message != blockMessage(kindHandle, v.Field) {
					continue
				}
				docNames++
				if !madeUp(q[1]) && !placeholder.MatchString(q[1]) && notAName[q[1]] == "" &&
					!steamCode.MatchString(q[1]) && !zeroedCode(q[1]) {
					t.Errorf("%s: the address or username in %q is not marked made up", where, q[1])
				}
			}
		}
	}
	if docNumbers < 3 {
		t.Fatalf("only %d numbers after the name of a line found in the docs and comments, want three and more",
			docNumbers)
	}
	if docNames < 40 {
		t.Fatalf("only %d addresses and usernames found in the docs and comments, want forty and more", docNames)
	}
	if numbers < 40 {
		t.Fatalf("only %d North American numbers found, want the corpus's and the tests' forty and more", numbers)
	}
	if aadhaars < 10 {
		t.Fatalf("only %d labelled Aadhaar numbers found, want the corpus's ten and more", aadhaars)
	}
	if serials < 20 {
		t.Fatalf("only %d published roll, application and student numbers found, want the corpus's twenty and more",
			serials)
	}
}

// BenchmarkCheckContent checks ordinary posts of the maximum length: one
// sentence over and over, and the corpus's published posts run together and
// cut into pieces of that length (ordinaryPieces), a different piece each
// time. Varied text gives the rules more to read than one sentence does, so
// "corpus" is the figure for an ordinary post; the README's timings are the
// process's CPU time on one core, the best of several runs of each piece.
func BenchmarkCheckContent(b *testing.B) {
	prepareSearches() // planned once, as the server does as it starts, not timed
	b.Run("sentence", func(b *testing.B) {
		body := strings.Repeat("Is the answer 42? I will check my notes tomorrow at school. ", maxBodyLen/62)
		for i := 0; i < b.N; i++ {
			checkContent(contentInput{Subject: "doubt", Name: "anon", Body: body}, false)
		}
	})
	b.Run("corpus", func(b *testing.B) {
		pieces := ordinaryPieces(b, maxBodyLen)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			checkContent(contentInput{Subject: "doubt", Name: "anon", Body: pieces[i%len(pieces)]}, false)
		}
	})
}

// ordinaryPieces runs together the bodies of the corpus's published posts,
// file by file and a blank line apart, and cuts them into pieces of n
// characters, dropping the remainder.
func ordinaryPieces(tb testing.TB, n int) []string {
	paths, err := filepath.Glob(filepath.Join("testdata", "filter_corpus", "*.jsonl"))
	if err != nil || len(paths) == 0 {
		tb.Fatalf("no corpus files: %v", err)
	}
	var bodies []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			tb.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			var it corpusItem
			if err := json.Unmarshal([]byte(line), &it); err != nil {
				tb.Fatalf("%s: %v", path, err)
			}
			if it.Expect == "publish" {
				bodies = append(bodies, it.Body)
			}
		}
	}
	text := []rune(strings.Join(bodies, "\n\n"))
	var pieces []string
	for i := 0; i+n <= len(text); i += n {
		pieces = append(pieces, string(text[i:i+n]))
	}
	return pieces
}
