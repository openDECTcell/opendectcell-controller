// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
)

func TestContactPages(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.Contacts = &contacts.Store{Pool: s.Pool} })
	user := e.browser()
	e.login(t, user, "user", "user password")
	if r := e.post(t, user, "/contacts", url.Values{"last_name": {"Meyer"}, "number0": {"030 123"}, "kind0": {"fixed"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("add: %d %s", r.StatusCode, readBody(r))
	}
	if r := e.post(t, user, "/contacts", url.Values{"number0": {"abc"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid: %d", r.StatusCode)
	}
	if r := e.post(t, user, "/contacts", url.Values{"last_name": {"x"}, "shared": {"on"}}); r.StatusCode != http.StatusForbidden {
		t.Errorf("user adding shared: %d", r.StatusCode)
	}
	admin := e.browser()
	e.login(t, admin, "admin", "admin password")
	if r := e.post(t, admin, "/contacts", url.Values{"last_name": {"Reception"}, "number0": {"100"}, "shared": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin shared: %d", r.StatusCode)
	}
	_, page := e.get(t, user, "/contacts")
	if !strings.Contains(page, "Meyer") || !strings.Contains(page, "030123") || !strings.Contains(page, "Reception") {
		t.Fatal("page lacks contacts")
	}
	// A user cannot delete a shared contact; the page says why.
	var shared int64
	e.store.Pool.QueryRow(ctx, "SELECT id FROM contact WHERE owner_user_id IS NULL").Scan(&shared)
	if r := e.post(t, user, "/contacts/"+strconv.FormatInt(shared, 10)+"/delete", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("user deleting shared: %d", r.StatusCode)
	}
}

func (e *env) upload(t *testing.T, c *http.Client, path, name, content string) (*http.Response, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write([]byte(content))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, readBody(resp)
}

// Contacts go out as vCard and CSV (the user's own only) and come in again, without doubles.
func TestContactsImportExport(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.Contacts = &contacts.Store{Pool: s.Pool} })
	user, admin := e.browser(), e.browser()
	e.login(t, user, "user", "user password")
	e.login(t, admin, "admin", "admin password")
	e.post(t, user, "/contacts", url.Values{"last_name": {"Meyer"}, "first_name": {"Anna"}, "number0": {"030 123"}, "kind0": {"work"}, "number1": {"0170 55"}, "kind1": {"mobile"}})
	e.post(t, user, "/contacts", url.Values{"last_name": {"=HYPERLINK(1)"}, "number0": {"1"}})
	e.post(t, admin, "/contacts", url.Values{"last_name": {"Reception"}, "number0": {"100"}, "shared": {"on"}})
	e.post(t, admin, "/contacts", url.Values{"last_name": {"AdminOnly"}, "number0": {"200"}})

	resp, vcf := e.get(t, user, "/contacts/export.vcf")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/vcard") || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("vcf: %d %v", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(vcf, "FN:Anna Meyer") || !strings.Contains(vcf, "TEL;TYPE=WORK,PREF:030123") || !strings.Contains(vcf, "TEL;TYPE=CELL:017055") {
		t.Errorf("vcf: %s", vcf)
	}
	if strings.Contains(vcf, "Reception") || strings.Contains(vcf, "AdminOnly") {
		t.Errorf("the export holds a shared contact or another user's: %s", vcf)
	}
	_, csv := e.get(t, user, "/contacts/export.csv")
	if !strings.Contains(csv, "last_name,first_name,number1,kind1") || !strings.Contains(csv, "Meyer,Anna,030123,work,017055,mobile") || !strings.Contains(csv, "'=HYPERLINK(1)") {
		t.Errorf("csv: %s", csv)
	}
	if resp, _ := e.get(t, e.browser(), "/contacts/export.vcf"); resp.StatusCode == http.StatusOK {
		t.Errorf("export without login")
	}

	// import into a second user: the cards come in, a second import adds nothing
	other := e.browser()
	e.login(t, other, "admin", "admin password")
	_, res := e.upload(t, other, "/contacts/import", "c.vcf", vcf)
	if !strings.Contains(res, "<strong>2 added.</strong>") {
		t.Fatalf("import: %s", res)
	}
	_, again := e.upload(t, other, "/contacts/import", "c.vcf", vcf)
	if !strings.Contains(again, "<strong>0 added.</strong>") || !strings.Contains(again, "2 were already in your list") {
		t.Errorf("a second import: %s", again)
	}
	// the same name with another number is another contact
	_, third := e.upload(t, other, "/contacts/import", "c.vcf", "BEGIN:VCARD\nVERSION:3.0\nN:Meyer;Anna;;;\nTEL;TYPE=HOME:0999\nEND:VCARD\n")
	if !strings.Contains(third, "<strong>1 added.</strong>") {
		t.Errorf("the same name with another number: %s", third)
	}
	_, page := e.get(t, other, "/contacts")
	if !strings.Contains(page, "Meyer") || !strings.Contains(page, "017055") {
		t.Errorf("the imported contact is not in the list")
	}
	// a file that is neither is refused with a reason; a missing file too
	_, bad := e.upload(t, other, "/contacts/import", "x.txt", "hello world")
	if !strings.Contains(bad, "<strong>0 added.</strong>") || !strings.Contains(bad, "no column for the name") {
		t.Errorf("a text file: %s", bad)
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/contacts/import", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if r, err := other.Do(req); err != nil || r.StatusCode == http.StatusOK {
		t.Errorf("an import without a file: %v", r)
	}
}
