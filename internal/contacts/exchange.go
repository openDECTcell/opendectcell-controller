// SPDX-License-Identifier: AGPL-3.0-or-later

package contacts

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// This file reads and writes contacts as vCard (versions 2.1, 3.0 and 4.0 in, 3.0 out: the one every address book
// reads) and as CSV (the columns of Outlook and Google read in, our own written out). It touches no database: the
// caller stores what it returns.

// Imported is what a file held: the contacts that could be made of it and what was left out, in words.
type Imported struct {
	Contacts []Contact
	Skipped  []string
}

// MaxImport bounds a file: more contacts than this are not read.
const MaxImport = 2000

func (im *Imported) skip(format string, a ...any) {
	im.Skipped = append(im.Skipped, fmt.Sprintf(format, a...))
}

// add keeps a contact if it has a name and a number; one without either is of no use on a handset.
func (im *Imported) add(c Contact, label string) {
	c.LastName, c.FirstName = strings.TrimSpace(c.LastName), strings.TrimSpace(c.FirstName)
	var good []Number
	for _, n := range c.Numbers {
		n.Number = normalise(n.Number)
		if !numberRE.MatchString(n.Number) {
			im.skip("%s: the number %q is not a telephone number", label, n.Number)
			continue
		}
		good = append(good, n)
	}
	if len(good) > MaxNumbers {
		im.skip("%s: only the first %d numbers were kept", label, MaxNumbers)
		good = good[:MaxNumbers]
	}
	c.Numbers = good
	switch {
	case c.LastName == "" && c.FirstName == "":
		im.skip("%s: no name", label)
	case len(c.LastName) > 64 || len(c.FirstName) > 64:
		im.skip("%s: a name longer than 64 characters", label)
	case len(c.Numbers) == 0:
		im.skip("%s: no number", c.FirstName+" "+c.LastName)
	case len(im.Contacts) >= MaxImport:
		im.skip("%s: more than %d contacts in the file", label, MaxImport)
	default:
		im.Contacts = append(im.Contacts, c)
	}
}

// ---- vCard

// kindOfTypes maps the TYPE values of a vCard TEL to one of our kinds.
func kindOfTypes(types []string) string {
	has := func(words ...string) bool {
		for _, t := range types {
			for _, w := range words {
				if strings.EqualFold(t, w) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("cell", "mobile", "car", "pager"):
		return "mobile"
	case has("work"):
		return "work"
	case has("home"):
		return "fixed"
	}
	return "other"
}

// unfold joins the lines a vCard folded (a line that starts with a space or a tab continues the one before).
func unfold(r io.Reader) []string {
	var lines []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if first {
			l = strings.TrimPrefix(l, "\uFEFF")
			first = false
		}
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] += l[1:]
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

// splitUnescaped splits at sep where a backslash does not hold it ("Müller\\; Söhne;Anna" is two fields).
func splitUnescaped(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case sep:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func vcUnescape(s string) string {
	return strings.NewReplacer(`\n`, " ", `\N`, " ", `\,`, ",", `\;`, ";", `\:`, ":", `\\`, `\`).Replace(s)
}

// splitName takes "Anna Maria Schmidt" for a card that has an FN and no N: the last word is the family name.
func splitName(full string) (last, first string) {
	full = strings.TrimSpace(full)
	if i := strings.LastIndexByte(full, ' '); i > 0 {
		return strings.TrimSpace(full[i+1:]), strings.TrimSpace(full[:i])
	}
	return full, ""
}

// ParseVCards reads the cards of a .vcf file.
func ParseVCards(r io.Reader) Imported {
	var im Imported
	var cur *Contact
	var fn string
	var hasN bool
	n := 0
	for _, l := range unfold(r) {
		up := strings.ToUpper(strings.TrimSpace(l))
		switch up {
		case "BEGIN:VCARD":
			cur, fn, hasN = &Contact{}, "", false
			n++
			continue
		case "END:VCARD":
			if cur != nil {
				if !hasN && fn != "" {
					cur.LastName, cur.FirstName = splitName(fn)
				}
				im.add(*cur, fmt.Sprintf("card %d", n))
			}
			cur = nil
			continue
		}
		if cur == nil {
			continue
		}
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		parts := strings.Split(name, ";")
		prop := strings.ToUpper(strings.TrimSpace(parts[0]))
		if i := strings.LastIndexByte(prop, '.'); i >= 0 { // a group: "item1.TEL"
			prop = prop[i+1:]
		}
		switch prop {
		case "N":
			f := splitUnescaped(value, ';')
			cur.LastName = vcUnescape(f[0])
			if len(f) > 1 {
				cur.FirstName = vcUnescape(f[1])
			}
			hasN = cur.LastName != "" || cur.FirstName != ""
		case "FN":
			fn = vcUnescape(value)
		case "TEL":
			var types []string
			for _, p := range parts[1:] {
				k, v, has := strings.Cut(p, "=")
				if !has { // version 2.1: a bare type
					types = append(types, k)
					continue
				}
				if strings.EqualFold(k, "TYPE") {
					types = append(types, strings.Split(strings.Trim(v, `"`), ",")...)
				}
			}
			num := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(value), "tel:"), "TEL:")
			if i := strings.IndexAny(num, ";,"); i >= 0 { // an extension of a tel: URI
				num = num[:i]
			}
			cur.Numbers = append(cur.Numbers, Number{Number: num, Kind: kindOfTypes(types)})
		}
	}
	if n == 0 {
		im.skip("no vCard in the file")
	}
	return im
}

func vcEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`, "\r", "").Replace(s)
}

// fold breaks a line at 75 octets, as RFC 6350 asks, continuing with a space.
func fold(l string) string {
	if len(l) <= 75 {
		return l
	}
	var b strings.Builder
	for len(l) > 75 {
		cut := 75
		for cut > 0 && l[cut]&0xC0 == 0x80 { // not in the middle of a UTF-8 character
			cut--
		}
		b.WriteString(l[:cut] + "\r\n ")
		l = l[cut:]
	}
	b.WriteString(l)
	return b.String()
}

// WriteVCards writes contacts as vCard 3.0.
func WriteVCards(w io.Writer, cs []*Contact) error {
	typeOf := map[string]string{"mobile": "CELL", "fixed": "HOME", "work": "WORK", "other": "VOICE"}
	for _, c := range cs {
		full := strings.TrimSpace(c.FirstName + " " + c.LastName)
		lines := []string{"BEGIN:VCARD", "VERSION:3.0", "N:" + vcEscape(c.LastName) + ";" + vcEscape(c.FirstName) + ";;;", "FN:" + vcEscape(full)}
		for _, n := range c.Numbers {
			t := typeOf[n.Kind]
			if t == "" {
				t = "VOICE"
			}
			if n.Default {
				t += ",PREF"
			}
			lines = append(lines, "TEL;TYPE="+t+":"+n.Number)
		}
		lines = append(lines, "END:VCARD")
		for _, l := range lines {
			if _, err := io.WriteString(w, fold(l)+"\r\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- CSV

// csvKind says which kind of number a column holds, from its heading ("Mobile Phone", "Business Phone", "Handy").
func csvKind(h string) string {
	h = strings.ToLower(h)
	switch {
	case containsAny(h, "mobile", "cell", "handy", "mobil"):
		return "mobile"
	case containsAny(h, "work", "business", "office", "arbeit", "firma", "dienst", "geschäft"):
		return "work"
	case containsAny(h, "home", "private", "privat", "fixed", "festnetz", "zuhause"):
		return "fixed"
	}
	return "other"
}

func containsAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

var googlePhone = regexp.MustCompile(`^phone (\d+) - value$`)

// CSVColumns are the columns ExportCSV writes.
var CSVColumns = []string{"last_name", "first_name", "number1", "kind1", "number2", "kind2", "number3", "kind3", "number4", "kind4", "number5", "kind5"}

// ParseCSV reads a CSV of contacts: our own export, or what Outlook and Google write (their headings are
// recognised; a column that holds a number is any whose heading says phone, mobile, tel, handy or telefon). Comma and
// semicolon both separate.
func ParseCSV(r io.Reader) Imported {
	var im Imported
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(4096)
	hs := strings.TrimPrefix(string(head), "\uFEFF")
	if i := strings.IndexByte(hs, '\n'); i >= 0 {
		hs = hs[:i]
	}
	cr := csv.NewReader(br)
	cr.Comma = ','
	if strings.Count(hs, ";") > strings.Count(hs, ",") {
		cr.Comma = ';'
	}
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	rows, err := cr.ReadAll()
	if err != nil && len(rows) == 0 {
		im.skip("the file is not a readable CSV: %v", err)
		return im
	}
	if len(rows) == 0 {
		im.skip("the file is empty")
		return im
	}
	header := make([]string, len(rows[0]))
	for i, h := range rows[0] {
		header[i] = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))
	}
	col := func(names ...string) int {
		for _, n := range names {
			for i, h := range header {
				if h == n {
					return i
				}
			}
		}
		return -1
	}
	last := col("last_name", "last name", "family name", "surname", "nachname", "name")
	first := col("first_name", "first name", "given name", "vorname")
	full := col("display name", "full name", "file as", "name")
	type numCol struct {
		value, kind int
		heading     string
	}
	var nums []numCol
	for i, h := range header {
		if m := googlePhone.FindStringSubmatch(h); m != nil {
			nums = append(nums, numCol{value: i, kind: col("phone " + m[1] + " - label"), heading: h})
			continue
		}
		if m := regexp.MustCompile(`^number(\d)$`).FindStringSubmatch(h); m != nil { // our own export
			nums = append(nums, numCol{value: i, kind: col("kind" + m[1]), heading: ""})
			continue
		}
		if strings.HasSuffix(h, " - label") || strings.HasSuffix(h, " - type") {
			continue
		}
		if containsAny(h, "phone", "mobile", "tel", "handy", "fax") && !strings.Contains(h, "fax") && !strings.Contains(h, "label") {
			nums = append(nums, numCol{value: i, kind: -1, heading: h})
		}
	}
	if last < 0 && first < 0 && full < 0 {
		im.skip("no column for the name (last name, first name or name)")
		return im
	}
	if len(nums) == 0 {
		im.skip("no column holds a telephone number")
		return im
	}
	get := func(row []string, i int) string {
		if i < 0 || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	for ri, row := range rows[1:] {
		c := Contact{LastName: get(row, last), FirstName: get(row, first)}
		if full >= 0 && c.FirstName == "" && (last < 0 || last == full) { // a single name column: "Anna Schmidt"
			c.LastName, c.FirstName = splitName(get(row, full))
		}
		for _, nc := range nums {
			v := get(row, nc.value)
			if v == "" {
				continue
			}
			kind := ""
			if nc.kind >= 0 {
				kind = get(row, nc.kind)
				if kind == "mobile" || kind == "fixed" || kind == "work" || kind == "other" {
					// our own export
				} else {
					kind = csvKind(kind)
				}
			} else {
				kind = csvKind(nc.heading)
			}
			for _, one := range strings.Split(v, ":::") { // Google joins several numbers of one column with " ::: "
				c.Numbers = append(c.Numbers, Number{Number: strings.TrimSpace(one), Kind: kind})
			}
		}
		im.add(c, fmt.Sprintf("row %d", ri+2))
	}
	return im
}

// WriteCSV writes contacts in our columns. The caller guards against formulas (text a spreadsheet would run).
func WriteCSV(w io.Writer, cs []*Contact, safe func(string) string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(CSVColumns); err != nil {
		return err
	}
	for _, c := range cs {
		row := []string{safe(c.LastName), safe(c.FirstName)}
		for i := 0; i < MaxNumbers; i++ {
			if i < len(c.Numbers) {
				row = append(row, safe(c.Numbers[i].Number), c.Numbers[i].Kind)
			} else {
				row = append(row, "", "")
			}
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// Same says whether a is a contact the user already has as b: the same name and a number in common.
func Same(a, b Contact) bool {
	if !strings.EqualFold(a.LastName, b.LastName) || !strings.EqualFold(a.FirstName, b.FirstName) {
		return false
	}
	for _, x := range a.Numbers {
		for _, y := range b.Numbers {
			if x.Number == y.Number {
				return true
			}
		}
	}
	return false
}
