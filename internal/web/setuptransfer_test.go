// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
	"github.com/openDECTcell/opendectcell-controller/internal/transfer"
)

func setupEnv(t *testing.T) *env {
	return newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.Contacts = &contacts.Store{Pool: s.Pool}
		s.Handsets = &handset.Store{Pool: s.Pool}
		s.SIPAccounts = &sipaccount.Store{Pool: s.Pool, Keyring: kr}
		s.Transfer = &transfer.Store{Pool: s.Pool, Records: &core.Store{Pool: s.Pool, Keyring: kr}, Lines: s.SIPAccounts, Contacts: s.Contacts, Handsets: s.Handsets}
	})
}

func (e *env) setupImport(t *testing.T, c *http.Client, file []byte, fields map[string]string) (*http.Response, string) {
	t.Helper()
	return e.upload2(t, c, "/account/setup/import", file, fields)
}

func (e *env) upload2(t *testing.T, c *http.Client, path string, file []byte, fields map[string]string) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("file", "setup.json")
	_, _ = fw.Write(file)
	_ = w.Close()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, readBody(resp)
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

const keyHex = "00112233445566778899aabbccddeeff"

func TestSetupExportAndImportPages(t *testing.T) {
	e := setupEnv(t)
	var uid uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	kr, _ := db.LoadKeyring(ctx, e.store.Pool, testMK, nil)
	in := core.Imported{IPUI: []byte{0x0f}, Name: "Hall"}
	copy(in.UAK[:], mustHex(keyHex))
	if _, err := (&core.Store{Pool: e.store.Pool, Keyring: kr}).ImportHandset(ctx, uid, in); err != nil {
		t.Fatal(err)
	}
	u := e.userBrowser(t)
	if resp, body := e.get(t, u, "/account/setup"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Export my setup") {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	// the account page links to it
	if _, body := e.get(t, u, "/account"); !strings.Contains(body, `href="/account/setup"`) {
		t.Error("no link from the account page")
	}

	// settings only: plain, no key in it, no passphrase asked
	r := e.post(t, u, "/account/setup/export", url.Values{"handsets": {"on"}})
	plain := readBody(r)
	if r.StatusCode != http.StatusOK || !strings.Contains(plain, `"encrypted": false`) || !strings.Contains(plain, "Hall") || strings.Contains(plain, keyHex) ||
		!strings.Contains(r.Header.Get("Content-Disposition"), "opendectcell-setup-") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("plain export: %d %s", r.StatusCode, plain)
	}
	// contacts have no secrets: ticking "keys and passwords" for them alone asks for nothing and writes a plain file
	if r := e.post(t, u, "/account/setup/export", url.Values{"contacts": {"on"}, "secrets": {"on"}}); r.StatusCode != http.StatusOK || !strings.Contains(readBody(r), `"encrypted": false`) {
		t.Errorf("contacts only with the box ticked: %d", r.StatusCode)
	}
	// nothing chosen
	if r := e.post(t, u, "/account/setup/export", url.Values{}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("nothing chosen: %d", r.StatusCode)
	}

	// with keys: a passphrase, twice, and the password
	good := url.Values{"handsets": {"on"}, "secrets": {"on"}, "passphrase": {"correct horse battery"}, "passphrase2": {"correct horse battery"}, "password": {"user password"}}
	for name, mod := range map[string]func(url.Values){
		"short passphrase": func(v url.Values) { v.Set("passphrase", "short"); v.Set("passphrase2", "short") },
		"different":        func(v url.Values) { v.Set("passphrase2", "correct horse batterx") },
		"wrong password":   func(v url.Values) { v.Set("password", "nope") },
		"no password":      func(v url.Values) { v.Del("password") },
	} {
		v := url.Values{}
		for k, x := range good {
			v[k] = x
		}
		mod(v)
		if r := e.post(t, u, "/account/setup/export", v); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, r.StatusCode)
		} else if body := readBody(r); strings.Contains(body, keyHex) || strings.Contains(body, "correct horse") {
			t.Errorf("%s: the page shows a secret", name)
		}
	}
	r = e.post(t, u, "/account/setup/export", good)
	file := readBody(r)
	if r.StatusCode != http.StatusOK || !strings.Contains(file, `"encrypted": true`) || strings.Contains(file, keyHex) || strings.Contains(file, "Hall") {
		t.Fatalf("encrypted export: %d %.200s", r.StatusCode, file)
	}

	// importing it into a user who has not got the handset: the handset still exists here (it is anna's... the same user), so it is
	// updated, never duplicated; a wrong passphrase says so and changes nothing
	resp, body := e.setupImport(t, u, []byte(file), map[string]string{"passphrase": "wrong horse battery", "mode": "check"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "wrong passphrase") {
		t.Errorf("wrong passphrase: %d", resp.StatusCode)
	}
	resp, body = e.setupImport(t, u, []byte(file), map[string]string{"passphrase": "correct horse battery", "mode": "check"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "What an import would do") || !strings.Contains(body, "Nothing was changed") || !strings.Contains(body, "Hall") {
		t.Fatalf("check: %d %s", resp.StatusCode, body)
	}
	// the import with keys asks for the password
	resp, _ = e.setupImport(t, u, []byte(file), map[string]string{"passphrase": "correct horse battery", "mode": "import"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("import without the password: %d", resp.StatusCode)
	}
	resp, body = e.setupImport(t, u, []byte(file), map[string]string{"passphrase": "correct horse battery", "mode": "import", "password": "user password"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "What the import did") {
		t.Fatalf("import: %d %s", resp.StatusCode, body)
	}
	if body := readBodyOf(t, e, u); strings.Contains(body, keyHex) {
		t.Error("the key is on a page")
	}
	// a file that is not ours
	resp, body = e.setupImport(t, u, []byte(`{"hello":1}`), nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not a opendectcell setup file") {
		t.Errorf("foreign file: %d", resp.StatusCode)
	}
	// someone who is not logged in gets nothing
	if r := e.post(t, e.browser(), "/account/setup/export", good); r.StatusCode == http.StatusOK {
		t.Error("an export without a login")
	}
}

func readBodyOf(t *testing.T, e *env, c *http.Client) string {
	t.Helper()
	resp, err := c.Get(e.srv.URL + "/account/setup")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestSetupLinesFromCSV(t *testing.T) {
	e := setupEnv(t)
	u := e.userBrowser(t)
	resp, body := e.get(t, u, "/account/setup/lines-template.csv")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "label,username,domain,password") || !strings.Contains(resp.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("template: %d %q", resp.StatusCode, body)
	}
	csv := []byte(body + "Work,200,pbx.example.net,pw-work,,,,,tcp\r\n,,,,,,,,\r\nBroken,,pbx.example.net,x,,,,,\r\n")
	// the check writes nothing and shows what would be added; passwords are not on the page
	resp, body = e.upload2(t, u, "/account/setup/lines-import", csv, map[string]string{"mode": "check"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "would be added") || !strings.Contains(body, "Work") || !strings.Contains(body, "row 5") ||
		strings.Contains(body, "pw-work") || strings.Contains(body, "change-me") {
		t.Fatalf("check: %d %s", resp.StatusCode, body)
	}
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM sip_account").Scan(&n)
	if n != 0 {
		t.Fatalf("the check made %d lines", n)
	}
	resp, body = e.upload2(t, u, "/account/setup/lines-import", csv, map[string]string{"mode": "import"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "What the import did") {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM sip_account WHERE owner_user_id IS NOT NULL").Scan(&n) // the template row, Work, and nothing for the two bad rows
	var pw bool
	e.store.Pool.QueryRow(ctx, "SELECT bool_and(password_enc IS NOT NULL) FROM sip_account").Scan(&pw)
	if n != 2 || !pw {
		t.Errorf("%d lines, passwords %v", n, pw)
	}
	// again: nothing new
	e.upload2(t, u, "/account/setup/lines-import", csv, map[string]string{"mode": "import"})
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM sip_account").Scan(&n)
	if n != 2 {
		t.Errorf("a second import made %d lines", n)
	}
	// a file without the columns
	if resp, body := e.upload2(t, u, "/account/setup/lines-import", []byte("a,b\r\n1,2\r\n"), nil); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "username and domain") {
		t.Errorf("no columns: %d", resp.StatusCode)
	}
	if r := e.post(t, e.browser(), "/account/setup/lines-import", url.Values{}); r.StatusCode == http.StatusOK {
		t.Error("an import without a login")
	}
}
