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
