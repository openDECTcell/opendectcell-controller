// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

func TestInitSecretsCreatesOnceWithTightPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	var out bytes.Buffer
	if err := run([]string{"init-secrets", "--dir", dir}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	read := func(f string) string {
		want := os.FileMode(0o600)
		if f == "db_password" {
			want = 0o644
		}
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(filepath.Join(dir, f))
		if st.Mode().Perm() != want {
			t.Errorf("%s mode %v", f, st.Mode())
		}
		return strings.TrimSpace(string(b))
	}
	mk, pw, dbURL := read("master_key"), read("db_password"), read("database_url")
	if _, err := secret.ParseMasterKey(mk); err != nil {
		t.Errorf("master key: %v", err)
	}
	if !strings.Contains(dbURL, "opendectcell:"+pw+"@db/opendectcell") {
		t.Errorf("database URL does not use the password: %s", dbURL)
	}
	out.Reset()
	if err := run([]string{"init-secrets", "--dir", dir}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || read("master_key") != mk || read("db_password") != pw {
		t.Fatalf("second run changed something: %q", out.String())
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer bad.Close()
	if err := run([]string{"healthcheck", "--url", ok.URL}, nil, nil, nil); err != nil {
		t.Errorf("healthy: %v", err)
	}
	if err := run([]string{"healthcheck", "--url", bad.URL}, nil, nil, nil); err == nil {
		t.Error("unhealthy reported healthy")
	}
}

func TestVersionAndKeygenNeedNoConfig(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"keygen"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.ParseMasterKey(out.String()); err != nil {
		t.Errorf("keygen output: %v", err)
	}
	if err := run([]string{"version"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
}

// With --admin-dir the controller's directory holds a limited role's URL and nothing of the database's
// administrative password; an installation from before keeps its URL and its password.
func TestInitSecretsSplitsTheAdministrativeCredentials(t *testing.T) {
	root := t.TempDir()
	dir, admin := filepath.Join(root, "s"), filepath.Join(root, "a")
	var out bytes.Buffer
	if err := run([]string{"init-secrets", "--dir", dir, "--admin-dir", admin}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	read := func(d, f string) string {
		b, err := os.ReadFile(filepath.Join(d, f))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	adminPW := read(admin, "db_password")
	if u := read(dir, "database_url"); !strings.Contains(u, "opendectcell_app:") || strings.Contains(u, adminPW) {
		t.Errorf("the controller's URL is not a limited role's: %s", u)
	}
	if u := read(admin, "database_admin_url"); !strings.Contains(u, "opendectcell:"+adminPW+"@db/opendectcell") {
		t.Errorf("the admin URL: %s", u)
	}
	for _, f := range []string{"db_password", "database_admin_url"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s is in the controller's directory", f)
		}
	}
	before := read(dir, "database_url")
	out.Reset()
	if err := run([]string{"init-secrets", "--dir", dir, "--admin-dir", admin}, nil, &out, &out); err != nil || out.Len() != 0 || read(dir, "database_url") != before {
		t.Fatalf("second run changed something: %q %v", out.String(), err)
	}

	// An installation from before: db_password and database_url (the owning role) are in the one directory.
	old := filepath.Join(root, "old")
	if err := run([]string{"init-secrets", "--dir", old}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	oldPW, oldURL := read(old, "db_password"), read(old, "database_url")
	newAdmin := filepath.Join(root, "newadmin")
	out.Reset()
	if err := run([]string{"init-secrets", "--dir", old, "--admin-dir", newAdmin}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if read(newAdmin, "db_password") != oldPW {
		t.Error("the database's password was not carried over: the database would not accept the new files")
	}
	if read(old, "database_url") != oldURL {
		t.Error("the existing database_url was replaced")
	}
	if !strings.Contains(out.String(), "note:") || !strings.Contains(out.String(), "owns the database") {
		t.Errorf("no note about the owning role: %q", out.String())
	}
	// The switch: --limited-role replaces that URL, once, and the database's password stays.
	out.Reset()
	if err := run([]string{"init-secrets", "--dir", old, "--admin-dir", newAdmin, "--limited-role"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if u := read(old, "database_url"); !strings.Contains(u, "opendectcell_app:") || strings.Contains(u, oldPW) || u == oldURL {
		t.Errorf("the URL after the switch: %s", u)
	}
	if read(newAdmin, "db_password") != oldPW {
		t.Error("the switch changed the database's password")
	}
	switched := read(old, "database_url")
	out.Reset()
	if err := run([]string{"init-secrets", "--dir", old, "--admin-dir", newAdmin, "--limited-role"}, nil, &out, &out); err != nil || read(old, "database_url") != switched {
		t.Fatalf("a second switch changed the URL: %q %v", out.String(), err)
	}
}

func TestVersionSaysWhetherTheDECTCoreIsIn(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dect-core: ") {
		t.Errorf("version output: %q", out.String())
	}
}
