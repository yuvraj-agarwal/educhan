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
	}
	for _, c := range cases {
		// support_seen, so that a post that would show the support message
		// first is seen past it to the hold.
		v := checkContent(contentInput{Body: c.body}, true)
		if v.Outcome != contentHold || v.Hold != c.reason || v.Message != "" || v.Match != "" {
			t.Errorf("%q: got %+v, want held for %s", c.body, v, c.reason)
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
	if v.Outcome != contentBlock || utf8.RuneCountInString(v.Match) > maxMatchLen || !strings.HasPrefix(v.Match, "add me on snap") {
		t.Fatalf("long match: %+v", v)
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
		"⑽":  "(10)",
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
