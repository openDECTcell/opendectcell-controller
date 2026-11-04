// SPDX-License-Identifier: AGPL-3.0-or-later

package contacts

import (
	"bytes"
	"strings"
	"testing"
)

func first(t *testing.T, im Imported) Contact {
	t.Helper()
	if len(im.Contacts) != 1 {
		t.Fatalf("%d contacts, skipped %q", len(im.Contacts), im.Skipped)
	}
	return im.Contacts[0]
}

func TestParseVCards(t *testing.T) {
	// 3.0: folded line, escapes, types, a group, the BOM
	c := first(t, ParseVCards(strings.NewReader("\uFEFFBEGIN:VCARD\r\nVERSION:3.0\r\nN:Schmidt\\, Jr.;Anna;;;\r\nFN:Anna Schmidt\r\nTEL;TYPE=CELL,VOICE:+49 170 1234567\r\nitem1.TEL;TYPE=WORK:030 / 123-45\r\n 67\r\nTEL;TYPE=HOME,PREF:(030) 555 0100\r\nEND:VCARD\r\n")))
	if c.LastName != "Schmidt, Jr." || c.FirstName != "Anna" || len(c.Numbers) != 3 {
		t.Fatalf("3.0: %+v", c)
	}
	for i, want := range []Number{{"+491701234567", "mobile", false}, {"030123456 7"[:0] + "0301234567", "work", false}, {"0305550100", "fixed", false}} {
		if c.Numbers[i] != want {
			t.Errorf("number %d: %+v, want %+v", i, c.Numbers[i], want)
		}
	}
	// 2.1 with bare types, and a card with only an FN
	c = first(t, ParseVCards(strings.NewReader("BEGIN:VCARD\nVERSION:2.1\nFN:Anna Maria Schmidt\nTEL;CELL;VOICE:0170 55\nEND:VCARD\n")))
	if c.LastName != "Schmidt" || c.FirstName != "Anna Maria" || c.Numbers[0].Kind != "mobile" || c.Numbers[0].Number != "017055" {
		t.Errorf("2.1 and FN only: %+v", c)
	}
	// 4.0: a tel: URI with a quoted type list
	c = first(t, ParseVCards(strings.NewReader("BEGIN:VCARD\nVERSION:4.0\nN:Meier;Ben;;;\nTEL;VALUE=uri;TYPE=\"work,voice\":tel:+4930123;ext=5\nEND:VCARD\n")))
	if c.Numbers[0].Number != "+4930123" || c.Numbers[0].Kind != "work" {
		t.Errorf("4.0: %+v", c)
	}
	// what cannot be used is left out, and said so
	im := ParseVCards(strings.NewReader("BEGIN:VCARD\nN:A;B\nTEL:abc\nEND:VCARD\nBEGIN:VCARD\nN:C;D\nEND:VCARD\nBEGIN:VCARD\nTEL:123\nEND:VCARD\nBEGIN:VCARD\nN:E;F\nTEL:1\nTEL:2\nTEL:3\nTEL:4\nTEL:5\nTEL:6\nEND:VCARD\n"))
	if len(im.Contacts) != 1 || len(im.Contacts[0].Numbers) != 5 || len(im.Skipped) != 5 {
		t.Errorf("imported %d, skipped %q", len(im.Contacts), im.Skipped)
	}
	if im := ParseVCards(strings.NewReader("hello")); len(im.Contacts) != 0 || len(im.Skipped) != 1 {
		t.Errorf("not a vCard: %+v", im)
	}
}

