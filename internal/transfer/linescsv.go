// SPDX-License-Identifier: AGPL-3.0-or-later

package transfer

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// LinesCSVTemplate is the file a person starts from: the headings the import reads, and one example row.
const LinesCSVTemplate = "label,username,domain,password,auth_username,display_name,registrar,outbound_proxy,transport\r\n" +
	"Home,4930123456,sip.example.net,change-me,,Anna Schmidt,,,udp\r\n"

// bom is the byte order mark some spreadsheets write at the start of a file.
const bom = "\xef\xbb\xbf"

// LinesCSV is the result of reading a spreadsheet of VoIP lines.
type LinesCSV struct {
	Lines   []Line
	Skipped []string // rows that could not be read, each with its row number and the reason
}

// ParseLinesCSV reads VoIP lines from a CSV (comma or semicolon; the first row names the columns, in any order). The columns are
// label, username and domain (those two are needed), password, auth_username, display_name, registrar, outbound_proxy and
// transport (udp, tcp or tls); the usual other names (user, server, proxy, ...) are understood. A line takes the defaults of a new
// line for everything the file does not say, and is on and registering. Nothing is validated beyond what is needed to read a row: the import
// does the rest, as it does for any line.
func ParseLinesCSV(r io.Reader) LinesCSV {
	var out LinesCSV
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(4096)
	hs := strings.TrimPrefix(string(head), bom)
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
		out.Skipped = append(out.Skipped, "the file is not a readable CSV")
		return out
	}
	if len(rows) == 0 {
		out.Skipped = append(out.Skipped, "the file is empty")
		return out
	}
	header := make([]string, len(rows[0]))
	for i, h := range rows[0] {
		header[i] = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, bom)))
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
	cLabel, cUser, cDomain := col("label", "name", "line name"), col("username", "user", "number", "account"), col("domain", "server", "host", "realm")
	cPass, cAuth, cDisp := col("password", "secret", "passwort"), col("auth_username", "auth username", "auth", "authuser"), col("display_name", "display name", "display")
	cReg, cProxy, cTrans := col("registrar", "registrar address"), col("outbound_proxy", "proxy", "outbound proxy"), col("transport", "protocol")
	if cUser < 0 || cDomain < 0 {
		out.Skipped = append(out.Skipped, "the first row must name the columns, at least username and domain")
		return out
	}
	get := func(row []string, c int) string {
		if c < 0 || c >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[c])
	}
	for i, row := range rows[1:] {
		n := i + 2
		if len(out.Lines) >= MaxLines {
			out.Skipped = append(out.Skipped, fmt.Sprintf("from row %d: at most %d lines in one file", n, MaxLines))
			break
		}
		user, domain := get(row, cUser), get(row, cDomain)
		if user == "" && domain == "" && strings.TrimSpace(strings.Join(row, "")) == "" {
			continue // an empty row
		}
		if user == "" || domain == "" {
			out.Skipped = append(out.Skipped, fmt.Sprintf("row %d: username and domain are needed", n))
			continue
		}
		label := get(row, cLabel)
		if label == "" {
			label = user + "@" + domain
		}
		l := Line{Ref: "c" + strconv.Itoa(n), Label: label, Username: user, Domain: domain, Password: get(row, cPass), AuthUsername: get(row, cAuth),
			DisplayName: get(row, cDisp), Registrar: get(row, cReg), OutboundProxy: get(row, cProxy), Transport: strings.ToLower(get(row, cTrans)),
			Register: true, Enabled: true}
		out.Lines = append(out.Lines, l)
	}
	return out
}
