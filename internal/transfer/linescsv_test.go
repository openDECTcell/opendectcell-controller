// SPDX-License-Identifier: AGPL-3.0-or-later

package transfer

import (
	"strings"
	"testing"
)

func TestParseLinesCSV(t *testing.T) {
	got := ParseLinesCSV(strings.NewReader(LinesCSVTemplate))
	if len(got.Skipped) != 0 || len(got.Lines) != 1 {
		t.Fatalf("template: %+v", got)
	}
	l := got.Lines[0]
	if l.Label != "Home" || l.Username != "4930123456" || l.Domain != "sip.example.net" || l.Password != "change-me" || l.DisplayName != "Anna Schmidt" ||
		l.Transport != "udp" || !l.Register || !l.Enabled || l.Ref == "" {
		t.Errorf("line: %+v", l)
	}
	// semicolons, another column order, other names, a byte order mark, an empty row, a bad row, a default label
	in := "\ufeffServer;User;Secret;Protocol\r\npbx.example.net;200;pw;TCP\r\n;;;\r\n;201;pw2;udp\r\nsip.example.net;202;;\r\n"
	got = ParseLinesCSV(strings.NewReader(in))
	if len(got.Lines) != 2 || got.Lines[0].Label != "200@pbx.example.net" || got.Lines[0].Transport != "tcp" || got.Lines[0].Password != "pw" || got.Lines[1].Password != "" {
		t.Fatalf("second file: %+v", got)
	}
	if len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0], "row 4") {
		t.Errorf("skipped: %v", got.Skipped)
	}
	// each line has a ref of its own
	if got.Lines[0].Ref == got.Lines[1].Ref {
		t.Error("two lines share a ref")
	}
	for _, bad := range []string{"", "label,password\r\nx,y\r\n"} {
		if g := ParseLinesCSV(strings.NewReader(bad)); len(g.Lines) != 0 || len(g.Skipped) != 1 {
			t.Errorf("%q: %+v", bad, g)
		}
	}
	// a file with more rows than a setup may hold stops there and says so
	var b strings.Builder
	b.WriteString("username,domain\r\n")
	for i := range MaxLines + 5 {
		b.WriteString("u" + strings.Repeat("x", i%3) + ",d.example\r\n")
	}
	got = ParseLinesCSV(strings.NewReader(b.String()))
	if len(got.Lines) != MaxLines || len(got.Skipped) != 1 {
		t.Errorf("limit: %d lines, %v", len(got.Lines), got.Skipped)
	}
}