func TestWriteVCardsRoundTrips(t *testing.T) {
	in := []*Contact{
		{LastName: "Müller; Söhne", FirstName: "Anna, Maria", Numbers: []Number{{"+49301", "work", true}, {"0170555", "mobile", false}}},
		{LastName: strings.Repeat("Lang", 16), FirstName: "X", Numbers: []Number{{"1", "other", true}}},
	}
	var b bytes.Buffer
	if err := WriteVCards(&b, in); err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(b.String(), "\r\n") {
		if len(l) > 75 {
			t.Errorf("a line of %d octets: %q", len(l), l)
		}
	}
	im := ParseVCards(&b)
	if len(im.Contacts) != 2 || len(im.Skipped) != 0 {
		t.Fatalf("read back %d, skipped %q", len(im.Contacts), im.Skipped)
	}
	got := im.Contacts[0]
	if got.LastName != "Müller; Söhne" || got.FirstName != "Anna, Maria" || got.Numbers[0].Kind != "work" || got.Numbers[1].Kind != "mobile" || got.Numbers[1].Number != "0170555" {
		t.Errorf("round trip: %+v", got)
	}
	if im.Contacts[1].LastName != strings.Repeat("Lang", 16) {
		t.Errorf("a folded name came back as %q", im.Contacts[1].LastName)
	}
}

func TestParseCSV(t *testing.T) {
	// our own export
	var b bytes.Buffer
	WriteCSV(&b, []*Contact{{LastName: "Schmidt", FirstName: "Anna", Numbers: []Number{{"+49301", "work", true}, {"0170", "mobile", false}}}}, func(s string) string { return s })
	c := first(t, ParseCSV(&b))
	if c.LastName != "Schmidt" || len(c.Numbers) != 2 || c.Numbers[0].Kind != "work" || c.Numbers[1].Kind != "mobile" {
		t.Errorf("own export: %+v", c)
	}
	// Outlook, German Excel style: semicolons, a BOM, kinds from the headings, a fax column ignored
	c = first(t, ParseCSV(strings.NewReader("\uFEFFVorname;Nachname;Mobiltelefon;Telefon privat;Telefon geschäftlich;Fax geschäftlich\nAnna;Schmidt;0170 111;030 222;030 333;030 444\n")))
	if c.FirstName != "Anna" || c.LastName != "Schmidt" || len(c.Numbers) != 3 || c.Numbers[0].Kind != "mobile" || c.Numbers[1].Kind != "fixed" || c.Numbers[2].Kind != "work" {
		t.Errorf("outlook: %+v", c)
	}
	// Google: the number and its label are two columns; a name in one column
	c = first(t, ParseCSV(strings.NewReader("Name,Phone 1 - Label,Phone 1 - Value,Phone 2 - Label,Phone 2 - Value\nAnna Schmidt,Mobile,+4917011,Work,030 5 ::: 030 6\n")))
	if c.FirstName != "Anna" || c.LastName != "Schmidt" || len(c.Numbers) != 3 || c.Numbers[0].Kind != "mobile" || c.Numbers[1].Kind != "work" || c.Numbers[2].Number != "0306" {
		t.Errorf("google: %+v", c)
	}
	// nothing usable says why
	for in, want := range map[string]string{"": "empty", "a,b\n1,2\n": "no column for the name", "Name,Note\nAnna,x\n": "telephone number"} {
		im := ParseCSV(strings.NewReader(in))
		if len(im.Contacts) != 0 || len(im.Skipped) != 1 || !strings.Contains(im.Skipped[0], want) {
			t.Errorf("%q: %d contacts, %q", in, len(im.Contacts), im.Skipped)
		}
	}
}

// A line longer than 75 octets is folded, and the fold is not in the middle of a UTF-8 character.
func TestVCardLinesAreFolded(t *testing.T) {
	name := strings.Repeat("Äpfel", 20) // 120 octets
	var b bytes.Buffer
	if err := WriteVCards(&b, []*Contact{{LastName: name, FirstName: "X", Numbers: []Number{{"1", "other", true}}}}); err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(b.String(), "\r\n ")) < 2 {
		t.Errorf("nothing was folded: %q", b.String())
	}
	for _, l := range strings.Split(b.String(), "\r\n") {
		if len(l) > 75 {
			t.Errorf("a line of %d octets", len(l))
		}
	}
	joined := unfold(strings.NewReader(b.String()))
	if joined[2] != "N:"+name+";X;;;" {
		t.Errorf("unfolded: %q", joined[2])
	}
}
